package monitor

import (
	"encoding/json"
	"net/http/httptest"
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
func TestMetricsRejectUnboundedLabels(t *testing.T) {
	r := New(1, 0, nil)
	if err := r.Inc("opusref_packets_total", map[string]string{"callsign": "N0CALL"}); err == nil {
		t.Fatal("accepted callsign label")
	}
	if err := r.Inc("opusref_queue_drops_total", map[string]string{"queue": "server_media", "item_type": "audio"}); err != nil {
		t.Fatal(err)
	}
}
