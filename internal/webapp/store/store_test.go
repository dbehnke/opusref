package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/auth"
)

func TestAccountAndSessionLifecycle(t *testing.T) {
	s, err := Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hash, err := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.CreateUser(context.Background(), CreateUser{Username: "alice", Role: RoleUser, Callsign: "N0CALL", PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateUser(context.Background(), CreateUser{Username: "operator", Role: RoleAdmin, PasswordHash: hash}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.EnabledAdminCount(context.Background()); err != nil || n != 1 {
		t.Fatalf("admins=%d err=%v", n, err)
	}
	raw, csrf, session, err := s.CreateSession(context.Background(), u.ID, time.Now(), 12*time.Hour, 7*24*time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 43 || len(csrf) != 43 {
		t.Fatalf("unexpected token lengths")
	}
	got, err := s.AuthenticateSession(context.Background(), raw, time.Now())
	if err != nil || got.ID != session.ID {
		t.Fatalf("session=%+v err=%v", got, err)
	}
	if err := s.DisableUser(context.Background(), u.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthenticateSession(context.Background(), raw, time.Now()); err == nil {
		t.Fatal("disabled user's session remained valid")
	}
}

func TestMigrationCreatesRestrictedBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	state, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = state.DB().Exec("DELETE FROM schema_migrations WHERE version>=2"); err != nil {
		t.Fatal(err)
	}
	_ = state.Close()
	state, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	backups, err := filepath.Glob(path + ".pre-v1-*.bak")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
	info, err := os.Stat(backups[0])
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("backup mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestOpenRejectsSecondProcessOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := Open(context.Background(), path); err == nil {
		second.Close()
		t.Fatal("second database owner was accepted")
	}
}

func TestPasskeyRegressionStatePersists(t *testing.T) {
	state, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	hash, _ := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	user, _ := state.CreateUser(context.Background(), CreateUser{Username: "alice", Role: RoleUser, PasswordHash: hash})
	id := []byte{1, 2, 3}
	if err = state.SaveWebAuthnCredential(context.Background(), user.ID, "example.test", "key", id, []byte(`{}`), 4, "", false, false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = state.MarkWebAuthnCredentialSuspected(context.Background(), user.ID, id, time.Now()); err != nil {
		t.Fatal(err)
	}
	items, err := state.ListPasskeys(context.Background(), user.ID)
	if err != nil || len(items) != 1 || !items[0].Suspected {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}

func TestTombstonePurgePreservesRecordingAttribution(t *testing.T) {
	state, err := Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	old := time.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err = state.DB().Exec(`INSERT INTO user_tombstones(user_id,username,deleted_at) VALUES('used','a',?),('unused','b',?)`, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err = state.DB().Exec(`INSERT INTO recordings(id,node_callsign,source_callsign,web_user_id,start_at,status,relative_path,created_at) VALUES('r','WEB','N0CALL','used',?,'complete','r.orar',?)`, old, old); err != nil {
		t.Fatal(err)
	}
	if count, err := state.PurgeTombstones(context.Background(), time.Now().Add(-30*24*time.Hour)); err != nil || count != 1 {
		t.Fatalf("count=%d err=%v", count, err)
	}
	var remaining int
	_ = state.DB().QueryRow(`SELECT COUNT(*) FROM user_tombstones WHERE user_id='used'`).Scan(&remaining)
	if remaining != 1 {
		t.Fatal("attributed tombstone was removed")
	}
}

func TestFourthSessionEvictsLeastRecentlyActive(t *testing.T) {
	s, err := Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hash, _ := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	u, _ := s.CreateUser(context.Background(), CreateUser{Username: "alice", Role: RoleAdmin, PasswordHash: hash})
	now := time.Now()
	tokens := make([]string, 4)
	for i := range tokens {
		tokens[i], _, _, err = s.CreateSession(context.Background(), u.ID, now.Add(time.Duration(i)*time.Minute), 12*time.Hour, 7*24*time.Hour, 3)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.AuthenticateSession(context.Background(), tokens[0], now.Add(5*time.Minute)); err == nil {
		t.Fatal("oldest session was not evicted")
	}
}
func TestReauthenticationProofIsOneUseAndSessionBound(t *testing.T) {
	s, err := Open(context.Background(), t.TempDir()+"/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	hash, _ := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	u, _ := s.CreateUser(context.Background(), CreateUser{Username: "alice", Role: RoleAdmin, PasswordHash: hash})
	_, _, session, err := s.CreateSession(context.Background(), u.ID, time.Now(), 12*time.Hour, 7*24*time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := s.IssueReauth(context.Background(), session.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConsumeReauth(context.Background(), session.ID, proof, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = s.ConsumeReauth(context.Background(), session.ID, proof, time.Now()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("replay returned %v", err)
	}
}
