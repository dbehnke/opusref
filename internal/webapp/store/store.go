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
	"syscall"
	"time"

	"github.com/dbehnke/opusref/internal/webapp/auth"
	"github.com/dbehnke/opusref/pkg/wire"
	"github.com/google/uuid"
	"modernc.org/sqlite"
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
	Suspected  bool       `json:"suspected"`
}

func (s *Store) ListPasskeys(ctx context.Context, userID string) ([]PasskeySummary, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id,label,created_at,last_used_at,suspected_at IS NOT NULL FROM webauthn_credentials WHERE user_id=? ORDER BY created_at", userID)
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
		if err = rows.Scan(&id, &item.Name, &created, &last, &item.Suspected); err != nil {
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
	ID, NodeCallsign, SourceCallsign, Status, EndReason string
	RelativePath                                        string
	StartedAt                                           time.Time
	EndedAt                                             *time.Time
	PacketCount, ByteSize                               int64
	PartialReasons                                      uint32
	SHA256                                              []byte
	FirstSequence, LastSequence                         *uint32
	FirstTimestamp, LastTimestamp                       *uint32
}
type RecordingPacketBounds struct {
	FirstSequence, LastSequence   uint32
	FirstTimestamp, LastTimestamp uint32
}
type RecordingQuery struct {
	Limit                         int
	BeforeStart, BeforeID, Source string
	From, To                      *time.Time
	Status                        string
}

func (s *Store) ListRecordings(ctx context.Context, limit int) ([]Recording, error) {
	return s.QueryRecordings(ctx, RecordingQuery{Limit: limit})
}
func (s *Store) QueryRecordings(ctx context.Context, query RecordingQuery) ([]Recording, error) {
	limit := query.Limit
	if limit < 1 || limit > 201 {
		limit = 50
	}
	statement := `SELECT id,node_callsign,source_callsign,status,COALESCE(end_reason,''),start_at,end_at,packet_count,byte_size,partial_reasons,relative_path,sha256,first_sequence,last_sequence,first_timestamp,last_timestamp FROM recordings WHERE status IN('complete','partial')`
	args := []any{}
	if query.Source != "" {
		statement += " AND source_callsign=?"
		args = append(args, query.Source)
	}
	if query.Status == "complete" || query.Status == "partial" {
		statement += " AND status=?"
		args = append(args, query.Status)
	}
	if query.From != nil {
		statement += " AND start_at>=?"
		args = append(args, query.From.UTC().Format(time.RFC3339Nano))
	}
	if query.To != nil {
		statement += " AND start_at<=?"
		args = append(args, query.To.UTC().Format(time.RFC3339Nano))
	}
	if query.BeforeStart != "" && query.BeforeID != "" {
		statement += " AND (start_at<? OR (start_at=? AND id<?))"
		args = append(args, query.BeforeStart, query.BeforeStart, query.BeforeID)
	}
	statement += " ORDER BY start_at DESC,id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recording
	for rows.Next() {
		var item Recording
		var start string
		var end sql.NullString
		var firstSequence, lastSequence, firstTimestamp, lastTimestamp sql.NullInt64
		if err = rows.Scan(&item.ID, &item.NodeCallsign, &item.SourceCallsign, &item.Status, &item.EndReason, &start, &end, &item.PacketCount, &item.ByteSize, &item.PartialReasons, &item.RelativePath, &item.SHA256, &firstSequence, &lastSequence, &firstTimestamp, &lastTimestamp); err != nil {
			return nil, err
		}
		setRecordingBounds(&item, firstSequence, lastSequence, firstTimestamp, lastTimestamp)
		item.StartedAt, _ = time.Parse(time.RFC3339Nano, start)
		if end.Valid {
			parsed, _ := time.Parse(time.RFC3339Nano, end.String)
			item.EndedAt = &parsed
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func (s *Store) RecordingByID(ctx context.Context, id string) (Recording, error) {
	var item Recording
	var start string
	var end sql.NullString
	var firstSequence, lastSequence, firstTimestamp, lastTimestamp sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id,node_callsign,source_callsign,status,COALESCE(end_reason,''),start_at,end_at,packet_count,byte_size,partial_reasons,relative_path,sha256,first_sequence,last_sequence,first_timestamp,last_timestamp FROM recordings WHERE id=? AND status IN('complete','partial')`, id).Scan(&item.ID, &item.NodeCallsign, &item.SourceCallsign, &item.Status, &item.EndReason, &start, &end, &item.PacketCount, &item.ByteSize, &item.PartialReasons, &item.RelativePath, &item.SHA256, &firstSequence, &lastSequence, &firstTimestamp, &lastTimestamp)
	if err != nil {
		return item, err
	}
	item.StartedAt, _ = time.Parse(time.RFC3339Nano, start)
	setRecordingBounds(&item, firstSequence, lastSequence, firstTimestamp, lastTimestamp)
	if end.Valid {
		parsed, _ := time.Parse(time.RFC3339Nano, end.String)
		item.EndedAt = &parsed
	}
	return item, nil
}

func setRecordingBounds(item *Recording, values ...sql.NullInt64) {
	targets := []**uint32{&item.FirstSequence, &item.LastSequence, &item.FirstTimestamp, &item.LastTimestamp}
	for index, value := range values {
		if value.Valid {
			converted := uint32(value.Int64)
			*targets[index] = &converted
		}
	}
}
func (s *Store) InsertRecording(ctx context.Context, id, node, source, webUserID, path string, start time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO recordings(id,node_callsign,source_callsign,web_user_id,start_at,status,relative_path,created_at)VALUES(?,?,?,?,?,'creating',?,?)`, id, node, source, nullString(webUserID), start.UTC().Format(time.RFC3339Nano), path, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func (s *Store) AttributeRecording(ctx context.Context, id, webUserID string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE recordings SET web_user_id=? WHERE id=? AND web_user_id IS NULL`, webUserID, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count > 1 {
		return errors.New("recording attribution update affected multiple rows")
	}
	return nil
}
func (s *Store) OpenRecording(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE recordings SET status='open' WHERE id=? AND status='creating'", id)
	return err
}
func (s *Store) BeginRecordingFinalize(ctx context.Context, id, intendedStatus, reason string, partialReasons uint32) error {
	if intendedStatus != "complete" && intendedStatus != "partial" {
		return errors.New("recording status is invalid")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE recordings SET status='finalizing',intended_status=?,end_reason=?,partial_reasons=? WHERE id=? AND status='open'`, intendedStatus, reason, partialReasons, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) PrepareRecoveryFinalize(ctx context.Context, id, intendedStatus, reason string, partialReasons uint32) error {
	result, err := s.db.ExecContext(ctx, `UPDATE recordings SET status='finalizing',intended_status=?,end_reason=?,partial_reasons=partial_reasons|? WHERE id=? AND status IN('creating','open','finalizing')`, intendedStatus, reason, partialReasons, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) FinishRecording(ctx context.Context, id, status, reason string, end time.Time, packets, size int64, sum []byte) error {
	return s.finishRecording(ctx, id, status, reason, end, packets, size, sum, nil)
}
func (s *Store) FinishRecordingWithBounds(ctx context.Context, id, status, reason string, end time.Time, packets, size int64, sum []byte, bounds RecordingPacketBounds) error {
	return s.finishRecording(ctx, id, status, reason, end, packets, size, sum, &bounds)
}
func (s *Store) BackfillRecordingBounds(ctx context.Context, id string, packets int64, bounds RecordingPacketBounds) error {
	var firstSequence, lastSequence, firstTimestamp, lastTimestamp any
	if packets > 0 {
		firstSequence, lastSequence = bounds.FirstSequence, bounds.LastSequence
		firstTimestamp, lastTimestamp = bounds.FirstTimestamp, bounds.LastTimestamp
	}
	result, err := s.db.ExecContext(ctx, `UPDATE recordings SET packet_count=?,first_sequence=?,last_sequence=?,first_timestamp=?,last_timestamp=? WHERE id=? AND status IN('complete','partial')`, packets, firstSequence, lastSequence, firstTimestamp, lastTimestamp, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) finishRecording(ctx context.Context, id, status, reason string, end time.Time, packets, size int64, sum []byte, bounds *RecordingPacketBounds) error {
	if status != "complete" && status != "partial" {
		return errors.New("recording status is invalid")
	}
	var firstSequence, lastSequence, firstTimestamp, lastTimestamp any
	if bounds != nil && packets > 0 {
		firstSequence, lastSequence = bounds.FirstSequence, bounds.LastSequence
		firstTimestamp, lastTimestamp = bounds.FirstTimestamp, bounds.LastTimestamp
	}
	result, err := s.db.ExecContext(ctx, `UPDATE recordings SET status=?,intended_status=NULL,end_reason=?,end_at=?,packet_count=?,byte_size=?,sha256=?,first_sequence=?,last_sequence=?,first_timestamp=?,last_timestamp=? WHERE id=? AND status='finalizing' AND intended_status=?`, status, reason, end.UTC().Format(time.RFC3339Nano), packets, size, sum, firstSequence, lastSequence, firstTimestamp, lastTimestamp, id, status)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) MarkRecordingUnavailable(ctx context.Context, id, reason string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE recordings SET status='unavailable',intended_status=NULL,end_reason=?,end_at=? WHERE id=?`, reason, now.UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) RecordingStatus(ctx context.Context, id string) (string, error) {
	var status string
	err := s.db.QueryRowContext(ctx, "SELECT status FROM recordings WHERE id=?", id).Scan(&status)
	return status, err
}

type RecordingRecoveryState struct {
	Status, IntendedStatus, EndReason string
	PartialReasons                    uint32
}

func (s *Store) RecordingRecoveryState(ctx context.Context, id string) (RecordingRecoveryState, error) {
	var value RecordingRecoveryState
	err := s.db.QueryRowContext(ctx, `SELECT status,COALESCE(intended_status,''),COALESCE(end_reason,''),partial_reasons FROM recordings WHERE id=?`, id).Scan(&value.Status, &value.IntendedStatus, &value.EndReason, &value.PartialReasons)
	return value, err
}

func (s *Store) PrepareRecoveryByState(ctx context.Context, id, fallbackStatus, fallbackReason string, reasons uint32) (string, string, uint32, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", 0, err
	}
	defer tx.Rollback()
	var current RecordingRecoveryState
	if err = tx.QueryRowContext(ctx, `SELECT status,COALESCE(intended_status,''),COALESCE(end_reason,''),partial_reasons FROM recordings WHERE id=?`, id).Scan(&current.Status, &current.IntendedStatus, &current.EndReason, &current.PartialReasons); err != nil {
		return "", "", 0, err
	}
	status, reason := fallbackStatus, fallbackReason
	if current.Status == "finalizing" {
		status, reason = current.IntendedStatus, current.EndReason
	}
	if status != "complete" && status != "partial" {
		return "", "", 0, errors.New("recording recovery status is invalid")
	}
	if reason == "" {
		reason = fallbackReason
	}
	combined := current.PartialReasons | reasons
	result, err := tx.ExecContext(ctx, `UPDATE recordings SET status='finalizing',intended_status=?,end_reason=?,partial_reasons=? WHERE id=? AND status IN('creating','open','finalizing')`, status, reason, combined, id)
	if err != nil {
		return "", "", 0, err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return "", "", 0, sql.ErrNoRows
	}
	if err = tx.Commit(); err != nil {
		return "", "", 0, err
	}
	return status, reason, combined, nil
}

func (s *Store) DeleteCreatingRecording(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM recordings WHERE id=? AND status='creating'`, id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) RecoverableRecordingIDs(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recordings WHERE status IN('creating','open','finalizing')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
func (s *Store) OldestRecordings(ctx context.Context, limit int) ([]string, error) {
	if limit < 1 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM recordings WHERE status IN('complete','partial') ORDER BY end_at,id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
func (s *Store) MarkRecordingDeleted(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE recordings SET status='deleted',intended_status=NULL,deleted_at=? WHERE id=?", now.UTC().Format(time.RFC3339Nano), id)
	return err
}
func (s *Store) BeginRecordingDelete(ctx context.Context, id string) error {
	result, err := s.db.ExecContext(ctx, "UPDATE recordings SET intended_status=status,status='deleting' WHERE id=? AND status IN('complete','partial')", id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
func (s *Store) BeginRecordingDeleteAudited(ctx context.Context, id, actor string, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE recordings SET intended_status=status,status='deleting' WHERE id=? AND status IN('complete','partial')", id)
	if err != nil {
		return err
	}
	count, _ := result.RowsAffected()
	if count != 1 {
		return sql.ErrNoRows
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(occurred_at,action,outcome,actor_id,recording_id,details)VALUES(?,'recording_delete','success',?,?, '{}')`, now.UTC().Format(time.RFC3339Nano), actor, id); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) DeletingRecordings(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id FROM recordings WHERE status='deleting'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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
	return s.ListAuditBefore(ctx, limit, 0)
}
func (s *Store) ListAuditBefore(ctx context.Context, limit int, beforeID int64) ([]AuditEvent, error) {
	if limit < 1 || limit > 201 {
		limit = 50
	}
	statement := `SELECT id,occurred_at,action,outcome,details FROM audit_events`
	args := []any{}
	if beforeID > 0 {
		statement += " WHERE id<?"
		args = append(args, beforeID)
	}
	statement += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
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
	return s.webAuthnMaterial(ctx, "u.id=?", userID, "")
}
func (s *Store) WebAuthnByHandle(ctx context.Context, handle []byte) (WebAuthnMaterial, error) {
	return s.webAuthnMaterial(ctx, "u.webauthn_handle=?", handle, "")
}
func (s *Store) WebAuthnByUserIDRP(ctx context.Context, userID, rpID string) (WebAuthnMaterial, error) {
	return s.webAuthnMaterial(ctx, "u.id=?", userID, rpID)
}
func (s *Store) WebAuthnByHandleRP(ctx context.Context, handle []byte, rpID string) (WebAuthnMaterial, error) {
	return s.webAuthnMaterial(ctx, "u.webauthn_handle=?", handle, rpID)
}
func (s *Store) webAuthnMaterial(ctx context.Context, where string, arg any, rpID string) (WebAuthnMaterial, error) {
	var out WebAuthnMaterial
	if err := s.db.QueryRowContext(ctx, "SELECT u.id,u.username,u.webauthn_handle FROM users u WHERE "+where+" AND u.disabled_at IS NULL AND u.deleted_at IS NULL", arg).Scan(&out.UserID, &out.Username, &out.Handle); err != nil {
		return out, err
	}
	query := "SELECT public_key FROM webauthn_credentials WHERE user_id=? AND suspected_at IS NULL"
	args := []any{out.UserID}
	if rpID != "" {
		query += " AND rp_id=?"
		args = append(args, rpID)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
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
func (s *Store) SaveWebAuthnCredential(ctx context.Context, userID, rpID, label string, id, encoded []byte, signCount uint32, transports string, backupEligible, backupState bool, now time.Time) error {
	if len(label) < 1 || len(label) > 64 {
		return errors.New("passkey label is invalid")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO webauthn_credentials(id,user_id,rp_id,public_key,sign_count,transports,backup_eligible,backup_state,label,created_at)VALUES(?,?,?,?,?,?,?,?,?,?)`, id, userID, rpID, encoded, signCount, transports, backupEligible, backupState, label, now.UTC().Format(time.RFC3339Nano))
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

func (s *Store) MarkWebAuthnCredentialSuspected(ctx context.Context, userID string, id []byte, now time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE webauthn_credentials SET suspected_at=? WHERE id=? AND user_id=?`, now.UTC().Format(time.RFC3339Nano), id, userID)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return sql.ErrNoRows
	}
	return nil
}

type Store struct {
	db       *sql.DB
	lockFile *os.File
}
type MonitoringCounts struct {
	Accounts, Sessions, Recordings, AuthenticationFailures int64
}

func (s *Store) MonitoringCounts(ctx context.Context, now time.Time) (MonitoringCounts, error) {
	var counts MonitoringCounts
	stamp := now.UTC().Format(time.RFC3339Nano)
	err := s.db.QueryRowContext(ctx, `SELECT
(SELECT COUNT(*) FROM users WHERE disabled_at IS NULL AND deleted_at IS NULL),
(SELECT COUNT(*) FROM sessions WHERE revoked_at IS NULL AND absolute_expiry>?),
(SELECT COUNT(*) FROM recordings WHERE status IN('complete','partial')),
(SELECT COUNT(*) FROM audit_events WHERE action IN('login','passkey_login') AND outcome='failure')`, stamp).Scan(&counts.Accounts, &counts.Sessions, &counts.Recordings, &counts.AuthenticationFailures)
	return counts, err
}

func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open database ownership lock: %w", err)
	}
	if err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, errors.New("database is owned by another opusrefweb process")
	}
	releaseLock := func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		releaseLock()
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, "PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		db.Close()
		releaseLock()
		return nil, err
	}
	s := &Store{db: db, lockFile: lockFile}
	if err = s.backupBeforeMigration(ctx, path); err != nil {
		db.Close()
		releaseLock()
		return nil, err
	}
	if err = s.migrate(ctx); err != nil {
		db.Close()
		releaseLock()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		releaseLock()
		return nil, err
	}
	return s, nil
}
func (s *Store) backupBeforeMigration(ctx context.Context, path string) error {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='schema_migrations'`).Scan(&exists); err != nil || exists == 0 {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil || version == 0 || version >= 4 {
		return err
	}
	backup := fmt.Sprintf("%s.pre-v%d-%d.bak", path, version, time.Now().UnixNano())
	connection, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration backup connection: %w", err)
	}
	defer connection.Close()
	err = connection.Raw(func(driverConn any) error {
		backuper, ok := driverConn.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return errors.New("SQLite driver does not support online backup")
		}
		operation, backupErr := backuper.NewBackup(backup)
		if backupErr != nil {
			return backupErr
		}
		if _, backupErr = operation.Step(-1); backupErr != nil {
			_, _ = operation.Commit()
			return backupErr
		}
		_, backupErr = operation.Commit()
		return backupErr
	})
	if err != nil {
		return fmt.Errorf("create migration backup: %w", err)
	}
	if err := os.Chmod(backup, 0600); err != nil {
		return fmt.Errorf("secure migration backup: %w", err)
	}
	return nil
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
func (s *Store) Close() error {
	err := s.db.Close()
	if s.lockFile != nil {
		if lockErr := syscall.Flock(int(s.lockFile.Fd()), syscall.LOCK_UN); err == nil {
			err = lockErr
		}
		if closeErr := s.lockFile.Close(); err == nil {
			err = closeErr
		}
		s.lockFile = nil
	}
	return err
}
func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		return err
	}
	var version int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return err
	}
	if version > 4 {
		return errors.New("database schema is newer than this program")
	}
	if version == 1 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS recordings_browse ON recordings(status,start_at DESC,id DESC);
CREATE INDEX IF NOT EXISTS recordings_retention ON recordings(status,end_at);
CREATE INDEX IF NOT EXISTS audit_browse ON audit_events(id DESC);
INSERT INTO schema_migrations(version,applied_at)VALUES(2,strftime('%Y-%m-%dT%H:%M:%fZ','now'));`); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return s.migrate(ctx)
	}
	if version == 2 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS recordings_status_insert BEFORE INSERT ON recordings WHEN NEW.status NOT IN('creating','open','finalizing','complete','partial','deleting','deleted','unavailable') BEGIN SELECT RAISE(ABORT,'invalid recording status'); END;
CREATE TRIGGER IF NOT EXISTS recordings_status_update BEFORE UPDATE OF status ON recordings WHEN NEW.status NOT IN('creating','open','finalizing','complete','partial','deleting','deleted','unavailable') BEGIN SELECT RAISE(ABORT,'invalid recording status'); END;
INSERT INTO schema_migrations(version,applied_at)VALUES(3,strftime('%Y-%m-%dT%H:%M:%fZ','now'));`); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return s.migrate(ctx)
	}
	if version == 3 {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var columnCount int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('webauthn_credentials') WHERE name='suspected_at'`).Scan(&columnCount); err != nil {
			return err
		}
		if columnCount == 0 {
			if _, err = tx.ExecContext(ctx, `ALTER TABLE webauthn_credentials ADD COLUMN suspected_at TEXT`); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at)VALUES(4,strftime('%Y-%m-%dT%H:%M:%fZ','now'))`); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return s.migrate(ctx)
	}
	if version == 4 {
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
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.migrate(ctx)
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
	return s.ListUsersAfter(ctx, limit, "", "")
}
func (s *Store) ListUsersAfter(ctx context.Context, limit int, afterUsername, afterID string) ([]UserSummary, error) {
	if limit < 1 || limit > 201 {
		limit = 50
	}
	statement := `SELECT id,username,COALESCE(callsign,''),role,password_change_required,disabled_at IS NOT NULL FROM users WHERE deleted_at IS NULL`
	args := []any{}
	if afterUsername != "" && afterID != "" {
		statement += " AND (username>? OR (username=? AND id>?))"
		args = append(args, afterUsername, afterUsername, afterID)
	}
	statement += " ORDER BY username,id LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, statement, args...)
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

func (s *Store) PurgeTombstones(ctx context.Context, before time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT user_id FROM user_tombstones t
WHERE deleted_at<?
AND NOT EXISTS(SELECT 1 FROM recordings r WHERE r.web_user_id=t.user_id)
AND NOT EXISTS(SELECT 1 FROM audit_events a WHERE a.actor_id=t.user_id OR a.target_id=t.user_id)`, before.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, `DELETE FROM webauthn_credentials WHERE user_id=?`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM users WHERE id=? AND deleted_at IS NOT NULL`, id); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM user_tombstones WHERE user_id=?`, id); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO audit_events(occurred_at,action,outcome,actor_id,target_id,details)VALUES(?,'admin_recovery','success',?,?, '{}')`, stamp, id, id); err != nil {
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
