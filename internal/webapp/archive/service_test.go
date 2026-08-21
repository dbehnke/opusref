package archive

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/store"
	"github.com/google/uuid"
)

func TestServiceCreatesOnlyOnFirstAudioAndListsFinal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service, err := NewService(ctx, state, t.TempDir(), 1024*1024, 8)
	if err != nil {
		t.Fatal(err)
	}
	key := StreamKey{SessionID: 1, StreamID: 1}
	service.Start(key, "WEB", "N0CALL", "", time.Now())
	service.End(key, "unused", false)
	items, err := state.ListRecordings(ctx, 50)
	if err != nil || len(items) != 0 {
		t.Fatalf("unused grant recorded: %v %v", items, err)
	}
	service.Start(key, "WEB", "N0CALL", "", time.Now())
	if !service.Audio(key, 0, 960, []byte{1, 2, 3}, time.Now()) {
		t.Fatal("audio rejected")
	}
	if !service.Audio(key, 1, 1920, []byte{4}, time.Now()) {
		t.Fatal("second audio rejected")
	}
	service.End(key, "normal", false)
	items, err = state.ListRecordings(ctx, 50)
	if err != nil || len(items) != 1 || items[0].Status != "complete" || items[0].FirstSequence == nil || *items[0].FirstSequence != 0 || items[0].LastSequence == nil || *items[0].LastSequence != 1 || items[0].FirstTimestamp == nil || *items[0].FirstTimestamp != 960 || items[0].LastTimestamp == nil || *items[0].LastTimestamp != 1920 {
		t.Fatalf("recording=%+v err=%v", items, err)
	}
}

func TestRecoveryRepairsOnlyTornTrailingEntryAndFinalizesPartial(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.New()
	if err = state.InsertRecording(ctx, id.String(), "WEB", "N0CALL", "", id.String()+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(directory, id.String()+".partial")
	file, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := NewWriter(file, id)
	if err = writer.WritePacket(Packet{Payload: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte{0, 1, 2}); err != nil {
		t.Fatal(err)
	}
	_ = file.Sync()
	_ = file.Close()
	service, err := NewService(ctx, state, directory, MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var recoveryAlerts int
	service.SetObserver(func(action, result string) {
		if action == "recover" && result == "partial" {
			recoveryAlerts++
		}
	})
	recording, err := state.RecordingByID(ctx, id.String())
	if err != nil || recording.Status != "partial" || recording.PartialReasons&PartialProcessRestart == 0 || recording.PartialReasons&PartialTruncatedEntry == 0 || recording.PacketCount != 1 || recording.FirstSequence == nil || *recording.FirstSequence != 0 || recoveryAlerts != 1 {
		t.Fatalf("recording=%+v err=%v", recording, err)
	}
	if _, err = os.Stat(filepath.Join(directory, id.String()+".orar")); err != nil {
		t.Fatal(err)
	}
}

func TestQuotaRemainderStopsNewArchiveAndRaisesTransition(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service, err := NewService(ctx, state, t.TempDir(), int64(HeaderSize+EntryHeaderSize+2), 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var full int
	service.SetObserver(func(action, result string) {
		if action == "quota" && result == "full" {
			full++
		}
	})
	key := StreamKey{SessionID: 4, StreamID: 5}
	service.Start(key, "WEB", "N0CALL", "", time.Now())
	service.Audio(key, 0, 0, []byte{1, 2, 3}, time.Now())
	service.End(key, "normal", false)
	if !service.QuotaFull() || !service.Ready() || full != 1 {
		t.Fatalf("quota_full=%v ready=%v alerts=%d", service.QuotaFull(), service.Ready(), full)
	}
	items, listErr := state.ListRecordings(ctx, 10)
	if listErr != nil || len(items) != 0 {
		t.Fatalf("recordings=%d err=%v", len(items), listErr)
	}
}

func TestRecoveryRemovesMissingCreatingRow(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.NewString()
	if err = state.InsertRecording(ctx, id, "WEB", "N0CALL", "", id+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ctx, state, t.TempDir(), MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var count int
	err = state.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM recordings WHERE id=?", id).Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestRecoveryPreservesFinalizingIntentAfterRename(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.New()
	if err = state.InsertRecording(ctx, id.String(), "WEB", "N0CALL", "", id.String()+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = state.OpenRecording(ctx, id.String()); err != nil {
		t.Fatal(err)
	}
	file, err := CreateFile(directory, id, MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Append(Packet{Payload: []byte{7}})
	if err = state.BeginRecordingFinalize(ctx, id.String(), "complete", "normal", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = file.Finalize(); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ctx, state, directory, MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	recording, err := state.RecordingByID(ctx, id.String())
	if err != nil || recording.Status != "complete" || recording.EndReason != "normal" || recording.PartialReasons&PartialProcessRestart == 0 {
		t.Fatalf("recording=%+v err=%v", recording, err)
	}
}

func TestRecoveryTurnsAlreadyRenamedOpenFilePartial(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.New()
	if err = state.InsertRecording(ctx, id.String(), "WEB", "N0CALL", "", id.String()+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = state.OpenRecording(ctx, id.String()); err != nil {
		t.Fatal(err)
	}
	file, err := CreateFile(directory, id, MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Append(Packet{Payload: []byte{1}})
	if _, _, _, err = file.Finalize(); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ctx, state, directory, MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	recording, err := state.RecordingByID(ctx, id.String())
	if err != nil || recording.Status != "partial" || recording.EndReason != "process_restart" || recording.PartialReasons&PartialProcessRestart == 0 {
		t.Fatalf("recording=%+v err=%v", recording, err)
	}
}

func TestRecoveryBackfillsFinalArchivePacketBounds(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.New()
	if err = state.InsertRecording(ctx, id.String(), "WEB", "N0CALL", "", id.String()+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = state.OpenRecording(ctx, id.String()); err != nil {
		t.Fatal(err)
	}
	file, err := CreateFile(directory, id, MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Append(Packet{Sequence: 9, Timestamp: 960, Payload: []byte{1}})
	_ = file.Append(Packet{Sequence: 10, Timestamp: 1920, Payload: []byte{2}})
	if err = state.BeginRecordingFinalize(ctx, id.String(), "complete", "normal", 0); err != nil {
		t.Fatal(err)
	}
	_, size, sum, err := file.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if err = state.FinishRecording(ctx, id.String(), "complete", "normal", time.Now(), 2, size, sum); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ctx, state, directory, MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	recording, err := state.RecordingByID(ctx, id.String())
	if err != nil || recording.FirstSequence == nil || *recording.FirstSequence != 9 || recording.LastTimestamp == nil || *recording.LastTimestamp != 1920 {
		t.Fatalf("recording=%+v err=%v", recording, err)
	}
}

func TestRecoveryMarksMissingOpenArchiveUnavailable(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.NewString()
	if err = state.InsertRecording(ctx, id, "WEB", "N0CALL", "", id+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = state.OpenRecording(ctx, id); err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ctx, state, t.TempDir(), MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var failures int
	service.SetObserver(func(action, result string) {
		if action == "recover" && result == "failure" {
			failures++
		}
	})
	stateValue, err := state.RecordingRecoveryState(ctx, id)
	if err != nil || stateValue.Status != "unavailable" || stateValue.EndReason != "archive_missing" || failures != 1 {
		t.Fatalf("state=%+v err=%v", stateValue, err)
	}
}

func TestRecoveryRemovesHeaderOnlyCreatingPartial(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	id := uuid.New()
	if err = state.InsertRecording(ctx, id.String(), "WEB", "N0CALL", "", id.String()+".orar", time.Now()); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(directory, id.String()+".partial"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = NewWriter(file, id)
	_ = file.Sync()
	_ = file.Close()
	service, err := NewService(ctx, state, directory, MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	var count int
	_ = state.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM recordings WHERE id=?", id.String()).Scan(&count)
	if count != 0 {
		t.Fatalf("creating row remains: %d", count)
	}
}

func TestServiceKeepsBackToBackStreamsSeparate(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service, err := NewService(ctx, state, t.TempDir(), 1024*1024, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()

	for index, key := range []StreamKey{{SessionID: 10, StreamID: 1}, {SessionID: 20, StreamID: 2}} {
		service.Start(key, "WEB", "N0CALL", "", time.Now())
		if !service.Audio(key, 0, 0, []byte{byte(index + 1)}, time.Now()) {
			t.Fatal("audio rejected")
		}
		service.End(key, "normal", false)
	}
	items, err := state.ListRecordings(ctx, 50)
	if err != nil || len(items) != 2 {
		t.Fatalf("recordings=%+v err=%v", items, err)
	}
	for _, item := range items {
		packets, readErr := service.ReadPackets(item.ID)
		if readErr != nil || len(packets) != 1 {
			t.Fatalf("packets=%+v err=%v", packets, readErr)
		}
	}
}

func TestLateExactGrantBindingUpdatesReceiveBeforeGrantRecording(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service, err := NewService(ctx, state, t.TempDir(), MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	key := StreamKey{SessionID: 77, StreamID: 9}
	service.Start(key, "WEB", "N0CALL", "", time.Now())
	service.Audio(key, 0, 0, []byte{1}, time.Now())
	service.Attribute(key, "user-1")
	service.End(key, "normal", false)
	var userID string
	err = state.DB().QueryRowContext(ctx, `SELECT web_user_id FROM recordings`).Scan(&userID)
	if err != nil || userID != "user-1" {
		t.Fatalf("web_user_id=%q err=%v", userID, err)
	}
}

func TestServiceRejectsEscapingRecordingID(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service, err := NewService(ctx, state, t.TempDir(), 1024*1024, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	if _, err = service.ReadPackets("../escape"); err == nil {
		t.Fatal("ReadPackets accepted an escaping ID")
	}
	if err = service.Delete(ctx, "../escape", time.Now()); err == nil {
		t.Fatal("Delete accepted an escaping ID")
	}
}

func TestServiceMarksSequenceGapPartialWithoutDelay(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service, err := NewService(ctx, state, t.TempDir(), 1024*1024, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	key := StreamKey{SessionID: 1, StreamID: 9}
	service.Start(key, "WEB", "N0CALL", "", time.Now())
	service.Audio(key, 0, 0, []byte{1}, time.Now())
	service.Data(key, 2)
	service.Audio(key, 3, 960, []byte{2}, time.Now())
	service.End(key, "normal", false)
	items, err := state.ListRecordings(ctx, 10)
	if err != nil || len(items) != 1 || items[0].Status != "partial" || items[0].EndReason != "normal" || items[0].PartialReasons&PartialSequenceGap == 0 {
		t.Fatalf("recordings=%+v err=%v", items, err)
	}
}

func TestServiceRejectsPlaybackAfterArchiveCorruption(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	service, err := NewService(ctx, state, t.TempDir(), 1024*1024, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	key := StreamKey{SessionID: 7, StreamID: 8}
	service.Start(key, "WEB", "N0CALL", "", time.Now())
	service.Audio(key, 0, 0, []byte{1, 2, 3}, time.Now())
	service.End(key, "normal", false)
	items, _ := state.ListRecordings(ctx, 1)
	file, err := os.OpenFile(service.Path(items[0].ID), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteAt([]byte{9}, HeaderSize+EntryHeaderSize); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, err = service.ReadPackets(items[0].ID); err == nil {
		t.Fatal("corrupt archive accepted")
	}
}

func TestQuotaFullKeepsRetainedRecordingAndClearsAfterDelete(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	quota := int64(HeaderSize + EntryHeaderSize + 3)
	service, err := NewService(ctx, state, t.TempDir(), quota, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	key := StreamKey{SessionID: 4, StreamID: 5}
	service.Start(key, "WEB", "N0CALL", "", time.Now())
	service.Audio(key, 0, 0, []byte{1, 2, 3}, time.Now())
	service.End(key, "normal", false)
	items, _ := state.ListRecordings(ctx, 1)
	if len(items) != 1 || !service.QuotaFull() || !service.Ready() {
		t.Fatalf("items=%v full=%v ready=%v", len(items), service.QuotaFull(), service.Ready())
	}
	if err = service.Purge(ctx, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(service.Path(items[0].ID)); err != nil {
		t.Fatalf("retained recording was purged: %v", err)
	}
	if err = service.Delete(ctx, items[0].ID, time.Now()); err != nil || service.QuotaFull() {
		t.Fatalf("delete=%v full=%v", err, service.QuotaFull())
	}
}

func TestDefaultTenGiBQuotaIsSimulatedWithoutAllocation(t *testing.T) {
	state, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	const quota = int64(10 * 1024 * 1024 * 1024)
	service, err := NewService(context.Background(), state, t.TempDir(), quota, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	used, configured := service.Usage()
	if used != 0 || configured != quota {
		t.Fatalf("used=%d quota=%d", used, configured)
	}
}

func TestSparsePlaybackStreamsMoreThan4096PacketsAndSeeks(t *testing.T) {
	ctx := context.Background()
	state, err := store.Open(ctx, t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	directory := t.TempDir()
	service, err := NewService(ctx, state, directory, MaxFileSize, 8)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	id := uuid.New()
	started := time.Now()
	if err = state.InsertRecording(ctx, id.String(), "WEB", "N0CALL", "", id.String()+".orar", started); err != nil {
		t.Fatal(err)
	}
	file, err := CreateFile(directory, id, MaxFileSize)
	if err != nil {
		t.Fatal(err)
	}
	if err = state.OpenRecording(ctx, id.String()); err != nil {
		t.Fatal(err)
	}
	for index := uint32(0); index < 5000; index++ {
		if err = file.Append(Packet{Sequence: index, Timestamp: index * 960, ArrivalMS: index * 20, Payload: []byte{1}}); err != nil {
			t.Fatal(err)
		}
	}
	if err = state.BeginRecordingFinalize(ctx, id.String(), "complete", "normal", 0); err != nil {
		t.Fatal(err)
	}
	_, size, sum, err := file.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	if err = state.FinishRecording(ctx, id.String(), "complete", "normal", time.Now(), 5000, size, sum); err != nil {
		t.Fatal(err)
	}
	playback, err := service.OpenPlayback(id.String())
	if err != nil || playback.PacketCount() != 5000 {
		t.Fatalf("playback=%+v err=%v", playback, err)
	}
	cursor, err := playback.NewCursor(80_000)
	if err != nil {
		t.Fatal(err)
	}
	defer cursor.Close()
	packet, err := cursor.Next()
	if err != nil || packet.ArrivalMS > 80_000 || packet.ArrivalMS < 79_980 {
		t.Fatalf("packet=%+v err=%v", packet, err)
	}
	indexed, err := playback.NewCursorAt(4321)
	if err != nil {
		t.Fatal(err)
	}
	defer indexed.Close()
	packet, err = indexed.Next()
	if err != nil || packet.Sequence != 4321 || indexed.Index() != 4322 {
		t.Fatalf("indexed packet=%+v index=%d err=%v", packet, indexed.Index(), err)
	}
}
