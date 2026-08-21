package archive

import (
	"context"
	"github.com/dbehnke/opusref/internal/webapp/store"
	"testing"
	"time"
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
	service.Start("WEB", "N0CALL", time.Now())
	service.End("unused")
	items, err := state.ListRecordings(ctx, 50)
	if err != nil || len(items) != 0 {
		t.Fatalf("unused grant recorded: %v %v", items, err)
	}
	service.Start("WEB", "N0CALL", time.Now())
	if !service.Audio(0, 0, []byte{1, 2, 3}, time.Now()) {
		t.Fatal("audio rejected")
	}
	time.Sleep(20 * time.Millisecond)
	service.End("normal")
	items, err = state.ListRecordings(ctx, 50)
	if err != nil || len(items) != 1 || items[0].Status != "complete" {
		t.Fatalf("recording=%+v err=%v", items, err)
	}
}
