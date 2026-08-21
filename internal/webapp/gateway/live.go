package gateway

import (
	"crypto/rand"
	"encoding/binary"
	"sync"
	"sync/atomic"

	"github.com/dbehnke/opusref/internal/webapp/socket"
)

type LiveEventKind uint8

const (
	LiveStart LiveEventKind = iota + 1
	LiveMedia
	LiveEnd
	LiveDiscontinuity
)

type LiveEvent struct {
	Kind                    LiveEventKind
	Media                   socket.Media
	ChannelID, OldChannelID uint64
	SourceCallsign, Reason  string
}
type liveSubscriber struct {
	queue    chan LiveEvent
	channel  uint64
	sequence uint32
}

// LiveHub publishes immutable packets without blocking a reflector receive loop.
type LiveHub struct {
	mu          sync.Mutex
	next        uint64
	subscribers map[uint64]*liveSubscriber
	active      bool
	source      string
	drops       atomic.Uint64
	used        map[uint64]struct{}
}

func NewLiveHub() *LiveHub {
	return &LiveHub{subscribers: map[uint64]*liveSubscriber{}, used: map[uint64]struct{}{}}
}
func (h *LiveHub) Subscribe(capacity int) (uint64, <-chan LiveEvent, func()) {
	if capacity < 4 {
		capacity = 4
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.next++
	id := h.next
	sub := &liveSubscriber{queue: make(chan LiveEvent, capacity)}
	h.subscribers[id] = sub
	if h.active {
		sub.channel = h.newChannelLocked()
		sub.queue <- LiveEvent{Kind: LiveStart, ChannelID: sub.channel, SourceCallsign: h.source}
	}
	return id, sub.queue, func() {
		h.mu.Lock()
		if current, ok := h.subscribers[id]; ok {
			delete(h.subscribers, id)
			close(current.queue)
		}
		h.mu.Unlock()
	}
}
func (h *LiveHub) Start(source string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.active = true
	h.source = source
	for _, sub := range h.subscribers {
		sub.channel = h.newChannelLocked()
		sub.sequence = 0
		h.enqueueControlLocked(sub, LiveEvent{Kind: LiveStart, ChannelID: sub.channel, SourceCallsign: source})
	}
}
func (h *LiveHub) End(reason string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active {
		return
	}
	h.active = false
	for _, sub := range h.subscribers {
		h.enqueueControlLocked(sub, LiveEvent{Kind: LiveEnd, ChannelID: sub.channel, Reason: reason})
		sub.channel = 0
		sub.sequence = 0
	}
}
func (h *LiveHub) Publish(media socket.Media) {
	media.Payload = append([]byte(nil), media.Payload...)
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.active {
		return
	}
	for _, sub := range h.subscribers {
		copy := media
		copy.Kind = socket.KindLive
		copy.ChannelID = sub.channel
		copy.Sequence = sub.sequence
		copy.Payload = append([]byte(nil), media.Payload...)
		select {
		case sub.queue <- LiveEvent{Kind: LiveMedia, Media: copy}:
			sub.sequence++
		default:
			old := sub.channel
			for len(sub.queue) > 0 {
				<-sub.queue
			}
			sub.channel = h.newChannelLocked()
			sub.sequence = 0
			sub.queue <- LiveEvent{Kind: LiveDiscontinuity, OldChannelID: old, ChannelID: sub.channel, Reason: "slow_consumer"}
			sub.queue <- LiveEvent{Kind: LiveStart, ChannelID: sub.channel, SourceCallsign: h.source}
			copy.ChannelID = sub.channel
			copy.Sequence = 0
			sub.queue <- LiveEvent{Kind: LiveMedia, Media: copy}
			sub.sequence = 1
			h.drops.Add(1)
		}
	}
}
func (h *LiveHub) enqueueControlLocked(sub *liveSubscriber, event LiveEvent) {
	select {
	case sub.queue <- event:
	default:
		for len(sub.queue) > 0 {
			<-sub.queue
		}
		sub.queue <- event
		h.drops.Add(1)
	}
}
func (h *LiveHub) newChannelLocked() uint64 {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			h.next++
			if h.next == 0 {
				h.next++
			}
			return h.next
		}
		id := binary.BigEndian.Uint64(raw[:])
		if id == 0 {
			continue
		}
		if _, ok := h.used[id]; ok {
			continue
		}
		h.used[id] = struct{}{}
		return id
	}
}
