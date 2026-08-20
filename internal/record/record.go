// Package record reads and writes diagnostic OpusRef records.
package record

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const HeaderSize = 16

type Kind uint8

const (
	KindAudio Kind = 1
	KindData  Kind = 2
)

type Record struct {
	Kind      Kind
	DataType  uint16
	Timestamp uint32
	Payload   []byte
}

func Read(r io.Reader) (Record, error) {
	var h [HeaderSize]byte
	n, err := io.ReadFull(r, h[:])
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			return Record{}, io.EOF
		}
		return Record{}, fmt.Errorf("truncated diagnostic header: %w", err)
	}
	if string(h[:4]) != "ORR1" {
		return Record{}, errors.New("bad diagnostic magic")
	}
	kind := Kind(h[4])
	typ := binary.BigEndian.Uint16(h[6:8])
	length := binary.BigEndian.Uint32(h[12:16])
	if h[5] != 0 || kind < KindAudio || kind > KindData || kind == KindAudio && typ != 0 || kind == KindData && typ == 0 {
		return Record{}, errors.New("invalid diagnostic header")
	}
	max := uint32(1168)
	if kind == KindData {
		max = 1160
	}
	if length == 0 || length > max {
		return Record{}, errors.New("invalid diagnostic payload length")
	}
	p := make([]byte, length)
	if _, err := io.ReadFull(r, p); err != nil {
		return Record{}, fmt.Errorf("truncated diagnostic payload: %w", err)
	}
	return Record{Kind: kind, DataType: typ, Timestamp: binary.BigEndian.Uint32(h[8:12]), Payload: p}, nil
}
func Write(w io.Writer, rec Record) error {
	max := 1168
	if rec.Kind == KindData {
		max = 1160
	}
	if rec.Kind < KindAudio || rec.Kind > KindData || len(rec.Payload) < 1 || len(rec.Payload) > max || rec.Kind == KindAudio && rec.DataType != 0 || rec.Kind == KindData && rec.DataType == 0 {
		return errors.New("invalid diagnostic record")
	}
	var h [HeaderSize]byte
	copy(h[:4], "ORR1")
	h[4] = byte(rec.Kind)
	binary.BigEndian.PutUint16(h[6:8], rec.DataType)
	binary.BigEndian.PutUint32(h[8:12], rec.Timestamp)
	binary.BigEndian.PutUint32(h[12:16], uint32(len(rec.Payload)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(rec.Payload)
	return err
}
