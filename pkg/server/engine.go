package server

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrInvalidSession  = errors.New("invalid session")
	ErrAddressMismatch = errors.New("session address mismatch")
	ErrInvalidStream   = errors.New("invalid stream")
)

type Limits struct {
	MaxClients, MaxCompletedTransactions                          int
	SessionTimeout, GrantTimeout, MediaTimeout, TransmitTimeLimit time.Duration
}

func (l Limits) defaults() Limits {
	if l.MaxClients == 0 {
		l.MaxClients = 100
	}
	if l.MaxCompletedTransactions == 0 {
		l.MaxCompletedTransactions = 1024
	}
	if l.SessionTimeout == 0 {
		l.SessionTimeout = 30 * time.Second
	}
	if l.GrantTimeout == 0 {
		l.GrantTimeout = 2 * time.Second
	}
	if l.MediaTimeout == 0 {
		l.MediaTimeout = time.Second
	}
	if l.TransmitTimeLimit == 0 {
		l.TransmitTimeLimit = 180 * time.Second
	}
	return l
}

type session struct {
	address, callsign string
	ready             bool
	last              time.Time
}
type FloorResult uint8

const (
	FloorGranted FloorResult = iota + 1
	FloorBusy
	FloorRejected
)

type EndReason string

const (
	EndNormal            EndReason = "normal"
	EndOwnerDisconnect   EndReason = "owner_disconnect"
	EndGrantTimeout      EndReason = "grant_timeout"
	EndMediaInactivity   EndReason = "media_inactivity"
	EndTransmitTimeLimit EndReason = "transmit_time_limit"
	EndServerShutdown    EndReason = "server_shutdown"
)

type FloorSnapshot struct {
	Active         bool   `json:"active"`
	SessionID      uint64 `json:"session_id,omitempty"`
	StreamID       uint32 `json:"stream_id,omitempty"`
	SourceCallsign string `json:"source_callsign,omitempty"`
}
type Snapshot struct {
	Ready        bool          `json:"ready"`
	Sessions     int           `json:"sessions"`
	SequenceGaps uint64        `json:"sequence_gaps"`
	Floor        FloorSnapshot `json:"floor"`
}
type Fanout struct {
	SessionID                     uint64
	StreamID, Sequence, Timestamp uint32
	Payload                       []byte
}
type StreamEnd struct {
	SessionID uint64
	StreamID  uint32
	Reason    EndReason
}
type floor struct {
	owner                          uint64
	stream                         uint32
	source                         string
	granted, firstMedia, lastMedia time.Time
	prior                          uint32
	hasPrior                       bool
}
type TransactionKey struct {
	SessionID uint64
	Type      uint8
	ID        uint64
}
type transaction struct {
	fingerprint, result []byte
	at                  time.Time
}
type TransactionState uint8

const (
	TransactionStored TransactionState = iota + 1
	TransactionDuplicate
	TransactionConflict
	TransactionOverloaded
)

// Engine is a deterministic policy engine. One caller must own it in production.
type Engine struct {
	mu       sync.Mutex
	limits   Limits
	now      func() time.Time
	sessions map[uint64]*session
	floor    *floor
	tx       map[TransactionKey]transaction
	gaps     uint64
	draining bool
}

func NewEngine(l Limits, now func() time.Time) *Engine {
	if now == nil {
		now = time.Now
	}
	return &Engine{limits: l.defaults(), now: now, sessions: map[uint64]*session{}, tx: map[TransactionKey]transaction{}}
}
func (e *Engine) AddSession(id uint64, address, callsign string, ready bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.draining || id == 0 || len(e.sessions) >= e.limits.MaxClients {
		return false
	}
	e.sessions[id] = &session{address: address, callsign: callsign, ready: ready, last: e.now()}
	return true
}
func (e *Engine) RequestFloor(id uint64, stream uint32, source string) FloorResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[id]
	if e.draining || s == nil || !s.ready || stream == 0 {
		return FloorRejected
	}
	s.last = e.now()
	if e.floor != nil {
		return FloorBusy
	}
	e.floor = &floor{owner: id, stream: stream, source: source, granted: e.now()}
	return FloorGranted
}
func (e *Engine) Media(id uint64, address string, stream, sequence, timestamp uint32, payload []byte) ([]Fanout, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.sessions[id]
	if s == nil {
		return nil, ErrInvalidSession
	}
	if s.address != address {
		return nil, ErrAddressMismatch
	}
	if e.draining || e.floor == nil || e.floor.owner != id || e.floor.stream != stream {
		return nil, ErrInvalidStream
	}
	now := e.now()
	s.last = now
	if e.floor.firstMedia.IsZero() {
		e.floor.firstMedia = now
	} else if e.floor.hasPrior {
		expected := e.floor.prior + 1
		delta := sequence - expected
		if delta > 0 && delta < 1<<31 {
			e.gaps += uint64(delta)
		} else if delta >= 1<<31 {
			return nil, nil
		}
	}
	e.floor.prior = sequence
	e.floor.hasPrior = true
	e.floor.lastMedia = now
	result := make([]Fanout, 0, len(e.sessions)-1)
	for sid, listener := range e.sessions {
		if sid != id && listener.ready {
			result = append(result, Fanout{SessionID: sid, StreamID: stream, Sequence: sequence, Timestamp: timestamp, Payload: append([]byte(nil), payload...)})
		}
	}
	return result, nil
}
func (e *Engine) End(id uint64, reason EndReason) *StreamEnd {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.releaseLocked(id, reason)
}
func (e *Engine) releaseLocked(id uint64, reason EndReason) *StreamEnd {
	if e.floor == nil || e.floor.owner != id {
		return nil
	}
	end := &StreamEnd{SessionID: id, StreamID: e.floor.stream, Reason: reason}
	e.floor = nil
	return end
}
func (e *Engine) Disconnect(id uint64) *StreamEnd {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.sessions, id)
	return e.releaseLocked(id, EndOwnerDisconnect)
}
func (e *Engine) Tick() *StreamEnd {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for id, s := range e.sessions {
		if now.Sub(s.last) > e.limits.SessionTimeout {
			delete(e.sessions, id)
			if end := e.releaseLocked(id, EndOwnerDisconnect); end != nil {
				return end
			}
		}
	}
	if e.floor == nil {
		return nil
	}
	if e.floor.firstMedia.IsZero() && now.Sub(e.floor.granted) > e.limits.GrantTimeout {
		return e.releaseLocked(e.floor.owner, EndGrantTimeout)
	}
	if !e.floor.firstMedia.IsZero() && now.Sub(e.floor.lastMedia) > e.limits.MediaTimeout {
		return e.releaseLocked(e.floor.owner, EndMediaInactivity)
	}
	if !e.floor.firstMedia.IsZero() && now.Sub(e.floor.firstMedia) > e.limits.TransmitTimeLimit {
		return e.releaseLocked(e.floor.owner, EndTransmitTimeLimit)
	}
	return nil
}
func (e *Engine) BeginShutdown() *StreamEnd {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.draining {
		return nil
	}
	e.draining = true
	if e.floor != nil {
		return e.releaseLocked(e.floor.owner, EndServerShutdown)
	}
	return nil
}
func (e *Engine) Snapshot() Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Snapshot{Ready: !e.draining, Sessions: len(e.sessions), SequenceGaps: e.gaps}
	if e.floor != nil {
		s.Floor = FloorSnapshot{Active: true, SessionID: e.floor.owner, StreamID: e.floor.stream, SourceCallsign: e.floor.source}
	}
	return s
}
func (e *Engine) Transaction(key TransactionKey, fingerprint, result []byte) ([]byte, TransactionState) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if old, ok := e.tx[key]; ok {
		if string(old.fingerprint) != string(fingerprint) {
			return nil, TransactionConflict
		}
		return append([]byte(nil), old.result...), TransactionDuplicate
	}
	if len(e.tx) >= e.limits.MaxCompletedTransactions {
		return nil, TransactionOverloaded
	}
	e.tx[key] = transaction{append([]byte(nil), fingerprint...), append([]byte(nil), result...), e.now()}
	return append([]byte(nil), result...), TransactionStored
}
