package client

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type memorySender struct {
	mu        sync.Mutex
	sent      []Outbound
	block     chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
	closed    bool
}
type busySender struct{ memorySender }

func (*busySender) RequestFloor(context.Context, Outbound) error { return ErrBusy }

type blockingConnectSender struct{ entered, release chan struct{} }

func (s *blockingConnectSender) Send(context.Context, Outbound) error {
	close(s.entered)
	<-s.release
	return nil
}

func (m *memorySender) Send(_ context.Context, o Outbound) error {
	if m.block != nil && (o.Kind == EventAudio || o.Kind == EventData) {
		if m.entered != nil {
			m.enterOnce.Do(func() { close(m.entered) })
		}
		<-m.block
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o.Payload = append([]byte(nil), o.Payload...)
	m.sent = append(m.sent, o)
	return nil
}
func (m *memorySender) Close() error { m.mu.Lock(); m.closed = true; m.mu.Unlock(); return nil }

type disconnectSender struct {
	memorySender
	disconnected chan struct{}
}

func (s *disconnectSender) Disconnect(context.Context) error {
	close(s.disconnected)
	return nil
}

func TestOptionsRejectInvalidCapacitiesBeforeAllocation(t *testing.T) {
	for _, options := range []Options{
		{ServerAddress: "x", NodeCallsign: "N", InboundQueuePackets: -1},
		{ServerAddress: "x", NodeCallsign: "N", ApplicationQueueEvents: -1},
		{ServerAddress: "x", NodeCallsign: "N", MediaSendQueueFrames: -1},
		{ServerAddress: "x", NodeCallsign: "N", ControlSendQueuePackets: -1},
	} {
		if _, err := New(options, &memorySender{}); err == nil {
			t.Fatalf("accepted options %#v", options)
		}
	}
	if _, err := NewUDP(Options{ServerAddress: "invalid.invalid:1", NodeCallsign: "N", InboundQueuePackets: -1}); err == nil || !strings.Contains(err.Error(), "queue") {
		t.Fatalf("NewUDP did not validate before resolving: %v", err)
	}
}

func TestCloseUsesTransactionalDisconnectBeforeTransportClose(t *testing.T) {
	s := &disconnectSender{disconnected: make(chan struct{})}
	c, err := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = c.CloseContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.disconnected:
	default:
		t.Fatal("disconnect was not sent")
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		t.Fatal("transport remained open")
	}
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
	s := &memorySender{block: make(chan struct{}), entered: make(chan struct{})}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N", MediaSendQueueFrames: 1}, s)
	_ = c.Connect(context.Background())
	_ = c.RequestStream(context.Background(), "N")
	_ = c.SendAudio(context.Background(), 10, []byte{1})
	<-s.entered
	if err := c.SendAudio(context.Background(), 20, []byte{1}); err != nil {
		t.Fatal(err)
	}
	got := c.SendAudio(context.Background(), 30, []byte{1})
	if !errors.Is(got, ErrBackpressure) {
		t.Fatalf("got %v", got)
	}
	close(s.block)
	if err := c.EndStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	end := s.sent[len(s.sent)-1]
	s.mu.Unlock()
	if end.Sequence != 2 || end.Timestamp != 20 {
		t.Fatalf("backpressure consumed metadata: %#v", end)
	}
	_ = c.Close()
}
func TestEndWaitsForAcceptedMedia(t *testing.T) {
	s := &memorySender{block: make(chan struct{}), entered: make(chan struct{})}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	_ = c.Connect(context.Background())
	_ = c.RequestStream(context.Background(), "N")
	_ = c.SendAudio(context.Background(), 960, []byte{1})
	<-s.entered
	done := make(chan error, 1)
	go func() { done <- c.EndStream(context.Background()) }()
	select {
	case err := <-done:
		t.Fatalf("end passed blocked media: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(s.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent[len(s.sent)-1].Kind != EventStreamEnd {
		t.Fatalf("order: %#v", s.sent)
	}
}

type blockingRequester struct {
	memorySender
	entered chan struct{}
	release chan struct{}
}

func (b *blockingRequester) RequestFloor(context.Context, Outbound) error {
	close(b.entered)
	<-b.release
	return nil
}
func TestConcurrentFloorRequestIsRejected(t *testing.T) {
	s := &blockingRequester{entered: make(chan struct{}), release: make(chan struct{})}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	_ = c.Connect(context.Background())
	first := make(chan error, 1)
	go func() { first <- c.RequestStream(context.Background(), "N") }()
	<-s.entered
	if err := c.RequestStream(context.Background(), "N"); err == nil {
		t.Fatal("concurrent request accepted")
	}
	close(s.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentConnectIsRejected(t *testing.T) {
	s := &blockingConnectSender{entered: make(chan struct{}), release: make(chan struct{})}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	first := make(chan error, 1)
	go func() { first <- c.Connect(context.Background()) }()
	<-s.entered
	if err := c.Connect(context.Background()); !errors.Is(err, ErrConnectInProgress) {
		t.Fatalf("got %v", err)
	}
	close(s.release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
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
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		t.Fatal("terminal failure did not close transport")
	}
	if c.Close() != c.Close() {
		t.Fatal("Close result changed")
	}
}
func TestSecondFloorRequestDoesNotCorruptActiveStream(t *testing.T) {
	s := &memorySender{}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	_ = c.Connect(context.Background())
	if err := c.RequestStream(context.Background(), "N"); err != nil {
		t.Fatal(err)
	}
	if err := c.RequestStream(context.Background(), "N"); err == nil {
		t.Fatal("second request accepted")
	}
	if err := c.SendAudio(context.Background(), 960, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := c.EndStream(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	end := s.sent[len(s.sent)-1]
	if end.StreamID != 1 || end.Sequence != 1 || end.Timestamp != 960 {
		t.Fatalf("bad end metadata: %#v", end)
	}
}
func TestClientDoesNotActivateStreamBeforeGrant(t *testing.T) {
	s := &busySender{}
	c, _ := New(Options{ServerAddress: "x", NodeCallsign: "N"}, s)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.RequestStream(context.Background(), "N"); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	if err := c.SendAudio(context.Background(), 0, []byte{1}); !errors.Is(err, ErrStreamInactive) {
		t.Fatalf("media accepted: %v", err)
	}
	_ = c.Close()
}
