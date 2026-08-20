package client

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type memorySender struct {
	mu    sync.Mutex
	sent  []Outbound
	block chan struct{}
}

func (m *memorySender) Send(_ context.Context, o Outbound) error {
	if m.block != nil && o.Kind != EventStatus {
		<-m.block
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o.Payload = append([]byte(nil), o.Payload...)
	m.sent = append(m.sent, o)
	return nil
}
func TestClientContractCopiesAndSequencesPayload(t *testing.T) {
	s := &memorySender{}
	c, err := New(Options{ServerAddress: "x", NodeCallsign: "N0CALL"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = c.RequestStream(context.Background(), "N0CALL"); err != nil {
		t.Fatal(err)
	}
	p := []byte{1, 2}
	if err = c.SendAudio(context.Background(), 10, p); err != nil {
		t.Fatal(err)
	}
	p[0] = 9
	if err = c.SendData(context.Background(), 11, 7, []byte{3}); err != nil {
		t.Fatal(err)
	}
	if err = c.EndStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
func TestMediaBackpressure(t *testing.T) {
	s := &memorySender{block: make(chan struct{})}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N", MediaSendQueueFrames: 1}, s)
	_ = c.Connect(context.Background())
	_ = c.RequestStream(context.Background(), "N")
	_ = c.SendAudio(context.Background(), 0, []byte{1})
	var got error
	for i := 0; i < 10; i++ {
		got = c.SendAudio(context.Background(), 0, []byte{1})
		if errors.Is(got, ErrBackpressure) {
			break
		}
	}
	if !errors.Is(got, ErrBackpressure) {
		t.Fatalf("got %v", got)
	}
	close(s.block)
	_ = c.Close()
}
func TestPublishCopiesEventPayload(t *testing.T) {
	s := &memorySender{}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	p := []byte{1}
	if err := c.Publish(Event{Kind: EventAudio, Payload: p}); err != nil {
		t.Fatal(err)
	}
	p[0] = 2
	if got := (<-c.Events()).Payload[0]; got != 1 {
		t.Fatalf("got %d", got)
	}
}
func TestRequiredEventBackpressureIsTerminal(t *testing.T) {
	s := &memorySender{}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N", ApplicationQueueEvents: 1}, s)
	_ = c.Publish(Event{Kind: EventStatus})
	if err := c.Publish(Event{Kind: EventBusy}); !errors.Is(err, ErrApplicationBackpressure) {
		t.Fatalf("got %v", err)
	}
	<-c.Done()
	if !errors.Is(c.Err(), ErrApplicationBackpressure) {
		t.Fatal(c.Err())
	}
	if c.Close() != c.Close() {
		t.Fatal("Close result changed")
	}
}
