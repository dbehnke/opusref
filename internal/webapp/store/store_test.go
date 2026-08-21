package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/auth"
)

func TestHistoricalSchemaFixturesMigrateWithDataPreserved(t *testing.T) {
	for _, version := range []string{"v1", "v2", "v3"} {
		t.Run(version, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.db")
			script, err := os.ReadFile(filepath.Join("testdata", version+".sql"))
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(string(script)); err != nil {
				t.Fatal(err)
			}
			_ = db.Close()
			state, err := Open(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			defer state.Close()
			var schema int
			var username string
			if err = state.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&schema); err != nil || schema != 4 {
				t.Fatalf("schema=%d err=%v", schema, err)
			}
			if err = state.DB().QueryRow(`SELECT username FROM users WHERE id='fixture-user'`).Scan(&username); err != nil || username != "fixture" {
				t.Fatalf("username=%q err=%v", username, err)
			}
			var suspectedColumn int
			_ = state.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('webauthn_credentials') WHERE name='suspected_at'`).Scan(&suspectedColumn)
			if suspectedColumn != 1 {
				t.Fatal("v4 suspected_at column is absent")
			}
		})
	}
}

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

func TestOpenRejectsCorruptDatabaseAndReleasesOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if err := os.WriteFile(path, []byte("not a sqlite database"), 0600); err != nil {
		t.Fatal(err)
	}
	if state, err := Open(context.Background(), path); err == nil {
		state.Close()
		t.Fatal("corrupt database was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	state, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("ownership lock was not released: %v", err)
	}
	state.Close()
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

func TestTombstonePurgeWaitsForAuditAndReleasesIdentifiers(t *testing.T) {
	ctx := context.Background()
	state, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	hash, _ := auth.HashPassword("quiet marble nebula orchard", auth.DefaultParams())
	_, _ = state.CreateUser(ctx, CreateUser{Username: "operator", Role: RoleAdmin, PasswordHash: hash})
	user, err := state.CreateUser(ctx, CreateUser{Username: "reuse.me", Callsign: "N0CALL", Role: RoleUser, PasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	deleted := time.Now().Add(-31 * 24 * time.Hour)
	if err = state.DeleteUser(ctx, user.ID, deleted); err != nil {
		t.Fatal(err)
	}
	if err = state.WriteAudit(ctx, "account_delete", "success", &user.ID, &user.ID, nil, "", deleted); err != nil {
		t.Fatal(err)
	}
	if count, err := state.PurgeTombstones(ctx, time.Now().Add(-30*24*time.Hour)); err != nil || count != 0 {
		t.Fatalf("audit-referenced count=%d err=%v", count, err)
	}
	if _, err = state.DB().ExecContext(ctx, `DELETE FROM audit_events WHERE actor_id=? OR target_id=?`, user.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if count, err := state.PurgeTombstones(ctx, time.Now().Add(-30*24*time.Hour)); err != nil || count != 1 {
		t.Fatalf("released count=%d err=%v", count, err)
	}
	if _, err = state.CreateUser(ctx, CreateUser{Username: "reuse.me", Callsign: "N0CALL", Role: RoleUser, PasswordHash: hash}); err != nil {
		t.Fatalf("identifiers were not released: %v", err)
	}
}

func TestRecordingRetentionUsesExactTwentyFourHourCutoff(t *testing.T) {
	ctx := context.Background()
	state, err := Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	for id, ended := range map[string]time.Time{"expired": now.Add(-24*time.Hour - time.Second), "retained": now.Add(-24*time.Hour + time.Second)} {
		if err = state.InsertRecording(ctx, id, "WEB", "N0CALL", "", id+".orar", ended); err != nil {
			t.Fatal(err)
		}
		if _, err = state.DB().ExecContext(ctx, `UPDATE recordings SET status='complete',end_at=? WHERE id=?`, ended.Format(time.RFC3339Nano), id); err != nil {
			t.Fatal(err)
		}
	}
	ids, err := state.ExpiredRecordings(ctx, now.Add(-24*time.Hour))
	if err != nil || len(ids) != 1 || ids[0] != "expired" {
		t.Fatalf("ids=%v err=%v", ids, err)
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
