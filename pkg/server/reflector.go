package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"github.com/dbehnke/opusref/pkg/wire"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

type ReflectorOptions struct {
	ID, DisplayName string
	SharedKey       []byte
	Limits          Limits
	Random          io.Reader
}
type challenge struct {
	node           string
	client, server []byte
	at             time.Time
}
type peer struct {
	id      uint64
	address net.Addr
	node    string
	ready   bool
}
type Reflector struct {
	conn       net.PacketConn
	options    ReflectorOptions
	engine     *Engine
	mu         sync.Mutex
	challenges map[string]challenge
	peers      map[uint64]*peer
	closed     bool
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
	if options.Random == nil {
		options.Random = rand.Reader
	}
	return &Reflector{conn: conn, options: options, engine: NewEngine(options.Limits, time.Now), challenges: map[string]challenge{}, peers: map[uint64]*peer{}}, nil
}
func (r *Reflector) Run(ctx context.Context) error {
	buf := make([]byte, wire.MaxDatagramSize)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		_ = r.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, addr, err := r.conn.ReadFrom(buf)
		if err == nil {
			r.handle(addr, append([]byte(nil), buf[:n]...))
		} else if ctx.Err() != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			r.expire()
		default:
		}
	}
}
func (r *Reflector) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	r.engine.BeginShutdown()
	r.mu.Unlock()
	return r.conn.Close()
}
func (r *Reflector) handle(addr net.Addr, data []byte) {
	p, err := wire.Decode(data)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch p.Header.Type {
	case wire.PacketHello:
		r.hello(addr, p)
	case wire.PacketAuthenticate:
		r.authenticate(addr, p)
	default:
		peer := r.peers[p.Header.SessionID]
		if peer == nil || peer.address.String() != addr.String() {
			return
		}
		r.admitted(peer, p, data)
	}
}
func (r *Reflector) hello(addr net.Addr, p wire.Packet) {
	if err := wire.Validate(p, wire.ValidationContext{Direction: wire.ClientToServer, Phase: wire.PreAdmission}); err != nil {
		return
	}
	tx, _ := wire.Find(p, wire.TLVTransactionID)
	node, _ := wire.Find(p, wire.TLVNodeCallsign)
	client, _ := wire.Find(p, wire.TLVClientNonce)
	server := make([]byte, 32)
	if _, err := io.ReadFull(r.options.Random, server); err != nil {
		return
	}
	r.challenges[addr.String()+string(tx)] = challenge{strings.TrimSpace(string(node)), client, server, time.Now()}
	id, _ := wire.ReflectorID(r.options.ID)
	r.send(addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketChallenge, Flags: wire.FlagResponse}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, {Type: wire.TLVServerNonce, Value: server}, {Type: wire.TLVReflectorID, Value: id}, {Type: wire.TLVDisplayName, Value: []byte(r.options.DisplayName)}}})
}
func (r *Reflector) authenticate(addr net.Addr, p wire.Packet) {
	tx, ok := wire.Find(p, wire.TLVTransactionID)
	if !ok {
		return
	}
	c, ok := r.challenges[addr.String()+string(tx)]
	if !ok || time.Since(c.at) > 10*time.Second {
		return
	}
	client, _ := wire.Find(p, wire.TLVClientNonce)
	server, _ := wire.Find(p, wire.TLVServerNonce)
	if !hmac.Equal(client, c.client) || !hmac.Equal(server, c.server) {
		return
	}
	if len(r.options.SharedKey) > 0 {
		tag, ok := wire.Find(p, wire.TLVAuthenticationTag)
		if !ok {
			return
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
			return
		}
	}
	var raw [8]byte
	if _, err := io.ReadFull(r.options.Random, raw[:]); err != nil {
		return
	}
	sid := binary.BigEndian.Uint64(raw[:])
	if sid == 0 {
		sid = 1
	}
	if !r.engine.AddSession(sid, addr.String(), c.node, false) {
		return
	}
	r.peers[sid] = &peer{sid, addr, c.node, false}
	delete(r.challenges, addr.String()+string(tx))
	id, _ := wire.ReflectorID(r.options.ID)
	r.send(addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketWelcome, Flags: wire.FlagResponse, SessionID: sid}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, {Type: wire.TLVReflectorID, Value: id}, {Type: wire.TLVDisplayName, Value: []byte(r.options.DisplayName)}}})
}
func (r *Reflector) admitted(peer *peer, p wire.Packet, raw []byte) {
	tx, _ := wire.Find(p, wire.TLVTransactionID)
	switch p.Header.Type {
	case wire.PacketKeepalive:
		peer.ready = true
		r.engine.SetReady(peer.id)
		r.response(peer.address, p, tx, nil)
	case wire.PacketDisconnect:
		r.response(peer.address, p, tx, nil)
		r.engine.Disconnect(peer.id)
		delete(r.peers, peer.id)
	case wire.PacketStreamRequest:
		source, _ := wire.Find(p, wire.TLVSourceCallsign)
		result := r.engine.RequestFloor(peer.id, p.Header.StreamID, strings.TrimSpace(string(source)))
		typ := wire.PacketStreamBusy
		extra := []wire.TLV(nil)
		if result == FloorGranted {
			typ = wire.PacketStreamGrant
			extra = []wire.TLV{wire.Uint32TLV(wire.TLVTransmitTimeLimit, uint32(r.engine.limits.TransmitTimeLimit/time.Second))}
		}
		out := wire.Packet{Header: wire.Header{Version: 1, Type: typ, Flags: wire.FlagResponse, SessionID: peer.id, StreamID: p.Header.StreamID}, Extensions: append([]wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}, extra...)}
		r.send(peer.address, out)
	case wire.PacketAudio, wire.PacketData:
		effects, err := r.engine.Media(peer.id, peer.address.String(), p.Header.StreamID, p.Header.Sequence, p.Header.Timestamp, p.Payload)
		if err != nil {
			return
		}
		for _, effect := range effects {
			if listener := r.peers[effect.SessionID]; listener != nil {
				_, _ = r.conn.WriteTo(raw, listener.address)
			}
		}
	case wire.PacketStreamEnd:
		end := r.engine.End(peer.id, EndNormal)
		if end != nil {
			r.response(peer.address, p, tx, []wire.TLV{wire.Uint16TLV(wire.TLVEndReason, uint16(wire.EndReasonNormal))})
		}
	}
}
func (r *Reflector) response(addr net.Addr, p wire.Packet, tx []byte, extra []wire.TLV) {
	ext := []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}
	ext = append(ext, extra...)
	r.send(addr, wire.Packet{Header: wire.Header{Version: 1, Type: p.Header.Type, Flags: wire.FlagResponse, SessionID: p.Header.SessionID, StreamID: p.Header.StreamID, Sequence: p.Header.Sequence, Timestamp: p.Header.Timestamp}, Extensions: ext})
}
func (r *Reflector) send(addr net.Addr, p wire.Packet) {
	data, err := wire.Encode(p)
	if err == nil {
		_, _ = r.conn.WriteTo(data, addr)
	}
}
func (r *Reflector) expire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.engine.Tick()
	now := time.Now()
	for key, c := range r.challenges {
		if now.Sub(c.at) > 10*time.Second {
			delete(r.challenges, key)
		}
	}
}
