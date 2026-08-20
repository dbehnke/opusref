package transport

import (
	"context"
	"net"
	"testing"
	"time"
)

type addr string

func (a addr) Network() string { return "test" }
func (a addr) String() string  { return string(a) }
func TestBoundedQueuesCountDropsAndCopy(t *testing.T) {
	u := NewUDP(nil, 1, 1, 1)
	data := []byte{1}
	if !u.EnqueueMedia(MediaBatch{Data: data, Recipients: []net.Addr{addr("a"), addr("b")}}) {
		t.Fatal("first dropped")
	}
	data[0] = 2
	if u.EnqueueMedia(MediaBatch{Data: []byte{3}, Recipients: []net.Addr{addr("a"), addr("b"), addr("c")}}) {
		t.Fatal("second accepted")
	}
	if u.MediaDrops.Load() != 1 || u.MediaDropRecipients.Load() != 3 {
		t.Fatal("bad counters")
	}
	if (<-u.Media).Data[0] != 1 {
		t.Fatal("payload was not copied")
	}
	if !u.EnqueueControl(Datagram{Address: addr("a"), Data: []byte{1}}) || u.EnqueueControl(Datagram{Address: addr("a"), Data: []byte{2}}) {
		t.Fatal("control capacity")
	}
	if u.ControlFailures.Load() != 1 {
		t.Fatal("control counter")
	}
}
func TestLiveUDPReadDropsWhenInboundQueueIsFull(t *testing.T) {
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	u := NewUDP(conn, 1, 1, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = u.Read(ctx, 1201) }()
	client, err := net.Dial("udp", conn.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	for index := 0; index < 100; index++ {
		if _, err = client.Write([]byte{byte(index)}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for u.InboundDrops.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(u.Inbound) != 1 || u.InboundDrops.Load() == 0 {
		t.Fatalf("queue=%d drops=%d", len(u.Inbound), u.InboundDrops.Load())
	}
}
