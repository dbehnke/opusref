package archive

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/store"
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
	if !service.Audio(key, 0, 0, []byte{1, 2, 3}, time.Now()) {
		t.Fatal("audio rejected")
	}
	service.End(key, "normal", false)
	items, err = state.ListRecordings(ctx, 50)
	if err != nil || len(items) != 1 || items[0].Status != "complete" {
		t.Fatalf("recording=%+v err=%v", items, err)
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
	if err != nil || len(items) != 1 || items[0].Status != "partial" || items[0].EndReason != "sequence_gap" {
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
