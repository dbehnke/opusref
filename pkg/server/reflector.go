package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/dbehnke/opusref/internal/transport"
	"github.com/dbehnke/opusref/pkg/wire"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ReflectorOptions struct {
	ID, DisplayName                                                                                                                                                                                 string
	SharedKey                                                                                                                                                                                       []byte
	Limits                                                                                                                                                                                          Limits
	Random                                                                                                                                                                                          io.Reader
	Clock                                                                                                                                                                                           func() time.Time
	InboundQueuePackets, OutboundControlQueuePackets, OutboundMediaQueueFrames, MaxPendingChallenges, MaxPendingNotifications, MaxPendingNotificationsPerClient, MaxCompletedTransactionsPerSession int
	MaxDatagramBytes                                                                                                                                                                                int
	ShutdownGrace                                                                                                                                                                                   time.Duration
	Metrics                                                                                                                                                                                         MetricSink
	Events                                                                                                                                                                                          func(EventRecord)
}
type MetricSink interface {
	Add(string, map[string]string, uint64) error
}
type EventRecord struct {
	Type, Severity string
	Details        map[string]any
}

func (o ReflectorOptions) defaults() ReflectorOptions {
	if o.Random == nil {
		o.Random = rand.Reader
	}
	if o.Clock == nil {
		o.Clock = time.Now
	}
	if o.InboundQueuePackets == 0 {
		o.InboundQueuePackets = 256
	}
	if o.MaxDatagramBytes == 0 {
		o.MaxDatagramBytes = wire.MaxDatagramSize
	}
	if o.OutboundControlQueuePackets == 0 {
		o.OutboundControlQueuePackets = 64
	}
	if o.OutboundMediaQueueFrames == 0 {
		o.OutboundMediaQueueFrames = 256
	}
	if o.MaxPendingChallenges == 0 {
		o.MaxPendingChallenges = 100
	}
	if o.MaxPendingNotifications == 0 {
		o.MaxPendingNotifications = 200
	}
	if o.MaxPendingNotificationsPerClient == 0 {
		o.MaxPendingNotificationsPerClient = 2
	}
	if o.MaxCompletedTransactionsPerSession == 0 {
		o.MaxCompletedTransactionsPerSession = 64
	}
	if o.ShutdownGrace == 0 {
		o.ShutdownGrace = 5 * time.Second
	}
	return o
}

type challenge struct {
	node           string
	client, server []byte
	at             time.Time
}
type peer struct {
	id                         uint64
	address                    net.Addr
	node                       string
	ready, notified, receiving bool
	notifiedFor, receivingFor  streamIdentity
	connected, last            time.Time
}
type streamIdentity struct {
	owner  uint64
	stream uint32
}
type completed struct {
	fingerprint, response []byte
	address               string
	at                    time.Time
}
type notificationKey struct {
	listener uint64
	typ      wire.PacketType
	tx       uint64
}
type notification struct {
	packet  wire.Packet
	attempt int
	next    time.Time
}
type Reflector struct {
	conn         net.PacketConn
	options      ReflectorOptions
	engine       *Engine
	transport    *transport.UDP
	challenges   map[string]challenge
	peers        map[uint64]*peer
	transactions map[string]completed
	pending      map[notificationKey]*notification
	closeOnce    sync.Once
	snapshot     atomic.Pointer[RuntimeSnapshot]
}
type ClientView struct {
	SessionID                   uint64
	NodeCallsign, RemoteAddress string
	Ready                       bool
	ConnectedAt                 time.Time
	LastActivity                time.Time
}
type RuntimeSnapshot struct {
	Ready                                                          bool
	Clients                                                        []ClientView
	Floor                                                          FloorSnapshot
	Updated                                                        time.Time
	InboundDrops, MediaDrops, MediaDropRecipients, ControlFailures uint64
	SequenceGaps                                                   uint64
}

func NewReflector(conn net.PacketConn, options ReflectorOptions) (*Reflector, error) {
	if conn == nil {
		return nil, errors.New("packet connection is required")
	}
	if _, err := wire.ReflectorID(options.ID); err != nil {
		return nil, err
	}
	if options.DisplayName == "" || len(options.DisplayName) > 64 {
		return nil, errors.New("display name is required")
	}
	options = options.defaults()
	options.Limits = options.Limits.defaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	if options.MaxDatagramBytes < wire.BaseHeaderSize || options.MaxDatagramBytes > wire.MaxDatagramSize {
		return nil, errors.New("maximum datagram size is invalid")
	}
	udp, err := transport.NewUDP(conn, options.InboundQueuePackets, options.OutboundControlQueuePackets, options.OutboundMediaQueueFrames)
	if err != nil {
		return nil, err
	}
	r := &Reflector{conn: conn, options: options, engine: NewEngine(options.Limits, options.Clock), transport: udp, challenges: map[string]challenge{}, peers: map[uint64]*peer{}, transactions: map[string]completed{}, pending: map[notificationKey]*notification{}}
	r.publish()
	return r, nil
}

func (o ReflectorOptions) validate() error {
	capacities := []int{o.InboundQueuePackets, o.OutboundControlQueuePackets, o.OutboundMediaQueueFrames, o.MaxPendingChallenges, o.MaxPendingNotifications, o.MaxPendingNotificationsPerClient, o.MaxCompletedTransactionsPerSession, o.Limits.MaxClients, o.Limits.MaxCompletedTransactions}
	for _, capacity := range capacities {
		if capacity <= 0 {
			return errors.New("all queue and state capacities must be positive")
		}
	}
	if o.MaxPendingNotificationsPerClient > o.MaxPendingNotifications || o.MaxCompletedTransactionsPerSession > o.Limits.MaxCompletedTransactions {
		return errors.New("per-session capacity exceeds the global capacity")
	}
	if o.ShutdownGrace <= 0 || o.Limits.SessionTimeout <= 0 || o.Limits.GrantTimeout <= 0 || o.Limits.MediaTimeout <= 0 || o.Limits.TransmitTimeLimit <= 0 {
		return errors.New("all timers must be positive")
	}
	return nil
}
func (r *Reflector) Run(ctx context.Context) error {
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := make(chan error, 1)
	writer := make(chan error, 1)
	go func() { reader <- r.transport.Read(runCtx, r.options.MaxDatagramBytes+1) }()
	go func() { writer <- r.transport.Write(runCtx) }()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case d := <-r.transport.Inbound:
			r.handle(d.Address, d.Data)
		case <-ticker.C:
			r.tick(false)
		case err := <-reader:
			if ctx.Err() == nil {
				return err
			}
			return r.drain(runCtx)
		case err := <-writer:
			if ctx.Err() == nil {
				return err
			}
			return r.drain(runCtx)
		case <-ctx.Done():
			return r.drain(runCtx)
		}
	}
}
func (r *Reflector) drain(ctx context.Context) error {
	drops := r.transport.DisableMedia()
	for kind, count := range drops {
		item := "audio"
		if kind == transport.MediaData {
			item = "data"
		}
		if count.Frames > 0 {
			r.queueDrop("server_media", item, count.Frames, count.Recipients)
		}
	}
	end := r.engine.BeginShutdown()
	if end != nil {
		r.notifyEnd(end)
	}
	r.publish()
	drainCtx, cancel := context.WithTimeout(ctx, r.options.ShutdownGrace)
	defer cancel()
	r.drainPending(drainCtx)
	if len(r.pending) > 0 {
		r.metric("opusref_timeouts_total", map[string]string{"kind": "shutdown"}, 1)
	}
	for _, p := range r.peers {
		packet := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketDisconnect, SessionID: p.id}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, r.transactionID())}}
		r.startNotification(p, packet)
	}
	r.drainPending(drainCtx)
	for _, peer := range r.peers {
		if end := r.disconnectPeer(peer, "server_shutdown"); end != nil {
			r.notifyEnd(end)
		}
	}
	select {
	case <-time.After(25 * time.Millisecond):
	case <-ctx.Done():
	}
	return nil
}
func (r *Reflector) drainPending(ctx context.Context) {
	for len(r.pending) > 0 {
		select {
		case <-ctx.Done():
			return
		case d := <-r.transport.Inbound:
			r.handleDrain(d.Address, d.Data)
		case <-time.After(50 * time.Millisecond):
			r.tick(true)
		}
	}
}
func (r *Reflector) Close() error {
	var err error
	r.closeOnce.Do(func() { err = r.conn.Close() })
	return err
}
func (r *Reflector) handle(addr net.Addr, data []byte) {
	defer r.publish()
	if len(data) > r.options.MaxDatagramBytes {
		r.metric("opusref_packet_errors_total", map[string]string{"reason": "limit_exceeded"}, 1)
		r.event("malformed_packet", "warn", nil)
		return
	}
	p, err := wire.Decode(data)
	if err != nil {
		r.metric("opusref_packet_errors_total", map[string]string{"reason": validationReason(err)}, 1)
		r.event("malformed_packet", "warn", nil)
		return
	}
	r.metric("opusref_packets_total", map[string]string{"direction": "rx", "packet_type": packetName(p.Header.Type)}, 1)
	r.metric("opusref_bytes_total", map[string]string{"direction": "rx", "packet_type": packetName(p.Header.Type)}, uint64(len(data)))
	if p.Header.Type == wire.PacketHello {
		if err := wire.Validate(p, wire.ValidationContext{Direction: wire.ClientToServer, Phase: wire.PreAdmission}); err == nil {
			r.transact(addr, p, func() wire.Packet { return r.hello(addr, p) })
		} else {
			r.metric("opusref_packet_errors_total", map[string]string{"reason": validationReason(err)}, 1)
			r.event("malformed_packet", "warn", nil)
		}
		return
	}
	if p.Header.Type == wire.PacketAuthenticate {
		if err := wire.Validate(p, wire.ValidationContext{Direction: wire.ClientToServer, Phase: wire.PreAdmission}); err == nil {
			r.transact(addr, p, func() wire.Packet { return r.authenticate(addr, p) })
		} else {
			r.metric("opusref_packet_errors_total", map[string]string{"reason": validationReason(err)}, 1)
			r.event("malformed_packet", "warn", nil)
		}
		return
	}
	if p.Header.Type == wire.PacketDisconnect && r.replayDisconnect(addr, p) {
		return
	}
	peer := r.peers[p.Header.SessionID]
	if peer == nil || peer.address.String() != addr.String() {
		reason := "invalid_session"
		if peer != nil {
			reason = "address_mismatch"
		}
		r.metric("opusref_packet_errors_total", map[string]string{"reason": reason}, 1)
		return
	}
	phase := wire.Connected
	if peer.ready {
		phase = wire.Ready
	}
	if err := wire.Validate(p, wire.ValidationContext{Direction: wire.ClientToServer, Phase: phase}); err != nil {
		r.metric("opusref_packet_errors_total", map[string]string{"reason": validationReason(err)}, 1)
		r.event("malformed_packet", "warn", nil)
		return
	}
	accepted := false
	switch p.Header.Type {
	case wire.PacketKeepalive, wire.PacketDisconnect, wire.PacketStreamRequest, wire.PacketStreamEnd:
		accepted = r.transact(addr, p, func() wire.Packet { return r.control(peer, p) })
	case wire.PacketStreamStart, wire.PacketStreamRevoke:
		accepted = r.ack(peer, p)
	case wire.PacketAudio, wire.PacketData:
		accepted = r.media(peer, p, data)
	}
	if accepted && r.peers[peer.id] != nil {
		peer.last = r.options.Clock()
		r.engine.Touch(peer.id, addr.String())
	}
}

func (r *Reflector) replayDisconnect(addr net.Addr, p wire.Packet) bool {
	if err := wire.Validate(p, wire.ValidationContext{Direction: wire.ClientToServer, Phase: wire.Ready}); err != nil {
		return false
	}
	tx, ok := wire.Find(p, wire.TLVTransactionID)
	if !ok {
		return false
	}
	key := r.transactionKey(addr, p, binary.BigEndian.Uint64(tx))
	old, ok := r.transactions[key]
	if !ok || old.address != addr.String() {
		return false
	}
	normalized := p
	normalized.Header.Flags &^= wire.FlagRetry
	sort.Slice(normalized.Extensions, func(i, j int) bool { return normalized.Extensions[i].Type < normalized.Extensions[j].Type })
	fingerprint, err := wire.Encode(normalized)
	if err != nil || !bytes.Equal(old.fingerprint, fingerprint) {
		return false
	}
	if !r.enqueueRaw(addr, old.response) {
		r.replayOverload(addr, old.response)
		return false
	}
	return true
}
func (r *Reflector) handleDrain(addr net.Addr, data []byte) {
	if len(data) > r.options.MaxDatagramBytes {
		return
	}
	p, err := wire.Decode(data)
	if err != nil {
		return
	}
	peer := r.peers[p.Header.SessionID]
	if peer == nil || peer.address.String() != addr.String() {
		return
	}
	if err := wire.Validate(p, wire.ValidationContext{Direction: wire.ClientToServer, Phase: wire.Ready}); err != nil {
		return
	}
	if (p.Header.Type == wire.PacketStreamRevoke || p.Header.Type == wire.PacketDisconnect) && p.Header.Flags == wire.FlagResponse {
		r.ack(peer, p)
	} else if p.Header.Type == wire.PacketDisconnect {
		r.transact(addr, p, func() wire.Packet { return r.control(peer, p) })
	}
}
func (r *Reflector) transact(addr net.Addr, p wire.Packet, apply func() wire.Packet) bool {
	tx, ok := wire.Find(p, wire.TLVTransactionID)
	if !ok {
		return false
	}
	key := r.transactionKey(addr, p, binary.BigEndian.Uint64(tx))
	normalized := p
	normalized.Header.Flags &^= wire.FlagRetry
	sort.Slice(normalized.Extensions, func(i, j int) bool { return normalized.Extensions[i].Type < normalized.Extensions[j].Type })
	fingerprint, _ := wire.Encode(normalized)
	if old, ok := r.transactions[key]; ok {
		if bytes.Equal(old.fingerprint, fingerprint) {
			if !r.enqueueRaw(addr, old.response) {
				r.replayOverload(addr, old.response)
				return false
			}
			response, err := wire.Decode(old.response)
			return err == nil && response.Header.Type != wire.PacketError
		} else {
			r.metric("opusref_packet_errors_total", map[string]string{"reason": "transaction_conflict"}, 1)
			r.sendError(addr, p, wire.ErrorMalformedPacket)
		}
		return false
	}
	if len(r.transactions) >= r.engine.limits.MaxCompletedTransactions {
		r.metric("opusref_packet_errors_total", map[string]string{"reason": "limit_exceeded"}, 1)
		r.sendError(addr, p, wire.ErrorLimitExceeded)
		return false
	}
	if p.Header.SessionID != 0 {
		prefix := fmt.Sprintf("%d/", p.Header.SessionID)
		count := 0
		for key := range r.transactions {
			if strings.HasPrefix(key, prefix) {
				count++
			}
		}
		if count >= r.options.MaxCompletedTransactionsPerSession {
			r.metric("opusref_packet_errors_total", map[string]string{"reason": "limit_exceeded"}, 1)
			r.sendError(addr, p, wire.ErrorLimitExceeded)
			return false
		}
	}
	response := apply()
	if response.Header.Type == 0 {
		return false
	}
	data, err := wire.Encode(response)
	if err != nil {
		return false
	}
	r.transactions[key] = completed{fingerprint: fingerprint, response: data, address: addr.String(), at: r.options.Clock()}
	if !r.enqueueRaw(addr, data) {
		retainDisconnect := p.Header.Type == wire.PacketDisconnect && p.Header.SessionID != 0
		if !retainDisconnect {
			delete(r.transactions, key)
		}
		if p.Header.SessionID == 0 {
			if p.Header.Type == wire.PacketHello {
				tx, _ := wire.Find(p, wire.TLVTransactionID)
				delete(r.challenges, addr.String()+string(tx))
			} else if p.Header.Type == wire.PacketAuthenticate {
				for id, peer := range r.peers {
					if peer.address.String() == addr.String() {
						r.disconnectPeer(r.peers[id], "admission_delivery_failure")
					}
				}
			}
		} else if peer := r.peers[p.Header.SessionID]; peer != nil {
			r.controlOverload(peer)
		}
		return false
	} else if p.Header.Type == wire.PacketKeepalive && p.Header.SessionID != 0 {
		if peer := r.peers[p.Header.SessionID]; peer != nil {
			if floor := r.engine.Snapshot().Floor; floor.Active && floor.SessionID != peer.id && peer.notifiedFor != (streamIdentity{owner: floor.SessionID, stream: floor.StreamID}) {
				r.notifyStart(peer, floor)
			}
		}
	}
	return response.Header.Type != wire.PacketError
}
func (r *Reflector) replayOverload(addr net.Addr, response []byte) {
	p, err := wire.Decode(response)
	if err != nil || p.Header.SessionID == 0 {
		return
	}
	if peer := r.peers[p.Header.SessionID]; peer != nil && peer.address.String() == addr.String() {
		r.controlOverload(peer)
	}
}
func (r *Reflector) transactionKey(addr net.Addr, p wire.Packet, tx uint64) string {
	if p.Header.SessionID == 0 {
		return fmt.Sprintf("%s/%d/%d", addr.String(), p.Header.Type, tx)
	}
	return fmt.Sprintf("%d/%d/%d", p.Header.SessionID, p.Header.Type, tx)
}
func (r *Reflector) hello(addr net.Addr, p wire.Packet) wire.Packet {
	tx, _ := wire.Find(p, wire.TLVTransactionID)
	node, _ := wire.Find(p, wire.TLVNodeCallsign)
	client, _ := wire.Find(p, wire.TLVClientNonce)
	key := addr.String() + string(tx)
	if len(r.challenges) >= r.options.MaxPendingChallenges {
		mode := "open"
		if len(r.options.SharedKey) > 0 {
			mode = "shared_key"
		}
		r.metric("opusref_authentication_total", map[string]string{"result": "overloaded", "mode": mode}, 1)
		return wire.Packet{}
	}
	server := make([]byte, 32)
	if _, err := io.ReadFull(r.options.Random, server); err != nil {
		return wire.Packet{}
	}
	r.challenges[key] = challenge{strings.TrimSpace(string(node)), client, server, r.options.Clock()}
	id, _ := wire.ReflectorID(r.options.ID)
	return wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketChallenge, Flags: wire.FlagResponse}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, {Type: wire.TLVServerNonce, Value: server}, {Type: wire.TLVReflectorID, Value: id}, {Type: wire.TLVDisplayName, Value: []byte(r.options.DisplayName)}}}
}
func (r *Reflector) authenticate(addr net.Addr, p wire.Packet) wire.Packet {
	tx, _ := wire.Find(p, wire.TLVTransactionID)
	key := addr.String() + string(tx)
	c, ok := r.challenges[key]
	if !ok || r.options.Clock().Sub(c.at) > 10*time.Second {
		r.authenticationFailure("rejected")
		return wire.Packet{}
	}
	client, _ := wire.Find(p, wire.TLVClientNonce)
	serverNonce, _ := wire.Find(p, wire.TLVServerNonce)
	if !hmac.Equal(client, c.client) || !hmac.Equal(serverNonce, c.server) {
		r.authenticationFailure("rejected")
		return wire.Packet{}
	}
	if len(r.options.SharedKey) > 0 {
		tag, ok := wire.Find(p, wire.TLVAuthenticationTag)
		if !ok {
			r.authenticationFailure("rejected")
			return wire.Packet{}
		}
		id, _ := wire.ReflectorID(r.options.ID)
		mac := hmac.New(sha256.New, r.options.SharedKey)
		mac.Write([]byte("OPRF-AUTH-V1"))
		node, _ := wire.Callsign(c.node)
		mac.Write(node)
		mac.Write(c.client)
		mac.Write(c.server)
		mac.Write(id)
		if !hmac.Equal(tag, mac.Sum(nil)) {
			r.authenticationFailure("rejected")
			return wire.Packet{}
		}
	}
	var raw [8]byte
	if r.engine.Snapshot().Sessions >= r.engine.limits.MaxClients {
		mode := "open"
		if len(r.options.SharedKey) > 0 {
			mode = "shared_key"
		}
		r.metric("opusref_authentication_total", map[string]string{"result": "overloaded", "mode": mode}, 1)
		r.event("authentication_failed", "warn", nil)
		return wire.Packet{}
	}
	for {
		if _, err := io.ReadFull(r.options.Random, raw[:]); err != nil {
			return wire.Packet{}
		}
		sid := binary.BigEndian.Uint64(raw[:])
		if sid != 0 && r.engine.AddSession(sid, addr.String(), c.node, false) {
			mode := "open"
			if len(r.options.SharedKey) > 0 {
				mode = "shared_key"
			}
			r.metric("opusref_authentication_total", map[string]string{"result": "accepted", "mode": mode}, 1)
			r.event("client_connected", "info", map[string]any{"session_id": sid, "node_callsign": c.node})
			now := r.options.Clock()
			r.peers[sid] = &peer{id: sid, address: addr, node: c.node, connected: now, last: now}
			delete(r.challenges, key)
			id, _ := wire.ReflectorID(r.options.ID)
			return wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketWelcome, Flags: wire.FlagResponse, SessionID: sid}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, {Type: wire.TLVReflectorID, Value: id}, {Type: wire.TLVDisplayName, Value: []byte(r.options.DisplayName)}}}
		}
	}
}

func (r *Reflector) authenticationFailure(result string) {
	mode := "open"
	if len(r.options.SharedKey) > 0 {
		mode = "shared_key"
	}
	r.metric("opusref_authentication_total", map[string]string{"result": result, "mode": mode}, 1)
	r.event("authentication_failed", "warn", nil)
}
func (r *Reflector) control(p *peer, packet wire.Packet) wire.Packet {
	tx, _ := wire.Find(packet, wire.TLVTransactionID)
	base := wire.Header{Version: 1, Type: packet.Header.Type, Flags: wire.FlagResponse, SessionID: p.id, StreamID: packet.Header.StreamID, Sequence: packet.Header.Sequence, Timestamp: packet.Header.Timestamp}
	ext := []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}
	switch packet.Header.Type {
	case wire.PacketKeepalive:
		p.ready = true
		r.engine.SetReady(p.id)
	case wire.PacketDisconnect:
		end := r.disconnectPeer(p, "client_request")
		if end != nil {
			r.notifyEnd(end)
		}
	case wire.PacketStreamRequest:
		source, _ := wire.Find(packet, wire.TLVSourceCallsign)
		result := r.engine.RequestFloor(p.id, packet.Header.StreamID, strings.TrimSpace(string(source)))
		if result == FloorGranted {
			r.metric("opusref_streams_total", map[string]string{"result": "granted"}, 1)
			r.event("stream_granted", "info", map[string]any{"session_id": p.id, "stream_id": packet.Header.StreamID})
			base.Type = wire.PacketStreamGrant
			ext = append(ext, wire.Uint32TLV(wire.TLVTransmitTimeLimit, uint32(r.engine.limits.TransmitTimeLimit/time.Second)))
			floor := r.engine.Snapshot().Floor
			for _, listener := range r.peers {
				if listener.id != p.id && listener.ready {
					r.notifyStart(listener, floor)
				}
			}
		} else {
			r.metric("opusref_streams_total", map[string]string{"result": "busy"}, 1)
			r.metric("opusref_busy_total", map[string]string{}, 1)
			r.event("stream_busy", "info", nil)
			base.Type = wire.PacketStreamBusy
		}
	case wire.PacketStreamEnd:
		end, err := r.engine.End(p.id, packet.Header.StreamID, packet.Header.Sequence, packet.Header.Timestamp, EndNormal)
		if err != nil {
			return r.errorResponse(packet, wire.ErrorInvalidStream)
		}
		r.notifyEnd(end)
		ext = append(ext, wire.Uint16TLV(wire.TLVEndReason, uint16(wire.EndReasonNormal)))
	}
	return wire.Packet{Header: base, Extensions: ext}
}
func (r *Reflector) errorResponse(request wire.Packet, code wire.ErrorCode) wire.Packet {
	ext := []wire.TLV{wire.Uint16TLV(wire.TLVErrorCode, uint16(code))}
	if tx, ok := wire.Find(request, wire.TLVTransactionID); ok {
		ext = append(ext, wire.TLV{Type: wire.TLVTransactionID, Value: tx})
	}
	response := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketError, Flags: wire.FlagResponse, SessionID: request.Header.SessionID, StreamID: request.Header.StreamID}, Extensions: ext}
	encoded, _ := wire.Encode(response)
	original, _ := wire.Encode(request)
	if len(encoded) > len(original) {
		response.Extensions = response.Extensions[:1]
	}
	return response
}
func (r *Reflector) notifyStart(listener *peer, floor FloorSnapshot) {
	owner := r.peers[floor.SessionID]
	if owner == nil {
		return
	}
	node, _ := wire.Callsign(owner.node)
	source, _ := wire.Callsign(floor.SourceCallsign)
	packet := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamStart, SessionID: floor.SessionID, StreamID: floor.StreamID}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, r.transactionID()), {Type: wire.TLVNodeCallsign, Value: node}, {Type: wire.TLVSourceCallsign, Value: source}, wire.Uint32TLV(wire.TLVTransmitTimeLimit, uint32(r.engine.limits.TransmitTimeLimit/time.Second))}}
	identity := streamIdentity{owner: floor.SessionID, stream: floor.StreamID}
	listener.notified = true
	listener.notifiedFor = identity
	r.startNotification(listener, packet)
}
func (r *Reflector) notifyEnd(end *StreamEnd) {
	r.metric("opusref_stream_end_total", map[string]string{"reason": string(end.Reason)}, 1)
	if observer, ok := r.options.Metrics.(interface{ ObserveStreamDuration(float64) }); ok && end.Duration > 0 {
		observer.ObserveStreamDuration(end.Duration.Seconds())
	}
	r.event("stream_ended", "info", map[string]any{"session_id": end.SessionID, "stream_id": end.StreamID, "reason": end.Reason})
	if end.Reason == EndGrantTimeout || end.Reason == EndMediaInactivity {
		r.event("stream_timeout", "warn", map[string]any{"session_id": end.SessionID, "stream_id": end.StreamID, "reason": end.Reason})
	} else if end.Reason == EndTransmitTimeLimit {
		r.event("transmit_time_limit", "warn", map[string]any{"session_id": end.SessionID, "stream_id": end.StreamID, "reason": end.Reason})
	}
	for key := range r.pending {
		if key.typ == wire.PacketStreamStart {
			delete(r.pending, key)
		}
	}
	for _, listener := range r.peers {
		if listener.id == end.SessionID && end.Reason == EndNormal {
			continue
		}
		identity := streamIdentity{owner: end.SessionID, stream: end.StreamID}
		if listener.id != end.SessionID && listener.notifiedFor != identity {
			continue
		}
		packet := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRevoke, SessionID: end.SessionID, StreamID: end.StreamID, Sequence: end.Sequence, Timestamp: end.Timestamp}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, r.transactionID()), wire.Uint16TLV(wire.TLVEndReason, endReason(end.Reason))}}
		if listener.receivingFor == identity {
			listener.receiving = false
			listener.receivingFor = streamIdentity{}
		}
		r.startNotification(listener, packet)
	}
}
func endReason(reason EndReason) uint16 {
	switch reason {
	case EndOwnerDisconnect:
		return uint16(wire.EndReasonOwnerDisconnect)
	case EndGrantTimeout:
		return uint16(wire.EndReasonGrantTimeout)
	case EndMediaInactivity:
		return uint16(wire.EndReasonMediaInactivity)
	case EndTransmitTimeLimit:
		return uint16(wire.EndReasonTransmitTimeLimit)
	case EndServerShutdown:
		return uint16(wire.EndReasonServerShutdown)
	default:
		return uint16(wire.EndReasonNormal)
	}
}
func (r *Reflector) startNotification(listener *peer, p wire.Packet) {
	if len(r.pending) >= r.options.MaxPendingNotifications {
		r.controlOverload(listener)
		return
	}
	count := 0
	for key := range r.pending {
		if key.listener == listener.id {
			count++
		}
	}
	if count >= r.options.MaxPendingNotificationsPerClient {
		r.controlOverload(listener)
		return
	}
	raw, ok := wire.Find(p, wire.TLVTransactionID)
	if !ok || len(raw) != 8 {
		r.controlOverload(listener)
		return
	}
	key := notificationKey{listener.id, p.Header.Type, binary.BigEndian.Uint64(raw)}
	r.pending[key] = &notification{p, 1, r.options.Clock().Add(500 * time.Millisecond)}
	if p.Header.Type == wire.PacketStreamStart {
		listener.receiving = true
		listener.receivingFor = streamIdentity{owner: p.Header.SessionID, stream: p.Header.StreamID}
	}
	r.sendControl(listener.address, p)
}
func (r *Reflector) ack(peer *peer, p wire.Packet) bool {
	raw, ok := wire.Find(p, wire.TLVTransactionID)
	if !ok || len(raw) != 8 {
		return false
	}
	key := notificationKey{peer.id, p.Header.Type, binary.BigEndian.Uint64(raw)}
	n, ok := r.pending[key]
	if !ok || p.Header.StreamID != n.packet.Header.StreamID || p.Header.Sequence != n.packet.Header.Sequence || p.Header.Timestamp != n.packet.Header.Timestamp {
		return false
	}
	delete(r.pending, key)
	if p.Header.Type == wire.PacketStreamStart {
		identity := streamIdentity{owner: n.packet.Header.SessionID, stream: n.packet.Header.StreamID}
		if peer.notifiedFor == identity {
			peer.receiving = true
			peer.receivingFor = identity
		}
	} else if p.Header.Type == wire.PacketStreamRevoke {
		identity := streamIdentity{owner: n.packet.Header.SessionID, stream: n.packet.Header.StreamID}
		if peer.notifiedFor == identity {
			peer.notified = false
			peer.notifiedFor = streamIdentity{}
		}
		if peer.receivingFor == identity {
			peer.receiving = false
			peer.receivingFor = streamIdentity{}
		}
	} else if p.Header.Type == wire.PacketDisconnect {
		r.disconnectPeer(peer, "server_shutdown")
	}
	return true
}
func (r *Reflector) media(owner *peer, p wire.Packet, raw []byte) bool {
	before := r.engine.Snapshot().Floor
	effects, err := r.engine.Media(owner.id, owner.address.String(), p.Header.StreamID, p.Header.Sequence, p.Header.Timestamp, p.Payload)
	if err != nil {
		return false
	}
	after := r.engine.Snapshot().Floor
	if before.StartedAt.IsZero() && !after.StartedAt.IsZero() && after.SessionID == owner.id && after.StreamID == p.Header.StreamID {
		r.event("stream_active", "info", map[string]any{"session_id": owner.id, "stream_id": p.Header.StreamID})
	}
	recipients := make([]net.Addr, 0, len(effects))
	for _, effect := range effects {
		identity := streamIdentity{owner: owner.id, stream: p.Header.StreamID}
		if listener := r.peers[effect.SessionID]; listener != nil && listener.receivingFor == identity {
			recipients = append(recipients, listener.address)
		}
	}
	if len(recipients) > 0 {
		item := "audio"
		if p.Header.Type == wire.PacketData {
			item = "data"
		}
		kind := transport.MediaAudio
		if p.Header.Type == wire.PacketData {
			kind = transport.MediaData
		}
		if r.transport.EnqueueMedia(transport.MediaBatch{Data: raw, Recipients: recipients, Kind: kind}) {
			r.metric("opusref_fanout_frames_total", map[string]string{"item_type": item}, 1)
			r.metric("opusref_fanout_recipients_total", map[string]string{"item_type": item}, uint64(len(recipients)))
		} else {
			r.queueDrop("server_media", item, 1, uint64(len(recipients)))
		}
	}
	return true
}
func (r *Reflector) tick(draining bool) {
	defer r.publish()
	now := r.options.Clock()
	for key, c := range r.challenges {
		if now.Sub(c.at) > 10*time.Second {
			r.metric("opusref_timeouts_total", map[string]string{"kind": "challenge"}, 1)
			delete(r.challenges, key)
		}
	}
	for key, c := range r.transactions {
		if now.Sub(c.at) > 30*time.Second {
			delete(r.transactions, key)
		}
	}
	for id, p := range r.peers {
		if now.Sub(p.last) > r.engine.limits.SessionTimeout {
			r.metric("opusref_timeouts_total", map[string]string{"kind": "session"}, 1)
			end := r.disconnectPeer(r.peers[id], "session_timeout")
			if end != nil {
				r.notifyEnd(end)
			}
		}
	}
	if end := r.engine.Tick(); end != nil {
		kind := "grant"
		if end.Reason == EndMediaInactivity {
			kind = "media_inactivity"
		} else if end.Reason == EndTransmitTimeLimit {
			kind = "transmit_time_limit"
		}
		r.metric("opusref_timeouts_total", map[string]string{"kind": kind}, 1)
		r.notifyEnd(end)
	}
	for key, n := range r.pending {
		if now.Before(n.next) {
			continue
		}
		if n.attempt >= 4 {
			r.metric("opusref_timeouts_total", map[string]string{"kind": "transaction"}, 1)
			delete(r.pending, key)
			if key.typ == wire.PacketStreamStart {
				if p := r.peers[key.listener]; p != nil {
					identity := streamIdentity{owner: n.packet.Header.SessionID, stream: n.packet.Header.StreamID}
					if p.receivingFor == identity {
						p.receiving = false
						p.receivingFor = streamIdentity{}
					}
				}
			}
			continue
		}
		n.packet.Header.Flags |= wire.FlagRetry
		n.attempt++
		delays := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
		if n.attempt < 4 {
			n.next = now.Add(delays[n.attempt-1])
		} else {
			n.next = now
		}
		if p := r.peers[key.listener]; p != nil {
			r.sendControl(p.address, n.packet)
		}
	}
	_ = draining
}
func (r *Reflector) sendError(addr net.Addr, p wire.Packet, code wire.ErrorCode) {
	response := r.errorResponse(p, code)
	original, _ := wire.Encode(p)
	encoded, _ := wire.Encode(response)
	if p.Header.SessionID == 0 || len(encoded) > len(original) {
		return
	}
	r.sendControl(addr, response)
}
func (r *Reflector) sendControl(addr net.Addr, p wire.Packet) bool {
	data, err := wire.Encode(p)
	if err != nil {
		return false
	}
	if r.transport.EnqueueControl(transport.Datagram{Address: addr, Data: data}) {
		r.metric("opusref_packets_total", map[string]string{"direction": "tx", "packet_type": packetName(p.Header.Type)}, 1)
		r.metric("opusref_bytes_total", map[string]string{"direction": "tx", "packet_type": packetName(p.Header.Type)}, uint64(len(data)))
		return true
	}
	r.queueDrop("server_control", "control", 1, 1)
	for _, peer := range r.peers {
		if peer.address.String() == addr.String() {
			r.controlOverload(peer)
			break
		}
	}
	return false
}
func (r *Reflector) controlOverload(peer *peer) {
	end := r.disconnectPeer(peer, "control_overload")
	for key := range r.pending {
		if key.listener == peer.id {
			delete(r.pending, key)
		}
	}
	r.event("control_overload", "error", map[string]any{"session_id": peer.id})
	if end != nil {
		r.notifyEnd(end)
	}
}
func (r *Reflector) enqueueRaw(addr net.Addr, data []byte) bool {
	if r.transport.EnqueueControl(transport.Datagram{Address: addr, Data: data}) {
		if p, err := wire.Decode(data); err == nil {
			r.metric("opusref_packets_total", map[string]string{"direction": "tx", "packet_type": packetName(p.Header.Type)}, 1)
			r.metric("opusref_bytes_total", map[string]string{"direction": "tx", "packet_type": packetName(p.Header.Type)}, uint64(len(data)))
		}
		return true
	}
	r.queueDrop("server_control", "control", 1, 1)
	return false
}

func (r *Reflector) disconnectPeer(peer *peer, reason string) *StreamEnd {
	if peer == nil || r.peers[peer.id] != peer {
		return nil
	}
	end := r.engine.Disconnect(peer.id)
	delete(r.peers, peer.id)
	r.event("client_disconnected", "info", map[string]any{"session_id": peer.id, "node_callsign": peer.node, "reason": reason})
	return end
}

func (r *Reflector) queueDrop(queue, item string, frames, recipients uint64) {
	r.metric("opusref_queue_drops_total", map[string]string{"queue": queue, "item_type": item}, frames)
	if recipients > 0 {
		r.metric("opusref_queue_drop_recipients_total", map[string]string{"queue": queue, "item_type": item}, recipients)
	}
	r.event("queue_drop", "warn", map[string]any{"queue": queue, "item_type": item, "frame_count": frames, "recipient_count": recipients})
}
func (r *Reflector) transactionID() uint64 {
	for {
		var raw [8]byte
		if _, err := io.ReadFull(r.options.Random, raw[:]); err != nil {
			return 0
		}
		id := binary.BigEndian.Uint64(raw[:])
		if id == 0 {
			continue
		}
		collision := false
		for key := range r.pending {
			if key.tx == id {
				collision = true
				break
			}
		}
		if !collision {
			return id
		}
	}
}
func (r *Reflector) publish() {
	clients := make([]ClientView, 0, len(r.peers))
	for _, p := range r.peers {
		host, _, err := net.SplitHostPort(p.address.String())
		masked := p.address.String()
		if err == nil {
			masked = net.JoinHostPort(host, "0")
		}
		clients = append(clients, ClientView{p.id, p.node, masked, p.ready, p.connected, p.last})
	}
	engineSnapshot := r.engine.Snapshot()
	s := &RuntimeSnapshot{Ready: !r.engine.draining, Clients: clients, Floor: engineSnapshot.Floor, Updated: r.options.Clock(), InboundDrops: r.transport.InboundDrops.Load(), MediaDrops: r.transport.MediaDrops.Load(), MediaDropRecipients: r.transport.MediaDropRecipients.Load(), ControlFailures: r.transport.ControlFailures.Load(), SequenceGaps: engineSnapshot.SequenceGaps}
	r.snapshot.Store(s)
}
func (r *Reflector) Snapshot() RuntimeSnapshot {
	s := r.snapshot.Load()
	if s == nil {
		return RuntimeSnapshot{}
	}
	copy := *s
	copy.Clients = append([]ClientView(nil), s.Clients...)
	return copy
}
func (r *Reflector) metric(name string, labels map[string]string, value uint64) {
	if r.options.Metrics != nil {
		_ = r.options.Metrics.Add(name, labels, value)
	}
}
func (r *Reflector) SetMetricSink(sink MetricSink)       { r.options.Metrics = sink }
func (r *Reflector) SetEventSink(sink func(EventRecord)) { r.options.Events = sink }
func (r *Reflector) event(typ, severity string, details map[string]any) {
	if r.options.Events != nil {
		r.options.Events(EventRecord{typ, severity, details})
	}
}
func packetName(typ wire.PacketType) string {
	names := map[wire.PacketType]string{wire.PacketHello: "hello", wire.PacketChallenge: "challenge", wire.PacketAuthenticate: "authenticate", wire.PacketWelcome: "welcome", wire.PacketKeepalive: "keepalive", wire.PacketDisconnect: "disconnect", wire.PacketError: "error", wire.PacketStreamRequest: "stream_request", wire.PacketStreamGrant: "stream_grant", wire.PacketStreamBusy: "stream_busy", wire.PacketStreamStart: "stream_start", wire.PacketStreamEnd: "stream_end", wire.PacketStreamRevoke: "stream_revoke", wire.PacketAudio: "audio", wire.PacketData: "data"}
	return names[typ]
}
func validationReason(err error) string {
	if errors.Is(err, wire.ErrUnsupportedVersion) {
		return "unsupported_version"
	}
	var v *wire.ValidationError
	if errors.As(err, &v) {
		return string(v.Reason)
	}
	return "malformed"
}
