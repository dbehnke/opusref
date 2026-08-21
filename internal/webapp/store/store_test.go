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
	if _, err = state.DB().Exec("DELETE FROM schema_migrations WHERE version=2"); err != nil {
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
