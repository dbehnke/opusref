package httpapi

import (
	"strings"
	"testing"
)

func TestOperatorEventsAreBoundedAndMetricsUseFixedLabels(t *testing.T) {
	metrics := newWebTelemetry()
	for index := 0; index < 300; index++ {
		metrics.event("quota", "warning", "Archive quota is full.")
	}
	if events := metrics.recent(); len(events) != 256 || events[0].ID != 300 || events[len(events)-1].ID != 45 {
		t.Fatalf("bounded events=%d first=%+v last=%+v", len(events), events[0], events[len(events)-1])
	}
	metrics.inc("opusrefweb_queue_drops_total", "live")
	output := metrics.render()
	for _, family := range []string{"http_requests", "websocket_closes", "audio_packets", "queue_drops", "ptt", "auth", "archive", "playback", "db_errors", "reconnect", "audit_writes"} {
		if !strings.Contains(output, "opusrefweb_"+family+"_total") {
			t.Fatalf("missing metric family %s", family)
		}
	}
	if strings.Contains(output, "N0CALL") || !strings.Contains(output, `opusrefweb_queue_drops_total{queue="live"} 1`) {
		t.Fatalf("metric output is not fixed-label/redacted:\n%s", output)
	}
}
