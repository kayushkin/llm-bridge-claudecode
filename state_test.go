package main

import (
	"path/filepath"
	"testing"
)

func openTestState(t *testing.T) (*State, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	st, err := OpenState(path)
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	return st, path
}

func TestOpenStateMigrationsAreIdempotent(t *testing.T) {
	st, path := openTestState(t)
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	st2, err := OpenState(path)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	defer st2.Close()
	// A second OpenState on the same path must not fail and the schema
	// helpers must still work.
	if err := st2.UpsertSession("bsid-x", "hsid-x"); err != nil {
		t.Fatalf("UpsertSession after reopen: %v", err)
	}
	row, err := st2.GetSession("bsid-x")
	if err != nil {
		t.Fatalf("GetSession after reopen: %v", err)
	}
	if row.CurrentHarnessID != "hsid-x" {
		t.Fatalf("expected hsid-x, got %q", row.CurrentHarnessID)
	}
}

func TestSessionsRoundTrip(t *testing.T) {
	st, _ := openTestState(t)
	defer st.Close()

	if err := st.UpsertSession("bsid-1", "hsid-a"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	row, err := st.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "hsid-a" {
		t.Fatalf("got %q want hsid-a", row.CurrentHarnessID)
	}
	if row.CreatedAt.IsZero() || row.UpdatedAt.IsZero() {
		t.Fatalf("timestamps not parsed: %+v", row)
	}

	// Upsert rotates current_harness_id and bumps updated_at.
	if err := st.UpsertSession("bsid-1", "hsid-b"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	row2, err := st.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row2.CurrentHarnessID != "hsid-b" {
		t.Fatalf("rotation failed: got %q", row2.CurrentHarnessID)
	}
	if !row2.CreatedAt.Equal(row.CreatedAt) {
		t.Fatalf("created_at must not change on upsert: %v vs %v", row.CreatedAt, row2.CreatedAt)
	}

	all, err := st.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(all) != 1 || all[0].BridgeSessionID != "bsid-1" {
		t.Fatalf("AllSessions = %+v", all)
	}
}

func TestRolloutsRoundTrip(t *testing.T) {
	st, _ := openTestState(t)
	defer st.Close()

	if err := st.UpsertSession("bsid-1", "hsid-a"); err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: "hsid-a",
		BridgeSessionID:  "bsid-1",
		RolloutPath:      "/tmp/a.jsonl",
		Sequence:         0,
		Kind:             "start",
	}); err != nil {
		t.Fatalf("InsertRollout start: %v", err)
	}
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: "hsid-b",
		BridgeSessionID:  "bsid-1",
		RolloutPath:      "/tmp/b.jsonl",
		Sequence:         1,
		ParentHarnessID:  "hsid-a",
		Kind:             "resume",
	}); err != nil {
		t.Fatalf("InsertRollout resume: %v", err)
	}

	rs, err := st.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 rollouts, got %d (%+v)", len(rs), rs)
	}
	if rs[0].Sequence != 0 || rs[1].Sequence != 1 {
		t.Fatalf("ordering: %+v", rs)
	}
	if rs[0].ParentHarnessID != "" {
		t.Fatalf("start row should have empty parent, got %q", rs[0].ParentHarnessID)
	}
	if rs[1].ParentHarnessID != "hsid-a" {
		t.Fatalf("resume parent: %q", rs[1].ParentHarnessID)
	}

	// Bad kind rejected.
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: "hsid-c",
		BridgeSessionID:  "bsid-1",
		Sequence:         2,
		Kind:             "branch",
	}); err == nil {
		t.Fatalf("expected error on invalid kind")
	}
}

func TestWALLifecycle(t *testing.T) {
	st, _ := openTestState(t)
	defer st.Close()

	if err := st.UpsertSession("bsid-1", ""); err != nil {
		t.Fatalf("session: %v", err)
	}

	id1, err := st.InsertWAL(WALRow{BridgeSessionID: "bsid-1", Intent: "start"})
	if err != nil {
		t.Fatalf("InsertWAL start: %v", err)
	}
	id2, err := st.InsertWAL(WALRow{BridgeSessionID: "bsid-1", Intent: "resume", ParentHarnessID: "hsid-a"})
	if err != nil {
		t.Fatalf("InsertWAL resume: %v", err)
	}

	pending, err := st.ListPendingWAL()
	if err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	}
	if len(pending) != 2 {
		t.Fatalf("want 2 pending, got %d", len(pending))
	}

	if err := st.CommitWAL(id1, "hsid-a", "/tmp/a.jsonl"); err != nil {
		t.Fatalf("CommitWAL: %v", err)
	}
	if err := st.OrphanWAL(id2); err != nil {
		t.Fatalf("OrphanWAL: %v", err)
	}

	pending, err = st.ListPendingWAL()
	if err != nil {
		t.Fatalf("ListPendingWAL after commits: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("expected 0 pending after commit+orphan, got %d (%+v)", len(pending), pending)
	}

	// Re-committing or re-orphaning a non-pending row is an error.
	if err := st.CommitWAL(id1, "hsid-a", ""); err == nil {
		t.Fatalf("expected error committing already-committed row")
	}
	if err := st.OrphanWAL(id2); err == nil {
		t.Fatalf("expected error orphaning already-orphaned row")
	}

	// Bad intent rejected up front.
	if _, err := st.InsertWAL(WALRow{BridgeSessionID: "bsid-1", Intent: "branch"}); err == nil {
		t.Fatalf("expected error on bad intent")
	}
}

func TestGetSessionMissing(t *testing.T) {
	st, _ := openTestState(t)
	defer st.Close()
	if _, err := st.GetSession("nope"); err == nil {
		t.Fatalf("expected sql.ErrNoRows for missing session")
	}
}
