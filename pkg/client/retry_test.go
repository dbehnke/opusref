package client

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/dbehnke/opusref/pkg/wire"
	"net"
	"testing"
	"time"
)

func TestCorrelatedProtocolErrorCompletesRequestAndPublishesFields(t *testing.T) {
	serverConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer serverConn.Close()
	clientConn, _ := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	s := &udpSender{conn: clientConn, options: Options{OperationTimeout: time.Second}, session: 1, pending: map[requestKey]pendingRequest{}, inbound: make(chan []byte, 2), controlTokens: make(chan struct{}, 1)}
	owner, _ := New(Options{ServerAddress: "x", NodeCallsign: "N0CALL"}, s)
	s.owner = owner
	go s.readLoop()
	defer s.Close()
	go func() {
		buf := make([]byte, wire.MaxDatagramSize)
		n, addr, _ := serverConn.ReadFromUDP(buf)
		request, _ := wire.Decode(buf[:n])
		tx, _ := wire.Find(request, wire.TLVTransactionID)
		_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketError, Flags: wire.FlagResponse, SessionID: 1, StreamID: request.Header.StreamID}, Extensions: []wire.TLV{wire.Uint16TLV(wire.TLVErrorCode, uint16(wire.ErrorInvalidStream)), {Type: wire.TLVTransactionID, Value: tx}, {Type: wire.TLVErrorText, Value: []byte("floor rejected")}}})
	}()
	started := time.Now()
	err := s.RequestFloor(context.Background(), Outbound{StreamID: 7, SourceCallsign: "N0CALL"})
	var protocolErr *ProtocolError
	if !errors.As(err, &protocolErr) || protocolErr.Code != uint16(wire.ErrorInvalidStream) || protocolErr.Message != "floor rejected" {
		t.Fatalf("error=%#v", err)
	}
	if time.Since(started) >= 400*time.Millisecond {
		t.Fatal("correlated ERROR waited for retry")
	}
	select {
	case event := <-owner.Events():
		if event.Kind != EventProtocolError || event.ProtocolErrorCode != uint16(wire.ErrorInvalidStream) || event.Message != "floor rejected" || event.StreamID != 7 {
			t.Fatalf("event=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("protocol error event not published")
	}
}

func TestCorrelatedBusyCompletesRequestAndPublishesEvent(t *testing.T) {
	serverConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer serverConn.Close()
	clientConn, _ := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	s := &udpSender{conn: clientConn, options: Options{OperationTimeout: time.Second}, session: 1, pending: map[requestKey]pendingRequest{}, inbound: make(chan []byte, 2), controlTokens: make(chan struct{}, 1)}
	owner, _ := New(Options{ServerAddress: "x", NodeCallsign: "N0CALL"}, s)
	s.owner = owner
	go s.readLoop()
	defer s.Close()
	go func() {
		buf := make([]byte, wire.MaxDatagramSize)
		n, addr, _ := serverConn.ReadFromUDP(buf)
		request, _ := wire.Decode(buf[:n])
		tx, _ := wire.Find(request, wire.TLVTransactionID)
		_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamBusy, Flags: wire.FlagResponse, SessionID: 1, StreamID: request.Header.StreamID}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}})
	}()
	if err := s.RequestFloor(context.Background(), Outbound{StreamID: 7, SourceCallsign: "N0CALL"}); !errors.Is(err, ErrBusy) {
		t.Fatalf("RequestFloor error=%v", err)
	}
	select {
	case event := <-owner.Events():
		if event.Kind != EventBusy || event.SessionID != 1 || event.StreamID != 7 || event.Message == "" {
			t.Fatalf("busy event=%#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("busy event not published")
	}
}

func TestCloseRetriesTransactionalDisconnect(t *testing.T) {
	serverConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer serverConn.Close()
	clientConn, _ := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	s := &udpSender{conn: clientConn, options: Options{OperationTimeout: 2 * time.Second}, session: 9, pending: map[requestKey]pendingRequest{}, inbound: make(chan []byte, 2), controlTokens: make(chan struct{}, 1)}
	owner, _ := New(Options{ServerAddress: "x", NodeCallsign: "N0CALL", OperationTimeout: 2 * time.Second}, s)
	s.owner = owner
	owner.connected = true
	go s.readLoop()
	received := make(chan [2]wire.Packet, 1)
	go func() {
		var packets [2]wire.Packet
		buf := make([]byte, wire.MaxDatagramSize)
		var addr *net.UDPAddr
		for index := range packets {
			n, remote, _ := serverConn.ReadFromUDP(buf)
			addr = remote
			packets[index], _ = wire.Decode(buf[:n])
		}
		tx, _ := wire.Find(packets[1], wire.TLVTransactionID)
		_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketDisconnect, Flags: wire.FlagResponse, SessionID: 9}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}}})
		received <- packets
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := owner.closeContext(ctx); err != nil {
		t.Fatal(err)
	}
	packets := <-received
	one, _ := wire.Find(packets[0], wire.TLVTransactionID)
	two, _ := wire.Find(packets[1], wire.TLVTransactionID)
	if packets[0].Header.Type != wire.PacketDisconnect || packets[1].Header.Flags != wire.FlagRetry || string(one) != string(two) {
		t.Fatalf("disconnect attempts=%#v", packets)
	}
}

func TestFloorRequestRetriesAndIgnoresMismatchedTransaction(t *testing.T) {
	serverConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer serverConn.Close()
	clientConn, err := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	s := &udpSender{conn: clientConn, options: Options{OperationTimeout: 2 * time.Second}, session: 1, pending: map[requestKey]pendingRequest{}, inbound: make(chan []byte, 1), controlTokens: make(chan struct{}, 1)}
	owner, _ := New(Options{ServerAddress: "x", NodeCallsign: "N0CALL"}, s)
	s.owner = owner
	go s.readLoop()
	defer s.Close()
	times := make(chan time.Time, 2)
	go func() {
		buf := make([]byte, 1201)
		for attempt := 0; attempt < 2; attempt++ {
			n, addr, _ := serverConn.ReadFromUDP(buf)
			p, _ := wire.Decode(buf[:n])
			times <- time.Now()
			tx, _ := wire.Find(p, wire.TLVTransactionID)
			value := binary.BigEndian.Uint64(tx)
			if attempt == 0 {
				value++
				_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamGrant, Flags: wire.FlagResponse, SessionID: 1, StreamID: p.Header.StreamID}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, value), wire.Uint32TLV(wire.TLVTransmitTimeLimit, 180)}})
				continue
			}
			_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamGrant, Flags: wire.FlagResponse, SessionID: 1, StreamID: p.Header.StreamID}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, wire.Uint32TLV(wire.TLVTransmitTimeLimit, 180)}})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.RequestFloor(ctx, Outbound{StreamID: 7, SourceCallsign: "N0CALL"}); err != nil {
		t.Fatal(err)
	}
	first, second := <-times, <-times
	delta := second.Sub(first)
	if delta < 400*time.Millisecond || delta > 800*time.Millisecond {
		t.Fatalf("retry after %s", delta)
	}
}
func TestFloorRequestIgnoresMatchingTransactionForWrongStream(t *testing.T) {
	serverConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer serverConn.Close()
	clientConn, _ := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	s := &udpSender{conn: clientConn, options: Options{}, session: 1, pending: map[requestKey]pendingRequest{}, inbound: make(chan []byte, 2), controlTokens: make(chan struct{}, 1)}
	owner, _ := New(Options{ServerAddress: "x", NodeCallsign: "N0CALL"}, s)
	s.owner = owner
	go s.readLoop()
	defer s.Close()
	go func() {
		buf := make([]byte, 1200)
		n, addr, _ := serverConn.ReadFromUDP(buf)
		p, _ := wire.Decode(buf[:n])
		tx, _ := wire.Find(p, wire.TLVTransactionID)
		_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamGrant, Flags: wire.FlagResponse, SessionID: 1, StreamID: 8}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, wire.Uint32TLV(wire.TLVTransmitTimeLimit, 180)}})
		time.Sleep(20 * time.Millisecond)
		_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamGrant, Flags: wire.FlagResponse, SessionID: 1, StreamID: 7}, Extensions: []wire.TLV{{Type: wire.TLVTransactionID, Value: tx}, wire.Uint32TLV(wire.TLVTransmitTimeLimit, 180)}})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.RequestFloor(ctx, Outbound{StreamID: 7, SourceCallsign: "N0CALL"}); err != nil {
		t.Fatal(err)
	}
}
func TestInboundDatagramQueueIsBounded(t *testing.T) {
	s := &udpSender{inbound: make(chan []byte, 1)}
	s.enqueueInbound([]byte{1})
	s.enqueueInbound([]byte{2})
	if len(s.inbound) != 1 || s.inboundDrops.Load() != 1 {
		t.Fatalf("queue=%d drops=%d", len(s.inbound), s.inboundDrops.Load())
	}
	if got := <-s.inbound; len(got) != 1 || got[0] != 1 {
		t.Fatalf("got %v", got)
	}
}
func TestHandshakeIgnoresUnrelatedPacketUntilRetryDeadline(t *testing.T) {
	serverConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer serverConn.Close()
	clientConn, _ := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	defer clientConn.Close()
	s := &udpSender{conn: clientConn}
	tx := uint64(9)
	times := make(chan time.Time, 2)
	go func() {
		buf := make([]byte, 1200)
		for attempt := 0; attempt < 2; attempt++ {
			n, addr, _ := serverConn.ReadFromUDP(buf)
			_, _ = wire.Decode(buf[:n])
			times <- time.Now()
			value := tx
			if attempt == 0 {
				value++
			}
			id, _ := wire.ReflectorID("OPUSREF")
			_ = sendUDP(serverConn, addr, wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketChallenge, Flags: wire.FlagResponse}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, value), {Type: wire.TLVServerNonce, Value: make([]byte, 32)}, {Type: wire.TLVReflectorID, Value: id}, {Type: wire.TLVDisplayName, Value: []byte("test")}}})
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	p := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketHello}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, tx)}}
	if _, err := s.exchange(ctx, p, wire.PacketChallenge, tx); err != nil {
		t.Fatal(err)
	}
	first, second := <-times, <-times
	if elapsed := second.Sub(first); elapsed < 400*time.Millisecond {
		t.Fatalf("unrelated packet accelerated retry: %s", elapsed)
	}
}
func TestReceiveStateUsesOwnerAndStreamIdentity(t *testing.T) {
	serverConn, _ := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	defer serverConn.Close()
	clientConn, _ := net.DialUDP("udp", nil, serverConn.LocalAddr().(*net.UDPAddr))
	s := &udpSender{conn: clientConn, session: 99, pending: map[requestKey]pendingRequest{}, inbound: make(chan []byte, 8), controlTokens: make(chan struct{}, 1)}
	owner, _ := New(Options{ServerAddress: "x", NodeCallsign: "N0LISTEN"}, s)
	s.owner = owner
	go s.processLoop()
	defer owner.Close()
	call, _ := wire.Callsign("N0OWNER")
	source, _ := wire.Callsign("N0SRC")
	start := func(session uint64) wire.Packet {
		return wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamStart, SessionID: session, StreamID: 7}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, session), {Type: wire.TLVNodeCallsign, Value: call}, {Type: wire.TLVSourceCallsign, Value: source}, wire.Uint32TLV(wire.TLVTransmitTimeLimit, 180)}}
	}
	encode := func(p wire.Packet) []byte { data, _ := wire.Encode(p); return data }
	s.enqueueInbound(encode(start(11)))
	if event := <-owner.Events(); event.SessionID != 11 {
		t.Fatalf("first start: %#v", event)
	}
	s.enqueueInbound(encode(start(22)))
	if event := <-owner.Events(); event.SessionID != 22 {
		t.Fatalf("turnover start suppressed: %#v", event)
	}
	oldRevoke := wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRevoke, SessionID: 11, StreamID: 7}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 77), wire.Uint16TLV(wire.TLVEndReason, 0)}}
	s.enqueueInbound(encode(oldRevoke))
	s.enqueueInbound(encode(start(11)))
	s.enqueueInbound(encode(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: 11, StreamID: 7}, Payload: []byte{9}}))
	s.enqueueInbound(encode(wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: 22, StreamID: 7}, Payload: []byte{1}}))
	select {
	case event := <-owner.Events():
		if event.Kind != EventAudio || event.SessionID != 22 {
			t.Fatalf("stale revoke changed owner: %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("new owner media rejected")
	}
}
func TestRemoteExpiryUsesItsOwnLastMediaTime(t *testing.T) {
	s := &udpSender{remote: remoteStream{owner: 11, stream: 7, lastMedia: time.Unix(1, 0)}}
	owner, _ := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	s.owner = owner
	s.expireRemote(time.Unix(4, 0))
	select {
	case event := <-owner.Events():
		if event.SessionID != 11 || event.StreamID != 7 {
			t.Fatalf("expiry: %#v", event)
		}
	default:
		t.Fatal("remote did not expire")
	}
}
func sendUDP(conn *net.UDPConn, addr *net.UDPAddr, p wire.Packet) error {
	data, err := wire.Encode(p)
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDP(data, addr)
	return err
}
