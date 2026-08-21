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
	entries map[[32]byte]map[[32]byte]entry
}

const (
	maxEntries     = 4096
	maxCategories  = 32
	stateRetention = 30 * time.Minute
)

func New() *Limiter {
	l := &Limiter{entries: map[[32]byte]map[[32]byte]entry{}}
	_, _ = rand.Read(l.key[:])
	return l
}
func (l *Limiter) Allow(category, value string, maximum int, window time.Duration, now time.Time) bool {
	if maximum <= 0 || window <= 0 {
		return false
	}
	categoryKey := l.digest(category)
	valueKey := l.digest(category + "\x00" + value)
	l.mu.Lock()
	defer l.mu.Unlock()
	for candidateCategory, candidates := range l.entries {
		for id, candidate := range candidates {
			if now.Sub(candidate.last) > stateRetention {
				delete(candidates, id)
			}
		}
		if len(candidates) == 0 {
			delete(l.entries, candidateCategory)
		}
	}
	entries := l.entries[categoryKey]
	if entries == nil {
		if len(l.entries) >= maxCategories {
			return false
		}
		entries = map[[32]byte]entry{}
		l.entries[categoryKey] = entries
	}
	item, exists := entries[valueKey]
	if !exists {
		if len(entries) >= maxEntries {
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
		entries[valueKey] = item
		return false
	}
	item.tokens--
	entries[valueKey] = item
	return true
}

func (l *Limiter) digest(value string) [32]byte {
	mac := hmac.New(sha256.New, l.key[:])
	_, _ = mac.Write([]byte(value))
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func (l *Limiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	var total int
	for _, entries := range l.entries {
		total += len(entries)
	}
	return total
}
