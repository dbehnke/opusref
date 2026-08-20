package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"github.com/dbehnke/opusref/internal/transport"
	"github.com/dbehnke/opusref/pkg/monitor"
	"github.com/dbehnke/opusref/pkg/wire"
	"net"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buffer := make([]byte, 1200)
	if _, _, err := c.ReadFrom(buffer); err == nil {
		t.Fatal("unauthenticated conflict received amplified response")
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
func TestProtectedHMACAdmissionAndRejection(t *testing.T) {
	serverConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer serverConn.Close()
	r, _ := NewReflector(serverConn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", SharedKey: []byte("0123456789abcdef")})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	defer r.Close()
	admit := func(t *testing.T, valid bool) {
		c, _ := net.ListenPacket("udp", "127.0.0.1:0")
		defer c.Close()
		tx := wire.Uint64TLV(wire.TLVTransactionID, 42)
		nonce := bytes.Repeat([]byte{3}, 32)
		call, _ := wire.Callsign("N0AUTH")
		sendPacket(t, c, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}, Extensions: []wire.TLV{tx, {Type: wire.TLVNodeCallsign, Value: call}, {Type: wire.TLVClientNonce, Value: nonce}}})
		challenge := readPacket(t, c)
		serverNonce, _ := wire.Find(challenge, wire.TLVServerNonce)
		id, _ := wire.Find(challenge, wire.TLVReflectorID)
		mac := hmac.New(sha256.New, []byte("0123456789abcdef"))
		mac.Write([]byte("OPRF-AUTH-V1"))
		mac.Write(call)
		mac.Write(nonce)
		mac.Write(serverNonce)
		mac.Write(id)
		tag := mac.Sum(nil)
		if !valid {
			tag[0] ^= 1
		}
		sendPacket(t, c, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAuthenticate}, Extensions: []wire.TLV{tx, {Type: wire.TLVClientNonce, Value: nonce}, {Type: wire.TLVServerNonce, Value: serverNonce}, {Type: wire.TLVAuthenticationTag, Value: tag}}})
		_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		buffer := make([]byte, 1200)
		n, _, err := c.ReadFrom(buffer)
		if valid {
			if err != nil {
				t.Fatal(err)
			}
			p, _ := wire.Decode(buffer[:n])
			if p.Header.Type != wire.PacketWelcome {
				t.Fatalf("got %v", p.Header.Type)
			}
		} else if err == nil {
			t.Fatal("invalid HMAC admitted")
		}
	}
	t.Run("valid", func(t *testing.T) { admit(t, true) })
	t.Run("invalid", func(t *testing.T) { admit(t, false) })
}
func TestUnauthenticatedMinimalPacketIsSilent(t *testing.T) {
	serverConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer serverConn.Close()
	r, _ := NewReflector(serverConn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	defer r.Close()
	c, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer c.Close()
	packet, _ := wire.Encode(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}})
	if _, err := c.WriteTo(packet, serverConn.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, _, err := c.ReadFrom(make([]byte, 1200)); err == nil {
		t.Fatal("minimal unauthenticated packet received a response")
	}
}
func TestChallengeExpiryUsesInjectedClock(t *testing.T) {
	var unix atomic.Int64
	unix.Store(100)
	serverConn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer serverConn.Close()
	r, _ := NewReflector(serverConn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", Clock: func() time.Time { return time.Unix(unix.Load(), 0) }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.Run(ctx)
	defer r.Close()
	c, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer c.Close()
	tx := wire.Uint64TLV(wire.TLVTransactionID, 5)
	nonce := bytes.Repeat([]byte{4}, 32)
	call, _ := wire.Callsign("N0TIME")
	sendPacket(t, c, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}, Extensions: []wire.TLV{tx, {Type: wire.TLVNodeCallsign, Value: call}, {Type: wire.TLVClientNonce, Value: nonce}}})
	challenge := readPacket(t, c)
	sn, _ := wire.Find(challenge, wire.TLVServerNonce)
	unix.Add(11)
	sendPacket(t, c, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAuthenticate}, Extensions: []wire.TLV{tx, {Type: wire.TLVClientNonce, Value: nonce}, {Type: wire.TLVServerNonce, Value: sn}}})
	_ = c.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	if _, _, err := c.ReadFrom(make([]byte, 1200)); err == nil {
		t.Fatal("expired challenge admitted")
	}
}
func TestRejectedStatefulPacketDoesNotRefreshActivity(t *testing.T) {
	now := time.Unix(100, 0)
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, _ := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", Clock: func() time.Time { return now }, Limits: Limits{SessionTimeout: 5 * time.Second}})
	ownerAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	badAddr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2}
	r.engine.AddSession(1, ownerAddr.String(), "N0ONE", true)
	r.engine.AddSession(2, badAddr.String(), "N0BAD", true)
	r.engine.RequestFloor(1, 7, "N0ONE")
	r.peers[2] = &peer{id: 2, address: badAddr, node: "N0BAD", ready: true, connected: now, last: now}
	p := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: 2, StreamID: 7}, Payload: []byte{1}}
	data, _ := wire.Encode(p)
	now = now.Add(4 * time.Second)
	r.handle(badAddr, data)
	ack := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamStart, Flags: wire.FlagResponse, SessionID: 2, StreamID: 7}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 9)}}
	ackData, _ := wire.Encode(ack)
	r.handle(badAddr, ackData)
	now = now.Add(2 * time.Second)
	r.tick(false)
	if r.peers[2] != nil {
		t.Fatal("invalid media refreshed session")
	}
}
func TestInvalidStreamEndReturnsBoundedError(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, _ := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test"})
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	r.engine.AddSession(1, addr.String(), "N", true)
	r.engine.RequestFloor(1, 7, "N")
	p := &peer{id: 1, address: addr, ready: true}
	request := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamEnd, SessionID: 1, StreamID: 8}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 1)}}
	response := r.control(p, request)
	encodedRequest, _ := wire.Encode(request)
	encodedResponse, _ := wire.Encode(response)
	if response.Header.Type != wire.PacketError || len(encodedResponse) > len(encodedRequest) || !r.engine.Snapshot().Floor.Active {
		t.Fatalf("response=%#v sizes=%d/%d", response, len(encodedResponse), len(encodedRequest))
	}
}
func TestDuplicateReplayControlOverloadClosesSession(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, _ := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", OutboundControlQueuePackets: 1})
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	r.engine.AddSession(1, addr.String(), "N", false)
	r.peers[1] = &peer{id: 1, address: addr}
	request := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketKeepalive, SessionID: 1}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 1)}}
	if !r.transact(addr, request, func() wire.Packet { return r.control(r.peers[1], request) }) {
		t.Fatal("first request failed")
	}
	if r.transact(addr, request, func() wire.Packet { return wire.Packet{} }) {
		t.Fatal("overloaded duplicate accepted")
	}
	if r.peers[1] != nil || r.transport.ControlFailures.Load() != 1 {
		t.Fatal("overload did not close admitted session")
	}
}
func TestInjectedClockControlsChallengeRetentionAndNotificationRetry(t *testing.T) {
	now := time.Unix(100, 0)
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, _ := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", Clock: func() time.Time { return now }, Limits: Limits{SessionTimeout: time.Hour}})
	r.challenges["old"] = challenge{at: now}
	r.transactions["old"] = completed{at: now}
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1}
	listener := &peer{id: 1, address: addr, last: now}
	r.peers[1] = listener
	notice := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketDisconnect, SessionID: 1}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 9)}}
	r.startNotification(listener, notice)
	key := notificationKey{listener: 1, typ: wire.PacketDisconnect, tx: 9}
	now = now.Add(600 * time.Millisecond)
	r.tick(false)
	if r.pending[key].attempt != 2 {
		t.Fatalf("notification attempt=%d", r.pending[key].attempt)
	}
	now = now.Add(31 * time.Second)
	r.tick(false)
	if len(r.challenges) != 0 || len(r.transactions) != 0 {
		t.Fatal("retention ignored injected clock")
	}
}

func TestDisconnectResponseReplaysAfterSessionRemoval(t *testing.T) {
	now := time.Unix(100, 0)
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, err := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	r.engine.AddSession(9, addr.String(), "N0ONE", true)
	r.peers[9] = &peer{id: 9, address: addr, ready: true, connected: now, last: now}
	request := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketDisconnect, SessionID: 9}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 77)}}
	data, _ := wire.Encode(request)
	r.handle(addr, data)
	first := <-r.transport.Control
	if r.peers[9] != nil || r.engine.Snapshot().Sessions != 0 {
		t.Fatal("disconnect retained live session")
	}
	request.Header.Flags = wire.FlagRetry
	retry, _ := wire.Encode(request)
	r.handle(addr, retry)
	second := <-r.transport.Control
	if !bytes.Equal(first.Data, second.Data) || r.engine.Snapshot().Sessions != 0 {
		t.Fatal("retry did not replay response without restoring session")
	}
	wrong := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 4321}
	r.handle(wrong, retry)
	if len(r.transport.Control) != 0 {
		t.Fatal("response replayed to a different address")
	}
	now = now.Add(31 * time.Second)
	r.tick(false)
	r.handle(addr, retry)
	if len(r.transport.Control) != 0 {
		t.Fatal("expired disconnect response replayed")
	}
}

func TestDisconnectRetainsResponseWhenInitialControlQueueIsFull(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, err := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", OutboundControlQueuePackets: 1})
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}
	r.engine.AddSession(9, addr.String(), "N0ONE", true)
	r.peers[9] = &peer{id: 9, address: addr, ready: true}
	r.transport.Control <- transport.Datagram{Address: addr, Data: []byte{0}}
	request := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketDisconnect, SessionID: 9}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 77)}}
	data, _ := wire.Encode(request)
	r.handle(addr, data)
	<-r.transport.Control
	request.Header.Flags = wire.FlagRetry
	retry, _ := wire.Encode(request)
	r.handle(addr, retry)
	if len(r.transport.Control) != 1 || r.engine.Snapshot().Sessions != 0 {
		t.Fatal("disconnect response was not retained across control overload")
	}
}

func TestShutdownDiscardsQueuedMediaBeforeControlDrain(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	r, err := NewReflector(conn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", ShutdownGrace: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if !r.transport.EnqueueMedia(transport.MediaBatch{Data: []byte{1}, Recipients: []net.Addr{conn.LocalAddr()}}) {
		t.Fatal("could not arrange queued media")
	}
	if err = r.drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.transport.Media) != 0 || r.transport.EnqueueMedia(transport.MediaBatch{Data: []byte{2}, Recipients: []net.Addr{conn.LocalAddr()}}) {
		t.Fatal("shutdown left media enabled")
	}
}

func TestReflectorRejectsInvalidCapacities(t *testing.T) {
	conn, _ := net.ListenPacket("udp", "127.0.0.1:0")
	defer conn.Close()
	for _, options := range []ReflectorOptions{
		{ID: "OPUSREF", DisplayName: "Test", InboundQueuePackets: -1},
		{ID: "OPUSREF", DisplayName: "Test", OutboundControlQueuePackets: -1},
		{ID: "OPUSREF", DisplayName: "Test", OutboundMediaQueueFrames: -1},
		{ID: "OPUSREF", DisplayName: "Test", MaxPendingNotificationsPerClient: -1},
		{ID: "OPUSREF", DisplayName: "Test", Limits: Limits{MaxClients: -1}},
	} {
		if _, err := NewReflector(conn, options); err == nil {
			t.Fatalf("accepted options %#v", options)
		}
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
