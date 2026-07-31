package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// state.db is one shared path per harness — ~/.local/share/llm-bridge-claudecode/state.db
// — and every bridge process of this kind opens it. On a fresh install several
// of them reach the file at once and each tries to convert it to WAL.
//
// Holding a write transaction on the fresh file makes that race deterministic
// rather than roughly-half-the-time. Against a conversion that cannot retry this
// fails on every run, and it fails instantly: the 0s is the finding, because a
// connection that had honoured its own 5s busy_timeout would have outlasted a
// 150ms lock. SQLite declines to run the busy handler for the lock upgrade the
// conversion needs, so busy_timeout has nothing to give and the wait has to be
// ours.
func TestOpenStateWaitsOutAConversionItLoses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")

	// Stand in for the bridge process that reached the fresh file first and is
	// mid-write. Its DSN is spelled out rather than built by OpenState's helper:
	// it has to leave the file on the rollback journal so the state store under
	// test is the one that must convert it.
	holder, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if _, err := holder.Exec("CREATE TABLE IF NOT EXISTS seed (v TEXT)"); err != nil {
		t.Fatal(err)
	}

	held, err := holder.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec("INSERT INTO seed VALUES ('x')"); err != nil {
		t.Fatal(err)
	}

	const holdFor = 150 * time.Millisecond
	releasing := time.AfterFunc(holdFor, func() { held.Commit() })
	defer releasing.Stop()

	opened := time.Now()
	st, err := OpenState(path)
	waited := time.Since(opened)
	if err != nil {
		t.Fatalf("OpenState gave up while another connection held the file, after %v: %v", waited, err)
	}
	defer st.Close()

	if waited < holdFor {
		t.Errorf("OpenState returned after %v, before the %v lock was released — it cannot have waited for it", waited, holdFor)
	}

	// Succeeding is not enough. A state store that came back on the rollback
	// journal has the serialized queue WAL exists to prevent, and says nothing
	// about it.
	var settled string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&settled); err != nil {
		t.Fatalf("read back journal_mode: %v", err)
	}
	if !strings.EqualFold(settled, journalMode) {
		t.Errorf("journal_mode settled on %q, want %q", settled, journalMode)
	}
}

// Complement, and the reason the test above is not just slow-and-lucky: every
// pragma has to actually reach SQLite. They arrive by two routes now —
// busy_timeout and foreign_keys in the DSN, journal_mode as the one statement
// switchJournalMode runs — and each route fails quietly in its own way.
// modernc's driver drops a DSN key it does not recognise instead of rejecting
// it, so a mattn-style ?_busy_timeout=5000 leaves the setting at its default and
// says nothing; and a lost journal_mode race can report the mode it stayed on
// rather than an error.
//
// foreign_keys is the one with teeth: rollouts references sessions, and SQLite
// enforces that only while the pragma is on.
func TestOpenStateAppliesItsConcurrencyPragmas(t *testing.T) {
	st, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	var appliedTimeout int
	if err := st.db.QueryRow("PRAGMA busy_timeout").Scan(&appliedTimeout); err != nil {
		t.Fatalf("read back busy_timeout: %v", err)
	}
	if appliedTimeout != busyTimeout {
		t.Errorf("busy_timeout is %d, want %d — the DSN key did not reach SQLite", appliedTimeout, busyTimeout)
	}

	var foreignKeys int
	if err := st.db.QueryRow("PRAGMA foreign_keys").Scan(&foreignKeys); err != nil {
		t.Fatalf("read back foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Errorf("foreign_keys is %d, want 1 — the DSN key did not reach SQLite", foreignKeys)
	}

	var appliedJournal string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&appliedJournal); err != nil {
		t.Fatalf("read back journal_mode: %v", err)
	}
	if !strings.EqualFold(appliedJournal, journalMode) {
		t.Errorf("journal_mode is %q, want %q — the conversion did not reach SQLite", appliedJournal, journalMode)
	}
}
