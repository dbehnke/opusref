package gateway

import (
	"github.com/dbehnke/opusref/internal/webapp/socket"
	"sync"
	"sync/atomic"
)

// LiveHub publishes immutable packets without blocking a reflector receive loop.
type LiveHub struct {
	mu          sync.RWMutex
	next        uint64
	subscribers map[uint64]chan socket.Media
	drops       atomic.Uint64
}

func NewLiveHub() *LiveHub { return &LiveHub{subscribers: map[uint64]chan socket.Media{}} }
func (h *LiveHub) Subscribe(capacity int) (uint64, <-chan socket.Media, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	queue := make(chan socket.Media, capacity)
	h.subscribers[id] = queue
	return id, queue, func() {
		h.mu.Lock()
		if q, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(q)
		}
		h.mu.Unlock()
	}
}
func (h *LiveHub) Publish(media socket.Media) {
	media.Payload = append([]byte(nil), media.Payload...)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, queue := range h.subscribers {
		copy := media
		copy.Payload = append([]byte(nil), media.Payload...)
		select {
		case queue <- copy:
		default:
			h.drops.Add(1)
		}
	}
}
