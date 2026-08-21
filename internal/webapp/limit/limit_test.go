package limit

import (
	"testing"
	"time"
)

func TestFixedWindowLimitAndReset(t *testing.T) {
	l := New()
	now := time.Now()
	if !l.Allow("login", "alice", 2, time.Minute, now) || !l.Allow("login", "alice", 2, time.Minute, now) || l.Allow("login", "alice", 2, time.Minute, now) {
		t.Fatal("limit failed")
	}
	if !l.Allow("login", "alice", 2, time.Minute, now.Add(time.Minute)) {
		t.Fatal("window did not reset")
	}
	if !l.Allow("login", "bob", 2, time.Minute, now) {
		t.Fatal("keys collided")
	}
}
