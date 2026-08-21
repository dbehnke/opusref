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

func TestGrantAttributionPublishesExactLateBinding(t *testing.T) {
	attribution := NewGrantAttribution()
	bindings, cancel := attribution.SubscribeBindings()
	defer cancel()
	attribution.Begin("user-1")
	key := ReflectorStream{SessionID: 88, StreamID: 3}
	attribution.Bind(key, "user-1")
	select {
	case binding := <-bindings:
		if binding.Stream != key || binding.UserID != "user-1" {
			t.Fatalf("binding=%+v", binding)
		}
	default:
		t.Fatal("binding was not published")
	}
}
