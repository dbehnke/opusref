package archive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/store"
	"github.com/google/uuid"
)

type StreamKey struct {
	SessionID uint64
	StreamID  uint32
}

const (
	PartialUnknownPrefix uint32 = 1 << iota
	PartialSequenceGap
	PartialBackpressure
	PartialSyntheticEnd
	PartialMissingEnd
	PartialQuota
	PartialWriteFailure
	PartialServerShutdown
	PartialProcessRestart
	PartialTruncatedEntry
)

type commandKind uint8

const (
	commandStart commandKind = iota + 1
	commandAudio
	commandData
	commandEnd
	commandAttribute
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
	healthy   atomic.Bool
	observer  func(string, string)
}
type activeStream struct {
	key          StreamKey
	id           uuid.UUID
	node, source string
	webUserID    string
	started      time.Time
	file         *File
	packets      int64
	reasons      uint32
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
	service.healthy.Store(true)
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
func (s *Service) Attribute(key StreamKey, webUserID string) {
	if webUserID != "" {
		s.control(command{kind: commandAttribute, key: key, webUserID: webUserID})
	}
}
func (s *Service) Close() {
	s.closeOnce.Do(func() { s.control(command{kind: commandClose}); <-s.closed })
}
func (s *Service) SetObserver(observer func(action, result string)) {
	s.mu.Lock()
	s.observer = observer
	s.mu.Unlock()
}
func (s *Service) observe(action, result string) {
	s.mu.Lock()
	observer := s.observer
	s.mu.Unlock()
	if observer != nil {
		observer(action, result)
	}
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
	recent := map[StreamKey]string{}
	var recentOrder []StreamKey
	for {
		select {
		case <-ctx.Done():
			if active != nil {
				active.reasons |= PartialServerShutdown
				s.finish(active, "server_shutdown")
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
				active.reasons |= PartialBackpressure
			}
			switch item.kind {
			case commandStart:
				if active != nil {
					active.reasons |= PartialMissingEnd
					s.finish(active, "missing_end")
				}
				active = &activeStream{key: item.key, id: uuid.New(), node: item.node, source: item.source, webUserID: item.webUserID, started: item.started}
			case commandAudio, commandData:
				if active == nil || active.key != item.key {
					break
				}
				if !active.seen && item.sequence != 0 {
					active.reasons |= PartialUnknownPrefix
				}
				if active.seen && item.sequence != active.lastSequence+1 {
					active.reasons |= PartialSequenceGap
				}
				active.seen = true
				active.lastSequence = item.sequence
				if item.kind == commandAudio {
					s.write(active, item)
				}
			case commandEnd:
				if active != nil && active.key == item.key {
					if item.partial {
						active.reasons |= PartialSyntheticEnd
					}
					s.finish(active, item.reason)
					active = nil
				}
			case commandAttribute:
				if active != nil && active.key == item.key {
					active.webUserID = item.webUserID
				}
				if recordingID := recent[item.key]; recordingID != "" {
					_ = s.state.AttributeRecording(context.Background(), recordingID, item.webUserID)
					delete(recent, item.key)
				}
			case commandClose:
				if active != nil {
					active.reasons |= PartialServerShutdown
					s.finish(active, "server_shutdown")
				}
				if item.ack != nil {
					close(item.ack)
				}
				return
			}
			if item.ack != nil {
				close(item.ack)
			}
			if active != nil && active.file != nil {
				if _, exists := recent[active.key]; !exists {
					recent[active.key] = active.id.String()
					recentOrder = append(recentOrder, active.key)
					if len(recentOrder) > 64 {
						delete(recent, recentOrder[0])
						recentOrder = recentOrder[1:]
					}
				}
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
			active.reasons |= PartialQuota
			return
		}
		relative := active.id.String() + ".orar"
		if err := s.state.InsertRecording(context.Background(), active.id.String(), active.node, active.source, active.webUserID, relative, active.started); err != nil {
			active.reasons |= PartialWriteFailure
			return
		}
		file, err := CreateFile(s.directory, active.id, s.remainingQuota())
		if err != nil {
			active.reasons |= PartialWriteFailure
			return
		}
		active.file = file
		s.addUsed(HeaderSize)
		if err = s.state.OpenRecording(context.Background(), active.id.String()); err != nil {
			active.reasons |= PartialWriteFailure
			return
		}
	}
	before := active.file.writer.Size()
	offset := item.arrival.Sub(active.started).Milliseconds()
	if offset < 0 {
		offset = 0
	}
	if err := active.file.Append(Packet{Sequence: item.sequence, Timestamp: item.timestamp, ArrivalMS: uint32(offset), Payload: item.payload}); err != nil {
		if errors.Is(err, ErrLimit) {
			active.reasons |= PartialQuota
		} else {
			active.reasons |= PartialWriteFailure
		}
		return
	}
	active.packets++
	s.addUsed(active.file.writer.Size() - before)
}
func (s *Service) finish(active *activeStream, reason string) {
	if active.file == nil {
		return
	}
	status := "complete"
	if active.reasons != 0 {
		status = "partial"
	}
	if err := s.state.BeginRecordingFinalize(context.Background(), active.id.String(), status, reason, active.reasons); err != nil {
		s.observe("finalize", "failure")
		s.healthy.Store(false)
		return
	}
	_, size, sum, err := active.file.Finalize()
	if err != nil {
		s.observe("finalize", "failure")
		_ = s.state.MarkRecordingUnavailable(context.Background(), active.id.String(), "finalize_error", time.Now())
		s.healthy.Store(false)
		return
	}
	if err = s.state.FinishRecording(context.Background(), active.id.String(), status, reason, time.Now(), active.packets, size, sum); err != nil {
		s.observe("finalize", "failure")
		s.healthy.Store(false)
	} else {
		s.observe("finalize", map[bool]string{true: "partial", false: "success"}[status == "partial"])
	}
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
	seen := map[string]bool{}
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
		filenameID, filenameErr := uuid.Parse(strings.TrimSuffix(name, extension))
		var persisted store.RecordingRecoveryState
		var persistedErr error
		if filenameErr == nil {
			seen[filenameID.String()] = true
			persisted, persistedErr = s.state.RecordingRecoveryState(ctx, filenameID.String())
		}
		before, _ := os.Stat(path)
		truncated := false
		id, size, sum, validateErr := ValidateFile(path)
		if validateErr != nil && extension == ".partial" {
			if repairErr := RepairTornTrailingEntry(path); repairErr == nil {
				id, size, sum, validateErr = ValidateFile(path)
				after, _ := os.Stat(path)
				truncated = before != nil && after != nil && after.Size() < before.Size()
			}
		}
		if validateErr != nil {
			if persistedErr == nil && persisted.Status == "creating" && extension == ".partial" {
				if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
				if err = s.state.DeleteCreatingRecording(ctx, filenameID.String()); err != nil {
					return err
				}
				continue
			}
			if persistedErr == nil {
				if markErr := s.state.MarkRecordingUnavailable(ctx, filenameID.String(), "corrupt_archive", time.Now()); markErr != nil {
					return markErr
				}
			}
			if renameErr := os.Rename(path, path+".quarantine"); renameErr != nil {
				return renameErr
			}
			continue
		}
		if persistedErr != nil || filenameID != id {
			if renameErr := os.Rename(path, path+".quarantine"); renameErr != nil {
				return renameErr
			}
			continue
		}
		count, countErr := CountPackets(path)
		if countErr != nil {
			return countErr
		}
		if persisted.Status == "creating" && extension == ".partial" && count == 0 {
			if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err = s.state.DeleteCreatingRecording(ctx, id.String()); err != nil {
				return err
			}
			continue
		}
		if persisted.Status == "deleting" {
			if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return removeErr
			}
			if err = syncDirectory(s.directory); err != nil {
				return err
			}
			if err = s.state.MarkRecordingDeleted(ctx, id.String(), time.Now()); err != nil {
				return err
			}
			continue
		}
		if persisted.Status == "complete" || persisted.Status == "partial" {
			if extension == ".partial" {
				if renameErr := os.Rename(path, path+".quarantine"); renameErr != nil {
					return renameErr
				}
			}
			continue
		}
		reasons := uint32(PartialProcessRestart)
		if truncated {
			reasons |= PartialTruncatedEntry
		}
		finalStatus, finalReason, _, prepareErr := s.state.PrepareRecoveryByState(ctx, id.String(), "partial", "process_restart", reasons)
		if prepareErr != nil {
			return prepareErr
		}
		if extension == ".partial" {
			final := filepath.Join(s.directory, id.String()+".orar")
			if err = os.Rename(path, final); err != nil {
				return err
			}
			if err = syncDirectory(s.directory); err != nil {
				return err
			}
		}
		if err = s.state.FinishRecording(ctx, id.String(), finalStatus, finalReason, time.Now(), count, size, sum); err != nil {
			return err
		}
	}
	recoverable, err := s.state.RecoverableRecordingIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range recoverable {
		if !seen[id] {
			state, stateErr := s.state.RecordingRecoveryState(ctx, id)
			if stateErr != nil {
				return stateErr
			}
			if state.Status == "creating" {
				err = s.state.DeleteCreatingRecording(ctx, id)
			} else {
				err = s.state.MarkRecordingUnavailable(ctx, id, "archive_missing", time.Now())
			}
			if err != nil {
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
			s.observe("purge", "failure")
			return err
		}
	}
	if len(ids) > 0 {
		s.observe("purge", "success")
	}
	return nil
}
func (s *Service) usedBytes() int64           { s.mu.Lock(); defer s.mu.Unlock(); return s.used }
func (s *Service) Usage() (used, quota int64) { return s.usedBytes(), s.quota }
func (s *Service) QuotaFull() bool            { s.mu.Lock(); defer s.mu.Unlock(); return s.used >= s.quota }
func (s *Service) Ready() bool                { return s.healthy.Load() }
func (s *Service) Probe(ctx context.Context) bool {
	if err := s.state.Ping(ctx); err != nil {
		s.healthy.Store(false)
		return false
	}
	file, err := os.CreateTemp(s.directory, ".writable-probe-")
	if err == nil {
		_, err = file.Write([]byte{0})
		if err == nil {
			err = file.Sync()
		}
		closeErr := file.Close()
		removeErr := os.Remove(file.Name())
		if err == nil {
			err = closeErr
		}
		if err == nil {
			err = removeErr
		}
	}
	s.healthy.Store(err == nil)
	return err == nil
}
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
	return s.delete(ctx, id, "", now)
}
func (s *Service) DeleteAs(ctx context.Context, id, actor string, now time.Time) error {
	return s.delete(ctx, id, actor, now)
}
func (s *Service) delete(ctx context.Context, id, actor string, now time.Time) error {
	path, err := s.validatedPath(id)
	if err != nil {
		return err
	}
	if actor != "" {
		err = s.state.BeginRecordingDeleteAudited(ctx, id, actor, now)
	} else {
		err = s.state.BeginRecordingDelete(ctx, id)
	}
	if err != nil {
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
		s.observe("delete", "failure")
		return err
	}
	s.addUsed(-info.Size())
	s.observe("delete", "success")
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
func (s *Service) OpenPlayback(id string) (*Playback, error) {
	path, err := s.validatedPath(id)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
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
	return buildPlayback(path, info.Size())
}
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
