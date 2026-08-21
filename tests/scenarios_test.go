package tests

import (
	"bytes"
	"context"
	"errors"
	"github.com/dbehnke/opusref/pkg/client"
	"github.com/dbehnke/opusref/pkg/server"
	"net"
	"testing"
	"time"
)

type rig struct {
	conn      net.PacketConn
	reflector *server.Reflector
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan error
}

func newRig(t *testing.T, limits server.Limits) *rig {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r, err := server.NewReflector(conn, server.ReflectorOptions{ID: "OPUSREF", DisplayName: "Test", Limits: limits, ShutdownGrace: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	return &rig{conn: conn, reflector: r, ctx: ctx, cancel: cancel, done: done}
}
func (r *rig) close() {
	r.cancel()
	select {
	case <-r.done:
	case <-time.After(time.Second):
	}
	_ = r.reflector.Close()
}
func (r *rig) client(t *testing.T, call string) client.Client {
	t.Helper()
	c, err := client.NewUDP(client.Options{ServerAddress: r.conn.LocalAddr().String(), NodeCallsign: call, ConnectTimeout: 2 * time.Second, OperationTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
	defer cancel()
	if err = c.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-c.Events():
		if event.Kind != client.EventStatus {
			t.Fatalf("status: %#v", event)
		}
	case <-ctx.Done():
		t.Fatal("status timeout")
	}
	return c
}
func waitKind(t *testing.T, c client.Client, kind client.EventKind) client.Event {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-c.Events():
			if event.Kind == kind {
				return event
			}
		case <-timer.C:
			t.Fatalf("event %d timeout", kind)
		}
	}
}
func TestBlackBoxContentionLateJoinAndOpaqueData(t *testing.T) {
	r := newRig(t, server.Limits{})
	defer r.close()
	owner := r.client(t, "N0ONE")
	defer owner.Close()
	contender := r.client(t, "N0TWO")
	defer contender.Close()
	ctx := context.Background()
	if err := owner.RequestStream(ctx, "N0ONE"); err != nil {
		t.Fatal(err)
	}
	_ = waitKind(t, contender, client.EventStreamStart)
	if err := contender.RequestStream(ctx, "N0TWO"); !errors.Is(err, client.ErrBusy) {
		t.Fatalf("got %v", err)
	}
	late := r.client(t, "N0LATE")
	defer late.Close()
	start := waitKind(t, late, client.EventStreamStart)
	if start.SourceCallsign != "N0ONE" {
		t.Fatalf("late metadata: %#v", start)
	}
	payload := []byte{9, 8, 7, 6}
	if err := owner.SendData(ctx, 1234, 0x8001, payload); err != nil {
		t.Fatal(err)
	}
	event := waitKind(t, late, client.EventData)
	if event.DataType != 0x8001 || event.Timestamp != 1234 || !bytes.Equal(event.Payload, payload) {
		t.Fatalf("data changed: %#v", event)
	}
}
func TestBlackBoxGrantAndTransmitTimeoutRelease(t *testing.T) {
	limits := server.Limits{GrantTimeout: 50 * time.Millisecond, MediaTimeout: 2 * time.Second, TransmitTimeLimit: time.Second, SessionTimeout: 5 * time.Second}
	r := newRig(t, limits)
	defer r.close()
	one := r.client(t, "N0ONE")
	defer one.Close()
	two := r.client(t, "N0TWO")
	defer two.Close()
	if err := one.RequestStream(r.ctx, "N0ONE"); err != nil {
		t.Fatal(err)
	}
	_ = waitKind(t, two, client.EventStreamStart)
	_ = waitKind(t, two, client.EventStreamEnd)
	if err := two.RequestStream(r.ctx, "N0TWO"); err != nil {
		t.Fatalf("grant timeout did not release: %v", err)
	}
	if err := two.SendAudio(r.ctx, 0, []byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = waitKind(t, one, client.EventStreamStart)
	_ = waitKind(t, one, client.EventAudio)
	_ = waitKind(t, one, client.EventStreamEnd)
	if err := one.RequestStream(r.ctx, "N0ONE"); err != nil {
		t.Fatalf("TOT did not release: %v", err)
	}
}
func TestDataOutsideActiveStreamIsRejected(t *testing.T) {
	sender := &testSender{}
	c, err := client.New(client.Options{ServerAddress: "memory", NodeCallsign: "N0CALL"}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = c.SendData(context.Background(), 0, 1, []byte{1}); !errors.Is(err, client.ErrStreamInactive) {
		t.Fatalf("got %v", err)
	}
	_ = c.Close()
}

type testSender struct{}

func (*testSender) Send(context.Context, client.Outbound) error { return nil }
func TestSequenceWrapAndLossAccounting(t *testing.T) {
	e := server.NewEngine(server.Limits{}, time.Now)
	e.AddSession(1, "a", "N0ONE", true)
	e.RequestFloor(1, 1, "N0ONE")
	for _, seq := range []uint32{0, 2, 1, 3} {
		_, _ = e.Media(1, "a", 1, seq, 0, []byte{1})
	}
	if e.Snapshot().SequenceGaps != 1 {
		t.Fatalf("gaps: %d", e.Snapshot().SequenceGaps)
	}
}
func TestBlackBoxShutdownSendsRevoke(t *testing.T) {
	r := newRig(t, server.Limits{})
	owner := r.client(t, "N0ONE")
	defer owner.Close()
	listener := r.client(t, "N0TWO")
	defer listener.Close()
	if err := owner.RequestStream(r.ctx, "N0ONE"); err != nil {
		t.Fatal(err)
	}
	_ = waitKind(t, listener, client.EventStreamStart)
	r.cancel()
	_ = waitKind(t, listener, client.EventStreamEnd)
	select {
	case <-listener.Done():
	case <-time.After(time.Second):
		t.Fatal("listener did not receive disconnect")
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reflector did not complete drain")
	}
	_ = r.reflector.Close()
}
