// Package httpapi exposes the versioned web console API.
package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	webarchive "github.com/dbehnke/opusref/internal/webapp/archive"
	"github.com/dbehnke/opusref/internal/webapp/auth"
	"github.com/dbehnke/opusref/internal/webapp/gateway"
	"github.com/dbehnke/opusref/internal/webapp/limit"
	"github.com/dbehnke/opusref/internal/webapp/passkey"
	reflectormonitor "github.com/dbehnke/opusref/internal/webapp/reflector"
	wsprotocol "github.com/dbehnke/opusref/internal/webapp/socket"
	"github.com/dbehnke/opusref/internal/webapp/store"
	"github.com/dbehnke/opusref/pkg/wire"
)

const CookieName = "__Host-opusref_session"

type Config struct {
	PublicOrigin                 string
	OpenAccess                   bool
	SessionIdle, SessionAbsolute time.Duration
	MaxSessions                  int
	Argon2                       auth.Params
	Assets                       http.Handler
	LiveHub                      *gateway.LiveHub
	PTT                          *gateway.PTTManager
	LiveQueuePackets             int
	LiveQueueBytes               int
	PlaybackQueuePackets         int
	PlaybackQueueBytes           int
	ControlQueueMessages         int
	MaxPlaybacks                 int
	MaxConcurrentHashes          int
	Passkeys                     *passkey.Manager
	TrustedProxyCIDRs            []string
	PasswordBlocklist            map[string]struct{}
	ReflectorMonitor             *reflectormonitor.Client
	MonitorStaleAfter            time.Duration
	Archives                     *webarchive.Service
	MaxWebSockets                int
	MaxWebSocketsPerSession      int
	ReadyCheck                   func() bool
}
type Server struct {
	cfg                                    Config
	store                                  *store.Store
	accepting, dependenciesReady, draining atomic.Bool
	public, monitor                        http.Handler
	hashSlots, hashQueue                   chan struct{}
	dummyPasswordHash                      string
	limiter                                *limit.Limiter
	trustedProxies                         []*net.IPNet
	wsMu                                   sync.Mutex
	wsActive                               int
	wsBySession                            map[string]int
	shutdown                               chan struct{}
	shutdownOnce                           sync.Once
	channelMu                              sync.Mutex
	channelUsed                            map[uint64]struct{}
	revocationMu                           sync.Mutex
	revocationNext                         uint64
	revocations                            map[uint64]socketRevocation
	playbackSlots                          chan struct{}
	telemetry                              *webTelemetry
}
type socketRevocation struct {
	sessionID, userID string
	revoke            func()
}

func New(cfg Config, state *store.Store) *Server {
	concurrent := cfg.MaxConcurrentHashes
	if concurrent <= 0 {
		concurrent = 2
	}
	maxPlaybacks := cfg.MaxPlaybacks
	if maxPlaybacks <= 0 {
		maxPlaybacks = 50
	}
	dummy, _ := auth.HashPassword("dummy authentication workload", cfg.Argon2)
	s := &Server{cfg: cfg, store: state, hashSlots: make(chan struct{}, concurrent), hashQueue: make(chan struct{}, 16), dummyPasswordHash: dummy, limiter: limit.New(), wsBySession: map[string]int{}, shutdown: make(chan struct{}), channelUsed: map[uint64]struct{}{}, revocations: map[uint64]socketRevocation{}, playbackSlots: make(chan struct{}, maxPlaybacks), telemetry: newWebTelemetry()}
	for _, value := range cfg.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(value)
		if err == nil {
			s.trustedProxies = append(s.trustedProxies, network)
		}
	}
	s.accepting.Store(true)
	s.RefreshReadiness(context.Background())
	s.public = s.security(s.publicMux())
	s.monitor = s.security(s.monitorMux())
	return s
}
func (s *Server) PublicHandler() http.Handler  { return s.public }
func (s *Server) MonitorHandler() http.Handler { return s.monitor }
func (s *Server) SetReady(v bool)              { s.accepting.Store(v) }
func (s *Server) BeginDrain() {
	s.accepting.Store(false)
	s.draining.Store(true)
}
func (s *Server) Shutdown() {
	s.BeginDrain()
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}
func (s *Server) RefreshReadiness(ctx context.Context) bool {
	ready := s.cfg.ReadyCheck == nil || s.cfg.ReadyCheck()
	if ready {
		n, err := s.store.EnabledAdminCount(ctx)
		ready = err == nil && n > 0
	}
	if ready && s.cfg.ReflectorMonitor != nil {
		snapshot, ok := s.cfg.ReflectorMonitor.Snapshot()
		ready = ok && snapshot.Ready && s.cfg.ReflectorMonitor.Fresh(s.cfg.MonitorStaleAfter)
	}
	s.dependenciesReady.Store(ready)
	return ready
}
func (s *Server) RecordArchive(action, result string) {
	if action == "quota" {
		s.telemetry.inc("opusrefweb_archive_alerts_total", result)
		s.telemetry.setQuota(result == "full")
		return
	}
	s.telemetry.inc("opusrefweb_archive_total", action, result)
	if action == "recover" && (result == "failure" || result == "partial") {
		s.telemetry.inc("opusrefweb_archive_alerts_total", "recovery")
		s.telemetry.event("archive_recovery", "warning", "Archive recovery found an anomaly that requires operator review.")
	}
	if result == "failure" || result == "partial" {
		s.telemetry.event("archive_"+result, map[bool]string{true: "error", false: "warning"}[result == "failure"], "An archive lifecycle operation requires operator attention.")
	}
}
func (s *Server) RecordReconnect(client, result string) {
	s.telemetry.inc("opusrefweb_reconnect_total", client, result)
	if result == "failure" {
		s.telemetry.event("reflector_reconnect", "warning", "A reflector client reconnect attempt failed.")
	}
}
func (s *Server) publicMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /api/v1/session", s.session)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/passkey/options", s.passkeyLoginOptions)
	mux.HandleFunc("POST /api/v1/auth/passkey/verify", s.passkeyLoginVerify)
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireSession(s.logout))
	mux.HandleFunc("POST /api/v1/me/reauth/password", s.requireSession(s.reauthPassword))
	mux.HandleFunc("POST /api/v1/me/reauth/passkey/options", s.requireSession(s.passkeyReauthOptions))
	mux.HandleFunc("POST /api/v1/me/reauth/passkey/verify", s.requireSession(s.passkeyReauthVerify))
	mux.HandleFunc("PUT /api/v1/me/password", s.requireSession(s.changePassword))
	mux.HandleFunc("GET /api/v1/me/sessions", s.requireSession(s.listSessions))
	mux.HandleFunc("DELETE /api/v1/me/sessions/{id}", s.requireSession(s.deleteSession))
	mux.HandleFunc("GET /api/v1/me/passkeys", s.requireSession(s.passkeysUnavailable))
	mux.HandleFunc("POST /api/v1/me/passkeys/options", s.requireFullSession(s.passkeyEnrollOptions))
	mux.HandleFunc("POST /api/v1/me/passkeys/verify", s.requireFullSession(s.passkeyEnrollVerify))
	mux.HandleFunc("PATCH /api/v1/me/passkeys/{id}", s.requireFullSession(s.renamePasskey))
	mux.HandleFunc("DELETE /api/v1/me/passkeys/{id}", s.requireFullSession(s.deletePasskey))
	mux.HandleFunc("GET /api/v1/recordings", s.requireFullSession(s.recordings))
	mux.HandleFunc("GET /api/v1/recordings/{id}", s.requireFullSession(s.recording))
	mux.HandleFunc("DELETE /api/v1/admin/recordings/{id}", s.requireAdmin(s.deleteRecording))
	mux.HandleFunc("GET /api/v1/admin/accounts", s.requireAdmin(s.listAccounts))
	mux.HandleFunc("POST /api/v1/admin/accounts", s.requireAdmin(s.createAccount))
	mux.HandleFunc("PATCH /api/v1/admin/accounts/{id}", s.requireAdmin(s.updateAccount))
	mux.HandleFunc("PUT /api/v1/admin/accounts/{id}/password", s.requireAdmin(s.resetAccountPassword))
	mux.HandleFunc("POST /api/v1/admin/accounts/{id}/sessions/revoke", s.requireAdmin(s.revokeAccountSessions))
	mux.HandleFunc("DELETE /api/v1/admin/accounts/{id}", s.requireAdmin(s.deleteAccount))
	mux.HandleFunc("DELETE /api/v1/admin/accounts/{id}/passkeys", s.requireAdmin(s.clearAccountPasskeys))
	mux.HandleFunc("GET /api/v1/admin/audit", s.requireAdmin(s.listAudit))
	mux.HandleFunc("GET /api/v1/admin/clients", s.requireAdmin(s.adminClients))
	mux.HandleFunc("GET /api/v1/admin/events", s.requireAdmin(s.adminEvents))
	mux.HandleFunc("GET /api/v1/public/status", s.publicStatus)
	mux.HandleFunc("GET /api/v1/ws", s.webSocket)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || strings.HasPrefix(r.URL.Path, "/api/") || s.cfg.Assets == nil {
			mux.ServeHTTP(w, r)
			return
		}
		s.cfg.Assets.ServeHTTP(w, r)
	})
}

type socketOutput struct {
	kind              websocket.MessageType
	data              []byte
	timestamp         uint32
	playback          bool
	generation        uint64
	packetIndex       int64
	sequence, elapsed uint32
}
type mediaOutputQueue struct {
	items           chan socketOutput
	mu              sync.Mutex
	bytes, maxBytes int
	timestamps      []uint32
}

func currentPlaybackOutput(item socketOutput, generation uint64) bool {
	return !item.playback || item.generation == generation
}

func newMediaOutputQueue(packets, maxBytes int) *mediaOutputQueue {
	if packets <= 0 {
		packets = 64
	}
	if maxBytes <= 0 {
		maxBytes = packets * 1200
	}
	return &mediaOutputQueue{items: make(chan socketOutput, packets), maxBytes: maxBytes}
}
func (q *mediaOutputQueue) enqueue(item socketOutput) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	spanTooLarge := len(q.timestamps) > 0 && item.timestamp-q.timestamps[0] > 24_000
	if spanTooLarge || q.bytes+len(item.data) > q.maxBytes {
		return false
	}
	select {
	case q.items <- item:
		q.timestamps = append(q.timestamps, item.timestamp)
		q.bytes += len(item.data)
		return true
	default:
		return false
	}
}
func (q *mediaOutputQueue) taken(item socketOutput) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.bytes -= len(item.data)
	if q.bytes < 0 {
		q.bytes = 0
	}
	if len(q.timestamps) > 0 {
		q.timestamps = q.timestamps[1:]
	}
}
func (q *mediaOutputQueue) discard() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) > 0 {
		<-q.items
	}
	q.bytes = 0
	q.timestamps = q.timestamps[:0]
}

type websocketMessageWriter interface {
	Write(context.Context, websocket.MessageType, []byte) error
}

func writeSocketOutputs(ctx context.Context, conn websocketMessageWriter, output <-chan socketOutput, liveOutput, playbackOutput *mediaOutputQueue, generation *atomic.Uint64, playbackWritten func(socketOutput)) error {
	var pendingMedia *socketOutput
	for {
		var item socketOutput
		select {
		case <-ctx.Done():
			return ctx.Err()
		case item = <-output:
		default:
			if pendingMedia != nil {
				item = *pendingMedia
				pendingMedia = nil
			} else {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case item = <-output:
				case item = <-liveOutput.items:
					liveOutput.taken(item)
				case item = <-playbackOutput.items:
					playbackOutput.taken(item)
				}
				if item.kind == websocket.MessageBinary {
					select {
					case control := <-output:
						copy := item
						pendingMedia = &copy
						item = control
					default:
					}
				}
			}
		}
		if !currentPlaybackOutput(item, generation.Load()) {
			continue
		}
		writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		err := conn.Write(writeCtx, item.kind, item.data)
		stop()
		if err != nil {
			return err
		}
		if item.playback && item.generation == generation.Load() {
			playbackWritten(item)
		}
	}
}

func (s *Server) webSocket(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.Allow("ws_ip", s.clientAddress(r), 10, time.Minute, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if r.Header.Get("Origin") != s.cfg.PublicOrigin {
		writeError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	session, sessionErr := s.requestSession(r)
	if !s.cfg.OpenAccess && sessionErr != nil {
		writeError(w, http.StatusUnauthorized, "authentication_required")
		return
	}
	sessionKey := ""
	if sessionErr == nil {
		sessionKey = session.ID
	}
	if !s.acquireSocket(sessionKey) {
		writeError(w, http.StatusTooManyRequests, "websocket_limit")
		return
	}
	defer s.releaseSocket(sessionKey)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(wsprotocol.MaxControlMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
		case <-s.shutdown:
			_ = conn.Close(4410, "server_restart")
			cancel()
		}
	}()
	helloCtx, helloCancel := context.WithTimeout(ctx, 5*time.Second)
	kind, data, err := conn.Read(helloCtx)
	helloCancel()
	if err != nil {
		_ = conn.Close(4408, "hello_timeout")
		return
	}
	if kind != websocket.MessageText {
		_ = conn.Close(4400, "hello_required")
		return
	}
	hello, err := wsprotocol.DecodeControl(data, wsprotocol.ClientToServer)
	if err != nil || hello.Type != "hello" {
		_ = conn.Close(4400, "hello_required")
		return
	}
	var body struct {
		Audio struct {
			Encoder     bool `json:"encoder"`
			Decoder     bool `json:"decoder"`
			ContextRate int  `json:"context_rate"`
		} `json:"audio"`
		CSRF string `json:"csrf_token"`
	}
	if err = decodeBody(hello.Body, &body); err != nil || !body.Audio.Encoder || !body.Audio.Decoder || body.Audio.ContextRate != 48000 {
		_ = conn.Close(4400, "audio_capability_required")
		return
	}
	var authState atomic.Bool
	authState.Store(sessionErr == nil)
	if authState.Load() && !s.store.VerifyCSRF(ctx, session.ID, body.CSRF) {
		authState.Store(false)
		if !s.cfg.OpenAccess {
			_ = conn.Close(4401, "session_invalid")
			return
		}
	}
	var playbackMu sync.Mutex
	var playbackCancel context.CancelFunc
	var playbackChannel uint64
	var playbackFile *webarchive.Playback
	var playbackElapsed uint32
	var playbackNextPacket int64
	var playbackSequence uint32
	var playbackPlaying bool
	var playbackGeneration atomic.Uint64
	playbackProgress := make(chan struct{}, 1)
	output := make(chan socketOutput, queueCapacity(s.cfg.ControlQueueMessages, 0, 16*1024))
	liveOutput := newMediaOutputQueue(s.cfg.LiveQueuePackets, s.cfg.LiveQueueBytes)
	playbackOutput := newMediaOutputQueue(s.cfg.PlaybackQueuePackets, s.cfg.PlaybackQueueBytes)
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writeSocketOutputs(ctx, conn, output, liveOutput, playbackOutput, &playbackGeneration, func(item socketOutput) {
			playbackMu.Lock()
			if item.generation == playbackGeneration.Load() {
				playbackElapsed = item.elapsed
				playbackNextPacket = item.packetIndex + 1
				playbackSequence = item.sequence + 1
			}
			playbackMu.Unlock()
			select {
			case playbackProgress <- struct{}{}:
			default:
			}
		})
	}()
	type replayEntry struct {
		digest [32]byte
		result []byte
		stored time.Time
	}
	replays := map[string]replayEntry{}
	replayOrder := make([]string, 0, 64)
	sendControl := func(value any) bool {
		encoded, err := json.Marshal(value)
		if err != nil {
			return false
		}
		var direct struct {
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(encoded, &direct)
		if direct.RequestID != "" {
			entry := replays[direct.RequestID]
			entry.result = append([]byte(nil), encoded...)
			entry.stored = time.Now()
			replays[direct.RequestID] = entry
		}
		select {
		case output <- socketOutput{kind: websocket.MessageText, data: encoded}:
			return true
		default:
			s.telemetry.inc("opusrefweb_queue_drops_total", "control")
			s.telemetry.inc("opusrefweb_websocket_closes_total", "overload")
			s.telemetry.event("control_overload", "error", "A WebSocket control queue overflowed.")
			_ = conn.Close(4409, "overload")
			cancel()
			return false
		}
	}
	sendUnsolicited := func(value any) bool {
		encoded, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return false
		}
		select {
		case output <- socketOutput{kind: websocket.MessageText, data: encoded}:
			return true
		default:
			return false
		}
	}
	sendLifecycle := func(value any) bool {
		if sendUnsolicited(value) {
			return true
		}
		_ = conn.Close(4409, "overload")
		cancel()
		return false
	}
	var playbackSlot atomic.Bool
	releasePlaybackSlot := func() {
		if playbackSlot.Swap(false) {
			<-s.playbackSlots
		}
	}
	defer releasePlaybackSlot()
	bodyResult := map[string]any{"authenticated": authState.Load(), "ptt_available": authState.Load() && !session.PasswordChangeRequired && session.Callsign != "" && s.cfg.PTT != nil, "passkey_available": s.cfg.Passkeys != nil, "limits": map[string]any{"media_bytes": 1200, "control_bytes": 16384, "live_queue_packets": s.cfg.LiveQueuePackets}, "status": s.statusData()}
	if authState.Load() {
		bodyResult["role"] = session.Role
	}
	if !sendControl(map[string]any{"api_version": 1, "type": "hello_ok", "request_id": hello.RequestID, "body": bodyResult}) {
		_ = conn.Close(4409, "overload")
		return
	}
	var rawSession string
	invalidateAuth := func() {}
	if authState.Load() {
		invalidateAuth = func() {
			if !authState.Swap(false) {
				return
			}
			if s.cfg.PTT != nil {
				_ = s.cfg.PTT.StopSession(context.Background(), session.ID)
			}
			playbackMu.Lock()
			if playbackCancel != nil {
				playbackCancel()
				playbackCancel = nil
			}
			playbackGeneration.Add(1)
			playbackOutput.discard()
			playbackMu.Unlock()
			releasePlaybackSlot()
			if s.cfg.OpenAccess {
				if !sendUnsolicited(map[string]any{"api_version": 1, "type": "error", "body": map[string]any{"code": "session_invalid", "text": "Your session ended. Live listening remains available."}}) {
					_ = conn.Close(4409, "overload")
					cancel()
				}
				return
			}
			_ = conn.Close(4401, "session_invalid")
			cancel()
		}
		unregister := s.registerRevocation(session.ID, session.UserID, invalidateAuth)
		defer unregister()
		cookie, _ := r.Cookie(CookieName)
		rawSession = cookie.Value
		go func(raw string) {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, checkErr := s.store.AuthenticateSessionWithIdle(context.Background(), raw, time.Now(), s.cfg.SessionIdle); checkErr != nil {
						invalidateAuth()
						return
					}
				}
			}
		}(cookie.Value)
	}
	privileged := func() bool {
		if !authState.Load() || rawSession == "" {
			return false
		}
		fresh, checkErr := s.store.AuthenticateSessionWithIdle(context.Background(), rawSession, time.Now(), s.cfg.SessionIdle)
		if checkErr != nil || fresh.ID != session.ID || fresh.UserID != session.UserID || fresh.Role != session.Role || fresh.PasswordChangeRequired != session.PasswordChangeRequired {
			invalidateAuth()
			return false
		}
		return true
	}
	if s.cfg.PTT != nil {
		endEvents, unsubscribe := s.cfg.PTT.SubscribeEnds()
		defer unsubscribe()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case ended := <-endEvents:
					if ended.Session == session.ID {
						sendLifecycle(map[string]any{"api_version": 1, "type": "ptt_ended", "body": map[string]any{"channel_id": strconv.FormatUint(ended.ChannelID, 10), "reason": ended.Reason}})
					}
				}
			}
		}()
	}
	var liveCancel func()
	if s.cfg.LiveHub != nil {
		capacity := s.cfg.LiveQueuePackets
		if capacity <= 0 {
			capacity = 64
		}
		_, events, cancelSub := s.cfg.LiveHub.Subscribe(capacity)
		liveCancel = cancelSub
		defer liveCancel()
		go func() {
			var outboundChannel uint64
			var outboundSequence uint32
			var source string
			for {
				select {
				case <-ctx.Done():
					return
				case event, ok := <-events:
					if !ok {
						return
					}
					switch event.Kind {
					case gateway.LiveStart:
						outboundChannel, outboundSequence, source = event.ChannelID, 0, event.SourceCallsign
						sendLifecycle(map[string]any{"api_version": 1, "type": "stream_start", "body": map[string]any{"channel_id": strconv.FormatUint(outboundChannel, 10), "source_callsign": source, "started_at": time.Now().UTC().Format(time.RFC3339), "tot_seconds": 180}})
					case gateway.LiveEnd:
						sendLifecycle(map[string]any{"api_version": 1, "type": "stream_end", "body": map[string]any{"channel_id": strconv.FormatUint(outboundChannel, 10), "reason": event.Reason}})
						outboundChannel = 0
					case gateway.LiveDiscontinuity:
						old := outboundChannel
						outboundChannel, outboundSequence = event.ChannelID, 0
						sendLifecycle(map[string]any{"api_version": 1, "type": "discontinuity", "body": map[string]any{"old_channel_id": strconv.FormatUint(old, 10), "new_channel_id": strconv.FormatUint(outboundChannel, 10), "reason": event.Reason}})
					case gateway.LiveMedia:
						media := event.Media
						media.ChannelID, media.Sequence = outboundChannel, outboundSequence
						encoded, encodeErr := wsprotocol.EncodeMedia(media)
						if encodeErr != nil {
							continue
						}
						if liveOutput.enqueue(socketOutput{kind: websocket.MessageBinary, data: encoded, timestamp: media.Timestamp}) {
							s.telemetry.inc("opusrefweb_audio_packets_total", "live")
							outboundSequence++
						} else {
							s.telemetry.inc("opusrefweb_queue_drops_total", "live")
							s.telemetry.event("live_discontinuity", "warning", "A slow listener caused a live-audio discontinuity.")
							liveOutput.discard()
							old := outboundChannel
							newChannel, channelErr := s.newChannel()
							if channelErr != nil {
								cancel()
								return
							}
							outboundChannel, outboundSequence = newChannel, 0
							if !sendLifecycle(map[string]any{"api_version": 1, "type": "discontinuity", "body": map[string]any{"old_channel_id": strconv.FormatUint(old, 10), "new_channel_id": strconv.FormatUint(newChannel, 10), "reason": "slow_consumer"}}) || !sendLifecycle(map[string]any{"api_version": 1, "type": "stream_start", "body": map[string]any{"channel_id": strconv.FormatUint(newChannel, 10), "source_callsign": source, "started_at": time.Now().UTC().Format(time.RFC3339), "tot_seconds": 180}}) {
								return
							}
						}
					}
				}
			}
		}()
	}
	startPlayback := func() {
		playbackMu.Lock()
		if playbackCancel != nil {
			playbackCancel()
		}
		playCtx, stop := context.WithCancel(ctx)
		playbackCancel = stop
		channel := playbackChannel
		archivePlayback := playbackFile
		nextPacket := playbackNextPacket
		sequence := playbackSequence
		generation := playbackGeneration.Load()
		playbackMu.Unlock()
		go func() {
			if archivePlayback == nil {
				return
			}
			cursor, cursorErr := archivePlayback.NewCursorAt(nextPacket)
			if cursorErr != nil {
				return
			}
			defer cursor.Close()
			previous := uint32(0)
			accepted := uint32(0)
			if nextPacket > 0 {
				playbackMu.Lock()
				previous, accepted = playbackElapsed, playbackElapsed
				playbackMu.Unlock()
			}
			outboundSequence := sequence
			first := true
			for {
				packetIndex := cursor.Index()
				packet, nextErr := cursor.Next()
				if errors.Is(nextErr, io.EOF) {
					for {
						playbackMu.Lock()
						drained := playbackNextPacket >= archivePlayback.PacketCount()
						playbackMu.Unlock()
						if drained {
							break
						}
						select {
						case <-playCtx.Done():
							return
						case <-playbackProgress:
						}
					}
					break
				}
				if nextErr != nil {
					return
				}
				if !first {
					wait := time.Duration(packet.ArrivalMS-previous) * time.Millisecond
					timer := time.NewTimer(wait)
					select {
					case <-playCtx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
				}
				first = false
				previous = packet.ArrivalMS
				encoded, encodeErr := wsprotocol.EncodeMedia(wsprotocol.Media{Kind: wsprotocol.KindPlayback, ChannelID: channel, Sequence: outboundSequence, Timestamp: packet.Timestamp, Payload: packet.Payload})
				if encodeErr != nil {
					return
				}
				if playbackOutput.enqueue(socketOutput{kind: websocket.MessageBinary, data: encoded, timestamp: packet.Timestamp, playback: true, generation: generation, packetIndex: packetIndex, sequence: outboundSequence, elapsed: packet.ArrivalMS}) {
					s.telemetry.inc("opusrefweb_audio_packets_total", "playback")
					outboundSequence++
				} else {
					s.telemetry.inc("opusrefweb_queue_drops_total", "playback")
					s.telemetry.event("playback_paused", "warning", "Playback paused because its output queue was full.")
					playbackMu.Lock()
					if generation == playbackGeneration.Load() {
						playbackGeneration.Add(1)
						playbackOutput.discard()
						accepted = playbackElapsed
						playbackPlaying = false
					}
					playbackMu.Unlock()
					sendLifecycle(map[string]any{"api_version": 1, "type": "playback_state", "body": map[string]any{"channel_id": strconv.FormatUint(channel, 10), "state": "paused", "elapsed_ms": accepted}})
					return
				}
			}
			playbackMu.Lock()
			ownsChannel := playbackChannel == channel
			if ownsChannel {
				playbackPlaying = false
				playbackChannel = 0
				playbackFile = nil
				playbackElapsed = 0
				playbackNextPacket = 0
				playbackSequence = 0
				playbackGeneration.Add(1)
			}
			playbackMu.Unlock()
			if ownsChannel {
				releasePlaybackSlot()
			}
			sendLifecycle(map[string]any{"api_version": 1, "type": "playback_state", "body": map[string]any{"channel_id": strconv.FormatUint(channel, 10), "state": "closed", "elapsed_ms": previous}})
		}()
	}
	defer func() {
		playbackMu.Lock()
		if playbackCancel != nil {
			playbackCancel()
		}
		playbackGeneration.Add(1)
		playbackOutput.discard()
		playbackMu.Unlock()
	}()
	for {
		kind, data, err = conn.Read(ctx)
		if err != nil {
			break
		}
		if kind == websocket.MessageBinary {
			media, decodeErr := wsprotocol.DecodeMedia(data, wsprotocol.ClientToServer)
			if decodeErr != nil {
				_ = conn.Close(4400, "invalid_media")
				break
			}
			if !privileged() || s.cfg.PTT == nil {
				_ = conn.Close(4403, "forbidden")
				break
			}
			if err = s.cfg.PTT.Send(ctx, session.ID, media.ChannelID, media.Sequence, media.Timestamp, media.Payload); err != nil {
				_ = s.cfg.PTT.StopSession(context.Background(), session.ID)
				_ = conn.Close(4400, "invalid_media_state")
				break
			}
			s.telemetry.inc("opusrefweb_audio_packets_total", "transmit")
			continue
		}
		control, decodeErr := wsprotocol.DecodeControl(data, wsprotocol.ClientToServer)
		if decodeErr != nil || control.Type == "hello" {
			_ = conn.Close(4400, "protocol_violation")
			break
		}
		digest := sha256.Sum256(data)
		now := time.Now()
		for len(replayOrder) > 0 {
			oldest := replayOrder[0]
			if now.Sub(replays[oldest].stored) <= 30*time.Second {
				break
			}
			delete(replays, oldest)
			replayOrder = replayOrder[1:]
		}
		if prior, ok := replays[control.RequestID]; ok && prior.digest != ([32]byte{}) {
			if prior.digest != digest {
				_ = conn.Close(4400, "request_id_reuse")
				break
			}
			if len(prior.result) > 0 {
				select {
				case output <- socketOutput{kind: websocket.MessageText, data: prior.result}:
				default:
					_ = conn.Close(4409, "overload")
					return
				}
			}
			continue
		}
		if len(replayOrder) == 64 {
			delete(replays, replayOrder[0])
			replayOrder = replayOrder[1:]
		}
		replayOrder = append(replayOrder, control.RequestID)
		entry := replays[control.RequestID]
		entry.digest = digest
		entry.stored = now
		replays[control.RequestID] = entry
		switch control.Type {
		case "ptt_start":
			if err = decodeBody(control.Body, &struct{}{}); err != nil {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			if !privileged() || session.PasswordChangeRequired || session.Callsign == "" || s.cfg.PTT == nil {
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "forbidden", "text": "PTT is not available."}})
				continue
			}
			if !s.limiter.Allow("ptt_second", session.UserID, 1, time.Second, time.Now()) || !s.limiter.Allow("ptt_minute", session.UserID, 10, time.Minute, time.Now()) {
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "rate_limited", "text": "PTT request rate exceeded."}})
				continue
			}
			grant, startErr := s.cfg.PTT.StartForUser(ctx, session.ID, session.UserID, session.Callsign)
			if startErr != nil {
				s.telemetry.inc("opusrefweb_ptt_total", "busy")
				sendControl(map[string]any{"api_version": 1, "type": "ptt_busy", "request_id": control.RequestID, "body": map[string]any{}})
				continue
			}
			s.telemetry.inc("opusrefweb_ptt_total", "grant")
			sendControl(map[string]any{"api_version": 1, "type": "ptt_granted", "request_id": control.RequestID, "body": map[string]any{"channel_id": strconv.FormatUint(grant.ChannelID, 10), "tot_seconds": 180}})
		case "ptt_stop":
			var stop struct {
				ChannelID string `json:"channel_id"`
			}
			if decodeBody(control.Body, &stop) != nil || s.cfg.PTT == nil {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			channelID, parseErr := parseChannelID(stop.ChannelID)
			if parseErr != nil {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			if !privileged() || s.cfg.PTT.Stop(ctx, session.ID, channelID) != nil {
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "not_owner", "text": "The PTT channel is not owned by this session."}})
				continue
			}
			s.telemetry.inc("opusrefweb_ptt_total", "stop")
			sendControl(map[string]any{"api_version": 1, "type": "ptt_ended", "request_id": control.RequestID, "body": map[string]any{"channel_id": stop.ChannelID, "reason": "normal"}})
		case "playback_open":
			if !privileged() || session.PasswordChangeRequired || s.cfg.Archives == nil {
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "forbidden", "text": "Playback is not available."}})
				continue
			}
			var open struct {
				RecordingID string `json:"recording_id"`
			}
			if decodeBody(control.Body, &open) != nil {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			acquiredPlaybackSlot := false
			if !playbackSlot.Load() {
				select {
				case s.playbackSlots <- struct{}{}:
					playbackSlot.Store(true)
					acquiredPlaybackSlot = true
				default:
					sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "playback_busy", "text": "Playback capacity is full."}})
					continue
				}
			}
			recording, readErr := s.store.RecordingByID(ctx, open.RecordingID)
			var indexed *webarchive.Playback
			if readErr == nil {
				indexed, readErr = s.cfg.Archives.OpenPlayback(open.RecordingID)
			}
			if readErr != nil {
				if acquiredPlaybackSlot {
					releasePlaybackSlot()
				}
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "recording_unavailable", "text": "The recording is unavailable."}})
				continue
			}
			channelID, channelErr := s.newChannel()
			if channelErr != nil {
				if acquiredPlaybackSlot {
					releasePlaybackSlot()
				}
				return
			}
			duration := indexed.DurationMS()
			playbackMu.Lock()
			if playbackCancel != nil {
				playbackCancel()
			}
			playbackGeneration.Add(1)
			playbackOutput.discard()
			playbackChannel = channelID
			playbackFile = indexed
			playbackElapsed = 0
			playbackNextPacket = 0
			playbackSequence = 0
			playbackPlaying = true
			playbackMu.Unlock()
			s.telemetry.inc("opusrefweb_playback_total", "open", "success")
			sendControl(map[string]any{"api_version": 1, "type": "playback_opened", "request_id": control.RequestID, "body": map[string]any{"channel_id": strconv.FormatUint(channelID, 10), "recording_id": open.RecordingID, "duration_ms": duration, "status": recording.Status}})
			startPlayback()
		case "playback_pause", "playback_resume", "playback_close":
			if !privileged() {
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "forbidden", "text": "Playback is not available."}})
				continue
			}
			var action struct {
				ChannelID string `json:"channel_id"`
			}
			if decodeBody(control.Body, &action) != nil {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			id, parseErr := parseChannelID(action.ChannelID)
			playbackMu.Lock()
			valid := parseErr == nil && id == playbackChannel
			if valid && control.Type == "playback_resume" && playbackPlaying {
				valid = false
			}
			if valid && control.Type != "playback_resume" {
				if playbackCancel != nil {
					playbackCancel()
				}
				playbackGeneration.Add(1)
				playbackOutput.discard()
				playbackPlaying = false
			}
			elapsed := 0
			elapsed = int(playbackElapsed)
			if valid && control.Type == "playback_close" {
				playbackChannel = 0
				playbackFile = nil
				playbackElapsed = 0
				playbackNextPacket = 0
				playbackSequence = 0
			}
			if valid && control.Type == "playback_resume" {
				playbackPlaying = true
			}
			playbackMu.Unlock()
			if !valid {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			if control.Type == "playback_close" {
				releasePlaybackSlot()
			}
			state := map[string]string{"playback_pause": "paused", "playback_resume": "playing", "playback_close": "closed"}[control.Type]
			s.telemetry.inc("opusrefweb_playback_total", strings.TrimPrefix(control.Type, "playback_"), "success")
			if !sendControl(map[string]any{"api_version": 1, "type": "playback_state", "request_id": control.RequestID, "body": map[string]any{"channel_id": action.ChannelID, "state": state, "elapsed_ms": elapsed}}) {
				return
			}
			if control.Type == "playback_resume" {
				startPlayback()
			}
		case "playback_seek":
			if !privileged() {
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "forbidden", "text": "Playback is not available."}})
				continue
			}
			var seek struct {
				ChannelID string `json:"channel_id"`
				ElapsedMS int    `json:"elapsed_ms"`
			}
			if decodeBody(control.Body, &seek) != nil || seek.ElapsedMS < 0 {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			id, parseErr := parseChannelID(seek.ChannelID)
			playbackMu.Lock()
			if parseErr != nil || id != playbackChannel {
				playbackMu.Unlock()
				_ = conn.Close(4400, "invalid_request")
				return
			}
			archivePlayback := playbackFile
			playbackMu.Unlock()
			cursor, cursorErr := archivePlayback.NewCursor(uint32(seek.ElapsedMS))
			if cursorErr != nil {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			packetIndex := cursor.Index()
			_ = cursor.Close()
			playbackMu.Lock()
			if id != playbackChannel || archivePlayback != playbackFile {
				playbackMu.Unlock()
				_ = conn.Close(4400, "invalid_request")
				return
			}
			if playbackCancel != nil {
				playbackCancel()
			}
			playbackGeneration.Add(1)
			playbackOutput.discard()
			playbackElapsed = uint32(seek.ElapsedMS)
			playbackNextPacket = packetIndex
			playbackSequence = 0
			playbackPlaying = true
			playbackMu.Unlock()
			s.telemetry.inc("opusrefweb_playback_total", "seek", "success")
			if !sendControl(map[string]any{"api_version": 1, "type": "playback_state", "request_id": control.RequestID, "body": map[string]any{"channel_id": seek.ChannelID, "state": "playing", "elapsed_ms": seek.ElapsedMS}}) {
				return
			}
			startPlayback()
		default:
			sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "unsupported", "text": "The request type is not supported."}})
		}
	}
	if authState.Load() && s.cfg.PTT != nil {
		_ = s.cfg.PTT.StopSession(context.Background(), session.ID)
	}
	cancel()
	select {
	case <-writerDone:
		return
	case <-time.After(time.Second):
		return
	}
}
func decodeBody(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func parseChannelID(value string) (uint64, error) {
	if value == "" || value[0] == '0' || len(value) > 20 {
		return 0, errors.New("channel ID is invalid")
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, errors.New("channel ID is invalid")
		}
	}
	id, err := strconv.ParseUint(value, 10, 64)
	if err != nil || id == 0 {
		return 0, errors.New("channel ID is invalid")
	}
	return id, nil
}
func queueCapacity(packets, bytes, maximumItem int) int {
	if packets <= 0 {
		packets = 32
	}
	if bytes > 0 && maximumItem > 0 {
		packets = min(packets, max(1, bytes/maximumItem))
	}
	return max(1, packets)
}
func (s *Server) newChannel() (uint64, error) {
	s.channelMu.Lock()
	defer s.channelMu.Unlock()
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, err
		}
		id := binary.BigEndian.Uint64(raw[:])
		if id == 0 {
			continue
		}
		if _, ok := s.channelUsed[id]; ok {
			continue
		}
		s.channelUsed[id] = struct{}{}
		return id, nil
	}
}
func (s *Server) monitorMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.wsMu.Lock()
		active := s.wsActive
		s.wsMu.Unlock()
		ready := 0
		if s.operationalReady() {
			ready = 1
		}
		counts, _ := s.store.MonitoringCounts(context.Background(), time.Now())
		var archiveUsed, archiveQuota int64
		quotaFull, degraded := 0, 0
		if s.cfg.Archives != nil {
			archiveUsed, archiveQuota = s.cfg.Archives.Usage()
			if s.cfg.Archives.QuotaFull() {
				quotaFull, degraded = 1, 1
			}
			if !s.cfg.Archives.Ready() {
				degraded = 1
			}
		}
		if ready == 0 {
			degraded = 1
		}
		_, _ = fmt.Fprintf(w, "# TYPE opusrefweb_up gauge\nopusrefweb_up 1\n# TYPE opusrefweb_ready gauge\nopusrefweb_ready %d\n# TYPE opusrefweb_degraded gauge\nopusrefweb_degraded %d\n# TYPE opusrefweb_websocket_connections gauge\nopusrefweb_websocket_connections %d\n# TYPE opusrefweb_accounts gauge\nopusrefweb_accounts %d\n# TYPE opusrefweb_sessions gauge\nopusrefweb_sessions %d\n# TYPE opusrefweb_recordings gauge\nopusrefweb_recordings %d\n# TYPE opusrefweb_archive_bytes gauge\nopusrefweb_archive_bytes %d\n# TYPE opusrefweb_archive_quota_bytes gauge\nopusrefweb_archive_quota_bytes %d\n# TYPE opusrefweb_archive_quota_full gauge\nopusrefweb_archive_quota_full %d\n# TYPE opusrefweb_authentication_failures_total counter\nopusrefweb_authentication_failures_total %d\n", ready, degraded, active, counts.Accounts, counts.Sessions, counts.Recordings, archiveUsed, archiveQuota, quotaFull, counts.AuthenticationFailures)
		_, _ = fmt.Fprint(w, s.telemetry.render())
	})
	return mux
}
func (s *Server) security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		wssOrigin := strings.Replace(s.cfg.PublicOrigin, "https://", "wss://", 1)
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self' "+wssOrigin+"; worker-src 'self'; media-src 'none'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "microphone=(self), camera=(), geolocation=()")
		if s.draining.Load() && r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && r.URL.Path != "/metrics" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"code": "server_draining"})
			return
		}
		observed := &observedWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(observed, r)
		s.telemetry.inc("opusrefweb_http_requests_total", routeLabel(r.Pattern, r.URL.Path), statusClass(observed.status))
	})
}
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.operationalReady() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready"})
}
func (s *Server) publicStatus(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.OpenAccess {
		if _, err := s.requestSession(r); err != nil {
			writeError(w, http.StatusUnauthorized, "authentication_required")
			return
		}
	}
	writeJSON(w, http.StatusOK, s.statusData())
}
func (s *Server) statusData() map[string]any {
	reflector := map[string]any{"id": "", "display_name": ""}
	clientCount := 0
	floor := map[string]any{"active": false}
	if s.cfg.ReflectorMonitor != nil {
		if snapshot, ok := s.cfg.ReflectorMonitor.Snapshot(); ok {
			reflector["id"] = snapshot.ReflectorID
			reflector["display_name"] = snapshot.DisplayName
			clientCount = snapshot.ClientCount
			floor = map[string]any{"active": snapshot.Stream.Active}
			if snapshot.Stream.Active {
				floor["source_callsign"] = snapshot.Stream.SourceCallsign
				floor["started_at"] = snapshot.Stream.StartedAt
				floor["remaining_seconds"] = snapshot.Stream.Remaining
			}
		}
	}
	quotaFull := s.cfg.Archives != nil && s.cfg.Archives.QuotaFull()
	s.telemetry.setQuota(quotaFull)
	health := "ok"
	if quotaFull {
		health = "degraded"
	}
	if s.cfg.Archives != nil && !s.cfg.Archives.Ready() {
		health = "unavailable"
	}
	return map[string]any{"health": health, "ready": s.operationalReady(), "reflector": reflector, "client_count": clientCount, "floor": floor, "recording": map[string]any{"available": s.cfg.Archives != nil && s.cfg.Archives.Ready(), "quota_full": quotaFull}, "server_time": time.Now().UTC().Format(time.RFC3339)}
}
func (s *Server) operationalReady() bool {
	return s.accepting.Load() && s.dependenciesReady.Load()
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	session, err := s.requestSession(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false, "passkey_available": s.cfg.Passkeys != nil})
		return
	}
	csrf, err := s.store.RotateCSRF(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "session_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "role": session.Role, "username": session.Username, "source_callsign": session.Callsign, "csrf_token": csrf, "forced_password_change": session.PasswordChangeRequired, "passkey_available": s.cfg.Passkeys != nil})
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	var in loginRequest
	if err := decodeExact(r, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	limiterName := strings.ToLower(in.Username)
	if !s.limiter.Allow("login_user", limiterName, 5, 15*time.Minute, time.Now()) || !s.limiter.Allow("login_ip", s.clientAddress(r), 5, 15*time.Minute, time.Now()) {
		s.telemetry.inc("opusrefweb_auth_total", "password", "rate_limited")
		s.telemetry.event("rate_limit", "warning", "A password authentication rate limit was reached.")
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	select {
	case s.hashQueue <- struct{}{}:
		defer func() { <-s.hashQueue }()
	default:
		writeError(w, http.StatusTooManyRequests, "authentication_busy")
		return
	}
	select {
	case s.hashSlots <- struct{}{}:
		defer func() { <-s.hashSlots }()
	case <-r.Context().Done():
		return
	}
	user, lookupErr := s.store.FindUserByUsername(r.Context(), in.Username)
	hash := s.dummyPasswordHash
	if lookupErr == nil && !user.Disabled {
		hash = user.PasswordHash
	}
	ok, rehash, err := auth.VerifyPassword(in.Password, hash, s.cfg.Argon2)
	if err != nil || !ok || lookupErr != nil || user.Disabled {
		s.telemetry.inc("opusrefweb_auth_total", "password", "failure")
		var target *string
		if lookupErr == nil {
			target = &user.ID
		}
		targetID := ""
		if target != nil {
			targetID = *target
		}
		s.audit(r.Context(), "login", "failure", "", targetID, "")
		writeError(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	if rehash {
		if upgraded, hashErr := auth.HashPassword(in.Password, s.cfg.Argon2); hashErr == nil {
			_ = s.store.UpgradePasswordHash(r.Context(), user.ID, upgraded, time.Now())
		}
	}
	raw, csrf, session, err := s.store.CreateSession(r.Context(), user.ID, time.Now(), s.cfg.SessionIdle, s.cfg.SessionAbsolute, s.cfg.MaxSessions)
	if err != nil {
		s.audit(r.Context(), "login", "failure", user.ID, user.ID, "")
		writeError(w, http.StatusServiceUnavailable, "authentication_unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: raw, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.AbsoluteExpiry})
	s.telemetry.inc("opusrefweb_auth_total", "password", "success")
	s.audit(r.Context(), "login", "success", user.ID, user.ID, "")
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "role": session.Role, "username": user.Username, "source_callsign": user.Callsign, "csrf_token": csrf, "forced_password_change": session.PasswordChangeRequired, "passkey_available": false})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		outcome := "failure"
		if succeeded {
			outcome = "success"
		}
		s.audit(r.Context(), "logout", outcome, session.UserID, session.UserID, "")
	}()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	cookie, _ := r.Cookie(CookieName)
	if err := s.store.RevokeCurrentSession(r.Context(), cookie.Value, time.Now()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "logout_failed")
		return
	}
	s.revokeSessionSocket(session.ID)
	succeeded = true
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"logged_out": true})
}
func (s *Server) reauthPassword(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "reauth_password", successOutcome(succeeded), session.UserID, session.UserID, "")
	}()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	user, err := s.store.FindUserByID(r.Context(), session.UserID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	ok, _, err := auth.VerifyPassword(in.Password, user.PasswordHash, s.cfg.Argon2)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	raw, err := s.store.IssueReauth(r.Context(), session.ID, time.Now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "reauth_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reauth_token": raw, "expires_in_seconds": 300})
	succeeded = true
}
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "password_change", successOutcome(succeeded), session.UserID, session.UserID, "")
	}()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if err := s.store.ConsumeReauth(r.Context(), session.ID, r.Header.Get("X-Reauth-Token"), time.Now()); err != nil {
		writeError(w, http.StatusForbidden, "reauth_required")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	policy := auth.Policy{Username: session.Username, Callsign: session.Callsign, ServiceTerms: []string{"OpusRef"}, Additional: s.cfg.PasswordBlocklist}
	if policyErr := policy.Check(in.Password); policyErr != nil {
		writeError(w, http.StatusUnprocessableEntity, string(policyErr.Code))
		return
	}
	hash, err := auth.HashPassword(in.Password, s.cfg.Argon2)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "password_unavailable")
		return
	}
	if err = s.store.UpdatePassword(r.Context(), session.UserID, session.ID, hash, time.Now()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "password_unavailable")
		return
	}
	s.revokeSockets(func(active socketRevocation) bool {
		return active.userID == session.UserID && active.sessionID != session.ID
	})
	succeeded = true
	writeJSON(w, http.StatusOK, map[string]any{"changed": true})
}
func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, session store.Session) {
	items, err := s.store.ListSessions(r.Context(), session.UserID, session.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "sessions_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, page(items))
}
func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "session_revoke", successOutcome(succeeded), session.UserID, session.UserID, "")
	}()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if err := s.store.ConsumeReauth(r.Context(), session.ID, r.Header.Get("X-Reauth-Token"), time.Now()); err != nil {
		writeError(w, http.StatusForbidden, "reauth_required")
		return
	}
	if err := s.store.RevokeOtherSession(r.Context(), session.UserID, session.ID, r.PathValue("id"), time.Now()); err != nil {
		writeError(w, http.StatusNotFound, "session_not_found")
		return
	}
	s.revokeSessionSocket(r.PathValue("id"))
	succeeded = true
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}
func (s *Server) passkeysUnavailable(w http.ResponseWriter, r *http.Request, session store.Session) {
	items, err := s.store.ListPasskeys(r.Context(), session.UserID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "passkeys_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, page(items))
}
func (s *Server) passkeyLoginOptions(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	if s.cfg.Passkeys == nil {
		writeError(w, http.StatusNotFound, "passkeys_unavailable")
		return
	}
	if !s.allowPasskeyAttempt(r, "") {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if decodeExact(r, &struct{}{}) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, options, err := s.cfg.Passkeys.BeginLogin()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "passkey_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, passkeyOptionsResponse(id, options))
}
func (s *Server) passkeyLoginVerify(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		writeError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	if s.cfg.Passkeys == nil {
		writeError(w, http.StatusNotFound, "passkeys_unavailable")
		return
	}
	if !s.allowPasskeyAttempt(r, "") {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var in struct {
		CeremonyID string          `json:"ceremony_id"`
		Credential json.RawMessage `json:"credential"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	userID, err := s.cfg.Passkeys.FinishLoginAllowed(r.Context(), in.CeremonyID, in.Credential, func(userID string) bool {
		return s.limiter.Allow("passkey_account", userID, 10, time.Minute, time.Now())
	})
	if err != nil {
		var failure *passkey.VerificationFailure
		result := "failure"
		if errors.As(err, &failure) && failure.UserID != "" {
			outcome := "failure"
			if failure.RateLimited {
				result = "rate_limited"
			}
			if failure.Regression {
				outcome = "sign_counter_regression"
				result = "sign_counter_regression"
			}
			s.audit(r.Context(), "passkey_login", outcome, failure.UserID, failure.UserID, "")
		} else {
			s.audit(r.Context(), "passkey_login", "failure", "", "", "")
		}
		s.telemetry.inc("opusrefweb_auth_total", "passkey", result)
		if result == "rate_limited" {
			s.telemetry.event("rate_limit", "warning", "A passkey account rate limit was reached.")
			writeError(w, http.StatusTooManyRequests, "rate_limited")
			return
		}
		writeError(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	user, err := s.store.FindUserByID(r.Context(), userID)
	if err != nil || user.Disabled {
		s.audit(r.Context(), "passkey_login", "failure", userID, userID, "")
		writeError(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	raw, csrf, session, err := s.store.CreateSession(r.Context(), user.ID, time.Now(), s.cfg.SessionIdle, s.cfg.SessionAbsolute, s.cfg.MaxSessions)
	if err != nil {
		s.audit(r.Context(), "passkey_login", "failure", user.ID, user.ID, "")
		writeError(w, http.StatusServiceUnavailable, "authentication_unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: raw, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.AbsoluteExpiry})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "username": user.Username, "role": user.Role, "source_callsign": user.Callsign, "csrf_token": csrf, "forced_password_change": user.PasswordChangeRequired, "passkey_available": true})
	s.telemetry.inc("opusrefweb_auth_total", "passkey", "success")
	s.audit(r.Context(), "passkey_login", "success", user.ID, user.ID, "")
}
func (s *Server) passkeyEnrollOptions(w http.ResponseWriter, r *http.Request, session store.Session) {
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if s.cfg.Passkeys == nil {
		writeError(w, http.StatusNotFound, "passkeys_unavailable")
		return
	}
	if !s.allowPasskeyAttempt(r, session.UserID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if err := s.store.ConsumeReauth(r.Context(), session.ID, r.Header.Get("X-Reauth-Token"), time.Now()); err != nil {
		writeError(w, http.StatusForbidden, "reauth_required")
		return
	}
	if decodeExact(r, &struct{}{}) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, options, err := s.cfg.Passkeys.BeginEnrollment(r.Context(), session.UserID, session.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "passkey_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, passkeyOptionsResponse(id, options))
}
func (s *Server) passkeyReauthOptions(w http.ResponseWriter, r *http.Request, session store.Session) {
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if s.cfg.Passkeys == nil {
		writeError(w, http.StatusNotFound, "passkeys_unavailable")
		return
	}
	if !s.allowPasskeyAttempt(r, session.UserID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	if decodeExact(r, &struct{}{}) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, options, err := s.cfg.Passkeys.BeginReauth(r.Context(), session.UserID, session.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "passkey_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, passkeyOptionsResponse(id, options))
}
func (s *Server) passkeyReauthVerify(w http.ResponseWriter, r *http.Request, session store.Session) {
	outcome := "failure"
	defer func() { s.audit(r.Context(), "reauth_passkey", outcome, session.UserID, session.UserID, "") }()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if s.cfg.Passkeys == nil {
		writeError(w, http.StatusNotFound, "passkeys_unavailable")
		return
	}
	if !s.allowPasskeyAttempt(r, session.UserID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var in struct {
		CeremonyID string          `json:"ceremony_id"`
		Credential json.RawMessage `json:"credential"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := s.cfg.Passkeys.FinishReauth(r.Context(), in.CeremonyID, session.UserID, session.ID, in.Credential); err != nil {
		var failure *passkey.VerificationFailure
		if errors.As(err, &failure) && failure.Regression {
			outcome = "sign_counter_regression"
		}
		writeError(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	raw, err := s.store.IssueReauth(r.Context(), session.ID, time.Now())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "reauth_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reauth_token": raw, "expires_in_seconds": 300})
	outcome = "success"
}
func (s *Server) passkeyEnrollVerify(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "passkey_enroll", successOutcome(succeeded), session.UserID, session.UserID, "")
	}()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if s.cfg.Passkeys == nil {
		writeError(w, http.StatusNotFound, "passkeys_unavailable")
		return
	}
	if !s.allowPasskeyAttempt(r, session.UserID) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return
	}
	var in struct {
		CeremonyID string          `json:"ceremony_id"`
		Name       string          `json:"name"`
		Credential json.RawMessage `json:"credential"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := s.cfg.Passkeys.FinishEnrollment(r.Context(), in.CeremonyID, session.UserID, session.ID, in.Name, in.Credential); err != nil {
		writeError(w, http.StatusBadRequest, "passkey_verification_failed")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"created": true})
	succeeded = true
}
func (s *Server) renamePasskey(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "passkey_rename", successOutcome(succeeded), session.UserID, session.UserID, "")
	}()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if err := s.store.ConsumeReauth(r.Context(), session.ID, r.Header.Get("X-Reauth-Token"), time.Now()); err != nil {
		writeError(w, http.StatusForbidden, "reauth_required")
		return
	}
	var in struct {
		Name string `json:"name"`
	}
	if decodeExact(r, &in) != nil || s.store.RenamePasskey(r.Context(), session.UserID, r.PathValue("id"), in.Name) != nil {
		writeError(w, http.StatusBadRequest, "passkey_update_failed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
	succeeded = true
}
func (s *Server) deletePasskey(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "passkey_delete", successOutcome(succeeded), session.UserID, session.UserID, "")
	}()
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	if err := s.store.ConsumeReauth(r.Context(), session.ID, r.Header.Get("X-Reauth-Token"), time.Now()); err != nil {
		writeError(w, http.StatusForbidden, "reauth_required")
		return
	}
	if s.store.DeletePasskey(r.Context(), session.UserID, r.PathValue("id")) != nil {
		writeError(w, http.StatusNotFound, "passkey_not_found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	succeeded = true
}
func (s *Server) recordings(w http.ResponseWriter, r *http.Request, _ store.Session) {
	query, err := recordingQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	query.Limit++
	items, err := s.store.QueryRecordings(r.Context(), query)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "recordings_unavailable")
		return
	}
	next := ""
	if len(items) == query.Limit {
		last := items[len(items)-2]
		next = encodeRecordingCursor(last.StartedAt, last.ID)
		items = items[:len(items)-1]
	}
	result := make([]map[string]any, 0, len(items))
	for _, item := range items {
		result = append(result, recordingResponse(item))
	}
	response := page(result)
	if next != "" {
		response["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) recording(w http.ResponseWriter, r *http.Request, _ store.Session) {
	item, err := s.store.RecordingByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "recording_not_found")
		return
	}
	writeJSON(w, http.StatusOK, recordingResponse(item))
}
func (s *Server) deleteRecording(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		if !succeeded {
			s.audit(r.Context(), "recording_delete", "failure", session.UserID, "", r.PathValue("id"))
		}
	}()
	if !s.adminMutation(w, r, session) {
		return
	}
	if s.cfg.Archives == nil {
		writeError(w, http.StatusServiceUnavailable, "archive_unavailable")
		return
	}
	if err := s.cfg.Archives.DeleteAs(r.Context(), r.PathValue("id"), session.UserID, time.Now()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "recording_delete_failed")
		return
	}
	succeeded = true
	s.telemetry.inc("opusrefweb_audit_writes_total", "success")
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request, _ store.Session) {
	limit, cursor, err := adminPageRequest(r, "accounts")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	items, err := s.store.ListUsersAfter(r.Context(), limit+1, cursor.Username, cursor.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "accounts_unavailable")
		return
	}
	response := page(items)
	if len(items) > limit {
		last := items[limit-1]
		response["items"] = items[:limit]
		response["next_cursor"] = encodeAdminCursor(adminCursor{Kind: "accounts", Username: last.Username, ID: last.ID})
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, _ store.Session) {
	limit, cursor, err := adminPageRequest(r, "audit")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	items, err := s.store.ListAuditBefore(r.Context(), limit+1, cursor.AuditID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "audit_unavailable")
		return
	}
	response := page(items)
	if len(items) > limit {
		last := items[limit-1]
		response["items"] = items[:limit]
		response["next_cursor"] = encodeAdminCursor(adminCursor{Kind: "audit", AuditID: last.ID})
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) adminClients(w http.ResponseWriter, r *http.Request, _ store.Session) {
	limit, cursor, err := adminPageRequest(r, "clients")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_query")
		return
	}
	if s.cfg.ReflectorMonitor == nil {
		writeJSON(w, http.StatusOK, page([]any{}))
		return
	}
	snapshot, ok := s.cfg.ReflectorMonitor.Snapshot()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "monitor_unavailable")
		return
	}
	start := cursor.Offset
	if start > len(snapshot.Clients) {
		start = len(snapshot.Clients)
	}
	end := min(start+limit, len(snapshot.Clients))
	response := page(snapshot.Clients[start:end])
	if end < len(snapshot.Clients) {
		response["next_cursor"] = encodeAdminCursor(adminCursor{Kind: "clients", Offset: end})
	}
	writeJSON(w, http.StatusOK, response)
}
func (s *Server) adminEvents(w http.ResponseWriter, _ *http.Request, _ store.Session) {
	writeJSON(w, http.StatusOK, page(s.telemetry.recent()))
}
func (s *Server) clearAccountPasskeys(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "passkeys_clear", successOutcome(succeeded), session.UserID, r.PathValue("id"), "")
	}()
	if !s.adminMutation(w, r, session) {
		return
	}
	if err := s.store.ClearPasskeysAndSessions(r.Context(), r.PathValue("id"), time.Now()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "credential_clear_failed")
		return
	}
	s.revokeUserSockets(r.PathValue("id"))
	succeeded = true
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}
func (s *Server) adminMutation(w http.ResponseWriter, r *http.Request, session store.Session) bool {
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return false
	}
	if !s.limiter.Allow("admin", session.ID, 60, time.Minute, time.Now()) {
		writeError(w, http.StatusTooManyRequests, "rate_limited")
		return false
	}
	if err := s.store.ConsumeReauth(r.Context(), session.ID, r.Header.Get("X-Reauth-Token"), time.Now()); err != nil {
		writeError(w, http.StatusForbidden, "reauth_required")
		return false
	}
	return true
}
func (s *Server) updateAccount(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "account_update", successOutcome(succeeded), session.UserID, r.PathValue("id"), "")
	}()
	if !s.adminMutation(w, r, session) {
		return
	}
	var in struct {
		Username       *string     `json:"username"`
		SourceCallsign *string     `json:"source_callsign"`
		Role           *store.Role `json:"role"`
		Disabled       *bool       `json:"disabled"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if err := s.store.UpdateUser(r.Context(), r.PathValue("id"), in.Username, in.SourceCallsign, in.Role, in.Disabled, time.Now()); err != nil {
		writeError(w, http.StatusConflict, "account_update_failed")
		return
	}
	s.revokeUserSockets(r.PathValue("id"))
	succeeded = true
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}
func (s *Server) resetAccountPassword(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "password_reset", successOutcome(succeeded), session.UserID, r.PathValue("id"), "")
	}()
	if !s.adminMutation(w, r, session) {
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	target, err := s.store.FindUserByID(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "account_not_found")
		return
	}
	policy := auth.Policy{Username: target.Username, Callsign: target.Callsign, ServiceTerms: []string{"OpusRef"}, Additional: s.cfg.PasswordBlocklist}
	if policyErr := policy.Check(in.Password); policyErr != nil {
		writeError(w, http.StatusUnprocessableEntity, string(policyErr.Code))
		return
	}
	hash, err := auth.HashPassword(in.Password, s.cfg.Argon2)
	if err != nil || s.store.SetPasswordHash(r.Context(), target.ID, hash, true, time.Now()) != nil {
		writeError(w, http.StatusServiceUnavailable, "password_reset_failed")
		return
	}
	s.revokeUserSockets(target.ID)
	succeeded = true
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}
func (s *Server) revokeAccountSessions(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "sessions_revoke", successOutcome(succeeded), session.UserID, r.PathValue("id"), "")
	}()
	if !s.adminMutation(w, r, session) {
		return
	}
	if decodeExact(r, &struct{}{}) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if s.store.RevokeAllSessions(r.Context(), r.PathValue("id"), time.Now()) != nil {
		writeError(w, http.StatusServiceUnavailable, "session_revoke_failed")
		return
	}
	s.revokeUserSockets(r.PathValue("id"))
	succeeded = true
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true})
}
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	defer func() {
		s.audit(r.Context(), "account_delete", successOutcome(succeeded), session.UserID, r.PathValue("id"), "")
	}()
	if !s.adminMutation(w, r, session) {
		return
	}
	if s.store.DeleteUser(r.Context(), r.PathValue("id"), time.Now()) != nil {
		writeError(w, http.StatusConflict, "account_delete_failed")
		return
	}
	s.revokeUserSockets(r.PathValue("id"))
	succeeded = true
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
func (s *Server) createAccount(w http.ResponseWriter, r *http.Request, session store.Session) {
	succeeded := false
	targetID := ""
	defer func() {
		s.audit(r.Context(), "account_create", successOutcome(succeeded), session.UserID, targetID, "")
	}()
	if !s.adminMutation(w, r, session) {
		return
	}
	var in struct {
		Username       string     `json:"username"`
		Role           store.Role `json:"role"`
		SourceCallsign *string    `json:"source_callsign"`
		Password       string     `json:"initial_password"`
	}
	if decodeExact(r, &in) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	callsign := ""
	if in.SourceCallsign != nil {
		callsign = *in.SourceCallsign
	}
	policy := auth.Policy{Username: in.Username, Callsign: callsign, ServiceTerms: []string{"OpusRef"}, Additional: s.cfg.PasswordBlocklist}
	if policyErr := policy.Check(in.Password); policyErr != nil {
		writeError(w, http.StatusUnprocessableEntity, string(policyErr.Code))
		return
	}
	hash, err := auth.HashPassword(in.Password, s.cfg.Argon2)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "account_unavailable")
		return
	}
	user, err := s.store.CreateUser(r.Context(), store.CreateUser{Username: in.Username, Role: in.Role, Callsign: callsign, PasswordHash: hash, PasswordChangeRequired: true})
	if err != nil {
		writeError(w, http.StatusConflict, "account_conflict")
		return
	}
	targetID = user.ID
	succeeded = true
	writeJSON(w, http.StatusCreated, map[string]any{"id": user.ID})
}
func (s *Server) requireSession(next func(http.ResponseWriter, *http.Request, store.Session)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		session, err := s.requestSession(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "authentication_required")
			return
		}
		next(w, r, session)
	}
}
func (s *Server) requireFullSession(next func(http.ResponseWriter, *http.Request, store.Session)) http.HandlerFunc {
	return s.requireSession(func(w http.ResponseWriter, r *http.Request, session store.Session) {
		if session.PasswordChangeRequired {
			writeError(w, http.StatusForbidden, "password_change_required")
			return
		}
		next(w, r, session)
	})
}
func (s *Server) requireAdmin(next func(http.ResponseWriter, *http.Request, store.Session)) http.HandlerFunc {
	return s.requireFullSession(func(w http.ResponseWriter, r *http.Request, session store.Session) {
		if session.Role != store.RoleAdmin {
			writeError(w, http.StatusForbidden, "admin_required")
			return
		}
		next(w, r, session)
	})
}
func (s *Server) requestSession(r *http.Request) (store.Session, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return store.Session{}, store.ErrUnauthorized
	}
	return s.store.AuthenticateSessionWithIdle(r.Context(), cookie.Value, time.Now(), s.cfg.SessionIdle)
}
func (s *Server) validCSRF(r *http.Request, session store.Session) bool {
	return s.sameOrigin(r) && s.store.VerifyCSRF(r.Context(), session.ID, r.Header.Get("X-CSRF-Token"))
}
func (s *Server) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == s.cfg.PublicOrigin && r.Header.Get("Sec-Fetch-Site") == "same-origin"
}
func (s *Server) clientAddress(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	direct := net.ParseIP(host)
	if direct == nil || !s.isTrustedProxy(direct) {
		return host
	}
	forwarded := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for index := len(forwarded) - 1; index >= 0; index-- {
		value := strings.TrimSpace(forwarded[index])
		ip := net.ParseIP(value)
		if ip != nil && !s.isTrustedProxy(ip) {
			return value
		}
	}
	return host
}

func (s *Server) allowPasskeyAttempt(r *http.Request, userID string) bool {
	now := time.Now()
	if !s.limiter.Allow("passkey_ip", s.clientAddress(r), 10, time.Minute, now) {
		s.telemetry.event("rate_limit", "warning", "A passkey source rate limit was reached.")
		return false
	}
	if userID != "" && !s.limiter.Allow("passkey_account", userID, 10, time.Minute, now) {
		s.telemetry.event("rate_limit", "warning", "A passkey account rate limit was reached.")
		return false
	}
	return true
}
func (s *Server) isTrustedProxy(ip net.IP) bool {
	for _, network := range s.trustedProxies {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}
func (s *Server) acquireSocket(session string) bool {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	global := s.cfg.MaxWebSockets
	if global <= 0 {
		global = 250
	}
	perSession := s.cfg.MaxWebSocketsPerSession
	if perSession <= 0 {
		perSession = 3
	}
	if s.wsActive >= global || session != "" && s.wsBySession[session] >= perSession {
		return false
	}
	s.wsActive++
	if session != "" {
		s.wsBySession[session]++
	}
	return true
}
func (s *Server) releaseSocket(session string) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.wsActive > 0 {
		s.wsActive--
	}
	if session != "" {
		s.wsBySession[session]--
		if s.wsBySession[session] <= 0 {
			delete(s.wsBySession, session)
		}
	}
}
func decodeExact(r *http.Request, target any) error {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return errors.New("content type is invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16*1024+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"api_version": 1, "code": code, "message": "The request could not be completed."})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"api_version": 1, "data": value})
}
func page(items any) map[string]any { return map[string]any{"items": items} }

func successOutcome(success bool) string {
	if success {
		return "success"
	}
	return "failure"
}

func passkeyOptionsResponse(ceremonyID string, options any) map[string]any {
	encoded, err := json.Marshal(options)
	if err != nil {
		return map[string]any{"ceremony_id": ceremonyID}
	}
	result := map[string]any{}
	if json.Unmarshal(encoded, &result) != nil {
		return map[string]any{"ceremony_id": ceremonyID}
	}
	result["ceremony_id"] = ceremonyID
	return result
}

func (s *Server) registerRevocation(sessionID, userID string, revoke func()) func() {
	s.revocationMu.Lock()
	s.revocationNext++
	id := s.revocationNext
	s.revocations[id] = socketRevocation{sessionID: sessionID, userID: userID, revoke: revoke}
	s.revocationMu.Unlock()
	return func() {
		s.revocationMu.Lock()
		delete(s.revocations, id)
		s.revocationMu.Unlock()
	}
}
func (s *Server) revokeSockets(match func(socketRevocation) bool) {
	s.revocationMu.Lock()
	callbacks := make([]func(), 0)
	for _, active := range s.revocations {
		if match(active) {
			callbacks = append(callbacks, active.revoke)
		}
	}
	s.revocationMu.Unlock()
	for _, callback := range callbacks {
		callback()
	}
}
func (s *Server) revokeUserSockets(userID string) {
	s.revokeSockets(func(active socketRevocation) bool { return active.userID == userID })
}
func (s *Server) revokeSessionSocket(sessionID string) {
	s.revokeSockets(func(active socketRevocation) bool { return active.sessionID == sessionID })
}
func (s *Server) audit(ctx context.Context, action, outcome, actor, target, recording string) {
	var actorID, targetID, recordingID *string
	if actor != "" {
		actorID = &actor
	}
	if target != "" {
		targetID = &target
	}
	if recording != "" {
		recordingID = &recording
	}
	if err := s.store.WriteAudit(ctx, action, outcome, actorID, targetID, recordingID, "{}", time.Now()); err != nil {
		s.telemetry.inc("opusrefweb_audit_writes_total", "failure")
		s.telemetry.inc("opusrefweb_db_errors_total", "audit")
		s.telemetry.event("audit_failure", "error", "An operator audit event could not be stored.")
	} else {
		s.telemetry.inc("opusrefweb_audit_writes_total", "success")
	}
}

type recordingCursor struct {
	Start string `json:"start"`
	ID    string `json:"id"`
}
type adminCursor struct {
	Kind     string `json:"kind"`
	Username string `json:"username,omitempty"`
	ID       string `json:"id,omitempty"`
	AuditID  int64  `json:"audit_id,omitempty"`
	Offset   int    `json:"offset,omitempty"`
}

func adminPageRequest(r *http.Request, kind string) (int, adminCursor, error) {
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			return 0, adminCursor{}, errors.New("limit is invalid")
		}
		limit = parsed
	}
	var cursor adminCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.Kind != kind || cursor.Offset < 0 || cursor.AuditID < 0 {
			return 0, adminCursor{}, errors.New("cursor is invalid")
		}
		if (kind == "accounts" && (cursor.Username == "" || cursor.ID == "")) || (kind == "audit" && cursor.AuditID == 0) || (kind == "clients" && cursor.Offset == 0) {
			return 0, adminCursor{}, errors.New("cursor is invalid")
		}
	}
	return limit, cursor, nil
}
func encodeAdminCursor(cursor adminCursor) string {
	encoded, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func recordingQuery(r *http.Request) (store.RecordingQuery, error) {
	values := r.URL.Query()
	source := values.Get("callsign")
	if explicit := values.Get("source_callsign"); explicit != "" {
		if source != "" && !strings.EqualFold(source, explicit) {
			return store.RecordingQuery{}, errors.New("callsign filters conflict")
		}
		source = explicit
	}
	query := store.RecordingQuery{Limit: 50, Source: strings.ToUpper(source), Status: values.Get("status")}
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			return query, errors.New("limit is invalid")
		}
		query.Limit = parsed
	}
	if query.Source != "" {
		if _, err := wire.Callsign(query.Source); err != nil {
			return query, err
		}
	}
	if query.Status != "" && query.Status != "complete" && query.Status != "partial" {
		return query, errors.New("status is invalid")
	}
	for key, destination := range map[string]**time.Time{"from": &query.From, "to": &query.To} {
		if raw := values.Get(key); raw != "" {
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return query, err
			}
			*destination = &parsed
		}
	}
	if query.From != nil && query.To != nil && query.From.After(*query.To) {
		return query, errors.New("time range is invalid")
	}
	if raw := values.Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		var cursor recordingCursor
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.Start == "" || cursor.ID == "" {
			return query, errors.New("cursor is invalid")
		}
		if _, err = time.Parse(time.RFC3339Nano, cursor.Start); err != nil {
			return query, errors.New("cursor is invalid")
		}
		query.BeforeStart, query.BeforeID = cursor.Start, cursor.ID
	}
	return query, nil
}
func encodeRecordingCursor(start time.Time, id string) string {
	encoded, _ := json.Marshal(recordingCursor{Start: start.UTC().Format(time.RFC3339Nano), ID: id})
	return base64.RawURLEncoding.EncodeToString(encoded)
}
func recordingResponse(item store.Recording) map[string]any {
	duration := int64(0)
	if item.EndedAt != nil {
		duration = item.EndedAt.Sub(item.StartedAt).Milliseconds()
	}
	return map[string]any{"id": item.ID, "source_callsign": item.SourceCallsign, "node_callsign": item.NodeCallsign, "started_at": item.StartedAt, "duration_ms": duration, "status": item.Status, "end_reason": item.EndReason, "packet_count": item.PacketCount, "byte_size": item.ByteSize, "first_sequence": item.FirstSequence, "last_sequence": item.LastSequence, "first_timestamp": item.FirstTimestamp, "last_timestamp": item.LastTimestamp}
}
