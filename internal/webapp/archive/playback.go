package archive

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

const (
	checkpointStride = 64
	maxCheckpoints   = 32768
)

type checkpoint struct {
	packet, offset int64
	arrival        uint32
}

// Playback is an immutable sparse index over one validated ORAR file.
type Playback struct {
	path        string
	size        int64
	checkpoints []checkpoint
	packets     int64
	duration    uint32
}

func buildPlayback(path string, size int64) (*Playback, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	index := &Playback{path: path, size: size}
	offset := int64(HeaderSize)
	for offset < size {
		packet, next, err := readPacketAt(file, size, offset)
		if err != nil {
			return nil, err
		}
		if index.packets%checkpointStride == 0 && len(index.checkpoints) < maxCheckpoints {
			index.checkpoints = append(index.checkpoints, checkpoint{packet: index.packets, offset: offset, arrival: packet.ArrivalMS})
		}
		index.packets++
		index.duration = packet.ArrivalMS
		offset = next
	}
	return index, nil
}
func (p *Playback) DurationMS() uint32 { return p.duration }
func (p *Playback) PacketCount() int64 { return p.packets }
func (p *Playback) NewCursor(elapsed uint32) (*PlaybackCursor, error) {
	file, err := os.Open(p.path)
	if err != nil {
		return nil, err
	}
	cursor := &PlaybackCursor{file: file, size: p.size, offset: HeaderSize}
	for _, item := range p.checkpoints {
		if item.arrival > elapsed {
			break
		}
		cursor.offset = item.offset
		cursor.packet = item.packet
	}
	for cursor.offset < cursor.size {
		packet, next, readErr := readPacketAt(cursor.file, cursor.size, cursor.offset)
		if readErr != nil {
			cursor.Close()
			return nil, readErr
		}
		if packet.ArrivalMS >= elapsed {
			break
		}
		cursor.offset = next
		cursor.packet++
	}
	return cursor, nil
}

type PlaybackCursor struct {
	file   *os.File
	size   int64
	offset int64
	packet int64
}

func (c *PlaybackCursor) Next() (Packet, error) {
	if c.offset >= c.size {
		return Packet{}, io.EOF
	}
	packet, next, err := readPacketAt(c.file, c.size, c.offset)
	if err != nil {
		return Packet{}, err
	}
	c.offset = next
	c.packet++
	return packet, nil
}
func (c *PlaybackCursor) Index() int64 { return c.packet }
func (c *PlaybackCursor) Close() error { return c.file.Close() }

func readPacketAt(file *os.File, size, offset int64) (Packet, int64, error) {
	if offset+EntryHeaderSize > size {
		return Packet{}, offset, io.ErrUnexpectedEOF
	}
	var header [EntryHeaderSize]byte
	if _, err := file.ReadAt(header[:], offset); err != nil {
		return Packet{}, offset, err
	}
	if binary.BigEndian.Uint16(header[14:16]) != 0 {
		return Packet{}, offset, errors.New("entry flags are not zero")
	}
	length := int(binary.BigEndian.Uint16(header[12:14]))
	if length < 1 || length > MaxPayload || offset+EntryHeaderSize+int64(length) > size {
		return Packet{}, offset, errors.New("entry length is invalid")
	}
	payload := make([]byte, length)
	if _, err := file.ReadAt(payload, offset+EntryHeaderSize); err != nil {
		return Packet{}, offset, err
	}
	next := offset + EntryHeaderSize + int64(length)
	return Packet{Sequence: binary.BigEndian.Uint32(header[0:4]), Timestamp: binary.BigEndian.Uint32(header[4:8]), ArrivalMS: binary.BigEndian.Uint32(header[8:12]), Payload: payload}, next, nil
}
