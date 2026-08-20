package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"github.com/dbehnke/opusref/pkg/wire"
	"net"
	"testing"
	"time"
)

func TestReflectorOpenHandshakeAndMediaFanout(t *testing.T) {
	serverConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewReflector(serverConn, ReflectorOptions{ID: "OPUSREF", DisplayName: "Test"})
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
	listener, _ := connect("N0TWO")
	defer listener.Close()
	source, _ := wire.Callsign("N0ONE")
	sendPacket(t, owner, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketStreamRequest, SessionID: sid, StreamID: 7}, Extensions: []wire.TLV{wire.Uint64TLV(wire.TLVTransactionID, 2), {Type: wire.TLVSourceCallsign, Value: source}}})
	if got := readPacket(t, owner); got.Header.Type != wire.PacketStreamGrant {
		t.Fatalf("got %v", got.Header.Type)
	}
	payload := []byte{0xf8, 0xff, 0xfe}
	sendPacket(t, owner, serverConn.LocalAddr(), wire.Packet{Header: wire.Header{Version: 1, Type: wire.PacketAudio, SessionID: sid, StreamID: 7, Timestamp: 48000}, Payload: payload})
	got := readPacket(t, listener)
	if !bytes.Equal(got.Payload, payload) || got.Header.Timestamp != 48000 {
		t.Fatalf("bad fanout %#v", got)
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
