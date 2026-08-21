package reflector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPollBuildsSanitizedSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/status":
			w.Write([]byte(`{"reflector_id":"TEST","ready":true,"client_count":2}`))
		case "/api/v1/clients":
			w.Write([]byte(`{"clients":[{"node_callsign":"N0CALL","remote_address":"secret"}]}`))
		case "/api/v1/stream":
			w.Write([]byte(`{"stream":{"active":true,"source_callsign":"N1CALL"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := New(server.URL)
	if err := client.Poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, ok := client.Snapshot()
	if !ok || snapshot.ReflectorID != "TEST" || snapshot.Clients[0].NodeCallsign != "N0CALL" || !client.Fresh(time.Second) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
}
