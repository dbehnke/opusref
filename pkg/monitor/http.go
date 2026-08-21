package monitor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type ClientSnapshot struct {
	NodeCallsign  string    `json:"node_callsign"`
	RemoteAddress string    `json:"remote_address"`
	SessionID     uint64    `json:"session_id"`
	ConnectedAt   time.Time `json:"connected_at"`
	LastActivity  time.Time `json:"last_activity"`
}
type StreamSnapshot struct {
	Active                   bool      `json:"active"`
	SessionID                uint64    `json:"session_id,omitempty"`
	StreamID                 uint32    `json:"stream_id,omitempty"`
	NodeCallsign             string    `json:"node_callsign,omitempty"`
	SourceCallsign           string    `json:"source_callsign,omitempty"`
	StartedAt                time.Time `json:"started_at,omitempty"`
	LastFrameAt              time.Time `json:"last_frame_at,omitempty"`
	RemainingTransmitSeconds float64   `json:"remaining_transmit_seconds"`
}
type Snapshot struct {
	APIVersion    int              `json:"api_version"`
	Version       string           `json:"version"`
	UptimeSeconds float64          `json:"uptime_seconds"`
	ReflectorID   string           `json:"reflector_id,omitempty"`
	Ready         bool             `json:"ready"`
	Clients       int              `json:"client_count"`
	ClientList    []ClientSnapshot `json:"-"`
	Floor         any              `json:"floor,omitempty"`
	Stream        StreamSnapshot   `json:"-"`
	Updated       time.Time        `json:"updated"`
}
type Event struct {
	ID       uint64         `json:"id"`
	Time     time.Time      `json:"time"`
	Type     string         `json:"type"`
	Severity string         `json:"severity"`
	Details  map[string]any `json:"details,omitempty"`
}
type metricSample struct {
	name   string
	labels map[string]string
	value  atomic.Uint64
}
type Registry struct {
	started         time.Time
	capacity        int
	next            atomic.Uint64
	snapshot        atomic.Pointer[Snapshot]
	mu              sync.Mutex
	events          []Event
	metrics         sync.Map
	health          func(time.Duration) bool
	deadline        time.Duration
	histogramCounts []uint64
	histogramCount  uint64
	histogramSum    float64
}

var labelValues = map[string]map[string]bool{"direction": set("rx", "tx"), "queue": set("server_inbound", "server_media", "server_control", "client_inbound", "client_events", "client_media", "client_control"), "item_type": set("datagram", "audio", "data", "control", "event"), "reason": set("malformed", "unsupported_version", "invalid_session", "address_mismatch", "invalid_stream", "unsupported_type", "limit_exceeded", "transaction_conflict", "normal", "owner_disconnect", "grant_timeout", "media_inactivity", "transmit_time_limit", "server_shutdown"), "result": set("accepted", "rejected", "overloaded", "granted", "busy"), "mode": set("open", "shared_key"), "kind": set("challenge", "session", "grant", "media_inactivity", "transmit_time_limit", "transaction", "shutdown"), "packet_type": set("hello", "challenge", "authenticate", "welcome", "keepalive", "disconnect", "error", "stream_request", "stream_grant", "stream_busy", "stream_start", "stream_end", "stream_revoke", "audio", "data")}
var metricLabels = map[string][]string{"opusref_packets_total": {"direction", "packet_type"}, "opusref_bytes_total": {"direction", "packet_type"}, "opusref_packet_errors_total": {"reason"}, "opusref_authentication_total": {"result", "mode"}, "opusref_streams_total": {"result"}, "opusref_stream_end_total": {"reason"}, "opusref_busy_total": {}, "opusref_timeouts_total": {"kind"}, "opusref_fanout_frames_total": {"item_type"}, "opusref_fanout_recipients_total": {"item_type"}, "opusref_queue_drops_total": {"queue", "item_type"}, "opusref_queue_drop_recipients_total": {"queue", "item_type"}, "opusref_sequence_gaps_total": {"direction"}}
var metricValueOverrides = map[string]map[string]map[string]bool{"opusref_packet_errors_total": {"reason": set("malformed", "unsupported_version", "invalid_session", "address_mismatch", "invalid_stream", "unsupported_type", "limit_exceeded", "transaction_conflict")}, "opusref_authentication_total": {"result": set("accepted", "rejected", "overloaded")}, "opusref_streams_total": {"result": set("granted", "busy", "rejected", "overloaded")}, "opusref_stream_end_total": {"reason": set("normal", "owner_disconnect", "grant_timeout", "media_inactivity", "transmit_time_limit", "server_shutdown")}}

func set(values ...string) map[string]bool {
	r := map[string]bool{}
	for _, v := range values {
		r[v] = true
	}
	return r
}
func New(capacity int, deadline time.Duration, health func(time.Duration) bool) *Registry {
	if capacity <= 0 {
		capacity = 256
	}
	if deadline <= 0 {
		deadline = 250 * time.Millisecond
	}
	r := &Registry{started: time.Now(), capacity: capacity, health: health, deadline: deadline, histogramCounts: make([]uint64, 11)}
	r.Publish(Snapshot{})
	return r
}
func (r *Registry) ObserveStreamDuration(seconds float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.histogramCount++
	r.histogramSum += seconds
	for i, bucket := range []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 180} {
		if seconds <= bucket {
			r.histogramCounts[i]++
		}
	}
}
func (r *Registry) Publish(s Snapshot) {
	s.APIVersion = 1
	s.Updated = time.Now()
	s.ClientList = append([]ClientSnapshot(nil), s.ClientList...)
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
	return r.Add(name, labels, 1)
}
func (r *Registry) Add(name string, labels map[string]string, amount uint64) error {
	expected, ok := metricLabels[name]
	if !ok {
		return fmt.Errorf("unknown metric")
	}
	if len(labels) != len(expected) {
		return fmt.Errorf("incorrect metric labels")
	}
	copyLabels := map[string]string{}
	for _, label := range expected {
		value, ok := labels[label]
		values := labelValues[label]
		if override := metricValueOverrides[name][label]; override != nil {
			values = override
		}
		if !ok || !values[value] {
			return fmt.Errorf("invalid %s label", label)
		}
		copyLabels[label] = value
	}
	key := sampleKey(name, copyLabels)
	value, _ := r.metrics.LoadOrStore(key, &metricSample{name: name, labels: copyLabels})
	value.(*metricSample).value.Add(amount)
	return nil
}
func sampleKey(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteByte(';')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
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
	mux.HandleFunc(RouteStatus, func(w http.ResponseWriter, _ *http.Request) {
		s := *r.snapshot.Load()
		if s.Version == "" {
			s.Version = "dev"
		}
		s.UptimeSeconds = time.Since(r.started).Seconds()
		jsonResponse(w, &s)
	})
	mux.HandleFunc(RouteClients, func(w http.ResponseWriter, _ *http.Request) {
		s := r.snapshot.Load()
		jsonResponse(w, struct {
			APIVersion int              `json:"api_version"`
			Clients    []ClientSnapshot `json:"clients"`
		}{1, s.ClientList})
	})
	mux.HandleFunc(RouteStream, func(w http.ResponseWriter, _ *http.Request) {
		s := r.snapshot.Load()
		jsonResponse(w, struct {
			APIVersion int            `json:"api_version"`
			Stream     StreamSnapshot `json:"stream"`
		}{1, s.Stream})
	})
	mux.HandleFunc(RouteEvents, func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		events := append([]Event(nil), r.events...)
		r.mu.Unlock()
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}
		jsonResponse(w, struct {
			APIVersion int     `json:"api_version"`
			Events     []Event `json:"events"`
		}{1, events})
	})
	mux.HandleFunc(RouteMetrics, r.writeMetrics)
	return mux
}
func jsonResponse(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func (r *Registry) writeMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	s := r.snapshot.Load()
	gauges := map[string]float64{"opusref_up": 1, "opusref_ready": boolFloat(s.Ready), "opusref_uptime_seconds": time.Since(r.started).Seconds(), "opusref_sessions": float64(s.Clients), "opusref_floor_active": boolFloat(s.Stream.Active)}
	names := make([]string, 0, len(gauges))
	for name := range gauges {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(w, "# TYPE %s gauge\n%s %g\n", name, name, gauges[name])
	}
	samples := []*metricSample{}
	r.metrics.Range(func(_, v any) bool { samples = append(samples, v.(*metricSample)); return true })
	sort.Slice(samples, func(i, j int) bool {
		return sampleKey(samples[i].name, samples[i].labels) < sampleKey(samples[j].name, samples[j].labels)
	})
	lastName := ""
	for _, sample := range samples {
		if sample.name != lastName {
			fmt.Fprintf(w, "# TYPE %s counter\n", sample.name)
			lastName = sample.name
		}
		fmt.Fprintf(w, "%s%s %d\n", sample.name, formatLabels(sample.labels), sample.value.Load())
	}
	fmt.Fprintln(w, "# TYPE opusref_stream_duration_seconds histogram")
	r.mu.Lock()
	counts := append([]uint64(nil), r.histogramCounts...)
	count, sum := r.histogramCount, r.histogramSum
	r.mu.Unlock()
	for i, bucket := range []float64{.1, .25, .5, 1, 2, 5, 10, 30, 60, 120, 180} {
		fmt.Fprintf(w, "opusref_stream_duration_seconds_bucket{le=%q} %d\n", strconv.FormatFloat(bucket, 'f', -1, 64), counts[i])
	}
	fmt.Fprintf(w, "opusref_stream_duration_seconds_bucket{le=\"+Inf\"} %d\nopusref_stream_duration_seconds_sum %g\nopusref_stream_duration_seconds_count %d\n", count, sum, count)
}
func formatLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+strconv.Quote(labels[k]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
func boolFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}
