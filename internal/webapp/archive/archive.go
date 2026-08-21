// Package archive reads and writes ORAR packet archives without inspecting Opus.
package archive

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const (
	HeaderSize      = 32
	EntryHeaderSize = 16
	MaxPayload      = 1168
	MaxFileSize     = int64(256 * 1024 * 1024)
)

var ErrLimit = errors.New("archive size limit reached")

type Packet struct {
	Sequence, Timestamp, ArrivalMS uint32
	Payload                        []byte
}
type Writer struct {
	w    io.Writer
	size int64
}

func NewWriter(w io.Writer, id uuid.UUID) (*Writer, error) {
	if w == nil {
		return nil, errors.New("writer is required")
	}
	h := make([]byte, HeaderSize)
	copy(h, "ORAR")
	binary.BigEndian.PutUint16(h[4:6], 1)
	binary.BigEndian.PutUint16(h[6:8], HeaderSize)
	copy(h[8:24], id[:])
	if err := writeAll(w, h); err != nil {
		return nil, err
	}
	return &Writer{w: w, size: HeaderSize}, nil
}
func (w *Writer) WritePacket(p Packet) error {
	if len(p.Payload) < 1 || len(p.Payload) > MaxPayload {
		return errors.New("Opus payload length is invalid")
	}
	if w.size+EntryHeaderSize+int64(len(p.Payload)) > MaxFileSize {
		return ErrLimit
	}
	h := make([]byte, EntryHeaderSize)
	binary.BigEndian.PutUint32(h[0:4], p.Sequence)
	binary.BigEndian.PutUint32(h[4:8], p.Timestamp)
	binary.BigEndian.PutUint32(h[8:12], p.ArrivalMS)
	binary.BigEndian.PutUint16(h[12:14], uint16(len(p.Payload)))
	if err := writeAll(w.w, h); err != nil {
		return err
	}
	if err := writeAll(w.w, p.Payload); err != nil {
		return err
	}
	w.size += EntryHeaderSize + int64(len(p.Payload))
	return nil
}
func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func (w *Writer) Size() int64 { return w.size }

type Reader struct {
	r         *bufio.Reader
	id        uuid.UUID
	remaining int64
}

func NewReader(source io.Reader, size int64) (*Reader, error) {
	if size < HeaderSize || size > MaxFileSize {
		return nil, errors.New("archive size is invalid")
	}
	h := make([]byte, HeaderSize)
	if _, err := io.ReadFull(source, h); err != nil {
		return nil, err
	}
	if string(h[:4]) != "ORAR" || binary.BigEndian.Uint16(h[4:6]) != 1 || binary.BigEndian.Uint16(h[6:8]) != HeaderSize {
		return nil, errors.New("archive header is invalid")
	}
	for _, b := range h[24:] {
		if b != 0 {
			return nil, errors.New("archive reserved bytes are not zero")
		}
	}
	id, err := uuid.FromBytes(h[8:24])
	if err != nil {
		return nil, err
	}
	return &Reader{bufio.NewReader(source), id, size - HeaderSize}, nil
}
func (r *Reader) ID() uuid.UUID { return r.id }
func (r *Reader) Next() (Packet, error) {
	if r.remaining == 0 {
		return Packet{}, io.EOF
	}
	if r.remaining < EntryHeaderSize {
		return Packet{}, io.ErrUnexpectedEOF
	}
	h := make([]byte, EntryHeaderSize)
	if _, err := io.ReadFull(r.r, h); err != nil {
		return Packet{}, err
	}
	if binary.BigEndian.Uint16(h[14:16]) != 0 {
		return Packet{}, errors.New("entry flags are not zero")
	}
	length := int(binary.BigEndian.Uint16(h[12:14]))
	if length < 1 || length > MaxPayload || int64(EntryHeaderSize+length) > r.remaining {
		return Packet{}, errors.New("entry length is invalid")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r.r, payload); err != nil {
		return Packet{}, err
	}
	r.remaining -= int64(EntryHeaderSize + length)
	return Packet{binary.BigEndian.Uint32(h[:4]), binary.BigEndian.Uint32(h[4:8]), binary.BigEndian.Uint32(h[8:12]), payload}, nil
}

type File struct {
	id                  uuid.UUID
	dir, partial, final string
	file                *os.File
	writer              *Writer
	hash                hash.Hash
	quota               int64
}

func CreateFile(dir string, id uuid.UUID, quotaRemaining int64) (*File, error) {
	if quotaRemaining < HeaderSize {
		return nil, ErrLimit
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive directory is invalid")
	}
	partial := filepath.Join(dir, id.String()+".partial")
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	writer, err := NewWriter(io.MultiWriter(file, h), id)
	if err != nil {
		file.Close()
		return nil, err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	return &File{id, dir, partial, filepath.Join(dir, id.String()+".orar"), file, writer, h, quotaRemaining}, nil
}
func (f *File) Append(p Packet) error {
	need := int64(EntryHeaderSize + len(p.Payload))
	if f.writer.Size()+need > f.quota {
		return ErrLimit
	}
	return f.writer.WritePacket(p)
}
func (f *File) Finalize() (string, int64, []byte, error) {
	if f.file == nil {
		return "", 0, nil, errors.New("archive is closed")
	}
	if err := f.file.Sync(); err != nil {
		return "", 0, nil, err
	}
	if err := f.file.Close(); err != nil {
		return "", 0, nil, err
	}
	f.file = nil
	if err := os.Rename(f.partial, f.final); err != nil {
		return "", 0, nil, err
	}
	dir, err := os.Open(f.dir)
	if err != nil {
		return "", 0, nil, err
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return "", 0, nil, err
	}
	return f.final, f.writer.Size(), f.hash.Sum(nil), nil
}
func (f *File) PartialPath() string { return f.partial }
func ValidateFile(path string) (uuid.UUID, int64, []byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return uuid.Nil, 0, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return uuid.Nil, 0, nil, errors.New("archive must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return uuid.Nil, 0, nil, err
	}
	defer file.Close()
	h := sha256.New()
	reader, err := NewReader(io.TeeReader(file, h), info.Size())
	if err != nil {
		return uuid.Nil, 0, nil, err
	}
	for {
		_, err = reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return uuid.Nil, 0, nil, fmt.Errorf("validate archive: %w", err)
		}
	}
	return reader.ID(), info.Size(), h.Sum(nil), nil
}
