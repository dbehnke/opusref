package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeTX struct {
	mu               sync.Mutex
	requested, ended int
	sent             [][]byte
	err              error
}

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
