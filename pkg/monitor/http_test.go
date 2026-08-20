package monitor

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealthReadinessEventsAndBounds(t *testing.T) {
	r := New(2, time.Millisecond, func(time.Duration) bool { return true })
	r.Publish(Snapshot{Ready: true, ReflectorID: "TEST"})
	r.AddEvent(Event{Type: "one"})
	r.AddEvent(Event{Type: "two"})
	r.AddEvent(Event{Type: "three"})
	for _, path := range []string{RouteHealth, RouteReady, RouteStatus, RouteEvents, RouteMetrics} {
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest("GET", RouteEvents, nil))
	var got struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Events) != 2 || got.Events[0].Type != "three" {
		t.Fatalf("%s: %v", w.Body.String(), err)
	}
}

func TestMetricsAreValidAndDoNotMutateLabels(t *testing.T) {
	r := New(1, 0, nil)
	labels := map[string]string{"queue": "server_media", "item_type": "audio"}
	if err := r.Inc("opusref_queue_drops_total", labels); err != nil {
		t.Fatal(err)
	}
	r.ObserveStreamDuration(1.5)
	if len(labels) != 2 {
		t.Fatalf("labels changed: %#v", labels)
	}
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest("GET", RouteMetrics, nil))
	body := w.Body.String()
	if !strings.Contains(body, "opusref_queue_drops_total{item_type=\"audio\",queue=\"server_media\"} 1") || !strings.Contains(body, "opusref_stream_duration_seconds_bucket{le=\"180\"} 1") {
		t.Fatalf("metrics:\n%s", body)
	}
}

func TestClientAndStreamEndpointsUseDedicatedShapes(t *testing.T) {
	r := New(1, 0, nil)
	r.Publish(Snapshot{Ready: true, Clients: 1, ClientList: []ClientSnapshot{{NodeCallsign: "N0CALL"}}, Stream: StreamSnapshot{Active: true, StreamID: 7, StartedAt: time.Unix(1, 0), LastFrameAt: time.Unix(2, 0), RemainingTransmitSeconds: 9}})
	for path, field := range map[string]string{RouteClients: "clients", RouteStream: "stream"} {
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got[field]; !ok {
			t.Fatalf("%s missing %s: %s", path, field, w.Body.String())
		}
		if path == RouteStream {
			stream := got["stream"].(map[string]any)
			for _, name := range []string{"started_at", "last_frame_at", "remaining_transmit_seconds"} {
				if _, ok := stream[name]; !ok {
					t.Fatalf("missing %s: %s", name, w.Body.String())
				}
			}
		}
	}
}
func TestMetricsRejectUnboundedLabels(t *testing.T) {
	r := New(1, 0, nil)
	if err := r.Inc("opusref_packets_total", map[string]string{"callsign": "N0CALL"}); err == nil {
		t.Fatal("accepted callsign label")
	}
	if err := r.Inc("opusref_queue_drops_total", map[string]string{"queue": "server_media", "item_type": "audio"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Inc("opusref_authentication_total", map[string]string{"result": "granted", "mode": "open"}); err == nil {
		t.Fatal("accepted stream result for authentication")
	}
}
