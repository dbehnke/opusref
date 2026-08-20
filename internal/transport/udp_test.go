package transport

import (
	"net"
	"testing"
)

type addr string

func (a addr) Network() string { return "test" }
func (a addr) String() string  { return string(a) }
func TestBoundedQueuesCountDropsAndCopy(t *testing.T) {
	u := NewUDP(nil, 1, 1, 1)
	data := []byte{1}
	if !u.EnqueueMedia(MediaBatch{Data: data, Recipients: []net.Addr{addr("a"), addr("b")}}) {
		t.Fatal("first dropped")
	}
	data[0] = 2
	if u.EnqueueMedia(MediaBatch{Data: []byte{3}, Recipients: []net.Addr{addr("a"), addr("b"), addr("c")}}) {
		t.Fatal("second accepted")
	}
	if u.MediaDrops.Load() != 1 || u.MediaDropRecipients.Load() != 3 {
		t.Fatal("bad counters")
	}
	if (<-u.Media).Data[0] != 1 {
		t.Fatal("payload was not copied")
	}
	if !u.EnqueueControl(Datagram{Address: addr("a"), Data: []byte{1}}) || u.EnqueueControl(Datagram{Address: addr("a"), Data: []byte{2}}) {
		t.Fatal("control capacity")
	}
	if u.ControlFailures.Load() != 1 {
		t.Fatal("control counter")
	}
}
