// Package store owns persistent web console state.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/auth"
	"github.com/dbehnke/opusref/pkg/wire"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

var ErrUnauthorized = errors.New("authentication failed")

type User struct {
	ID, Username, Callsign, PasswordHash string
	Role                                 Role
	PasswordChangeRequired, Disabled     bool
	CreatedAt                            time.Time
}
type UserSummary struct {
	ID                     string `json:"id"`
	Username               string `json:"username"`
	Callsign               string `json:"source_callsign,omitempty"`
	Role                   Role   `json:"role"`
	PasswordChangeRequired bool   `json:"forced_password_change"`
	Disabled               bool   `json:"disabled"`
}
type CreateUser struct {
	Username               string
	Role                   Role
	Callsign, PasswordHash string
	PasswordChangeRequired bool
}
type Session struct {
	ID, UserID, CSRFHash                string
	Role                                Role
	Username, Callsign                  string
	PasswordChangeRequired              bool
	CreatedAt, LastSeen, AbsoluteExpiry time.Time
}
type SessionSummary struct {
	ID           string    `json:"id"`
	CreatedAt    time.Time `json:"created_at"`
	LastActiveAt time.Time `json:"last_active_at"`
	Current      bool      `json:"current"`
}
type WebAuthnMaterial struct {
	UserID, Username string
	Handle           []byte
	Credentials      [][]byte
}
type PasskeySummary struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (s *Store) ListPasskeys(ctx context.Context, userID string) ([]PasskeySummary, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,label,created_at,last_used_at FROM webauthn_credentials WHERE user_id=? ORDER BY created_at", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PasskeySummary
	for rows.Next() {
		var id []byte
		var item PasskeySummary
		var created string
		var last sql.NullString
		if err = rows.Scan(&id, &item.Name, &created, &last); err != nil {
			return nil, err
		}
		item.ID = base64.RawURLEncoding.EncodeToString(id)
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		if last.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, last.String)
			item.LastUsedAt = &parsed
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) RenamePasskey(ctx context.Context, userID, id, name string) error {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(name) < 1 || len(name) > 64 {
		return errors.New("passkey input is invalid")
	}
	result, err := s.db.ExecContext(ctx, "UPDATE webauthn_credentials SET label=? WHERE id=? AND user_id=?", name, raw, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) DeletePasskey(ctx context.Context, userID, id string) error {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return errors.New("passkey ID is invalid")
	}
	result, err := s.db.ExecContext(ctx, "DELETE FROM webauthn_credentials WHERE id=? AND user_id=?", raw, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) ClearPasskeysAndSessions(ctx context.Context, userID string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM webauthn_credentials WHERE user_id=?", userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", now.UTC().Format(time.RFC3339Nano), userID); err != nil {
		return err
	}
	return tx.Commit()
}

type AuditEvent struct {
	ID         int64     `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	Action     string    `json:"action"`
	Outcome    string    `json:"outcome"`
	Details    string    `json:"details"`
}
type Recording struct {
	ID, NodeCallsign, SourceCallsign, Status, EndReason string `json:"-"`
	StartedAt                                           time.Time
	EndedAt                                             *time.Time
	PacketCount, ByteSize                               int64
}

func (s *Store) ListRecordings(ctx context.Context, limit int) ([]Recording, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,node_callsign,source_callsign,status,COALESCE(end_reason,''),start_at,end_at,packet_count,byte_size FROM recordings WHERE status IN('complete','partial') ORDER BY start_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recording
	for rows.Next() {
		var item Recording
		var start string
		var end sql.NullString
		if err = rows.Scan(&item.ID, &item.NodeCallsign, &item.SourceCallsign, &item.Status, &item.EndReason, &start, &end, &item.PacketCount, &item.ByteSize); err != nil {
			return nil, err
		}
		item.StartedAt, _ = time.Parse(time.RFC3339Nano, start)
		if end.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, end.String)
			item.EndedAt = &parsed
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) InsertRecording(ctx context.Context, id, node, source, path string, start time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO recordings(id,node_callsign,source_callsign,start_at,status,relative_path,created_at)VALUES(?,?,?,?,'creating',?,?)`, id, node, source, start.UTC().Format(time.RFC3339Nano), path, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) OpenRecording(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE recordings SET status='open' WHERE id=? AND status='creating'", id)
	return err
}
func (s *Store) FinishRecording(ctx context.Context, id, status, reason string, end time.Time, packets, size int64, sum []byte) error {
	if status != "complete" && status != "partial" {
		return errors.New("recording status is invalid")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET status=?,end_reason=?,end_at=?,packet_count=?,byte_size=?,sha256=? WHERE id=? AND status IN('creating','open','finalizing')`, status, reason, end.UTC().Format(time.RFC3339Nano), packets, size, sum, id)
	return err
}
func (s *Store) RecordingStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, "SELECT status FROM recordings WHERE id=?", id).Scan(&status)
	return status, err
}
func (s *Store) ExpiredRecordings(ctx context.Context, before time.Time) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recordings WHERE status IN('complete','partial') AND end_at<?`, before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (s *Store) MarkRecordingDeleted(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE recordings SET status='deleted',deleted_at=? WHERE id=?", now.UTC().Format(time.RFC3339Nano), id)
	return err
}

func (s *Store) WriteAudit(ctx context.Context, action, outcome string, actor, target, recording *string, details string, now time.Time) error {
	if len(action) > 64 || len(outcome) > 32 || len(details) > 2048 {
		return errors.New("audit value is too large")
	}
	if details == "" {
		details = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit_events(occurred_at,action,outcome,actor_id,target_id,recording_id,details)VALUES(?,?,?,?,?,?,?)`, now.UTC().Format(time.RFC3339Nano), action, outcome, actor, target, recording, details)
	return err
}
func (s *Store) ListAudit(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,occurred_at,action,outcome,details FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var item AuditEvent
		var stamp string
		if err = rows.Scan(&item.ID, &stamp, &item.Action, &item.Outcome, &item.Details); err != nil {
			return nil, err
		}
		item.OccurredAt, _ = time.Parse(time.RFC3339Nano, stamp)
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) PurgeAudit(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, "DELETE FROM audit_events WHERE occurred_at<?", before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) WebAuthnByUserID(ctx context.Context, userID string) (WebAuthnMaterial, error) {
	return s.webAuthnMaterial(ctx, "u.id=?", userID)
}
func (s *Store) WebAuthnByHandle(ctx context.Context, handle []byte) (WebAuthnMaterial, error) {
	return s.webAuthnMaterial(ctx, "u.webauthn_handle=?", handle)
}
func (s *Store) webAuthnMaterial(ctx context.Context, where string, arg any) (WebAuthnMaterial, error) {
	var out WebAuthnMaterial
	if err := s.db.QueryRowContext(ctx, "SELECT u.id,u.username,u.webauthn_handle FROM users u WHERE "+where+" AND u.disabled_at IS NULL AND u.deleted_at IS NULL", arg).Scan(&out.UserID, &out.Username, &out.Handle); err != nil {
		return out, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT public_key FROM webauthn_credentials WHERE user_id=?", out.UserID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return out, err
		}
		out.Credentials = append(out.Credentials, data)
	}
	return out, rows.Err()
}
func (s *Store) SaveWebAuthnCredential(ctx context.Context, userID, rpID, label string, id, encoded []byte, now time.Time) error {
	if len(label) < 1 || len(label) > 64 {
		return errors.New("passkey label is invalid")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO webauthn_credentials(id,user_id,rp_id,public_key,sign_count,transports,backup_eligible,backup_state,label,created_at)VALUES(?,?,?,?,0,'',0,0,?,?)`, id, userID, rpID, encoded, label, now.UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) UpdateWebAuthnCredential(ctx context.Context, userID string, id, encoded []byte, signCount uint32, backupEligible, backupState bool, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE webauthn_credentials SET public_key=?,sign_count=?,backup_eligible=?,backup_state=?,last_used_at=? WHERE id=? AND user_id=?`, encoded, signCount, backupEligible, backupState, now.UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

type Store struct{ db *sql.DB }

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("database path must be a regular file")
	}
	return nil
}
func (s *Store) Close() error { return s.db.Close() }
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return errors.New("database schema is newer than this program")
	}
	if version == 1 {
		return nil
	}
	schema := `
CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY,username TEXT NOT NULL UNIQUE,role TEXT NOT NULL CHECK(role IN('admin','user')),callsign TEXT UNIQUE,webauthn_handle BLOB NOT NULL UNIQUE,password_hash TEXT NOT NULL,password_change_required INTEGER NOT NULL DEFAULT 0,disabled_at TEXT,deleted_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS webauthn_credentials(id BLOB PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),rp_id TEXT NOT NULL,public_key BLOB NOT NULL,sign_count INTEGER NOT NULL,transports TEXT NOT NULL,backup_eligible INTEGER NOT NULL,backup_state INTEGER NOT NULL,label TEXT NOT NULL,created_at TEXT NOT NULL,last_used_at TEXT);
CREATE TABLE IF NOT EXISTS sessions(token_hash BLOB PRIMARY KEY,id TEXT NOT NULL UNIQUE,user_id TEXT NOT NULL REFERENCES users(id),csrf_hash BLOB NOT NULL,reauth_hash BLOB,reauth_expiry TEXT,created_at TEXT NOT NULL,last_seen TEXT NOT NULL,absolute_expiry TEXT NOT NULL,revoked_at TEXT);
CREATE TABLE IF NOT EXISTS recordings(id TEXT PRIMARY KEY,node_callsign TEXT NOT NULL,source_callsign TEXT NOT NULL,web_user_id TEXT,start_at TEXT NOT NULL,end_at TEXT,status TEXT NOT NULL,intended_status TEXT,partial_reasons INTEGER NOT NULL DEFAULT 0,end_reason TEXT,packet_count INTEGER NOT NULL DEFAULT 0,first_sequence INTEGER,last_sequence INTEGER,first_timestamp INTEGER,last_timestamp INTEGER,byte_size INTEGER NOT NULL DEFAULT 0,relative_path TEXT NOT NULL UNIQUE,sha256 BLOB,created_at TEXT NOT NULL,deleted_at TEXT);
CREATE TABLE IF NOT EXISTS user_tombstones(user_id TEXT PRIMARY KEY,username TEXT NOT NULL,source_callsign TEXT,deleted_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS audit_events(id INTEGER PRIMARY KEY AUTOINCREMENT,occurred_at TEXT NOT NULL,action TEXT NOT NULL,outcome TEXT NOT NULL,actor_id TEXT,target_id TEXT,recording_id TEXT,details TEXT NOT NULL DEFAULT '{}');
INSERT OR IGNORE INTO schema_migrations(version,applied_at)VALUES(1,strftime('%Y-%m-%dT%H:%M:%fZ','now'));`
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, schema); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateUser(ctx context.Context, in CreateUser) (User, error) {
	name, err := auth.NormalizeUsername(in.Username)
	if err != nil {
		return User{}, err
	}
	if in.Role != RoleAdmin && in.Role != RoleUser {
		return User{}, errors.New("invalid role")
	}
	if in.PasswordHash == "" {
		return User{}, errors.New("password hash is required")
	}
	id := uuid.NewString()
	handle := make([]byte, 32)
	if _, err = rand.Read(handle); err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	callsign := strings.ToUpper(in.Callsign)
	if callsign != "" {
		if _, err = wire.Callsign(callsign); err != nil {
			return User{}, err
		}
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO users(id,username,role,callsign,webauthn_handle,password_hash,password_change_required,created_at,updated_at)VALUES(?,?,?,?,?,?,?,?,?)`, id, name, in.Role, nullString(callsign), handle, in.PasswordHash, in.PasswordChangeRequired, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		return User{}, err
	}
	return User{id, name, callsign, in.PasswordHash, in.Role, in.PasswordChangeRequired, false, now}, nil
}
func (s *Store) FindUserByUsername(ctx context.Context, name string) (User, error) {
	name, err := auth.NormalizeUsername(name)
	if err != nil {
		return User{}, ErrUnauthorized
	}
	return s.scanUser(s.db.QueryRowContext(ctx, `SELECT id,username,COALESCE(callsign,''),password_hash,role,password_change_required,disabled_at IS NOT NULL,created_at FROM users WHERE username=? AND deleted_at IS NULL`, name))
}
func (s *Store) FindUserByID(ctx context.Context, id string) (User, error) {
	return s.scanUser(s.db.QueryRowContext(ctx, `SELECT id,username,COALESCE(callsign,''),password_hash,role,password_change_required,disabled_at IS NOT NULL,created_at FROM users WHERE id=? AND deleted_at IS NULL`, id))
}
func (s *Store) ListUsers(ctx context.Context, limit int) ([]UserSummary, error) {
	if limit < 1 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,username,COALESCE(callsign,''),role,password_change_required,disabled_at IS NOT NULL FROM users WHERE deleted_at IS NULL ORDER BY username LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserSummary
	for rows.Next() {
		var u UserSummary
		if err = rows.Scan(&u.ID, &u.Username, &u.Callsign, &u.Role, &u.PasswordChangeRequired, &u.Disabled); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(...any) error }

func (s *Store) scanUser(row rowScanner) (User, error) {
	var u User
	var created string
	if err := row.Scan(&u.ID, &u.Username, &u.Callsign, &u.PasswordHash, &u.Role, &u.PasswordChangeRequired, &u.Disabled, &created); err != nil {
		return User{}, err
	}
	u.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return u, nil
}
func (s *Store) EnabledAdminCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled_at IS NULL AND deleted_at IS NULL`).Scan(&n)
	return n, err
}
func (s *Store) DisableUser(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var role string
	var enabledAdmins int
	if err = tx.QueryRowContext(ctx, "SELECT role FROM users WHERE id=? AND deleted_at IS NULL", id).Scan(&role); err != nil {
		return err
	}
	if role == string(RoleAdmin) {
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled_at IS NULL AND deleted_at IS NULL`).Scan(&enabledAdmins); err != nil {
			return err
		}
		if enabledAdmins <= 1 {
			return errors.New("cannot disable the last enabled administrator")
		}
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, "UPDATE users SET disabled_at=?,updated_at=? WHERE id=?", stamp, stamp, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", stamp, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) UpdateUser(ctx context.Context, id string, username, callsign *string, role *Role, disabled *bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentRole string
	var currentDisabled bool
	if err = tx.QueryRowContext(ctx, "SELECT role,disabled_at IS NOT NULL FROM users WHERE id=? AND deleted_at IS NULL", id).Scan(&currentRole, &currentDisabled); err != nil {
		return err
	}
	removesAdmin := currentRole == string(RoleAdmin) && ((role != nil && *role != RoleAdmin) || (disabled != nil && *disabled))
	if removesAdmin {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled_at IS NULL AND deleted_at IS NULL`).Scan(&count); err != nil {
			return err
		}
		if count <= 1 {
			return errors.New("cannot change the last enabled administrator")
		}
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if username != nil {
		normalized, normalizeErr := auth.NormalizeUsername(*username)
		if normalizeErr != nil {
			return normalizeErr
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET username=?,updated_at=? WHERE id=?", normalized, stamp, id); err != nil {
			return err
		}
	}
	if callsign != nil {
		value := strings.ToUpper(*callsign)
		if value != "" {
			if _, validateErr := wire.Callsign(value); validateErr != nil {
				return validateErr
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET callsign=?,updated_at=? WHERE id=?", nullString(value), stamp, id); err != nil {
			return err
		}
	}
	if role != nil {
		if *role != RoleAdmin && *role != RoleUser {
			return errors.New("invalid role")
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET role=?,updated_at=? WHERE id=?", *role, stamp, id); err != nil {
			return err
		}
	}
	if disabled != nil {
		var disabledAt any
		if *disabled {
			disabledAt = stamp
		}
		if _, err = tx.ExecContext(ctx, "UPDATE users SET disabled_at=?,updated_at=? WHERE id=?", disabledAt, stamp, id); err != nil {
			return err
		}
		if *disabled {
			if _, err = tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", stamp, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (s *Store) RevokeAllSessions(ctx context.Context, userID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", now.UTC().Format(time.RFC3339Nano), userID)
	return err
}
func (s *Store) DeleteUser(ctx context.Context, id string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var username, role string
	var callsign sql.NullString
	if err = tx.QueryRowContext(ctx, "SELECT username,role,callsign FROM users WHERE id=? AND deleted_at IS NULL", id).Scan(&username, &role, &callsign); err != nil {
		return err
	}
	if role == string(RoleAdmin) {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role='admin' AND disabled_at IS NULL AND deleted_at IS NULL`).Scan(&count); err != nil {
			return err
		}
		if count <= 1 {
			return errors.New("cannot delete the last enabled administrator")
		}
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, `INSERT INTO user_tombstones(user_id,username,source_callsign,deleted_at)VALUES(?,?,?,?)`, id, username, callsign, stamp); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE users SET deleted_at=?,disabled_at=?,updated_at=? WHERE id=?", stamp, stamp, stamp, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", stamp, id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateSession(ctx context.Context, userID string, now time.Time, idle, absolute time.Duration, max int) (string, string, Session, error) {
	raw, hash, err := token()
	if err != nil {
		return "", "", Session{}, err
	}
	csrf, csrfHash, err := token()
	if err != nil {
		return "", "", Session{}, err
	}
	id := uuid.NewString()
	now = now.UTC()
	expiry := now.Add(absolute)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", Session{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO sessions(token_hash,id,user_id,csrf_hash,created_at,last_seen,absolute_expiry)VALUES(?,?,?,?,?,?,?)`, hash, id, userID, csrfHash, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano)); err != nil {
		return "", "", Session{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE sessions SET revoked_at=? WHERE token_hash IN (SELECT token_hash FROM sessions WHERE user_id=? AND revoked_at IS NULL ORDER BY last_seen DESC LIMIT -1 OFFSET ?)`, now.Format(time.RFC3339Nano), userID, max); err != nil {
		return "", "", Session{}, err
	}
	if err = tx.Commit(); err != nil {
		return "", "", Session{}, err
	}
	session, err := s.authenticateHash(ctx, hash, now, idle)
	return raw, csrf, session, err
}
func (s *Store) AuthenticateSession(ctx context.Context, raw string, now time.Time) (Session, error) {
	return s.AuthenticateSessionWithIdle(ctx, raw, now, 12*time.Hour)
}
func (s *Store) AuthenticateSessionWithIdle(ctx context.Context, raw string, now time.Time, idle time.Duration) (Session, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return Session{}, ErrUnauthorized
	}
	hash := sha256.Sum256(decoded)
	return s.authenticateHash(ctx, hash[:], now, idle)
}
func (s *Store) authenticateHash(ctx context.Context, hash []byte, now time.Time, idle time.Duration) (Session, error) {
	var out Session
	var created, last, absolute string
	var disabled bool
	err := s.db.QueryRowContext(ctx, `SELECT s.id,s.user_id,hex(s.csrf_hash),u.role,u.username,COALESCE(u.callsign,''),u.password_change_required,s.created_at,s.last_seen,s.absolute_expiry,u.disabled_at IS NOT NULL FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.revoked_at IS NULL AND u.deleted_at IS NULL`, hash).Scan(&out.ID, &out.UserID, &out.CSRFHash, &out.Role, &out.Username, &out.Callsign, &out.PasswordChangeRequired, &created, &last, &absolute, &disabled)
	if err != nil || disabled {
		return Session{}, ErrUnauthorized
	}
	out.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	out.LastSeen, _ = time.Parse(time.RFC3339Nano, last)
	out.AbsoluteExpiry, _ = time.Parse(time.RFC3339Nano, absolute)
	if !now.Before(out.AbsoluteExpiry) || now.Sub(out.LastSeen) > idle {
		return Session{}, ErrUnauthorized
	}
	if now.Sub(out.LastSeen) >= time.Minute {
		_, _ = s.db.ExecContext(ctx, "UPDATE sessions SET last_seen=? WHERE id=?", now.UTC().Format(time.RFC3339Nano), out.ID)
		out.LastSeen = now.UTC()
	}
	return out, nil
}
func (s *Store) RevokeSession(ctx context.Context, id string, now time.Time) error {
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE id=? AND revoked_at IS NULL", now.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) RevokeCurrentSession(ctx context.Context, raw string, now time.Time) error {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return ErrUnauthorized
	}
	hash := sha256.Sum256(decoded)
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE token_hash=? AND revoked_at IS NULL", now.UTC().Format(time.RFC3339Nano), hash[:])
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrUnauthorized
	}
	return nil
}
func (s *Store) VerifyCSRF(ctx context.Context, sessionID, raw string) bool {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return false
	}
	sum := sha256.Sum256(decoded)
	var stored []byte
	if err = s.db.QueryRowContext(ctx, "SELECT csrf_hash FROM sessions WHERE id=? AND revoked_at IS NULL", sessionID).Scan(&stored); err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(stored, sum[:]) == 1
}
func (s *Store) RotateCSRF(ctx context.Context, sessionID string) (string, error) {
	raw, hash, err := token()
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET csrf_hash=? WHERE id=? AND revoked_at IS NULL", hash, sessionID)
	if err != nil {
		return "", err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return "", ErrUnauthorized
	}
	return raw, nil
}
func (s *Store) IssueReauth(ctx context.Context, sessionID string, now time.Time) (string, error) {
	raw, hash, err := token()
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET reauth_hash=?,reauth_expiry=? WHERE id=? AND revoked_at IS NULL", hash, now.UTC().Add(5*time.Minute).Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return "", err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return "", ErrUnauthorized
	}
	return raw, nil
}
func (s *Store) ConsumeReauth(ctx context.Context, sessionID, raw string, now time.Time) error {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return ErrUnauthorized
	}
	hash := sha256.Sum256(decoded)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var stored []byte
	var expiryText string
	if err = tx.QueryRowContext(ctx, "SELECT reauth_hash,reauth_expiry FROM sessions WHERE id=? AND revoked_at IS NULL", sessionID).Scan(&stored, &expiryText); err != nil {
		return ErrUnauthorized
	}
	expiry, err := time.Parse(time.RFC3339Nano, expiryText)
	if err != nil || !now.Before(expiry) || subtle.ConstantTimeCompare(stored, hash[:]) != 1 {
		return ErrUnauthorized
	}
	result, err := tx.ExecContext(ctx, "UPDATE sessions SET reauth_hash=NULL,reauth_expiry=NULL WHERE id=? AND reauth_hash=?", sessionID, stored)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrUnauthorized
	}
	return tx.Commit()
}
func (s *Store) UpdatePassword(ctx context.Context, userID, currentSessionID, hash string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, "UPDATE users SET password_hash=?,password_change_required=0,updated_at=? WHERE id=? AND deleted_at IS NULL", hash, stamp, userID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND id<>? AND revoked_at IS NULL", stamp, userID, currentSessionID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ListSessions(ctx context.Context, userID, currentID string) ([]SessionSummary, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,created_at,last_seen FROM sessions WHERE user_id=? AND revoked_at IS NULL ORDER BY last_seen DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionSummary
	for rows.Next() {
		var item SessionSummary
		var created, last string
		if err = rows.Scan(&item.ID, &created, &last); err != nil {
			return nil, err
		}
		item.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		item.LastActiveAt, _ = time.Parse(time.RFC3339Nano, last)
		item.Current = item.ID == currentID
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) RevokeOtherSession(ctx context.Context, userID, currentID, targetID string, now time.Time) error {
	if targetID == currentID {
		return errors.New("current session cannot be revoked with this route")
	}
	result, err := s.db.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE id=? AND user_id=? AND revoked_at IS NULL", now.UTC().Format(time.RFC3339Nano), targetID, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) SetPasswordHash(ctx context.Context, userID, hash string, forced bool, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stamp := now.UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, "UPDATE users SET password_hash=?,password_change_required=?,updated_at=? WHERE id=? AND deleted_at IS NULL", hash, forced, stamp, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", stamp, userID); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) UpgradePasswordHash(ctx context.Context, userID, hash string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET password_hash=?,updated_at=? WHERE id=? AND deleted_at IS NULL", hash, now.UTC().Format(time.RFC3339Nano), userID)
	return err
}
func (s *Store) RecoverAdmin(ctx context.Context, username, hash string, now time.Time) error {
	name, err := auth.NormalizeUsername(username)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err = tx.QueryRowContext(ctx, "SELECT id FROM users WHERE username=? AND deleted_at IS NULL", name).Scan(&id); err != nil {
		return err
	}
	stamp := now.UTC().Format(time.RFC3339Nano)
	if _, err = tx.ExecContext(ctx, "UPDATE users SET role='admin',disabled_at=NULL,password_hash=?,password_change_required=1,updated_at=? WHERE id=?", hash, stamp, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE sessions SET revoked_at=? WHERE user_id=? AND revoked_at IS NULL", stamp, id); err != nil {
		return err
	}
	return tx.Commit()
}
func token() (string, []byte, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(raw)
	return base64.RawURLEncoding.EncodeToString(raw), sum[:], nil
}
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (s *Store) DB() *sql.DB          { return s.db }
func wrap(op string, err error) error { return fmt.Errorf("%s: %w", op, err) }
