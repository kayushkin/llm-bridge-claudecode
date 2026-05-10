package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// State is the per-bridge persistent store of the bridge_session_id ↔
// harness_session_id chain plus WAL for atomic chain mutations.
//
// Per ARCHITECTURE.md "Session Identity & Resumption" the bridge owns a
// local state.db. This file implements the storage layer only — chain
// rotation behavior is wired in subsequent slices.
type State struct {
	db *sql.DB
}

// SessionRow is a row from the sessions table.
type SessionRow struct {
	BridgeSessionID  string
	CurrentHarnessID string
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// RolloutRow is a row from the rollouts table.
type RolloutRow struct {
	HarnessSessionID string
	BridgeSessionID  string
	RolloutPath      string
	Sequence         int
	ParentHarnessID  string // empty for the originating start
	Kind             string // 'start' | 'resume' | 'fork'
	CreatedAt        time.Time
}

// WALRow is a row from the wal table.
type WALRow struct {
	ID              int64
	BridgeSessionID string
	Intent          string // 'start' | 'resume' | 'fork'
	ParentHarnessID string
	NewHarnessID    string
	RolloutPath     string
	Status          string // 'pending' | 'committed' | 'orphaned'
	CreatedAt       time.Time
	CommittedAt     *time.Time
}

// DefaultStatePath returns the canonical on-disk location for state.db.
func DefaultStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".local", "share", "llm-bridge-claudecode", "state.db")
}

// OpenState opens or creates a state.db at the given path, applies pragmas,
// and runs idempotent migrations.
//
// modernc.org/sqlite under WAL+busy_timeout still leaks SQLITE_BUSY when
// multiple goroutines write concurrently. We pin the pool to a single
// connection so writes serialize through Go's sql layer; cross-process
// contention is still handled by WAL + busy_timeout.
func OpenState(path string) (*State, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state.db dir: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("sqlite pragmas: %w", err)
	}
	db.SetMaxOpenConns(1)

	s := &State{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the underlying database handle.
func (s *State) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *State) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS sessions (
    bridge_session_id    TEXT PRIMARY KEY,
    current_harness_id   TEXT NOT NULL DEFAULT '',
    created_at           DATETIME NOT NULL,
    updated_at           DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS rollouts (
    harness_session_id   TEXT PRIMARY KEY,
    bridge_session_id    TEXT NOT NULL,
    rollout_path         TEXT NOT NULL DEFAULT '',
    sequence             INTEGER NOT NULL,
    parent_harness_id    TEXT,
    kind                 TEXT NOT NULL,
    created_at           DATETIME NOT NULL,
    FOREIGN KEY (bridge_session_id) REFERENCES sessions(bridge_session_id)
);
CREATE INDEX IF NOT EXISTS idx_rollouts_bridge_session_id ON rollouts(bridge_session_id);

CREATE TABLE IF NOT EXISTS wal (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    bridge_session_id    TEXT NOT NULL,
    intent               TEXT NOT NULL,
    parent_harness_id    TEXT,
    new_harness_id       TEXT,
    rollout_path         TEXT,
    status               TEXT NOT NULL,
    created_at           DATETIME NOT NULL,
    committed_at         DATETIME
);
CREATE INDEX IF NOT EXISTS idx_wal_status ON wal(status);

-- Phase III.B: per-session harness_message_id -> bridge_message_id
-- mapping that the adapter maintains so it can pre-stamp Event.MessageID
-- before the event reaches bridge-server. PRIMARY KEY (bridge_session_id,
-- harness_message_id) — same harness id reappearing in a different bridge
-- session must mint a fresh bridge id (sessions are isolated).
CREATE TABLE IF NOT EXISTS harness_messages (
    bridge_session_id    TEXT NOT NULL,
    harness_message_id   TEXT NOT NULL,
    bridge_message_id    TEXT NOT NULL,
    created_at           DATETIME NOT NULL,
    PRIMARY KEY (bridge_session_id, harness_message_id)
);
`)
	if err != nil {
		return fmt.Errorf("state.db migrate: %w", err)
	}
	return nil
}

// Timestamps are written as RFC3339Nano with explicit zone offset so any
// later cross-bridge or cross-tool comparison is unambiguous.
func tsNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t.UTC()
	}
	return time.Time{}
}

// UpsertSession inserts a new sessions row or updates current_harness_id +
// updated_at on an existing row.
func (s *State) UpsertSession(bridgeSessionID, currentHarnessID string) error {
	if bridgeSessionID == "" {
		return errors.New("bridge_session_id required")
	}
	now := tsNow()
	_, err := s.db.Exec(`
INSERT INTO sessions (bridge_session_id, current_harness_id, created_at, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(bridge_session_id) DO UPDATE SET
    current_harness_id = excluded.current_harness_id,
    updated_at         = excluded.updated_at
`, bridgeSessionID, currentHarnessID, now, now)
	return err
}

// GetSession returns the session row for the given bridge_session_id, or
// (nil, sql.ErrNoRows) if none exists.
func (s *State) GetSession(bridgeSessionID string) (*SessionRow, error) {
	var row SessionRow
	var created, updated string
	err := s.db.QueryRow(`
SELECT bridge_session_id, current_harness_id, created_at, updated_at
FROM sessions WHERE bridge_session_id = ?`, bridgeSessionID).
		Scan(&row.BridgeSessionID, &row.CurrentHarnessID, &created, &updated)
	if err != nil {
		return nil, err
	}
	row.CreatedAt = parseTS(created)
	row.UpdatedAt = parseTS(updated)
	return &row, nil
}

// AllSessions returns every sessions row, ordered by updated_at descending.
func (s *State) AllSessions() ([]SessionRow, error) {
	rows, err := s.db.Query(`
SELECT bridge_session_id, current_harness_id, created_at, updated_at
FROM sessions ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRow
	for rows.Next() {
		var r SessionRow
		var created, updated string
		if err := rows.Scan(&r.BridgeSessionID, &r.CurrentHarnessID, &created, &updated); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		r.UpdatedAt = parseTS(updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertRollout records a single rollout in the chain. Caller must have
// already inserted (or upserted) the parent sessions row.
func (s *State) InsertRollout(r RolloutRow) error {
	if r.HarnessSessionID == "" || r.BridgeSessionID == "" {
		return errors.New("harness_session_id and bridge_session_id required")
	}
	if r.Kind != "start" && r.Kind != "resume" && r.Kind != "fork" {
		return fmt.Errorf("invalid kind %q", r.Kind)
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var parent any
	if r.ParentHarnessID != "" {
		parent = r.ParentHarnessID
	}
	_, err := s.db.Exec(`
INSERT INTO rollouts (harness_session_id, bridge_session_id, rollout_path, sequence, parent_harness_id, kind, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.HarnessSessionID, r.BridgeSessionID, r.RolloutPath, r.Sequence, parent, r.Kind,
		created.UTC().Format(time.RFC3339Nano))
	return err
}

// ListRollouts returns the rollouts for one bridge_session_id ordered by
// sequence ascending.
func (s *State) ListRollouts(bridgeSessionID string) ([]RolloutRow, error) {
	rows, err := s.db.Query(`
SELECT harness_session_id, bridge_session_id, rollout_path, sequence, COALESCE(parent_harness_id, ''), kind, created_at
FROM rollouts WHERE bridge_session_id = ? ORDER BY sequence ASC`, bridgeSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RolloutRow
	for rows.Next() {
		var r RolloutRow
		var created string
		if err := rows.Scan(&r.HarnessSessionID, &r.BridgeSessionID, &r.RolloutPath, &r.Sequence, &r.ParentHarnessID, &r.Kind, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseTS(created)
		out = append(out, r)
	}
	return out, rows.Err()
}

// InsertWAL writes a pending WAL row and returns its id. Use CommitWAL or
// OrphanWAL once the harness call returns or fails.
func (s *State) InsertWAL(w WALRow) (int64, error) {
	if w.BridgeSessionID == "" {
		return 0, errors.New("bridge_session_id required")
	}
	if w.Intent != "start" && w.Intent != "resume" && w.Intent != "fork" {
		return 0, fmt.Errorf("invalid intent %q", w.Intent)
	}
	status := w.Status
	if status == "" {
		status = "pending"
	}
	created := w.CreatedAt
	if created.IsZero() {
		created = time.Now().UTC()
	}
	var parent, newID, path any
	if w.ParentHarnessID != "" {
		parent = w.ParentHarnessID
	}
	if w.NewHarnessID != "" {
		newID = w.NewHarnessID
	}
	if w.RolloutPath != "" {
		path = w.RolloutPath
	}
	res, err := s.db.Exec(`
INSERT INTO wal (bridge_session_id, intent, parent_harness_id, new_harness_id, rollout_path, status, created_at, committed_at)
VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
		w.BridgeSessionID, w.Intent, parent, newID, path, status,
		created.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CommitWAL flips a pending WAL row to committed, recording the new
// harness_session_id and rollout path returned by the harness.
func (s *State) CommitWAL(id int64, newHarnessID, rolloutPath string) error {
	now := tsNow()
	res, err := s.db.Exec(`
UPDATE wal SET new_harness_id = ?, rollout_path = ?, status = 'committed', committed_at = ?
WHERE id = ? AND status = 'pending'`, newHarnessID, rolloutPath, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("wal row %d not pending or missing", id)
	}
	return nil
}

// OrphanWAL marks a pending WAL row as orphaned. Used during boot recovery
// for any pending row left behind by a crash between pending and committed.
func (s *State) OrphanWAL(id int64) error {
	res, err := s.db.Exec(`
UPDATE wal SET status = 'orphaned', committed_at = ?
WHERE id = ? AND status = 'pending'`, tsNow(), id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("wal row %d not pending or missing", id)
	}
	return nil
}

// LookupHarnessMessage returns the bridge message id previously bound to
// (bridgeSessionID, harnessMessageID), or ("", false, nil) if none.
// Implements identity.Store.Lookup for the adapter's session-scoped
// Tracker (see translate.go).
func (s *State) LookupHarnessMessage(bridgeSessionID, harnessMessageID string) (string, bool, error) {
	var bridgeMessageID string
	err := s.db.QueryRow(`
SELECT bridge_message_id FROM harness_messages
WHERE bridge_session_id = ? AND harness_message_id = ?`,
		bridgeSessionID, harnessMessageID).Scan(&bridgeMessageID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return bridgeMessageID, true, nil
}

// PutHarnessMessage records a (bridgeSessionID, harnessMessageID) ->
// bridgeMessageID binding. Idempotent on repeat with the same triple.
func (s *State) PutHarnessMessage(bridgeSessionID, harnessMessageID, bridgeMessageID string) error {
	_, err := s.db.Exec(`
INSERT INTO harness_messages (bridge_session_id, harness_message_id, bridge_message_id, created_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(bridge_session_id, harness_message_id) DO NOTHING`,
		bridgeSessionID, harnessMessageID, bridgeMessageID, tsNow())
	return err
}

// ListPendingWAL returns every WAL row whose status is still 'pending'.
func (s *State) ListPendingWAL() ([]WALRow, error) {
	rows, err := s.db.Query(`
SELECT id, bridge_session_id, intent, COALESCE(parent_harness_id, ''), COALESCE(new_harness_id, ''), COALESCE(rollout_path, ''), status, created_at, committed_at
FROM wal WHERE status = 'pending' ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WALRow
	for rows.Next() {
		var w WALRow
		var created string
		var committed sql.NullString
		if err := rows.Scan(&w.ID, &w.BridgeSessionID, &w.Intent, &w.ParentHarnessID, &w.NewHarnessID, &w.RolloutPath, &w.Status, &created, &committed); err != nil {
			return nil, err
		}
		w.CreatedAt = parseTS(created)
		if committed.Valid {
			t := parseTS(committed.String)
			w.CommittedAt = &t
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
