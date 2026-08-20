package server

import (
	"testing"
	"time"
)

func TestFloorLifecycleAndOpaqueFanout(t *testing.T) {
	now := time.Unix(100, 0)
	e := NewEngine(Limits{}, func() time.Time { return now })
	e.AddSession(1, "a", "N0ONE", true)
	e.AddSession(2, "b", "N0TWO", true)
	if got := e.RequestFloor(1, 7, "N0ONE"); got != FloorGranted {
		t.Fatalf("got %v", got)
	}
	if got := e.RequestFloor(2, 8, "N0TWO"); got != FloorBusy {
		t.Fatalf("got %v", got)
	}
	payload := []byte{1, 2, 3}
	effects, err := e.Media(1, "a", 7, 0, 48000, payload)
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = 9
	if len(effects) != 1 || effects[0].SessionID != 2 || effects[0].Payload[0] != 1 {
		t.Fatalf("bad fanout: %#v", effects)
	}
	now = now.Add(time.Second + time.Nanosecond)
	ended := e.Tick()
	if ended == nil || ended.Reason != EndMediaInactivity || ended.Sequence != 1 || ended.Timestamp != 48000 || e.Snapshot().Floor.Active {
		t.Fatalf("bad release: %#v", ended)
	}
}

func TestSequenceGapModuloRules(t *testing.T) {
	e := NewEngine(Limits{}, time.Now)
	e.AddSession(1, "a", "N0ONE", true)
	if e.RequestFloor(1, 3, "N0ONE") != FloorGranted {
		t.Fatal("grant")
	}
	for _, seq := range []uint32{0, 2, 1, 3} {
		_, _ = e.Media(1, "a", 3, seq, 0, []byte{1})
	}
	if got := e.Snapshot().SequenceGaps; got != 1 {
		t.Fatalf("got %d gaps, want 1", got)
	}
}
func TestSequenceWrapAfterZeroOrigin(t *testing.T) {
	e := NewEngine(Limits{}, time.Now)
	e.AddSession(1, "a", "N", true)
	e.RequestFloor(1, 1, "N")
	_, _ = e.Media(1, "a", 1, 0, 0, []byte{1})
	e.floor.prior = ^uint32(0)
	if _, err := e.Media(1, "a", 1, 0, 0, []byte{1}); err != nil || e.Snapshot().SequenceGaps != 0 {
		t.Fatalf("wrap: %v gaps=%d", err, e.Snapshot().SequenceGaps)
	}
}
func TestFirstMediaSequenceMustBeZero(t *testing.T) {
	e := NewEngine(Limits{}, time.Now)
	e.AddSession(1, "a", "N0ONE", true)
	e.RequestFloor(1, 3, "N0ONE")
	if _, err := e.Media(1, "a", 3, 1, 0, []byte{1}); err != ErrInvalidStream {
		t.Fatalf("got %v", err)
	}
	if _, err := e.Media(1, "a", 3, 0, 0, []byte{1}); err != nil {
		t.Fatal(err)
	}
}
func TestFloorSnapshotReportsMediaTimesAndRemainingTOT(t *testing.T) {
	now := time.Unix(100, 0)
	e := NewEngine(Limits{TransmitTimeLimit: 10 * time.Second}, func() time.Time { return now })
	e.AddSession(1, "a", "N0ONE", true)
	e.RequestFloor(1, 1, "N0ONE")
	_, _ = e.Media(1, "a", 1, 0, 0, []byte{1})
	now = now.Add(3 * time.Second)
	s := e.Snapshot().Floor
	if s.StartedAt != time.Unix(100, 0) || s.LastFrameAt != time.Unix(100, 0) || s.RemainingTransmitTime != 7*time.Second {
		t.Fatalf("snapshot: %#v", s)
	}
}

func TestSessionAddressAndExpiry(t *testing.T) {
	now := time.Unix(100, 0)
	e := NewEngine(Limits{}, func() time.Time { return now })
	e.AddSession(1, "a", "N0ONE", true)
	if _, err := e.Media(1, "wrong", 1, 0, 0, []byte{1}); err != ErrAddressMismatch {
		t.Fatalf("got %v", err)
	}
	now = now.Add(31 * time.Second)
	e.Tick()
	if e.Snapshot().Sessions != 0 {
		t.Fatal("session did not expire")
	}
}

func TestTransactionConflictAndRetention(t *testing.T) {
	e := NewEngine(Limits{MaxCompletedTransactions: 1}, time.Now)
	key := TransactionKey{SessionID: 1, Type: 5, ID: 9}
	if got, state := e.Transaction(key, []byte("a"), []byte("one")); state != TransactionStored || string(got) != "one" {
		t.Fatal(state)
	}
	if got, state := e.Transaction(key, []byte("a"), nil); state != TransactionDuplicate || string(got) != "one" {
		t.Fatal(state)
	}
	if _, state := e.Transaction(key, []byte("b"), nil); state != TransactionConflict {
		t.Fatal(state)
	}
	if _, state := e.Transaction(TransactionKey{ID: 2}, []byte("x"), nil); state != TransactionOverloaded {
		t.Fatal(state)
	}
}

func TestShutdownRejectsNewWorkAndReleasesFloor(t *testing.T) {
	e := NewEngine(Limits{}, time.Now)
	e.AddSession(1, "a", "N0ONE", true)
	e.RequestFloor(1, 1, "N0ONE")
	e.BeginShutdown()
	if e.RequestFloor(1, 2, "N0ONE") != FloorRejected {
		t.Fatal("accepted during shutdown")
	}
	if e.Snapshot().Floor.Active || e.Snapshot().Ready {
		t.Fatal("bad shutdown snapshot")
	}
}
