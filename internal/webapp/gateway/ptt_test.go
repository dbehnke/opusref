package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dbehnke/opusref/pkg/client"
)

type fakeTX struct {
	mu               sync.Mutex
	requested, ended int
	sent             [][]byte
	err              error
}

type confirmedTX struct{ fakeTX }

func (*confirmedTX) ConfirmedGrant() (uint64, uint32, bool) { return 12, 34, true }

func (f *fakeTX) RequestStream(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requested++
	return f.err
}
func (f *fakeTX) SendAudio(_ context.Context, _ uint32, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, append([]byte(nil), p...))
	return f.err
}
func (f *fakeTX) EndStream(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended++
	return nil
}
func TestPTTIsSingleOwnerAndPreservesPayload(t *testing.T) {
	tx := &fakeTX{}
	m := NewPTTManager(tx)
	grant, err := m.Start(context.Background(), "session-a", "N0CALL")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Start(context.Background(), "session-b", "N1CALL"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second owner: %v", err)
	}
	payload := []byte{0xf8, 0xff}
	if err = m.Send(context.Background(), "session-a", grant.ChannelID, 0, 0, payload); err != nil {
		t.Fatal(err)
	}
	payload[0] = 0
	if tx.sent[0][0] != 0xf8 {
		t.Fatal("payload ownership escaped")
	}
	if err = m.Stop(context.Background(), "session-a", grant.ChannelID); err != nil {
		t.Fatal(err)
	}
}
func TestPTTRejectsSequenceOrTimestampGap(t *testing.T) {
	m := NewPTTManager(&fakeTX{})
	g, _ := m.Start(context.Background(), "s", "N0CALL")
	if err := m.Send(context.Background(), "s", g.ChannelID, 1, 0, []byte{1}); !errors.Is(err, ErrSequence) {
		t.Fatalf("got %v", err)
	}
}
func TestPTTReflectorEndClearsOwnerAndNotifies(t *testing.T) {
	m := NewPTTManager(&fakeTX{})
	grant, err := m.Start(context.Background(), "session-a", "N0CALL")
	if err != nil {
		t.Fatal(err)
	}
	ends, cancel := m.SubscribeEnds()
	defer cancel()
	m.Observe(client.Event{Kind: client.EventStreamEnd})
	select {
	case ended := <-ends:
		if ended.Session != "session-a" || ended.ChannelID != grant.ChannelID {
			t.Fatalf("end=%+v", ended)
		}
	case <-time.After(time.Second):
		t.Fatal("missing PTT end")
	}
	if _, err = m.Start(context.Background(), "session-b", "N0CALL"); err != nil {
		t.Fatalf("floor was not cleared: %v", err)
	}
}

func TestPTTBindsConfirmedGrantBeforeAsynchronousEvent(t *testing.T) {
	attribution := NewGrantAttribution()
	m := NewPTTManager(&confirmedTX{})
	m.SetAttribution(attribution)
	if _, err := m.StartForUser(context.Background(), "session", "user-1", "N0CALL"); err != nil {
		t.Fatal(err)
	}
	if got := attribution.User(ReflectorStream{SessionID: 12, StreamID: 34}); got != "user-1" {
		t.Fatalf("attribution=%q", got)
	}
}
