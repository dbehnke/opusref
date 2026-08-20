package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/dbehnke/opusref/pkg/monitor"
	"github.com/dbehnke/opusref/pkg/wire"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReflectorOpenHandshakeAndMediaFanout(t *testing.T) {
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	metrics := monitor.New(8, 0, nil)
	r, err := NewReflector(serverConn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	defer r.Close()
	connect := func(call string) (net.PacketConn, uint64) {
		c, e := net.ListenPacket("udp", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		tx := []byte{0, 0, 0, 0, 0, 0, 0, 1}
		nonce := bytes.Repeat([]byte{2}, 32)
		cs, _ := wire.Callsign(call)
		sendPacket(t, c, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, {Type: wire.TLVNodeCallsign, Value: cs}, {Type: wire.TLVClientNonce, Value: nonce}}})
		challenge := readPacket(t, c)
		sn, _ := wire.Find(challenge, wire.TLVServerNonce)
		sendPacket(t, c, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAuthenticate}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, {Type: wire.TLVClientNonce, Value: nonce}, {Type: wire.TLVServerNonce, Value: sn}}})
		welcome := readPacket(t, c)
		sid := welcome.Header.SessionID
		sendPacket(t, c, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketKeepalive, SessionID: sid}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}})
		_ = readPacket(t, c)
		return c, sid
	}
	owner, sid := connect("N0ONE")
	defer owner.Close()
	listener, listenerID := connect("N0TWO")
	defer listener.Close()
	source, _ := wire.Callsign("N0ONE")
	request := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRequest, SessionID: sid, StreamID: 7}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 2), {Type: wire.TLVSourceCallsign, Value: source}}}
	sendPacket(t, owner, serverConn.LocalAddr(), request)
	grant := readPacket(t, owner)
	if grant.Header.Type != wire.PacketStreamGrant {
		t.Fatalf("got %v", grant.Header.Type)
	}
	sendPacket(t, owner, serverConn.LocalAddr(), request)
	duplicateGrant := readPacket(t, owner)
	a, _ := wire.Encode(grant)
	b, _ := wire.Encode(duplicateGrant)
	if !bytes.Equal(a, b) {
		t.Fatal("duplicate control request did not replay grant")
	}
	if got := grant; got.Header.Type != wire.PacketStreamGrant {
		t.Fatalf("got %v", got.Header.Type)
	}
	start := readPacket(t, listener)
	if start.Header.Type != wire.PacketStreamStart {
		t.Fatalf("got %v", start.Header.Type)
	}
	tx, _ := wire.Find(start, wire.TLVTransactionID)
	retry := readPacket(t, listener)
	retryTx, _ := wire.Find(retry, wire.TLVTransactionID)
	if retry.Header.Flags != wire.FlagRetry || !bytes.Equal(tx, retryTx) {
		t.Fatalf("bad notification retry: %#v", retry)
	}
	sendPacket(t, listener, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamStart, Flags: wire.FlagResponse, SessionID: listenerID, StreamID: start.Header.StreamID}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}})
	large := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: sid, StreamID: 7}, Payload: make([]byte, 1168)}
	encoded, err := wire.Encode(large)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = owner.WriteTo(append(encoded, 1), serverConn.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = listener.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	discard := make([]byte, 1201)
	if _, _, err = listener.ReadFrom(discard); err == nil {
		t.Fatal("oversize datagram was forwarded")
	}
	payload := []byte{0xf8, 0xff, 0xfe}
	sendPacket(t, owner, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: sid, StreamID: 7, Timestamp: 48000}, Payload: payload})
	got := readPacket(t, listener)
	if !bytes.Equal(got.Payload, payload) || got.Header.Timestamp != 48000 {
		t.Fatalf("bad fanout %#v", got)
	}
	w := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(w, httptest.NewRequest("GET", monitor.RouteMetrics, nil))
	if !strings.Contains(w.Body.String(), "opusref_packets_total") || !strings.Contains(w.Body.String(), "opusref_fanout_frames_total") {
		t.Fatalf("missing runtime metrics:\n%s", w.Body.String())
	}
}

func TestReflectorReplaysDuplicateAndRejectsConflictingHello(t *testing.T) {
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r, _ := NewReflector(serverConn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	defer r.Close()
	c, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer c.Close()
	tx := wire.Uint64TLV(wire.TLVTransactionID, 9)
	nonce := bytes.Repeat([]byte{3}, 32)
	one, _ := wire.Callsign("N0ONE")
	hello := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}, Extensions: []wire.TLV{tx, {Type: wire.TLVNodeCallsign, Value: one}, {Type: wire.TLVClientNonce, Value: nonce}}}
	sendPacket(t, c, serverConn.LocalAddr(), hello)
	first := readPacket(t, c)
	sendPacket(t, c, serverConn.LocalAddr(), hello)
	second := readPacket(t, c)
	a, _ := wire.Encode(first)
	b, _ := wire.Encode(second)
	if !bytes.Equal(a, b) {
		t.Fatal("duplicate did not replay retained result")
	}
	two, _ := wire.Callsign("N0TWO")
	hello.Extensions[1].Value = two
	sendPacket(t, c, serverConn.LocalAddr(), hello)
	if got := readPacket(t, c); got.Header.Type != wire.PacketError {
		t.Fatalf("got %v", got.Header.Type)
	}
}
func TestDrainRejectsMalformedAndMismatchedAcknowledgements(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, _ := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test"})
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	p := &peer{id: 9, address: addr, ready: true}
	r.peers[9] = p
	tx := wire.Uint64TLV(wire.TLVTransactionID, 7)
	notice := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRevoke, SessionID: 1, StreamID: 2, Sequence: 3, Timestamp: 4}, Extensions: []wire.TLV{tx, wire.Uint16TLV(wire.TLVEndReason, 0)}}
	key := notificationKey{listener: 9, typ: wire.PacketStreamRevoke, tx: 7}
	r.pending[key] = &notification{packet: notice}
	malformed, _ := wire.Encode(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRevoke, Flags: wire.FlagResponse, SessionID: 9, StreamID: 2}})
	r.handleDrain(addr, malformed)
	wrong, _ := wire.Encode(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRevoke, Flags: wire.FlagResponse, SessionID: 9, StreamID: 2, Sequence: 99, Timestamp: 4}, Extensions: []wire.TLV{tx}})
	r.handleDrain(addr, wrong)
	if _, ok := r.pending[key]; !ok {
		t.Fatal("mismatched ACK cleared notification")
	}
	valid, _ := wire.Encode(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRevoke, Flags: wire.FlagResponse, SessionID: 9, StreamID: 2, Sequence: 3, Timestamp: 4}, Extensions: []wire.TLV{tx}})
	r.handleDrain(addr, valid)
	if _, ok := r.pending[key]; ok {
		t.Fatal("valid ACK did not clear notification")
	}
}
func TestTransactionIDsUseRandomReader(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, _ := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", Random: bytes.NewReader(append(transactionID(41), transactionID(99)...))})
	if a, b := r.transactionID(), r.transactionID(); a != 41 || b != 99 {
		t.Fatalf("got %d %d", a, b)
	}
}
func TestOwnerControlOverloadNotifiesListeners(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, _ := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test"})
	ownerAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	listenerAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2}
	r.engine.AddSession(1, ownerAddr.String(), "N0ONE", true)
	r.engine.AddSession(2, listenerAddr.String(), "N0TWO", true)
	r.engine.RequestFloor(1, 7, "N0ONE")
	r.peers[1] = &peer{id: 1, address: ownerAddr, ready: true}
	r.peers[2] = &peer{id: 2, address: listenerAddr, ready: true, notified: true}
	r.controlOverload(r.peers[1])
	found := false
	for key := range r.pending {
		if key.listener == 2 && key.typ == wire.PacketStreamRevoke {
			found = true
		}
	}
	if !found {
		t.Fatal("owner overload omitted revoke")
	}
}

func sendPacket(t *testing.T, c net.PacketConn, addr net.Addr, p wire.Packet) {
	t.Helper()
	data, err := wire.Encode(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.WriteTo(data, addr); err != nil {
		t.Fatal(err)
	}
}
func readPacket(t *testing.T, c net.PacketConn) wire.Packet {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 1200)
	n, _, err := c.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	p, err := wire.Decode(buf[:n])
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func transactionID(value uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, value)
	return b
}
