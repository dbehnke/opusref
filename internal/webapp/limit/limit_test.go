package limit

import (
	"testing"
	"time"
)

func TestTokenBucketRefillsWithoutBoundaryBurst(t *testing.T) {
	l := New()
	now := time.Unix(100, 0)
	if !l.Allow("login", "alice", 2, time.Minute, now) || !l.Allow("login", "alice", 2, time.Minute, now) || l.Allow("login", "alice", 2, time.Minute, now) {
		t.Fatal("limit failed")
	}
	if l.Allow("login", "alice", 2, time.Minute, now.Add(29*time.Second)) {
		t.Fatal("bucket refilled before one token was available")
	}
	if !l.Allow("login", "alice", 2, time.Minute, now.Add(30*time.Second)) || l.Allow("login", "alice", 2, time.Minute, now.Add(30*time.Second)) {
		t.Fatal("bucket did not refill exactly one token")
	}
	if !l.Allow("login", "bob", 2, time.Minute, now) {
		t.Fatal("keys collided")
	}
}

func TestLimiterExpiresAndBoundsCardinality(t *testing.T) {
	l := New()
	now := time.Unix(100, 0)
	for i := 0; i < maxEntries; i++ {
		if !l.Allow("ip", string(rune(i)), 1, time.Minute, now) {
			t.Fatalf("new key %d was refused", i)
		}
	}
	if l.Allow("ip", "overflow", 1, time.Minute, now) {
		t.Fatal("cardinality overflow was accepted")
	}
	if got := l.size(); got > maxEntries {
		t.Fatalf("cardinality=%d", got)
	}
	if !l.Allow("fresh", "value", 1, time.Minute, now.Add(stateRetention+time.Second)) {
		t.Fatal("expired state blocked a fresh key")
	}
	if got := l.size(); got != 1 {
		t.Fatalf("expired state retained: %d", got)
	}
}

func TestCardinalityIsIsolatedByCategory(t *testing.T) {
	l := New()
	now := time.Unix(100, 0)
	for i := 0; i < maxEntries; i++ {
		if !l.Allow("login_user", string(rune(i)), 1, time.Minute, now) {
			t.Fatalf("new login key %d was refused", i)
		}
	}
	if l.Allow("login_user", "overflow", 1, time.Minute, now) {
		t.Fatal("category overflow was accepted")
	}
	for _, category := range []string{"login_ip", "ws_ip", "passkey_ip", "passkey_account", "admin"} {
		if !l.Allow(category, "independent", 1, time.Minute, now) {
			t.Fatalf("%s was starved by login usernames", category)
		}
	}
}

func TestCategoryCountIsBounded(t *testing.T) {
	l := New()
	now := time.Unix(100, 0)
	for index := 0; index < maxCategories; index++ {
		if !l.Allow(string(rune(index)), "value", 1, time.Minute, now) {
			t.Fatalf("category %d was refused", index)
		}
	}
	if l.Allow("overflow", "value", 1, time.Minute, now) {
		t.Fatal("category overflow was accepted")
	}
}
