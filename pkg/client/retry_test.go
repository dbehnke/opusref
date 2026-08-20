package client

import (
	"context"
	"encoding/binary"
	"github.com/dbehnke/opusref/pkg/wire"
	"net"
	"testing"
	"time"
)

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
func sendUDP(conn *net.UDPConn, addr *net.UDPAddr, p wire.Packet) error {
	data, err := wire.Encode(p)
	if err != nil {
		return err
	}
	_, err = conn.WriteToUDP(data, addr)
	return err
}
