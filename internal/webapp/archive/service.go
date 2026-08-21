package archive

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/store"
	"github.com/google/uuid"
)

type queuedPacket struct{ packet Packet }
type Service struct {
	state     *store.Store
	directory string
	quota     int64
	queue     chan queuedPacket
	mu        sync.Mutex
	pending   *pendingStream
	used      int64
	closed    chan struct{}
	retention time.Duration
}
type pendingStream struct {
	id           uuid.UUID
	node, source string
	started      time.Time
	file         *File
	packets      int64
	partial      bool
	reason       string
}

func NewService(ctx context.Context, state *store.Store, directory string, quota int64, queuePackets int) (*Service, error) {
	if queuePackets <= 0 {
		queuePackets = 512
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive directory is invalid")
	}
	var used int64
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		info, statErr := entry.Info()
		if statErr == nil && info.Mode().IsRegular() {
			used += info.Size()
		}
	}
	service := &Service{state: state, directory: directory, quota: quota, queue: make(chan queuedPacket, queuePackets), used: used, closed: make(chan struct{})}
	if err = service.recover(ctx); err != nil {
		return nil, err
	}
	go service.run(ctx)
	return service, nil
}
func (s *Service) recover(ctx context.Context) error {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(s.directory, name)
		extension := filepath.Ext(name)
		if extension == ".deleting" {
			if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			continue
		}
		if extension != ".partial" && extension != ".orar" {
			continue
		}
		id, size, sum, validateErr := ValidateFile(path)
		if validateErr != nil {
			if renameErr := os.Rename(path, path+".quarantine"); renameErr != nil {
				return renameErr
			}
			continue
		}
		status, statusErr := s.state.RecordingStatus(ctx, id.String())
		if statusErr != nil {
			if renameErr := os.Rename(path, path+".quarantine"); renameErr != nil {
				return renameErr
			}
			continue
		}
		if extension == ".partial" {
			final := filepath.Join(s.directory, id.String()+".orar")
			if err = os.Rename(path, final); err != nil {
				return err
			}
			if err = s.state.FinishRecording(ctx, id.String(), "partial", "process_restart", time.Now(), 0, size, sum); err != nil {
				return err
			}
		} else if status != "complete" && status != "partial" {
			if err = s.state.FinishRecording(ctx, id.String(), "partial", "process_restart", time.Now(), 0, size, sum); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Service) Purge(ctx context.Context, retention time.Duration, now time.Time) error {
	ids, err := s.state.ExpiredRecordings(ctx, now.Add(-retention))
	if err != nil {
		return err
	}
	for _, id := range ids {
		path := s.Path(id)
		deleting := filepath.Join(s.directory, id+".deleting")
		if err = os.Rename(path, deleting); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				_ = s.state.MarkRecordingDeleted(ctx, id, now)
				continue
			}
			return err
		}
		if err = os.Remove(deleting); err != nil {
			return err
		}
		if err = s.state.MarkRecordingDeleted(ctx, id, now); err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) Start(node, source string, started time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil {
		s.finishLocked("partial", "missing_end")
	}
	s.pending = &pendingStream{id: uuid.New(), node: node, source: source, started: started}
}
func (s *Service) Audio(sequence, timestamp uint32, payload []byte, arrival time.Time) bool {
	s.mu.Lock()
	active := s.pending
	s.mu.Unlock()
	if active == nil {
		return false
	}
	packet := Packet{Sequence: sequence, Timestamp: timestamp, ArrivalMS: uint32(max(0, arrival.Sub(active.started).Milliseconds())), Payload: append([]byte(nil), payload...)}
	select {
	case s.queue <- queuedPacket{packet}:
		return true
	default:
		s.mu.Lock()
		if s.pending == active {
			s.pending.partial = true
			s.pending.reason = "archive_backpressure"
		}
		s.mu.Unlock()
		return false
	}
}
func (s *Service) End(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked("complete", reason)
}
func (s *Service) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishLocked("partial", "server_shutdown")
}
func (s *Service) run(ctx context.Context) {
	defer close(s.closed)
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case item := <-s.queue:
			s.write(item.packet)
		}
	}
}
func (s *Service) write(packet Packet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	active := s.pending
	if active == nil {
		return
	}
	if active.file == nil {
		if s.used+HeaderSize+EntryHeaderSize+int64(len(packet.Payload)) > s.quota {
			active.partial = true
			active.reason = "quota_limit"
			return
		}
		relative := active.id.String() + ".orar"
		if err := s.state.InsertRecording(context.Background(), active.id.String(), active.node, active.source, relative, active.started); err != nil {
			active.partial = true
			active.reason = "database_error"
			return
		}
		file, err := CreateFile(s.directory, active.id, s.quota-s.used)
		if err != nil {
			active.partial = true
			active.reason = "archive_create"
			return
		}
		active.file = file
		if err = s.state.OpenRecording(context.Background(), active.id.String()); err != nil {
			active.partial = true
			active.reason = "database_error"
			return
		}
	}
	before := active.file.writer.Size()
	if err := active.file.Append(packet); err != nil {
		active.partial = true
		if errors.Is(err, ErrLimit) {
			active.reason = "file_or_quota_limit"
		} else {
			active.reason = "archive_write"
		}
		return
	}
	active.packets++
	s.used += active.file.writer.Size() - before
}
func (s *Service) finishLocked(status, reason string) {
	active := s.pending
	if active == nil {
		return
	}
	s.pending = nil
	if active.file == nil {
		return
	}
	if active.partial {
		status = "partial"
		if active.reason != "" {
			reason = active.reason
		}
	}
	_, size, sum, err := active.file.Finalize()
	if err != nil {
		status = "partial"
		reason = "finalize_error"
	}
	_ = s.state.FinishRecording(context.Background(), active.id.String(), status, reason, time.Now(), active.packets, size, sum)
}
func (s *Service) QuotaFull() bool       { s.mu.Lock(); defer s.mu.Unlock(); return s.used >= s.quota }
func (s *Service) Path(id string) string { return filepath.Join(s.directory, id+".orar") }
func (s *Service) Delete(ctx context.Context, id string, now time.Time) error {
	path := s.Path(id)
	deleting := filepath.Join(s.directory, id+".deleting")
	if err := os.Rename(path, deleting); err != nil {
		return err
	}
	dir, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	if err = dir.Sync(); err != nil {
		dir.Close()
		return err
	}
	dir.Close()
	if err = os.Remove(deleting); err != nil {
		return err
	}
	dir, err = os.Open(s.directory)
	if err != nil {
		return err
	}
	err = dir.Sync()
	dir.Close()
	if err != nil {
		return err
	}
	return s.state.MarkRecordingDeleted(ctx, id, now)
}
func (s *Service) ReadPackets(id string) ([]Packet, error) {
	path := s.Path(id)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive file is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader, err := NewReader(file, info.Size())
	if err != nil {
		return nil, err
	}
	var packets []Packet
	for {
		packet, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			return packets, nil
		}
		if nextErr != nil {
			return nil, nextErr
		}
		packets = append(packets, packet)
	}
}
