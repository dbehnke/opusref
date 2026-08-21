// Package limit implements bounded process-local request limits.
package limit

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"sync"
	"time"
)

type entry struct {
	start, last time.Time
	count       int
}
type Limiter struct {
	mu      sync.Mutex
	key     [32]byte
	entries map[[32]byte]entry
}

func New() *Limiter {
	l := &Limiter{entries: map[[32]byte]entry{}}
	_, _ = rand.Read(l.key[:])
	return l
}
func (l *Limiter) Allow(category, value string, maximum int, window time.Duration, now time.Time) bool {
	mac := hmac.New(sha256.New, l.key[:])
	mac.Write([]byte(category))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	l.mu.Lock()
	defer l.mu.Unlock()
	item := l.entries[key]
	if item.start.IsZero() || now.Sub(item.start) >= window {
		item = entry{start: now, last: now}
	}
	item.last = now
	if item.count >= maximum {
		l.entries[key] = item
		return false
	}
	item.count++
	l.entries[key] = item
	if len(l.entries) > 4096 {
		for id, candidate := range l.entries {
			if now.Sub(candidate.last) > 30*time.Minute {
				delete(l.entries, id)
			}
		}
	}
	return true
}
