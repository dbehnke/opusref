package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/dbehnke/opusref/pkg/client"
)

type supervisedFake struct {
	events chan client.Event
	done   chan struct{}
	once   sync.Once
}

func newSupervisedFake() *supervisedFake {
	return &supervisedFake{events: make(chan client.Event), done: make(chan struct{})}
}
func (f *supervisedFake) Connect(context.Context) error                          { return nil }
func (f *supervisedFake) RequestStream(context.Context, string) error            { return nil }
func (f *supervisedFake) SendAudio(context.Context, uint32, []byte) error        { return nil }
func (f *supervisedFake) SendData(context.Context, uint32, uint16, []byte) error { return nil }
func (f *supervisedFake) EndStream(context.Context) error                        { return nil }
func (f *supervisedFake) Events() <-chan client.Event                            { return f.events }
func (f *supervisedFake) Done() <-chan struct{}                                  { return f.done }
func (f *supervisedFake) Err() error                                             { return nil }
func (f *supervisedFake) Close() error {
	f.once.Do(func() { close(f.done) })
	return nil
}

func TestSupervisedClientReconnectsWithStableFacade(t *testing.T) {
	var mu sync.Mutex
	created := []*supervisedFake{}
	supervisor := NewSupervisedClient(func() (client.Client, error) {
		fake := newSupervisedFake()
		mu.Lock()
		created = append(created, fake)
		mu.Unlock()
		return fake, nil
	}, 16)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := supervisor.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	waitReady(t, supervisor, true)
	mu.Lock()
	first := created[0]
	mu.Unlock()
	first.Close()
	waitReady(t, supervisor, false)
	waitReady(t, supervisor, true)
	if err := supervisor.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitReady(t *testing.T, supervisor *SupervisedClient, expected bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for supervisor.Ready() != expected && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if supervisor.Ready() != expected {
		t.Fatalf("ready=%v want %v", supervisor.Ready(), expected)
	}
}
