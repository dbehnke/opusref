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
	updated, last time.Time
	tokens        float64
}
type Limiter struct {
	mu      sync.Mutex
	key     [32]byte
	entries map[[32]byte]entry
}

const (
	maxEntries     = 4096
	stateRetention = 30 * time.Minute
)

func New() *Limiter {
	l := &Limiter{entries: map[[32]byte]entry{}}
	_, _ = rand.Read(l.key[:])
	return l
}
func (l *Limiter) Allow(category, value string, maximum int, window time.Duration, now time.Time) bool {
	if maximum <= 0 || window <= 0 {
		return false
	}
	mac := hmac.New(sha256.New, l.key[:])
	mac.Write([]byte(category))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	var key [32]byte
	copy(key[:], mac.Sum(nil))
	l.mu.Lock()
	defer l.mu.Unlock()
	for id, candidate := range l.entries {
		if now.Sub(candidate.last) > stateRetention {
			delete(l.entries, id)
		}
	}
	item, exists := l.entries[key]
	if !exists {
		if len(l.entries) >= maxEntries {
			return false
		}
		item = entry{updated: now, last: now, tokens: float64(maximum)}
	} else if now.After(item.updated) {
		item.tokens += now.Sub(item.updated).Seconds() * float64(maximum) / window.Seconds()
		if item.tokens > float64(maximum) {
			item.tokens = float64(maximum)
		}
		item.updated = now
	}
	item.last = now
	if item.tokens < 1 {
		l.entries[key] = item
		return false
	}
	item.tokens--
	l.entries[key] = item
	return true
}

func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
