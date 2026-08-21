package gateway

import (
	"testing"

	"github.com/dbehnke/opusref/pkg/client"
)

func TestGrantAttributionCorrelatesAndRetiresReflectorStream(t *testing.T) {
	attribution := NewGrantAttribution()
	attribution.Begin("user-1")
	key := ReflectorStream{SessionID: 10, StreamID: 20}
	attribution.Observe(client.Event{Kind: client.EventStreamGranted, SessionID: key.SessionID, StreamID: key.StreamID})
	if got := attribution.User(key); got != "user-1" {
		t.Fatalf("user=%q", got)
	}
	attribution.Observe(client.Event{Kind: client.EventStreamEnd, SessionID: key.SessionID, StreamID: key.StreamID})
	if got := attribution.User(key); got != "" {
		t.Fatalf("retired user=%q", got)
	}
}
