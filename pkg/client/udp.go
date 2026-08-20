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

type udpSender struct {
	conn    *net.UDPConn
	options Options
	owner   *QueueClient
	session uint64
	tx      atomic.Uint64
	mu      sync.Mutex
	closed  atomic.Bool
}

func (s *udpSender) Close() error {
	if s.closed.CompareAndSwap(false, true) {
		return s.conn.Close()
	}
	return nil
}

// NewUDP creates a client that uses one connected UDP socket.
func NewUDP(options Options) (Client, error) {
	options = options.defaults()
	addr, err := net.ResolveUDPAddr("udp", options.ServerAddress)
	if err != nil {
		return nil, err
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, err
	}
	s := &udpSender{conn: conn, options: options}
	c, err := New(options, s)
	if err != nil {
		conn.Close()
		return nil, err
	}
	s.owner = c
	return c, nil
}
func (s *udpSender) Send(ctx context.Context, out Outbound) error {
	switch out.Kind {
	case EventStatus:
		if s.session == 0 {
			return s.handshake(ctx)
		}
		return s.write(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketKeepalive, SessionID: s.session}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, s.nextTx())}})
	case EventStreamStart:
		source, err := wire.Callsign(out.SourceCallsign)
		if err != nil {
			return err
		}
		return s.write(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRequest, SessionID: s.session, StreamID: out.StreamID}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, s.nextTx()), {Type: wire.TLVSourceCallsign, Value: source}}})
	case EventAudio:
		return s.write(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: s.session, StreamID: out.StreamID, Sequence: out.Sequence, Timestamp: out.Timestamp}, Payload: out.Payload})
	case EventData:
		return s.write(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketData, SessionID: s.session, StreamID: out.StreamID, Sequence: out.Sequence, Timestamp: out.Timestamp}, Extensions: []wire.TLV{wire.Uint16TLV(wire.TLVDataType, out.DataType)}, Payload: out.Payload})
	case EventStreamEnd:
		return s.write(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamEnd, SessionID: s.session, StreamID: out.StreamID, Sequence: out.Sequence}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, s.nextTx())}})
	}
	return errors.New("unsupported outbound event")
}
func (s *udpSender) nextTx() uint64 {
	v := s.tx.Add(1)
	if v == 0 {
		v = s.tx.Add(1)
	}
	return v
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
	tx := s.nextTx()
	hello := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx), {Type: wire.TLVNodeCallsign, Value: node}, {Type: wire.TLVClientNonce, Value: nonce}}}
	challenge, err := s.exchange(ctx, hello, wire.PacketChallenge)
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
	welcome, err := s.exchange(ctx, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAuthenticate}, Extensions: extensions}, wire.PacketWelcome)
	if err != nil {
		return err
	}
	s.session = welcome.Header.SessionID
	keep := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketKeepalive, SessionID: s.session}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, s.nextTx())}}
	if _, err = s.exchange(ctx, keep, wire.PacketKeepalive); err != nil {
		return err
	}
	go s.readLoop()
	go s.keepalive()
	return nil
}
func (s *udpSender) exchange(ctx context.Context, p wire.Packet, want wire.PacketType) (wire.Packet, error) {
	delays := []time.Duration{0, 500 * time.Millisecond, time.Second, 2 * time.Second}
	for attempt, delay := range delays {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return wire.Packet{}, ctx.Err()
			case <-time.After(delay):
			}
		}
		if attempt > 0 {
			p.Header.Flags |= wire.FlagRetry
		}
		if err := s.write(p); err != nil {
			return wire.Packet{}, err
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		_ = s.conn.SetReadDeadline(deadline)
		buf := make([]byte, wire.MaxDatagramSize)
		n, err := s.conn.Read(buf)
		if err != nil {
			continue
		}
		reply, err := wire.Decode(buf[:n])
		if err == nil && reply.Header.Type == want {
			return reply, nil
		}
	}
	return wire.Packet{}, errors.New("protocol request timed out")
}
func (s *udpSender) write(p wire.Packet) error {
	data, err := wire.Encode(p)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.conn.Write(data)
	return err
}
func (s *udpSender) readLoop() {
	buf := make([]byte, wire.MaxDatagramSize)
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
		p, err := wire.Decode(buf[:n])
		if err != nil {
			continue
		}
		event := Event{SessionID: p.Header.SessionID, StreamID: p.Header.StreamID, Sequence: p.Header.Sequence, Timestamp: p.Header.Timestamp, Payload: p.Payload}
		switch p.Header.Type {
		case wire.PacketAudio:
			event.Kind = EventAudio
		case wire.PacketData:
			event.Kind = EventData
			if raw, ok := wire.Find(p, wire.TLVDataType); ok {
				event.DataType = binary.BigEndian.Uint16(raw)
			}
		case wire.PacketStreamStart:
			event.Kind = EventStreamStart
			if v, ok := wire.Find(p, wire.TLVNodeCallsign); ok {
				event.NodeCallsign = strings.TrimSpace(string(v))
			}
			if v, ok := wire.Find(p, wire.TLVSourceCallsign); ok {
				event.SourceCallsign = strings.TrimSpace(string(v))
			}
			s.ack(p)
		case wire.PacketStreamRevoke:
			event.Kind = EventStreamEnd
			s.ack(p)
		case wire.PacketStreamBusy:
			event.Kind = EventBusy
		case wire.PacketError:
			event.Kind = EventProtocolError
		default:
			continue
		}
		_ = s.owner.Publish(event)
	}
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
		if s.closed.Load() {
			return
		}
		_ = s.Send(context.Background(), Outbound{Kind: EventStatus})
	}
}
