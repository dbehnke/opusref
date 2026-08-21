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
	schema := `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS users(id TEXT PRIMARY KEY,username TEXT NOT NULL UNIQUE,role TEXT NOT NULL CHECK(role IN('admin','user')),callsign TEXT UNIQUE,webauthn_handle BLOB NOT NULL UNIQUE,password_hash TEXT NOT NULL,password_change_required INTEGER NOT NULL DEFAULT 0,disabled_at TEXT,deleted_at TEXT,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS webauthn_credentials(id BLOB PRIMARY KEY,user_id TEXT NOT NULL REFERENCES users(id),rp_id TEXT NOT NULL,public_key BLOB NOT NULL,sign_count INTEGER NOT NULL,transports TEXT NOT NULL,backup_eligible INTEGER NOT NULL,backup_state INTEGER NOT NULL,label TEXT NOT NULL,created_at TEXT NOT NULL,last_used_at TEXT);
CREATE TABLE IF NOT EXISTS sessions(token_hash BLOB PRIMARY KEY,id TEXT NOT NULL UNIQUE,user_id TEXT NOT NULL REFERENCES users(id),csrf_hash BLOB NOT NULL,reauth_hash BLOB,reauth_expiry TEXT,created_at TEXT NOT NULL,last_seen TEXT NOT NULL,absolute_expiry TEXT NOT NULL,revoked_at TEXT);
CREATE TABLE IF NOT EXISTS recordings(id TEXT PRIMARY KEY,node_callsign TEXT NOT NULL,source_callsign TEXT NOT NULL,web_user_id TEXT,start_at TEXT NOT NULL,end_at TEXT,status TEXT NOT NULL,intended_status TEXT,partial_reasons INTEGER NOT NULL DEFAULT 0,end_reason TEXT,packet_count INTEGER NOT NULL DEFAULT 0,first_sequence INTEGER,last_sequence INTEGER,first_timestamp INTEGER,last_timestamp INTEGER,byte_size INTEGER NOT NULL DEFAULT 0,relative_path TEXT NOT NULL UNIQUE,sha256 BLOB,created_at TEXT NOT NULL,deleted_at TEXT);
CREATE TABLE IF NOT EXISTS user_tombstones(user_id TEXT PRIMARY KEY,username TEXT NOT NULL,source_callsign TEXT,deleted_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS audit_events(id INTEGER PRIMARY KEY AUTOINCREMENT,occurred_at TEXT NOT NULL,action TEXT NOT NULL,outcome TEXT NOT NULL,actor_id TEXT,target_id TEXT,recording_id TEXT,details TEXT NOT NULL DEFAULT '{}');
INSERT OR IGNORE INTO schema_migrations(version,applied_at)VALUES(1,strftime('%Y-%m-%dT%H:%M:%fZ','now'));`
	_, err := s.db.ExecContext(ctx, schema)
	return err
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
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(decoded) != 32 {
		return Session{}, ErrUnauthorized
	}
	hash := sha256.Sum256(decoded)
	return s.authenticateHash(ctx, hash[:], now, 12*time.Hour)
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
