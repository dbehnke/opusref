package httpapi

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type operatorEvent struct {
	ID       uint64    `json:"id"`
	Time     time.Time `json:"time"`
	Kind     string    `json:"kind"`
	Severity string    `json:"severity"`
	Message  string    `json:"message"`
}

type webTelemetry struct {
	mu        sync.Mutex
	counters  map[string]uint64
	events    []operatorEvent
	nextEvent uint64
	quotaSet  bool
	quotaFull bool
}

func newWebTelemetry() *webTelemetry { return &webTelemetry{counters: map[string]uint64{}} }
func (m *webTelemetry) inc(name string, labels ...string) {
	m.mu.Lock()
	m.counters[name+"\x00"+strings.Join(labels, "\x00")]++
	m.mu.Unlock()
}
func (m *webTelemetry) event(kind, severity, message string) {
	m.mu.Lock()
	m.nextEvent++
	m.events = append(m.events, operatorEvent{m.nextEvent, time.Now().UTC(), kind, severity, message})
	if len(m.events) > 256 {
		m.events = append([]operatorEvent(nil), m.events[len(m.events)-256:]...)
	}
	m.mu.Unlock()
}
func (m *webTelemetry) setQuota(full bool) {
	m.mu.Lock()
	changed := (!m.quotaSet && full) || (m.quotaSet && m.quotaFull != full)
	m.quotaSet, m.quotaFull = true, full
	m.mu.Unlock()
	if changed {
		if full {
			m.event("quota", "warning", "Archive quota is full; new recordings are paused.")
		} else {
			m.event("quota", "info", "Archive quota has available capacity.")
		}
	}
}
func (m *webTelemetry) recent() []operatorEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := append([]operatorEvent(nil), m.events...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}
func (m *webTelemetry) value(name string, labels ...string) uint64 {
	return m.counters[name+"\x00"+strings.Join(labels, "\x00")]
}
func (m *webTelemetry) render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out strings.Builder
	write := func(name, labelNames string, dimensions [][]string) {
		fmt.Fprintf(&out, "# TYPE %s counter\n", name)
		for _, values := range dimensions {
			labels := ""
			if labelNames != "" {
				parts := strings.Split(labelNames, ",")
				pairs := make([]string, len(parts))
				for index := range parts {
					pairs[index] = fmt.Sprintf(`%s=%q`, parts[index], values[index])
				}
				labels = "{" + strings.Join(pairs, ",") + "}"
			}
			fmt.Fprintf(&out, "%s%s %d\n", name, labels, m.value(name, values...))
		}
	}
	product := func(groups ...[]string) [][]string {
		result := [][]string{{}}
		for _, group := range groups {
			var next [][]string
			for _, prefix := range result {
				for _, value := range group {
					next = append(next, append(append([]string(nil), prefix...), value))
				}
			}
			result = next
		}
		return result
	}
	write("opusrefweb_http_requests_total", "route,status_class", product([]string{"health", "ready", "status", "auth", "accounts", "recordings", "websocket", "metrics", "other"}, []string{"1xx", "2xx", "3xx", "4xx", "5xx"}))
	write("opusrefweb_websocket_closes_total", "reason", product([]string{"normal", "protocol", "authentication", "overload", "restart", "transport"}))
	write("opusrefweb_audio_packets_total", "direction", product([]string{"live", "transmit", "playback"}))
	write("opusrefweb_queue_drops_total", "queue", product([]string{"control", "live", "playback", "archive"}))
	write("opusrefweb_ptt_total", "result", product([]string{"grant", "busy", "stop", "revoke", "error"}))
	write("opusrefweb_auth_total", "method,result", product([]string{"password", "passkey"}, []string{"success", "failure", "rate_limited", "sign_counter_regression"}))
	write("opusrefweb_archive_total", "action,result", product([]string{"create", "finalize", "recover", "delete", "purge"}, []string{"success", "partial", "failure"}))
	write("opusrefweb_archive_alerts_total", "kind", product([]string{"full", "clear", "recovery", "recovery_overflow"}))
	write("opusrefweb_playback_total", "action,result", product([]string{"open", "seek", "pause", "resume", "close"}, []string{"success", "busy", "failure"}))
	write("opusrefweb_db_errors_total", "operation", product([]string{"read", "write", "migration", "backup", "audit"}))
	write("opusrefweb_reconnect_total", "client,result", product([]string{"receive", "transmit"}, []string{"attempt", "success", "failure"}))
	write("opusrefweb_audit_writes_total", "result", product([]string{"success", "failure"}))
	return out.String()
}

type observedWriter struct {
	http.ResponseWriter
	status int
}

func (w *observedWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.ResponseWriter.(http.Hijacker).Hijack()
}
func (w *observedWriter) Flush() { http.NewResponseController(w.ResponseWriter).Flush() }

func routeLabel(pattern, path string) string {
	value := pattern + " " + path
	for label, token := range map[string]string{"health": "/healthz", "ready": "/readyz", "status": "/status", "auth": "/auth/", "accounts": "/accounts", "recordings": "/recordings", "websocket": "/ws", "metrics": "/metrics"} {
		if strings.Contains(value, token) {
			return label
		}
	}
	return "other"
}
func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "5xx"
	}
	return fmt.Sprintf("%dxx", status/100)
}
