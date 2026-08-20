// Package transport provides bounded UDP adapters without protocol policy.
package transport

import (
	"context"
	"net"
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
}

func NewUDP(conn net.PacketConn, inbound, control, media int) *UDP {
	return &UDP{Conn: conn, Inbound: make(chan Datagram, inbound), Control: make(chan Datagram, control), Media: make(chan MediaBatch, media)}
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
