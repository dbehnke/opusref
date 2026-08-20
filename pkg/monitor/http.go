package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Snapshot struct {
	APIVersion  int       `json:"api_version"`
	ReflectorID string    `json:"reflector_id,omitempty"`
	Ready       bool      `json:"ready"`
	Clients     int       `json:"client_count"`
	Floor       any       `json:"floor,omitempty"`
	Updated     time.Time `json:"updated"`
}
type Event struct {
	ID       uint64         `json:"id"`
	Time     time.Time      `json:"time"`
	Type     string         `json:"type"`
	Severity string         `json:"severity"`
	Details  map[string]any `json:"details,omitempty"`
}
type Registry struct {
	started  time.Time
	capacity int
	next     atomic.Uint64
	snapshot atomic.Pointer[Snapshot]
	mu       sync.Mutex
	events   []Event
	counters sync.Map
	health   func(time.Duration) bool
	deadline time.Duration
}

func New(capacity int, deadline time.Duration, health func(time.Duration) bool) *Registry {
	if capacity <= 0 {
		capacity = 256
	}
	if deadline <= 0 {
		deadline = 250 * time.Millisecond
	}
	r := &Registry{started: time.Now(), capacity: capacity, health: health, deadline: deadline}
	r.Publish(Snapshot{APIVersion: 1})
	return r
}
func (r *Registry) Publish(s Snapshot) {
	s.APIVersion = 1
	s.Updated = time.Now()
	copy := s
	r.snapshot.Store(&copy)
}
func (r *Registry) AddEvent(event Event) {
	event.ID = r.next.Add(1)
	event.Time = time.Now().UTC()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == r.capacity {
		copy(r.events, r.events[1:])
		r.events = r.events[:r.capacity-1]
	}
	r.events = append(r.events, event)
}
func (r *Registry) Inc(name string, labels map[string]string) error {
	key, err := metricKey(name, labels)
	if err != nil {
		return err
	}
	value, _ := r.counters.LoadOrStore(key, new(atomic.Uint64))
	value.(*atomic.Uint64).Add(1)
	return nil
}
func metricKey(name string, labels map[string]string) (string, error) {
	allowed := map[string]map[string]bool{"direction": {"rx": true, "tx": true}, "queue": {"server_inbound": true, "server_media": true, "server_control": true, "client_inbound": true, "client_events": true, "client_media": true, "client_control": true}, "item_type": {"datagram": true, "audio": true, "data": true, "control": true, "event": true}}
	parts := []string{name}
	for _, label := range []string{"direction", "queue", "item_type"} {
		if v, ok := labels[label]; ok {
			if !allowed[label][v] {
				return "", fmt.Errorf("invalid %s label", label)
			}
			parts = append(parts, label+"="+v)
			delete(labels, label)
		}
	}
	if len(labels) != 0 {
		return "", fmt.Errorf("unsupported metric label")
	}
	return strings.Join(parts, ";"), nil
}
func (r *Registry) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(RouteHealth, func(w http.ResponseWriter, _ *http.Request) {
		if r.health != nil && !r.health(r.deadline) {
			http.Error(w, "unhealthy", 503)
			return
		}
		w.WriteHeader(200)
	})
	mux.HandleFunc(RouteReady, func(w http.ResponseWriter, _ *http.Request) {
		if !r.snapshot.Load().Ready {
			http.Error(w, "not ready", 503)
			return
		}
		w.WriteHeader(200)
	})
	jsonSnapshot := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(r.snapshot.Load())
	}
	mux.HandleFunc(RouteStatus, jsonSnapshot)
	mux.HandleFunc(RouteClients, jsonSnapshot)
	mux.HandleFunc(RouteStream, jsonSnapshot)
	mux.HandleFunc(RouteEvents, func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		events := append([]Event(nil), r.events...)
		r.mu.Unlock()
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			APIVersion int     `json:"api_version"`
			Events     []Event `json:"events"`
		}{1, events})
	})
	mux.HandleFunc(RouteMetrics, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "# TYPE opusref_up gauge\nopusref_up 1")
		ready := 0
		if r.snapshot.Load().Ready {
			ready = 1
		}
		fmt.Fprintf(w, "# TYPE opusref_ready gauge\nopusref_ready %d\n", ready)
		r.counters.Range(func(k, v any) bool {
			key := strings.ReplaceAll(k.(string), ";", "_")
			fmt.Fprintf(w, "%s %d\n", key, v.(*atomic.Uint64).Load())
			return true
		})
	})
	return mux
}
