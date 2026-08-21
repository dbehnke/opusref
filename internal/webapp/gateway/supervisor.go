package gateway

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbehnke/opusref/pkg/client"
)

type ClientFactory func() (client.Client, error)

// SupervisedClient keeps one replaceable OpusRef client connected. It keeps
// the public event channel stable across reconnects.
type SupervisedClient struct {
	factory                        ClientFactory
	events                         chan client.Event
	done                           chan struct{}
	mu                             sync.RWMutex
	active                         client.Client
	cancel                         context.CancelFunc
	ready                          atomic.Bool
	once                           sync.Once
	dropped                        map[ReflectorStream]bool
	initialBackoff, maximumBackoff time.Duration
	observer                       func(string)
}

func NewSupervisedClient(factory ClientFactory, eventCapacity int) *SupervisedClient {
	return NewSupervisedClientWithBackoff(factory, eventCapacity, 250*time.Millisecond, 10*time.Second)
}
func NewSupervisedClientWithBackoff(factory ClientFactory, eventCapacity int, initial, maximum time.Duration) *SupervisedClient {
	if eventCapacity <= 0 {
		eventCapacity = 512
	}
	if initial <= 0 {
		initial = 250 * time.Millisecond
	}
	if maximum < initial {
		maximum = 10 * time.Second
	}
	return &SupervisedClient{factory: factory, events: make(chan client.Event, eventCapacity), done: make(chan struct{}), dropped: map[ReflectorStream]bool{}, initialBackoff: initial, maximumBackoff: maximum}
}
func (s *SupervisedClient) Connect(ctx context.Context) error {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return client.ErrConnectInProgress
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()
	go s.run(runCtx)
	return nil
}
func (s *SupervisedClient) run(ctx context.Context) {
	defer s.once.Do(func() { close(s.done); close(s.events) })
	backoff := s.initialBackoff
	for ctx.Err() == nil {
		s.observe("attempt")
		current, err := s.factory()
		if err == nil {
			err = current.Connect(ctx)
		}
		if err != nil {
			s.observe("failure")
			if current != nil {
				_ = current.Close()
			}
			if !waitContext(ctx, jitter(backoff)) {
				return
			}
			backoff = min(backoff*2, s.maximumBackoff)
			continue
		}
		connectedAt := time.Now()
		s.mu.Lock()
		s.active = current
		s.mu.Unlock()
		s.ready.Store(true)
		s.observe("success")
		s.publish(ctx, client.Event{Kind: client.EventStatus, Message: "connected"})
	connected:
		for {
			select {
			case <-ctx.Done():
				_ = current.Close()
				s.clear(current)
				return
			case event, ok := <-current.Events():
				if ok {
					s.publish(ctx, event)
					continue
				}
				s.clear(current)
				_ = current.Close()
				s.publish(ctx, client.Event{Kind: client.EventStatus, Message: "disconnected", Synthetic: true})
				if time.Since(connectedAt) >= 30*time.Second {
					backoff = s.initialBackoff
				}
				if !waitContext(ctx, jitter(backoff)) {
					return
				}
				backoff = min(backoff*2, s.maximumBackoff)
				break connected
			case <-current.Done():
				s.clear(current)
				_ = current.Close()
				s.publish(ctx, client.Event{Kind: client.EventStatus, Message: "disconnected", Synthetic: true})
				if time.Since(connectedAt) >= 30*time.Second {
					backoff = s.initialBackoff
				}
				if !waitContext(ctx, jitter(backoff)) {
					return
				}
				backoff = min(backoff*2, s.maximumBackoff)
				break connected
			}
		}
	}
}
func (s *SupervisedClient) SetObserver(observer func(string)) {
	s.mu.Lock()
	s.observer = observer
	s.mu.Unlock()
}
func (s *SupervisedClient) observe(result string) {
	s.mu.RLock()
	observer := s.observer
	s.mu.RUnlock()
	if observer != nil {
		observer(result)
	}
}
func jitter(duration time.Duration) time.Duration {
	// Spread reconnects uniformly across ±20 percent to avoid synchronized clients.
	factor := 0.8 + rand.Float64()*0.4
	return time.Duration(float64(duration) * factor)
}
func (s *SupervisedClient) clear(current client.Client) {
	s.ready.Store(false)
	s.mu.Lock()
	if s.active == current {
		s.active = nil
	}
	s.mu.Unlock()
}
func (s *SupervisedClient) publish(ctx context.Context, event client.Event) {
	key := ReflectorStream{SessionID: event.SessionID, StreamID: event.StreamID}
	if event.Kind == client.EventAudio || event.Kind == client.EventData {
		select {
		case s.events <- event:
		default:
			s.dropped[key] = true
		}
		return
	}
	if event.Kind == client.EventStreamStart {
		delete(s.dropped, key)
	}
	if event.Kind == client.EventStreamEnd && s.dropped[key] {
		event.Synthetic = true
		if event.Message == "" {
			event.Message = "supervisor_backpressure"
		}
		delete(s.dropped, key)
	}
	select {
	case s.events <- event:
	case <-ctx.Done():
	}
}
func (s *SupervisedClient) withActive(call func(client.Client) error) error {
	s.mu.RLock()
	active := s.active
	s.mu.RUnlock()
	if active == nil {
		return client.ErrNotConnected
	}
	return call(active)
}
func (s *SupervisedClient) RequestStream(ctx context.Context, source string) error {
	return s.withActive(func(active client.Client) error { return active.RequestStream(ctx, source) })
}
func (s *SupervisedClient) ConfirmedGrant() (uint64, uint32, bool) {
	s.mu.RLock()
	active := s.active
	s.mu.RUnlock()
	provider, ok := active.(client.GrantIdentity)
	if !ok {
		return 0, 0, false
	}
	return provider.ConfirmedGrant()
}
func (s *SupervisedClient) SendAudio(ctx context.Context, timestamp uint32, payload []byte) error {
	return s.withActive(func(active client.Client) error { return active.SendAudio(ctx, timestamp, payload) })
}
func (s *SupervisedClient) SendData(ctx context.Context, timestamp uint32, dataType uint16, payload []byte) error {
	return s.withActive(func(active client.Client) error { return active.SendData(ctx, timestamp, dataType, payload) })
}
func (s *SupervisedClient) EndStream(ctx context.Context) error {
	return s.withActive(func(active client.Client) error { return active.EndStream(ctx) })
}
func (s *SupervisedClient) Events() <-chan client.Event { return s.events }
func (s *SupervisedClient) Done() <-chan struct{}       { return s.done }
func (s *SupervisedClient) Ready() bool                 { return s.ready.Load() }
func (s *SupervisedClient) Err() error {
	select {
	case <-s.done:
		return errors.New("supervised client stopped")
	default:
		return nil
	}
}
func (s *SupervisedClient) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	active := s.active
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if active != nil {
		_ = active.Close()
	}
	if cancel != nil {
		<-s.done
	}
	return nil
}
func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
