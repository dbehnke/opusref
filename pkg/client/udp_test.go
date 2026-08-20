package client_test

import (
	"bytes"
	"context"
	"github.com/dbehnke/opusref/pkg/client"
	"github.com/dbehnke/opusref/pkg/server"
	"net"
	"testing"
	"time"
)

func TestUDPClientsExchangeOpaqueFrame(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	reflector, err := server.NewReflector(conn, server.ReflectorOptions{ID: "OPUSREF", DisplayName: "Test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go reflector.Run(ctx)
	defer reflector.Close()
	owner, err := client.NewUDP(client.Options{ServerAddress: conn.LocalAddr().String(), NodeCallsign: "N0ONE"})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	listener, err := client.NewUDP(client.Options{ServerAddress: conn.LocalAddr().String(), NodeCallsign: "N0TWO"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err = owner.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	if err = listener.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	drainStatus(t, owner)
	drainStatus(t, listener)
	if err = owner.RequestStream(ctx, "N0ONE"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	payload := []byte{0xf8, 0xff, 0xfe}
	if err = owner.SendAudio(ctx, 48000, payload); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-listener.Events():
		if event.Kind != client.EventAudio || event.Timestamp != 48000 || !bytes.Equal(event.Payload, payload) {
			t.Fatalf("bad event %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out")
	}
}
func drainStatus(t *testing.T, c client.Client) {
	t.Helper()
	select {
	case event := <-c.Events():
		if event.Kind != client.EventStatus {
			t.Fatalf("got %#v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("status timeout")
	}
}
