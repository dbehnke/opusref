package gateway

import (
	"sync"

	"github.com/dbehnke/opusref/pkg/client"
)

type ReflectorStream struct {
	SessionID uint64
	StreamID  uint32
}

// GrantAttribution correlates a serialized web PTT request with the reflector
// grant that the transmitter client reports.
type GrantAttribution struct {
	mu          sync.Mutex
	pending     string
	grants      map[ReflectorStream]string
	subscribers map[uint64]chan GrantBinding
	nextSub     uint64
}
type GrantBinding struct {
	Stream ReflectorStream
	UserID string
}

func NewGrantAttribution() *GrantAttribution {
	return &GrantAttribution{grants: map[ReflectorStream]string{}, subscribers: map[uint64]chan GrantBinding{}}
}
func (a *GrantAttribution) Begin(userID string) {
	a.mu.Lock()
	a.pending = userID
	a.mu.Unlock()
}
func (a *GrantAttribution) Cancel(userID string) {
	a.mu.Lock()
	if a.pending == userID {
		a.pending = ""
	}
	a.mu.Unlock()
}
func (a *GrantAttribution) Bind(key ReflectorStream, userID string) {
	a.mu.Lock()
	a.grants[key] = userID
	if a.pending == userID {
		a.pending = ""
	}
	for _, subscriber := range a.subscribers {
		select {
		case subscriber <- GrantBinding{Stream: key, UserID: userID}:
		default:
		}
	}
	a.mu.Unlock()
}
func (a *GrantAttribution) Observe(event client.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := ReflectorStream{SessionID: event.SessionID, StreamID: event.StreamID}
	switch event.Kind {
	case client.EventStreamGranted:
		if a.pending != "" {
			userID := a.pending
			a.grants[key] = userID
			a.pending = ""
			for _, subscriber := range a.subscribers {
				select {
				case subscriber <- GrantBinding{Stream: key, UserID: userID}:
				default:
				}
			}
		}
	case client.EventStreamEnd:
		delete(a.grants, key)
	}
}
func (a *GrantAttribution) SubscribeBindings() (<-chan GrantBinding, func()) {
	a.mu.Lock()
	a.nextSub++
	id := a.nextSub
	channel := make(chan GrantBinding, 16)
	a.subscribers[id] = channel
	a.mu.Unlock()
	return channel, func() {
		a.mu.Lock()
		delete(a.subscribers, id)
		a.mu.Unlock()
	}
}
func (a *GrantAttribution) User(key ReflectorStream) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grants[key]
}
