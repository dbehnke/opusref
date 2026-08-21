// Package httpapi exposes the versioned web console API.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/dbehnke/opusref/internal/webapp/auth"
	"github.com/dbehnke/opusref/internal/webapp/gateway"
	wsprotocol "github.com/dbehnke/opusref/internal/webapp/socket"
	"github.com/dbehnke/opusref/internal/webapp/store"
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
	MaxConcurrentHashes          int
}
type Server struct {
	cfg                  Config
	store                *store.Store
	ready                atomic.Bool
	public, monitor      http.Handler
	hashSlots, hashQueue chan struct{}
}

func New(cfg Config, state *store.Store) *Server {
	concurrent := cfg.MaxConcurrentHashes
	if concurrent <= 0 {
		concurrent = 2
	}
	s := &Server{cfg: cfg, store: state, hashSlots: make(chan struct{}, concurrent), hashQueue: make(chan struct{}, 16)}
	s.ready.Store(true)
	s.public = s.security(s.publicMux())
	s.monitor = s.security(s.monitorMux())
	return s
}
func (s *Server) PublicHandler() http.Handler  { return s.public }
func (s *Server) MonitorHandler() http.Handler { return s.monitor }
func (s *Server) SetReady(v bool)              { s.ready.Store(v) }
func (s *Server) publicMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /api/v1/session", s.session)
	mux.HandleFunc("POST /api/v1/auth/login", s.login)
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireSession(s.logout))
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
	kind websocket.MessageType
	data []byte
}

func (s *Server) webSocket(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != s.cfg.PublicOrigin {
		writeError(w, http.StatusForbidden, "origin_rejected")
		return
	}
	session, sessionErr := s.requestSession(r)
	if !s.cfg.OpenAccess && sessionErr != nil {
		writeError(w, http.StatusUnauthorized, "authentication_required")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(wsprotocol.MaxControlMessage)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
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
	if err = json.Unmarshal(hello.Body, &body); err != nil || !body.Audio.Encoder || !body.Audio.Decoder || body.Audio.ContextRate != 48000 {
		_ = conn.Close(4400, "audio_capability_required")
		return
	}
	authenticated := sessionErr == nil
	if authenticated && !s.store.VerifyCSRF(ctx, session.ID, body.CSRF) {
		authenticated = false
		if !s.cfg.OpenAccess {
			_ = conn.Close(4401, "session_invalid")
			return
		}
	}
	output := make(chan socketOutput, 32)
	writerDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				writerDone <- ctx.Err()
				return
			case item := <-output:
				writeCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				err := conn.Write(writeCtx, item.kind, item.data)
				stop()
				if err != nil {
					writerDone <- err
					return
				}
			}
		}
	}()
	sendControl := func(value any) bool {
		encoded, err := json.Marshal(value)
		if err != nil {
			return false
		}
		select {
		case output <- socketOutput{websocket.MessageText, encoded}:
			return true
		default:
			return false
		}
	}
	if !sendControl(map[string]any{"api_version": 1, "type": "hello_ok", "request_id": hello.RequestID, "body": map[string]any{"authenticated": authenticated, "role": session.Role, "ptt_available": authenticated && !session.PasswordChangeRequired && session.Callsign != "" && s.cfg.PTT != nil, "passkey_available": false, "limits": map[string]any{"media_bytes": 1200, "control_bytes": 16384}}}) {
		_ = conn.Close(4409, "overload")
		return
	}
	var liveCancel func()
	if s.cfg.LiveHub != nil {
		capacity := s.cfg.LiveQueuePackets
		if capacity <= 0 {
			capacity = 64
		}
		_, media, cancelSub := s.cfg.LiveHub.Subscribe(capacity)
		liveCancel = cancelSub
		defer liveCancel()
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case packet, ok := <-media:
					if !ok {
						return
					}
					encoded, err := wsprotocol.EncodeMedia(packet)
					if err != nil {
						continue
					}
					select {
					case output <- socketOutput{websocket.MessageBinary, encoded}:
					default:
						cancel()
						return
					}
				}
			}
		}()
	}
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
			if !authenticated || s.cfg.PTT == nil {
				_ = conn.Close(4403, "forbidden")
				break
			}
			if err = s.cfg.PTT.Send(ctx, session.ID, media.ChannelID, media.Sequence, media.Timestamp, media.Payload); err != nil {
				_ = s.cfg.PTT.StopSession(context.Background(), session.ID)
				_ = conn.Close(4400, "invalid_media_state")
				break
			}
			continue
		}
		control, decodeErr := wsprotocol.DecodeControl(data, wsprotocol.ClientToServer)
		if decodeErr != nil || control.Type == "hello" {
			_ = conn.Close(4400, "protocol_violation")
			break
		}
		switch control.Type {
		case "ptt_start":
			if !authenticated || session.PasswordChangeRequired || session.Callsign == "" || s.cfg.PTT == nil {
				sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "forbidden", "text": "PTT is not available."}})
				continue
			}
			grant, startErr := s.cfg.PTT.Start(ctx, session.ID, session.Callsign)
			if startErr != nil {
				sendControl(map[string]any{"api_version": 1, "type": "ptt_busy", "request_id": control.RequestID, "body": map[string]any{}})
				continue
			}
			sendControl(map[string]any{"api_version": 1, "type": "ptt_granted", "request_id": control.RequestID, "body": map[string]any{"channel_id": grant.ChannelID, "tot_seconds": 180}})
		case "ptt_stop":
			var stop struct {
				ChannelID uint64 `json:"channel_id"`
			}
			if json.Unmarshal(control.Body, &stop) != nil || s.cfg.PTT == nil {
				_ = conn.Close(4400, "invalid_request")
				return
			}
			_ = s.cfg.PTT.Stop(ctx, session.ID, stop.ChannelID)
			sendControl(map[string]any{"api_version": 1, "type": "ptt_ended", "request_id": control.RequestID, "body": map[string]any{"channel_id": stop.ChannelID, "reason": "normal"}})
		default:
			sendControl(map[string]any{"api_version": 1, "type": "error", "request_id": control.RequestID, "body": map[string]any{"code": "unsupported", "text": "The request type is not supported."}})
		}
	}
	if authenticated && s.cfg.PTT != nil {
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
func (s *Server) monitorMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte("opusrefweb_up 1\n"))
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
		next.ServeHTTP(w, r)
	})
}
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
func (s *Server) readyz(w http.ResponseWriter, _ *http.Request) {
	if !s.ready.Load() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "not_ready"})
		return
	}
	n, err := s.store.EnabledAdminCount(context.Background())
	if err != nil || n == 0 {
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
	writeJSON(w, http.StatusOK, map[string]any{"health": "ok", "ready": s.ready.Load(), "reflector": map[string]any{"id": "", "display_name": ""}, "client_count": 0, "floor": map[string]any{"active": false}, "recording": map[string]any{"available": true, "quota_full": false}, "server_time": time.Now().UTC().Format(time.RFC3339)})
}
func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	session, err := s.requestSession(r)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": false, "passkey_available": false})
		return
	}
	csrf, err := s.store.RotateCSRF(r.Context(), session.ID)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "session_unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "role": session.Role, "username": session.Username, "source_callsign": session.Callsign, "csrf_token": csrf, "forced_password_change": session.PasswordChangeRequired, "passkey_available": false})
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
	user, err := s.store.FindUserByUsername(r.Context(), in.Username)
	if err != nil || user.Disabled {
		writeError(w, http.StatusUnauthorized, "authentication_failed")
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
	ok, _, err := auth.VerifyPassword(in.Password, user.PasswordHash, s.cfg.Argon2)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "authentication_failed")
		return
	}
	raw, csrf, session, err := s.store.CreateSession(r.Context(), user.ID, time.Now(), s.cfg.SessionIdle, s.cfg.SessionAbsolute, s.cfg.MaxSessions)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "authentication_unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: raw, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: session.AbsoluteExpiry})
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "role": session.Role, "username": user.Username, "source_callsign": user.Callsign, "csrf_token": csrf, "forced_password_change": session.PasswordChangeRequired, "passkey_available": false})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request, session store.Session) {
	if !s.validCSRF(r, session) {
		writeError(w, http.StatusForbidden, "csrf_rejected")
		return
	}
	cookie, _ := r.Cookie(CookieName)
	_ = s.store.RevokeCurrentSession(r.Context(), cookie.Value, time.Now())
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	writeJSON(w, http.StatusOK, map[string]any{"logged_out": true})
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
func (s *Server) requestSession(r *http.Request) (store.Session, error) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return store.Session{}, store.ErrUnauthorized
	}
	return s.store.AuthenticateSession(r.Context(), cookie.Value, time.Now())
}
func (s *Server) validCSRF(r *http.Request, session store.Session) bool {
	return s.sameOrigin(r) && s.store.VerifyCSRF(r.Context(), session.ID, r.Header.Get("X-CSRF-Token"))
}
func (s *Server) sameOrigin(r *http.Request) bool {
	return r.Header.Get("Origin") == s.cfg.PublicOrigin && r.Header.Get("Sec-Fetch-Site") == "same-origin"
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
