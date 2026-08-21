package client

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"github.com/dbehnke/opusref/pkg/wire"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var ErrBusy = errors.New("reflector floor is busy")

type udpSender struct {
	conn                      *net.UDPConn
	options                   Options
	owner                     *QueueClient
	session                   uint64
	writeMu, pendingMu        sync.Mutex
	pending                   map[requestKey]pendingRequest
	inbound                   chan []byte
	closed                    atomic.Bool
	closing                   atomic.Bool
	inboundDrops              atomic.Uint64
	remote                    remoteStream
	retired                   [8]remoteKey
	retiredCount, retiredNext int
	remoteMu                  sync.Mutex
	controlTokens             chan struct{}
}
type requestKey struct {
	tx     uint64
	stream uint32
}
type pendingRequest struct {
	responses chan wire.Packet
	types     map[wire.PacketType]bool
}
type remoteStream struct {
	owner     uint64
	stream    uint32
	lastMedia time.Time
}
type remoteKey struct {
	owner  uint64
	stream uint32
}

func (s *udpSender) sessionID() uint64 { return s.session }

func (s *udpSender) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		return s.conn.Close()
	}
	return nil
}
func NewUDP(options Options) (Client, error) {
	options = options.defaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	addr, err := net.ResolveUDPAddr("udp", options.ServerAddress)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}
	s := &udpSender{conn: conn, options: options, pending: map[requestKey]pendingRequest{}, inbound: make(chan []byte, options.InboundQueuePackets), controlTokens: make(chan struct{}, options.ControlSendQueuePackets)}
	c, err := New(options, s)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s.owner = c
	return c, nil
}
func (s *udpSender) Disconnect(ctx context.Context) error {
	if s.session == 0 || s.closed.Load() {
		return nil
	}
	s.closing.Store(true)
	tx, err := s.nextTx()
	if err != nil {
		return err
	}
	_, err = s.request(ctx, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketDisconnect, SessionID: s.session}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx)}}, wire.PacketDisconnect)
	return err
}
func (s *udpSender) Send(ctx context.Context, out Outbound) error {
	switch out.Kind {
	case EventStatus:
		if s.session == 0 {
			return s.handshake(ctx)
		}
		tx, err := s.nextTx()
		if err != nil {
			return err
		}
		_, err = s.request(ctx, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketKeepalive, SessionID: s.session}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx)}}, wire.PacketKeepalive)
		return err
	case EventAudio:
		return s.write(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: s.session, StreamID: out.StreamID, Sequence: out.Sequence, Timestamp: out.Timestamp}, Payload: out.Payload})
	case EventData:
		return s.write(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketData, SessionID: s.session, StreamID: out.StreamID, Sequence: out.Sequence, Timestamp: out.Timestamp}, Extensions: []wire.TLV{wire.Uint16TLV(wire.TLVDataType, out.DataType)}, Payload: out.Payload})
	}
	return errors.New("unsupported outbound event")
}
func (s *udpSender) RequestFloor(ctx context.Context, out Outbound) error {
	source, err := wire.Callsign(out.SourceCallsign)
	if err != nil {
		return err
	}
	tx, err := s.nextTx()
	if err != nil {
		return err
	}
	reply, err := s.request(ctx, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRequest, SessionID: s.session, StreamID: out.StreamID}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx), {Type: wire.TLVSourceCallsign, Value: source}}}, wire.PacketStreamGrant, wire.PacketStreamBusy)
	if err != nil {
		return err
	}
	if reply.Header.Type == wire.PacketStreamBusy {
		_ = s.owner.Publish(Event{Kind: EventBusy, SessionID: reply.Header.SessionID, StreamID: reply.Header.StreamID, Message: "reflector floor is busy"})
		return ErrBusy
	}
	if err := s.owner.Publish(Event{Kind: EventStreamGranted, SessionID: s.session, StreamID: out.StreamID, SourceCallsign: out.SourceCallsign}); err != nil {
		return err
	}
	return nil
}
func (s *udpSender) EndFloor(ctx context.Context, out Outbound) error {
	tx, err := s.nextTx()
	if err != nil {
		return err
	}
	_, err = s.request(ctx, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamEnd, SessionID: s.session, StreamID: out.StreamID, Sequence: out.Sequence, Timestamp: out.Timestamp}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx)}}, wire.PacketStreamEnd)
	return err
}
func (s *udpSender) nextTx() (uint64, error) {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, err
		}
		if v := binary.BigEndian.Uint64(raw[:]); v != 0 {
			return v, nil
		}
	}
}
func (s *udpSender) handshake(ctx context.Context) error {
	node, err := wire.Callsign(s.options.NodeCallsign)
	if err != nil {
		return err
	}
	nonce := make([]byte, 32)
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	tx, err := s.nextTx()
	if err != nil {
		return err
	}
	hello := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx), {Type: wire.TLVNodeCallsign, Value: node}, {Type: wire.TLVClientNonce, Value: nonce}}}
	challenge, err := s.exchange(ctx, hello, wire.PacketChallenge, tx)
	if err != nil {
		return err
	}
	serverNonce, _ := wire.Find(challenge, wire.TLVServerNonce)
	extensions := []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx), {Type: wire.TLVClientNonce, Value: nonce}, {Type: wire.TLVServerNonce, Value: serverNonce}}
	if s.options.SharedKey != "" {
		reflector, _ := wire.Find(challenge, wire.TLVReflectorID)
		mac := hmac.New(sha256.New, []byte(s.options.SharedKey))
		mac.Write([]byte("OPRF-AUTH-V1"))
		mac.Write(node)
		mac.Write(nonce)
		mac.Write(serverNonce)
		mac.Write(reflector)
		extensions = append(extensions, wire.TLV{Type: wire.TLVAuthenticationTag, Value: mac.Sum(nil)})
	}
	welcome, err := s.exchange(ctx, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAuthenticate}, Extensions: extensions}, wire.PacketWelcome, tx)
	if err != nil {
		return err
	}
	s.session = welcome.Header.SessionID
	keepTx, err := s.nextTx()
	if err != nil {
		return err
	}
	keep := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketKeepalive, SessionID: s.session}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, keepTx)}}
	if _, err = s.exchange(ctx, keep, wire.PacketKeepalive, keepTx); err != nil {
		return err
	}
	go s.readLoop()
	go s.keepalive()
	return nil
}
func (s *udpSender) exchange(ctx context.Context, p wire.Packet, want wire.PacketType, tx uint64) (wire.Packet, error) {
	waits := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second}
	for attempt, wait := range waits {
		if attempt > 0 {
			p.Header.Flags |= wire.FlagRetry
		}
		if err := s.write(p); err != nil {
			return wire.Packet{}, err
		}
		deadline := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		for {
			if !time.Now().Before(deadline) {
				break
			}
			_ = s.conn.SetReadDeadline(deadline)
			buf := make([]byte, wire.MaxDatagramSize+1)
			n, err := s.conn.Read(buf)
			if err != nil {
				if ctx.Err() != nil {
					return wire.Packet{}, ctx.Err()
				}
				if ne, ok := err.(net.Error); ok && ne.Timeout() {
					break
				}
				continue
			}
			if n > wire.MaxDatagramSize {
				continue
			}
			reply, err := wire.Decode(buf[:n])
			if err != nil {
				continue
			}
			phase := wire.PreAdmission
			if want == wire.PacketKeepalive {
				phase = wire.Connected
			}
			if wire.Validate(reply, wire.ValidationContext{Direction: wire.ServerToClient, Phase: phase}) != nil || reply.Header.Type != want {
				continue
			}
			if want == wire.PacketKeepalive && reply.Header.SessionID != s.session {
				continue
			}
			raw, ok := wire.Find(reply, wire.TLVTransactionID)
			if !ok || binary.BigEndian.Uint64(raw) != tx {
				continue
			}
			return reply, nil
		}
	}
	return wire.Packet{}, errors.New("protocol request timed out")
}
func (s *udpSender) request(ctx context.Context, p wire.Packet, wants ...wire.PacketType) (wire.Packet, error) {
	select {
	case s.controlTokens <- struct{}{}:
		defer func() { <-s.controlTokens }()
	default:
		s.owner.fail(ErrControlBackpressure)
		return wire.Packet{}, ErrControlBackpressure
	}
	raw, _ := wire.Find(p, wire.TLVTransactionID)
	tx := binary.BigEndian.Uint64(raw)
	responses := make(chan wire.Packet, 1)
	types := make(map[wire.PacketType]bool, len(wants)+1)
	for _, typ := range wants {
		types[typ] = true
	}
	types[wire.PacketError] = true
	s.pendingMu.Lock()
	key := requestKey{tx: tx, stream: p.Header.StreamID}
	s.pending[key] = pendingRequest{responses: responses, types: types}
	s.pendingMu.Unlock()
	defer func() { s.pendingMu.Lock(); delete(s.pending, key); s.pendingMu.Unlock() }()
	waits := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 2 * time.Second}
	for attempt, wait := range waits {
		if attempt > 0 {
			p.Header.Flags |= wire.FlagRetry
		}
		if err := s.write(p); err != nil {
			return wire.Packet{}, err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return wire.Packet{}, ctx.Err()
		case reply := <-responses:
			timer.Stop()
			if reply.Header.Type == wire.PacketError {
				event := protocolErrorEvent(reply)
				_ = s.owner.Publish(event)
				return wire.Packet{}, &ProtocolError{Code: event.ProtocolErrorCode, Message: event.Message}
			}
			for _, want := range wants {
				if reply.Header.Type == want {
					return reply, nil
				}
			}
			return wire.Packet{}, errors.New("unexpected protocol response")
		case <-timer.C:
		}
	}
	return wire.Packet{}, errors.New("protocol request timed out")
}
func (s *udpSender) write(p wire.Packet) error {
	data, err := wire.Encode(p)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.conn.Write(data)
	return err
}
func (s *udpSender) readLoop() {
	go s.processLoop()
	buf := make([]byte, wire.MaxDatagramSize+1)
	for {
		_ = s.conn.SetReadDeadline(time.Now().Add(time.Second))
		n, err := s.conn.Read(buf)
		if err != nil {
			if s.closed.Load() {
				return
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			s.owner.fail(err)
			return
		}
		if n > wire.MaxDatagramSize {
			continue
		}
		s.enqueueInbound(buf[:n])
	}
}
func (s *udpSender) enqueueInbound(data []byte) {
	copy := append([]byte(nil), data...)
	select {
	case s.inbound <- copy:
	default:
		s.inboundDrops.Add(1)
	}
}
func (s *udpSender) processLoop() {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var data []byte
		select {
		case data = <-s.inbound:
		case <-ticker.C:
			s.expireRemote(time.Now())
			continue
		case <-s.owner.Done():
			return
		}
		p, err := wire.Decode(data)
		if err != nil || wire.Validate(p, wire.ValidationContext{Direction: wire.ServerToClient, Phase: wire.Ready}) != nil {
			continue
		}
		if p.Header.Type != wire.PacketAudio && p.Header.Type != wire.PacketData && p.Header.Type != wire.PacketStreamStart && p.Header.Type != wire.PacketStreamRevoke && p.Header.SessionID != s.session {
			continue
		}
		if raw, ok := wire.Find(p, wire.TLVTransactionID); ok {
			tx := binary.BigEndian.Uint64(raw)
			s.pendingMu.Lock()
			var pending pendingRequest
			if p.Header.SessionID == s.session {
				pending = s.pending[requestKey{tx: tx, stream: p.Header.StreamID}]
			}
			s.pendingMu.Unlock()
			if pending.responses != nil && pending.types[p.Header.Type] {
				select {
				case pending.responses <- p:
				default:
				}
				continue
			}
		}
		event := Event{SessionID: p.Header.SessionID, StreamID: p.Header.StreamID, Sequence: p.Header.Sequence, Timestamp: p.Header.Timestamp, Payload: p.Payload}
		s.remoteMu.Lock()
		switch p.Header.Type {
		case wire.PacketAudio:
			if s.remote.owner != p.Header.SessionID || s.remote.stream != p.Header.StreamID {
				s.remoteMu.Unlock()
				continue
			}
			s.remote.lastMedia = time.Now()
			event.Kind = EventAudio
		case wire.PacketData:
			if s.remote.owner != p.Header.SessionID || s.remote.stream != p.Header.StreamID {
				s.remoteMu.Unlock()
				continue
			}
			s.remote.lastMedia = time.Now()
			event.Kind = EventData
			if raw, ok := wire.Find(p, wire.TLVDataType); ok {
				event.DataType = binary.BigEndian.Uint16(raw)
			}
		case wire.PacketStreamStart:
			s.ack(p)
			key := remoteKey{p.Header.SessionID, p.Header.StreamID}
			if s.remote.owner == key.owner && s.remote.stream == key.stream {
				s.remoteMu.Unlock()
				continue
			}
			if s.isRetiredLocked(key) {
				s.remoteMu.Unlock()
				continue
			}
			if s.remote.stream != 0 {
				s.retireLocked(remoteKey{s.remote.owner, s.remote.stream})
			}
			s.remote = remoteStream{owner: p.Header.SessionID, stream: p.Header.StreamID, lastMedia: time.Now()}
			event.Kind = EventStreamStart
			if v, ok := wire.Find(p, wire.TLVNodeCallsign); ok {
				event.NodeCallsign = strings.TrimSpace(string(v))
			}
			if v, ok := wire.Find(p, wire.TLVSourceCallsign); ok {
				event.SourceCallsign = strings.TrimSpace(string(v))
			}
		case wire.PacketStreamRevoke:
			s.ack(p)
			local := p.Header.SessionID == s.session && s.owner.revoke(p.Header.StreamID)
			key := remoteKey{p.Header.SessionID, p.Header.StreamID}
			matches := s.remote.owner == key.owner && s.remote.stream == key.stream
			s.retireLocked(key)
			if !matches && !local {
				s.remoteMu.Unlock()
				continue
			}
			if matches {
				s.remote = remoteStream{}
			}
			event.Kind = EventStreamEnd
			if raw, ok := wire.Find(p, wire.TLVEndReason); ok {
				event.EndReason = wire.EndReason(binary.BigEndian.Uint16(raw))
			}
		case wire.PacketDisconnect:
			s.ack(p)
			go s.owner.remoteClose()
			s.remoteMu.Unlock()
			return
		case wire.PacketError:
			event = protocolErrorEvent(p)
		default:
			s.remoteMu.Unlock()
			continue
		}
		s.remoteMu.Unlock()
		_ = s.owner.Publish(event)
	}
}

func protocolErrorEvent(p wire.Packet) Event {
	event := Event{Kind: EventProtocolError, SessionID: p.Header.SessionID, StreamID: p.Header.StreamID, Sequence: p.Header.Sequence, Timestamp: p.Header.Timestamp}
	if raw, ok := wire.Find(p, wire.TLVErrorCode); ok && len(raw) == 2 {
		event.ProtocolErrorCode = binary.BigEndian.Uint16(raw)
	}
	if raw, ok := wire.Find(p, wire.TLVErrorText); ok {
		event.Message = string(raw)
	}
	return event
}
func (s *udpSender) expireRemote(now time.Time) {
	s.remoteMu.Lock()
	if s.remote.stream == 0 || now.Sub(s.remote.lastMedia) <= 2*time.Second {
		s.remoteMu.Unlock()
		return
	}
	expired := s.remote
	s.retireLocked(remoteKey{expired.owner, expired.stream})
	s.remote = remoteStream{}
	s.remoteMu.Unlock()
	_ = s.owner.Publish(Event{Kind: EventStreamEnd, SessionID: expired.owner, StreamID: expired.stream, Message: "receive state expired", Synthetic: true})
}
func (s *udpSender) isRetiredLocked(key remoteKey) bool {
	for index := 0; index < s.retiredCount; index++ {
		if s.retired[index] == key {
			return true
		}
	}
	return false
}
func (s *udpSender) retireLocked(key remoteKey) {
	if key.stream == 0 || s.isRetiredLocked(key) {
		return
	}
	if s.retiredCount < len(s.retired) {
		s.retired[s.retiredCount] = key
		s.retiredCount++
		return
	}
	s.retired[s.retiredNext] = key
	s.retiredNext = (s.retiredNext + 1) % len(s.retired)
}
func (s *udpSender) ack(p wire.Packet) {
	tx, ok := wire.Find(p, wire.TLVTransactionID)
	if !ok {
		return
	}
	_ = s.write(wire.Packet{Header: wire.Header{Version: 1, Type: p.Header.Type, Flags: wire.FlagResponse, SessionID: s.session, StreamID: p.Header.StreamID, Sequence: p.Header.Sequence, Timestamp: p.Header.Timestamp}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}})
}
func (s *udpSender) keepalive() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		if s.closed.Load() || s.closing.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.options.OperationTimeout)
		if err := s.Send(ctx, Outbound{Kind: EventStatus}); err != nil {
			s.owner.fail(err)
			cancel()
			return
		}
		cancel()
	}
}
