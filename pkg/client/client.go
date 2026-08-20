// Package client defines the future raw-frame client boundaries.
package client

import (
	"context"
	"errors"
)

// ErrNotImplemented marks bootstrap-only operations.
var ErrNotImplemented = errors.New("opusref client networking is not implemented")

// ReceivedFrame is an opaque audio or typed-data frame received from a stream.
type ReceivedFrame struct {
	SessionID uint64
	StreamID  uint32
	Sequence  uint32
	Timestamp uint32
	DataType  uint16
	Payload   []byte
}

// Connector manages the future protocol session.
type Connector interface {
	Connect(context.Context) error
	Close() error
}

// Transmitter manages the future local stream.
type Transmitter interface {
	RequestStream(context.Context, string) error
	SendAudio(context.Context, uint32, []byte) error
	SendData(context.Context, uint32, uint16, []byte) error
	EndStream(context.Context) error
}

// Receiver supplies raw frames from remote streams.
type Receiver interface {
	Frames() <-chan ReceivedFrame
}

// Client composes the small session, transmit, and receive contracts.
type Client interface {
	Connector
	Transmitter
	Receiver
}
