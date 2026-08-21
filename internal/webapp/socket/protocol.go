// Package socket defines strict ORWB WebSocket application messages.
package socket

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const (
	MediaHeaderSize   = 32
	MaxMediaMessage   = 1200
	MaxControlMessage = 16 * 1024
)

type Direction uint8

const (
	ClientToServer Direction = iota + 1
	ServerToClient
)

type MediaKind uint8

const (
	KindLive     MediaKind = 1
	KindTransmit MediaKind = 2
	KindPlayback MediaKind = 3
)

type Media struct {
	Kind                MediaKind
	ChannelID           uint64
	Sequence, Timestamp uint32
	Payload             []byte
}

func EncodeMedia(m Media) ([]byte, error) {
	if m.ChannelID == 0 || len(m.Payload) < 1 || len(m.Payload) > 1168 {
		return nil, errors.New("media fields are invalid")
	}
	if m.Kind < KindLive || m.Kind > KindPlayback {
		return nil, errors.New("media kind is invalid")
	}
	data := make([]byte, MediaHeaderSize+len(m.Payload))
	copy(data, "ORWB")
	data[4] = 1
	data[5] = byte(m.Kind)
	binary.BigEndian.PutUint64(data[8:16], m.ChannelID)
	binary.BigEndian.PutUint32(data[16:20], m.Sequence)
	binary.BigEndian.PutUint32(data[20:24], m.Timestamp)
	binary.BigEndian.PutUint16(data[24:26], uint16(len(m.Payload)))
	copy(data[32:], m.Payload)
	return data, nil
}
func DecodeMedia(data []byte, direction Direction) (Media, error) {
	if len(data) < MediaHeaderSize || len(data) > MaxMediaMessage || string(data[:4]) != "ORWB" || data[4] != 1 {
		return Media{}, errors.New("media header is invalid")
	}
	kind := MediaKind(data[5])
	if direction == ClientToServer && kind != KindTransmit {
		return Media{}, errors.New("client media kind is invalid")
	}
	if direction == ServerToClient && kind != KindLive && kind != KindPlayback {
		return Media{}, errors.New("server media kind is invalid")
	}
	if data[6] != 0 || data[7] != 0 {
		return Media{}, errors.New("media flags are not zero")
	}
	for _, v := range data[26:32] {
		if v != 0 {
			return Media{}, errors.New("media reserved bytes are not zero")
		}
	}
	id := binary.BigEndian.Uint64(data[8:16])
	length := int(binary.BigEndian.Uint16(data[24:26]))
	if id == 0 || length < 1 || length > 1168 || len(data) != MediaHeaderSize+length {
		return Media{}, errors.New("media length or channel is invalid")
	}
	return Media{kind, id, binary.BigEndian.Uint32(data[16:20]), binary.BigEndian.Uint32(data[20:24]), append([]byte(nil), data[32:]...)}, nil
}

type Control struct {
	APIVersion int             `json:"api_version"`
	Type       string          `json:"type"`
	RequestID  string          `json:"request_id,omitempty"`
	Body       json.RawMessage `json:"body"`
}

func DecodeControl(data []byte, direction Direction) (Control, error) {
	if len(data) > MaxControlMessage {
		return Control{}, errors.New("control message is too large")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return Control{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Control
	if err := dec.Decode(&c); err != nil {
		return Control{}, err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return Control{}, errors.New("control message has trailing data")
	}
	if c.APIVersion != 1 || c.Type == "" {
		return Control{}, errors.New("control envelope is invalid")
	}
	if direction == ClientToServer && !validRequestID(c.RequestID) {
		return Control{}, errors.New("request ID is invalid")
	}
	if c.Body == nil {
		c.Body = json.RawMessage(`{}`)
	}
	return c, nil
}
func validRequestID(v string) bool {
	if len(v) < 1 || len(v) > 64 {
		return false
	}
	for i := range v {
		if v[i] < 0x20 || v[i] > 0x7e {
			return false
		}
	}
	return true
}
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				keyToken, err := dec.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("object key is invalid")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[key] = true
				if err = walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err = walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	return walk()
}
