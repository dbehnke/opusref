package archive

import (
	"bytes"
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

type StreamKey struct {
	SessionID uint64
	StreamID  uint32
}
type commandKind uint8

const (
	commandStart commandKind = iota + 1
	commandAudio
	commandData
	commandEnd
	commandClose
)

type command struct {
	kind                 commandKind
	key                  StreamKey
	node, source, reason string
	webUserID            string
	started, arrival     time.Time
	sequence, timestamp  uint32
	payload              []byte
	partial              bool
	ack                  chan struct{}
}
type Service struct {
	state     *store.Store
	directory string
	quota     int64
	commands  chan command
	mu        sync.Mutex
	used      int64
	dropped   map[StreamKey]bool
	closed    chan struct{}
	closeOnce sync.Once
}
type activeStream struct {
	key          StreamKey
	id           uuid.UUID
	node, source string
	webUserID    string
	started      time.Time
	file         *File
	packets      int64
	partial      bool
	reason       string
	seen         bool
	lastSequence uint32
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
	service := &Service{state: state, directory: directory, quota: quota, commands: make(chan command, queuePackets+4), dropped: map[StreamKey]bool{}, closed: make(chan struct{})}
	if err = service.recover(ctx); err != nil {
		return nil, err
	}
	if err = service.recount(); err != nil {
		return nil, err
	}
	go service.run(ctx)
	return service, nil
}
func (s *Service) Start(key StreamKey, node, source, webUserID string, started time.Time) {
	s.control(command{kind: commandStart, key: key, node: node, source: source, webUserID: webUserID, started: started})
}
func (s *Service) Audio(key StreamKey, sequence, timestamp uint32, payload []byte, arrival time.Time) bool {
	return s.media(command{kind: commandAudio, key: key, sequence: sequence, timestamp: timestamp, payload: append([]byte(nil), payload...), arrival: arrival})
}
func (s *Service) Data(key StreamKey, sequence uint32) bool {
	return s.media(command{kind: commandData, key: key, sequence: sequence})
}
func (s *Service) End(key StreamKey, reason string, partial bool) {
	s.control(command{kind: commandEnd, key: key, reason: reason, partial: partial})
}
func (s *Service) Close() {
	s.closeOnce.Do(func() { s.control(command{kind: commandClose}); <-s.closed })
}
func (s *Service) media(item command) bool {
	select {
	case s.commands <- item:
		return true
	default:
		s.mu.Lock()
		s.dropped[item.key] = true
		s.mu.Unlock()
		return false
	}
}
func (s *Service) control(item command) {
	item.ack = make(chan struct{})
	select {
	case s.commands <- item:
		select {
		case <-item.ack:
		case <-s.closed:
		}
	case <-s.closed:
	}
}
func (s *Service) run(ctx context.Context) {
	defer close(s.closed)
	var active *activeStream
	for {
		select {
		case <-ctx.Done():
			if active != nil {
				s.finish(active, "partial", "server_shutdown")
			}
			return
		case item := <-s.commands:
			s.mu.Lock()
			dropped := s.dropped[item.key]
			if dropped {
				delete(s.dropped, item.key)
			}
			s.mu.Unlock()
			if active != nil && dropped {
				active.partial = true
				active.reason = "archive_backpressure"
			}
			switch item.kind {
			case commandStart:
				if active != nil {
					s.finish(active, "partial", "missing_end")
				}
				active = &activeStream{key: item.key, id: uuid.New(), node: item.node, source: item.source, webUserID: item.webUserID, started: item.started}
			case commandAudio, commandData:
				if active == nil || active.key != item.key {
					break
				}
				if !active.seen && item.sequence != 0 {
					active.partial = true
					active.reason = "unknown_prefix"
				}
				if active.seen && item.sequence != active.lastSequence+1 {
					active.partial = true
					active.reason = "sequence_gap"
				}
				active.seen = true
				active.lastSequence = item.sequence
				if item.kind == commandAudio {
					s.write(active, item)
				}
			case commandEnd:
				if active != nil && active.key == item.key {
					status := "complete"
					if item.partial || active.partial {
						status = "partial"
					}
					reason := item.reason
					if active.reason != "" {
						reason = active.reason
					}
					s.finish(active, status, reason)
					active = nil
				}
			case commandClose:
				if active != nil {
					s.finish(active, "partial", "server_shutdown")
				}
				if item.ack != nil {
					close(item.ack)
				}
				return
			}
			if item.ack != nil {
				close(item.ack)
			}
		}
	}
}
func (s *Service) write(active *activeStream, item command) {
	if active.file == nil {
		need := int64(HeaderSize + EntryHeaderSize + len(item.payload))
		s.mu.Lock()
		available := s.used+need <= s.quota
		s.mu.Unlock()
		if !available {
			active.partial = true
			active.reason = "quota_limit"
			return
		}
		relative := active.id.String() + ".orar"
		if err := s.state.InsertRecording(context.Background(), active.id.String(), active.node, active.source, active.webUserID, relative, active.started); err != nil {
			active.partial = true
			active.reason = "database_error"
			return
		}
		file, err := CreateFile(s.directory, active.id, s.remainingQuota())
		if err != nil {
			active.partial = true
			active.reason = "archive_create"
			return
		}
		active.file = file
		s.addUsed(HeaderSize)
		if err = s.state.OpenRecording(context.Background(), active.id.String()); err != nil {
			active.partial = true
			active.reason = "database_error"
			return
		}
	}
	before := active.file.writer.Size()
	offset := item.arrival.Sub(active.started).Milliseconds()
	if offset < 0 {
		offset = 0
	}
	if err := active.file.Append(Packet{Sequence: item.sequence, Timestamp: item.timestamp, ArrivalMS: uint32(offset), Payload: item.payload}); err != nil {
		active.partial = true
		if errors.Is(err, ErrLimit) {
			active.reason = "file_or_quota_limit"
		} else {
			active.reason = "archive_write"
		}
		return
	}
	active.packets++
	s.addUsed(active.file.writer.Size() - before)
}
func (s *Service) finish(active *activeStream, status, reason string) {
	if active.file == nil {
		return
	}
	_, size, sum, err := active.file.Finalize()
	if err != nil {
		status = "partial"
		reason = "finalize_error"
	}
	_ = s.state.FinishRecording(context.Background(), active.id.String(), status, reason, time.Now(), active.packets, size, sum)
}
func (s *Service) remainingQuota() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.quota - s.used }
func (s *Service) addUsed(value int64)   { s.mu.Lock(); s.used += value; s.mu.Unlock() }
func (s *Service) recount() error {
	entries, err := os.ReadDir(s.directory)
	if err != nil {
		return err
	}
	var used int64
	for _, entry := range entries {
		info, statErr := entry.Info()
		if statErr == nil && info.Mode().IsRegular() {
			used += info.Size()
		}
	}
	s.mu.Lock()
	s.used = used
	s.mu.Unlock()
	return nil
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
			id, parseErr := uuid.Parse(name[:len(name)-len(extension)])
			if parseErr != nil {
				_ = os.Rename(path, path+".quarantine")
				continue
			}
			if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err = s.state.MarkRecordingDeleted(ctx, id.String(), time.Now()); err != nil {
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
	deleting, err := s.state.DeletingRecordings(ctx)
	if err != nil {
		return err
	}
	for _, id := range deleting {
		path, pathErr := s.validatedPath(id)
		if pathErr != nil {
			return pathErr
		}
		for _, candidate := range []string{path, filepath.Join(s.directory, id+".deleting")} {
			if removeErr := os.Remove(candidate); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
		}
		if err = s.state.MarkRecordingDeleted(ctx, id, time.Now()); err != nil {
			return err
		}
	}
	return syncDirectory(s.directory)
}
func (s *Service) Purge(ctx context.Context, retention time.Duration, now time.Time) error {
	ids, err := s.state.ExpiredRecordings(ctx, now.Add(-retention))
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.Delete(ctx, id, now); err != nil {
			return err
		}
	}
	return s.enforceQuota(ctx, now)
}
func (s *Service) enforceQuota(ctx context.Context, now time.Time) error {
	target := s.quota * 9 / 10
	for s.usedBytes() > target {
		ids, err := s.state.OldestRecordings(ctx, 100)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		for _, id := range ids {
			if err = s.Delete(ctx, id, now); err != nil {
				return err
			}
			if s.usedBytes() <= target {
				return nil
			}
		}
	}
	return nil
}
func (s *Service) usedBytes() int64           { s.mu.Lock(); defer s.mu.Unlock(); return s.used }
func (s *Service) Usage() (used, quota int64) { return s.usedBytes(), s.quota }
func (s *Service) QuotaFull() bool            { s.mu.Lock(); defer s.mu.Unlock(); return s.used >= s.quota }
func (s *Service) Ready() bool                { return !s.QuotaFull() }
func (s *Service) validatedPath(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.String() != id {
		return "", errors.New("recording ID is invalid")
	}
	path := filepath.Join(s.directory, id+".orar")
	relative, err := filepath.Rel(s.directory, path)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		return "", errors.New("recording path escapes archive directory")
	}
	return path, nil
}
func (s *Service) Path(id string) string { path, _ := s.validatedPath(id); return path }
func (s *Service) Delete(ctx context.Context, id string, now time.Time) error {
	path, err := s.validatedPath(id)
	if err != nil {
		return err
	}
	if err = s.state.BeginRecordingDelete(ctx, id); err != nil {
		return err
	}
	deleting := filepath.Join(s.directory, id+".deleting")
	if err = os.Rename(path, deleting); err != nil {
		return err
	}
	if err = syncDirectory(s.directory); err != nil {
		return err
	}
	info, err := os.Stat(deleting)
	if err != nil {
		return err
	}
	if err = os.Remove(deleting); err != nil {
		return err
	}
	if err = syncDirectory(s.directory); err != nil {
		return err
	}
	if err = s.state.MarkRecordingDeleted(ctx, id, now); err != nil {
		return err
	}
	s.addUsed(-info.Size())
	return nil
}
func (s *Service) ReadPackets(id string) ([]Packet, error) {
	path, err := s.validatedPath(id)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("archive file is invalid")
	}
	recording, err := s.state.RecordingByID(context.Background(), id)
	if err != nil || recording.RelativePath != id+".orar" || recording.ByteSize != info.Size() {
		return nil, errors.New("archive metadata does not match the file")
	}
	_, _, checksum, err := ValidateFile(path)
	if err != nil || !bytes.Equal(checksum, recording.SHA256) {
		return nil, errors.New("archive checksum is invalid")
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
	var bytesUsed int
	for {
		packet, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			return packets, nil
		}
		if nextErr != nil {
			return nil, nextErr
		}
		bytesUsed += len(packet.Payload) + EntryHeaderSize
		if len(packets) >= 4096 || bytesUsed > 1024*1024 {
			return nil, errors.New("playback index exceeds memory bound")
		}
		packets = append(packets, packet)
	}
}
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
