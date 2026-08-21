package gateway

import (
	"github.com/dbehnke/opusref/internal/webapp/socket"
	"testing"
)

func TestSlowLiveSubscriberGetsNewChannelAndContiguousSequence(t *testing.T) {
	hub := NewLiveHub()
	_, events, cancel := hub.Subscribe(4)
	defer cancel()
	hub.Start("N0CALL")
	start := <-events
	if start.Kind != LiveStart {
		t.Fatalf("got %v", start.Kind)
	}
	for i := 0; i < 6; i++ {
		hub.Publish(socket.Media{Timestamp: uint32(i * 960), Payload: []byte{1}})
	}
	var discontinuity bool
	for len(events) > 0 {
		event := <-events
		if event.Kind == LiveDiscontinuity {
			discontinuity = true
			if event.ChannelID == event.OldChannelID || event.ChannelID == 0 {
				t.Fatal("channel was reused")
			}
		}
	}
	if !discontinuity {
		t.Fatal("missing discontinuity")
	}
}
