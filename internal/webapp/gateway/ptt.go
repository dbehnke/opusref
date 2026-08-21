// Package gateway composes browser state with ordinary OpusRef clients.
package gateway

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"sync"

	"github.com/dbehnke/opusref/pkg/client"
)

var (
	ErrBusy     = errors.New("PTT is busy")
	ErrNotOwner = errors.New("PTT channel is not owned by the session")
	ErrSequence = errors.New("PTT sequence or timestamp is invalid")
)

type ReflectorTransmitter interface {
	RequestStream(context.Context, string) error
	SendAudio(context.Context, uint32, []byte) error
	EndStream(context.Context) error
}
type Grant struct{ ChannelID uint64 }
type PTTEnd struct {
	Session, Reason string
	ChannelID       uint64
}
type pttState struct {
	session                     string
	channel                     uint64
	nextSequence, nextTimestamp uint32
}
type PTTManager struct {
	mu          sync.Mutex
	tx          ReflectorTransmitter
	state       pttState
	used        map[uint64]struct{}
	attribution *GrantAttribution
	subscribers map[uint64]chan PTTEnd
	nextSub     uint64
}

func NewPTTManager(tx ReflectorTransmitter) *PTTManager {
	return &PTTManager{tx: tx, used: map[uint64]struct{}{}, subscribers: map[uint64]chan PTTEnd{}}
}
func (m *PTTManager) SetAttribution(attribution *GrantAttribution) { m.attribution = attribution }
func (m *PTTManager) Start(ctx context.Context, session, callsign string) (Grant, error) {
	return m.StartForUser(ctx, session, "", callsign)
}
func (m *PTTManager) StartForUser(ctx context.Context, session, userID, callsign string) (Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.channel != 0 {
		return Grant{}, ErrBusy
	}
	if m.attribution != nil {
		m.attribution.Begin(userID)
	}
	if err := m.tx.RequestStream(ctx, callsign); err != nil {
		if m.attribution != nil {
			m.attribution.Cancel(userID)
		}
		return Grant{}, err
	}
	channel, err := m.channel()
	if err != nil {
		_ = m.tx.EndStream(ctx)
		return Grant{}, err
	}
	m.state = pttState{session: session, channel: channel}
	return Grant{channel}, nil
}
func (m *PTTManager) Send(ctx context.Context, session string, channel uint64, sequence, timestamp uint32, payload []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.channel != channel || m.state.session != session {
		return ErrNotOwner
	}
	if sequence != m.state.nextSequence || timestamp != m.state.nextTimestamp {
		_ = m.tx.EndStream(ctx)
		m.state = pttState{}
		return ErrSequence
	}
	if len(payload) < 1 || len(payload) > 1168 {
		_ = m.tx.EndStream(ctx)
		m.state = pttState{}
		return errors.New("Opus packet length is invalid")
	}
	if err := m.tx.SendAudio(ctx, timestamp, append([]byte(nil), payload...)); err != nil {
		_ = m.tx.EndStream(ctx)
		m.state = pttState{}
		return err
	}
	m.state.nextSequence++
	m.state.nextTimestamp += 960
	return nil
}
func (m *PTTManager) Stop(ctx context.Context, session string, channel uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.channel != channel || m.state.session != session {
		return ErrNotOwner
	}
	m.state = pttState{}
	return m.tx.EndStream(ctx)
}
func (m *PTTManager) StopSession(ctx context.Context, session string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.session != session {
		return nil
	}
	m.state = pttState{}
	return m.tx.EndStream(ctx)
}
func (m *PTTManager) Observe(event client.Event) {
	if event.Kind != client.EventStreamEnd && !(event.Kind == client.EventStatus && event.Message == "disconnected") {
		return
	}
	m.mu.Lock()
	if m.state.channel == 0 {
		m.mu.Unlock()
		return
	}
	ended := PTTEnd{Session: m.state.session, ChannelID: m.state.channel, Reason: event.Message}
	if ended.Reason == "" {
		ended.Reason = "reflector_revoke"
	}
	m.state = pttState{}
	for _, subscriber := range m.subscribers {
		select {
		case subscriber <- ended:
		default:
		}
	}
	m.mu.Unlock()
}
func (m *PTTManager) SubscribeEnds() (<-chan PTTEnd, func()) {
	m.mu.Lock()
	m.nextSub++
	id := m.nextSub
	channel := make(chan PTTEnd, 4)
	m.subscribers[id] = channel
	m.mu.Unlock()
	return channel, func() {
		m.mu.Lock()
		delete(m.subscribers, id)
		m.mu.Unlock()
	}
}
func (m *PTTManager) channel() (uint64, error) {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, err
		}
		id := binary.BigEndian.Uint64(raw[:])
		if id == 0 {
			continue
		}
		if _, ok := m.used[id]; ok {
			continue
		}
		m.used[id] = struct{}{}
		return id, nil
	}
}
