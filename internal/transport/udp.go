// Package transport provides bounded UDP adapters without protocol policy.
package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

type Datagram struct {
	Address net.Addr
	Data    []byte
}
type MediaBatch struct {
	Data       []byte
	Recipients []net.Addr
}
type UDP struct {
	Conn                                                           net.PacketConn
	Inbound                                                        chan Datagram
	Control                                                        chan Datagram
	Media                                                          chan MediaBatch
	InboundDrops, MediaDrops, MediaDropRecipients, ControlFailures atomic.Uint64
	mediaMu                                                        sync.Mutex
	mediaEnabled                                                   atomic.Bool
}

func NewUDP(conn net.PacketConn, inbound, control, media int) (*UDP, error) {
	if inbound <= 0 || control <= 0 || media <= 0 {
		return nil, errors.New("transport queue capacities must be positive")
	}
	u := &UDP{Conn: conn, Inbound: make(chan Datagram, inbound), Control: make(chan Datagram, control), Media: make(chan MediaBatch, media)}
	u.mediaEnabled.Store(true)
	return u, nil
}
func (u *UDP) EnqueueControl(d Datagram) bool {
	d.Data = append([]byte(nil), d.Data...)
	select {
	case u.Control <- d:
		return true
	default:
		u.ControlFailures.Add(1)
		return false
	}
}
func (u *UDP) EnqueueMedia(b MediaBatch) bool {
	u.mediaMu.Lock()
	defer u.mediaMu.Unlock()
	if !u.mediaEnabled.Load() {
		u.MediaDrops.Add(1)
		u.MediaDropRecipients.Add(uint64(len(b.Recipients)))
		return false
	}
	b.Data = append([]byte(nil), b.Data...)
	b.Recipients = append([]net.Addr(nil), b.Recipients...)
	select {
	case u.Media <- b:
		return true
	default:
		u.MediaDrops.Add(1)
		u.MediaDropRecipients.Add(uint64(len(b.Recipients)))
		return false
	}
}

// DisableMedia rejects future media and removes every queued media batch.
func (u *UDP) DisableMedia() (frames, recipients uint64) {
	u.mediaMu.Lock()
	defer u.mediaMu.Unlock()
	u.mediaEnabled.Store(false)
	for {
		select {
		case batch := <-u.Media:
			frames++
			recipients += uint64(len(batch.Recipients))
		default:
			return frames, recipients
		}
	}
}
func (u *UDP) Read(ctx context.Context, max int) error {
	buf := make([]byte, max)
	for {
		if err := u.Conn.SetReadDeadline(deadline(ctx)); err != nil {
			return err
		}
		n, addr, err := u.Conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		d := Datagram{Address: addr, Data: append([]byte(nil), buf[:n]...)}
		select {
		case u.Inbound <- d:
		default:
			u.InboundDrops.Add(1)
		}
	}
}
func (u *UDP) Write(ctx context.Context) error {
	for {
		var d Datagram
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d = <-u.Control:
			if _, err := u.Conn.WriteTo(d.Data, d.Address); err != nil {
				return err
			}
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d = <-u.Control:
			if _, err := u.Conn.WriteTo(d.Data, d.Address); err != nil {
				return err
			}
		case b := <-u.Media:
			for _, addr := range b.Recipients {
				if !u.mediaEnabled.Load() {
					break
				}
				select {
				case d = <-u.Control:
					if _, err := u.Conn.WriteTo(d.Data, d.Address); err != nil {
						return err
					}
				default:
				}
				if _, err := u.Conn.WriteTo(b.Data, addr); err != nil {
					return err
				}
			}
		}
	}
}
func deadline(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(time.Second)
}
