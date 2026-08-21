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
	mu      sync.Mutex
	pending string
	grants  map[ReflectorStream]string
}

func NewGrantAttribution() *GrantAttribution {
	return &GrantAttribution{grants: map[ReflectorStream]string{}}
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
	a.mu.Unlock()
}
func (a *GrantAttribution) Observe(event client.Event) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := ReflectorStream{SessionID: event.SessionID, StreamID: event.StreamID}
	switch event.Kind {
	case client.EventStreamGranted:
		if a.pending != "" {
			a.grants[key] = a.pending
			a.pending = ""
		}
	case client.EventStreamEnd:
		delete(a.grants, key)
	}
}
func (a *GrantAttribution) User(key ReflectorStream) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.grants[key]
}
