package main

import (
	"path/filepath"
	"testing"
)

// newTestHarness returns a Harness with only the chain-machinery fields
// populated (state.db, bridgeSessionID). No CC subprocess, no event channel —
// callers drive recordChainOnInit directly to simulate CC's system:init
// arrival without spawning the CLI. HOME is pointed at a temp dir so any
// stray filesystem lookup (findRolloutForUUID) cannot escape into the host's
// real ~/.claude/projects tree.
func newTestHarness(t *testing.T, bridgeSessionID string) *Harness {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	st, err := OpenState(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	return &Harness{state: st, bridgeSessionID: bridgeSessionID}
}

// stageStart simulates what handleStart's cold-start branch does (without
// actually spawning CC) so tests can drive recordChainOnInit through the
// post-init code path.
func stageStart(t *testing.T, h *Harness) int64 {
	t.Helper()
	if err := h.state.UpsertSession(h.bridgeSessionID, ""); err != nil {
		t.Fatalf("seed UpsertSession: %v", err)
	}
	walID, err := h.state.InsertWAL(WALRow{
		BridgeSessionID: h.bridgeSessionID,
		Intent:          "start",
	})
	if err != nil {
		t.Fatalf("seed InsertWAL: %v", err)
	}
	h.pendingWALID = walID
	h.pendingIntent = "start"
	h.pendingParent = ""
	return walID
}

// stageResume simulates what handleStart's resume branch does (without
// actually spawning CC).
func stageResume(h *Harness, parentHarnessID string) {
	h.pendingWALID = 0
	h.pendingIntent = "resume"
	h.pendingParent = parentHarnessID
	h.sessionID = parentHarnessID
}

// stageFork simulates what handleStart's fork branch does (without actually
// spawning CC). parentHarnessID is the harness UUID being forked from — the
// same value handleStart receives in params.Fork.
func stageFork(t *testing.T, h *Harness, parentHarnessID string) int64 {
	t.Helper()
	walID, err := h.state.InsertWAL(WALRow{
		BridgeSessionID: h.bridgeSessionID,
		Intent:          "fork",
		ParentHarnessID: parentHarnessID,
	})
	if err != nil {
		t.Fatalf("seed InsertWAL fork: %v", err)
	}
	h.pendingWALID = walID
	h.pendingIntent = "fork"
	h.pendingParent = parentHarnessID
	return walID
}

// TestRecordChainOnInit_StartHappyPath confirms cold-start commits the WAL,
// inserts a kind='start' rollout, and rotates current_harness_id.
func TestRecordChainOnInit_StartHappyPath(t *testing.T) {
	h := newTestHarness(t, "bsid-1")
	stageStart(t, h)

	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	pending, err := h.state.ListPendingWAL()
	if err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("WAL must be committed: %d pending rows remain", len(pending))
	}

	rs, err := h.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 1 {
		t.Fatalf("want 1 rollout, got %d (%+v)", len(rs), rs)
	}
	if rs[0].Kind != "start" || rs[0].Sequence != 0 || rs[0].HarnessSessionID != "uuid-a" || rs[0].ParentHarnessID != "" {
		t.Fatalf("rollout shape: %+v", rs[0])
	}

	row, err := h.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "uuid-a" {
		t.Fatalf("session current_harness_id = %q, want uuid-a", row.CurrentHarnessID)
	}

	if h.pendingWALID != 0 || h.pendingIntent != "" || h.pendingParent != "" {
		t.Fatalf("pending state must be cleared after init: walID=%d intent=%q parent=%q",
			h.pendingWALID, h.pendingIntent, h.pendingParent)
	}
}

// TestRecordChainOnInit_ResumeSameUUIDTouchesUpdatedAt confirms the common
// case: CC's --resume returns the same UUID, so we just bump
// sessions.updated_at without inserting a rollout row.
func TestRecordChainOnInit_ResumeSameUUIDTouchesUpdatedAt(t *testing.T) {
	h := newTestHarness(t, "bsid-1")

	// Seed: a prior cold-start landed.
	stageStart(t, h)
	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	pre, err := h.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("seed GetSession: %v", err)
	}
	preCount := func() int {
		rs, err := h.state.ListRollouts("bsid-1")
		if err != nil {
			t.Fatalf("ListRollouts: %v", err)
		}
		return len(rs)
	}()
	if preCount != 1 {
		t.Fatalf("seed must produce 1 rollout, got %d", preCount)
	}

	// Resume returns the same UUID — the expected case under current CC
	// semantics. State must touch updated_at but NOT add a rollout.
	stageResume(h, "uuid-a")
	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	rs, err := h.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != preCount {
		t.Fatalf("resume same-UUID must NOT add a rollout: got %d, want %d", len(rs), preCount)
	}

	post, err := h.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if post.CurrentHarnessID != "uuid-a" {
		t.Fatalf("current_harness_id should remain uuid-a, got %q", post.CurrentHarnessID)
	}
	if !post.UpdatedAt.After(pre.UpdatedAt) && !post.UpdatedAt.Equal(pre.UpdatedAt) {
		t.Fatalf("updated_at should be >= seed updated_at: pre=%v post=%v", pre.UpdatedAt, post.UpdatedAt)
	}

	// WAL untouched (no rotation, no orphans).
	if pending, err := h.state.ListPendingWAL(); err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("resume must not open WAL rows: got %d pending", len(pending))
	}

	if h.pendingWALID != 0 || h.pendingIntent != "" || h.pendingParent != "" {
		t.Fatalf("pending state must be cleared after resume init")
	}
}

// TestRecordChainOnInit_ResumeRotatedUUIDInsertsResumeRollout exercises the
// defensive guard: if CC ever rotates the UUID on --resume, the chain
// records that as a kind='resume' rollout with parent set to the previous
// UUID. This shouldn't happen under current CC semantics but the guard
// keeps the chain spec-correct if upstream changes.
func TestRecordChainOnInit_ResumeRotatedUUIDInsertsResumeRollout(t *testing.T) {
	h := newTestHarness(t, "bsid-1")

	// Seed cold start with uuid-a.
	stageStart(t, h)
	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	// Resume but CC unexpectedly returns a new UUID.
	stageResume(h, "uuid-a")
	h.recordChainOnInit("uuid-b", "/tmp/b.jsonl")

	rs, err := h.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 rollouts after rotated resume, got %d (%+v)", len(rs), rs)
	}
	if rs[1].Kind != "resume" || rs[1].HarnessSessionID != "uuid-b" || rs[1].ParentHarnessID != "uuid-a" || rs[1].Sequence != 1 {
		t.Fatalf("rotated-resume rollout shape: %+v", rs[1])
	}

	row, err := h.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "uuid-b" {
		t.Fatalf("current_harness_id should rotate to uuid-b, got %q", row.CurrentHarnessID)
	}

	if pending, err := h.state.ListPendingWAL(); err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("resume rotation should not leave pending WAL rows: got %d", len(pending))
	}
}

// TestRecordChainOnInit_NoPendingIntentIsNoOp confirms an init event with no
// staged intent does nothing — used when a stray init slips through (e.g.
// after an interrupt).
func TestRecordChainOnInit_NoPendingIntentIsNoOp(t *testing.T) {
	h := newTestHarness(t, "bsid-1")

	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	rs, err := h.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 0 {
		t.Fatalf("no-pending-intent init must not write rollouts: got %d", len(rs))
	}

	if _, err := h.state.GetSession("bsid-1"); err == nil {
		t.Fatalf("no-pending-intent init must not write a sessions row")
	}
}

// TestRecordChainOnInit_ForkAppendsRollout exercises the post-init commit
// path for fork: starting from a parent UUID, CC mints a new UUID; the chain
// must record [start, fork] with the fork's parent_harness_id pointing at
// the start's UUID.
func TestRecordChainOnInit_ForkAppendsRollout(t *testing.T) {
	h := newTestHarness(t, "bsid-1")

	// Seed a cold-start so the chain has [start@uuid-a].
	stageStart(t, h)
	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	// Fork from uuid-a; CC mints uuid-b.
	stageFork(t, h, "uuid-a")
	h.recordChainOnInit("uuid-b", "/tmp/b.jsonl")

	rs, err := h.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 rollouts after fork, got %d (%+v)", len(rs), rs)
	}
	if rs[0].Kind != "start" || rs[0].HarnessSessionID != "uuid-a" || rs[0].Sequence != 0 || rs[0].ParentHarnessID != "" {
		t.Fatalf("start rollout shape: %+v", rs[0])
	}
	if rs[1].Kind != "fork" || rs[1].HarnessSessionID != "uuid-b" || rs[1].ParentHarnessID != "uuid-a" || rs[1].Sequence != 1 {
		t.Fatalf("fork rollout shape: %+v", rs[1])
	}

	row, err := h.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "uuid-b" {
		t.Fatalf("current_harness_id should rotate to uuid-b, got %q", row.CurrentHarnessID)
	}

	if pending, err := h.state.ListPendingWAL(); err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("fork must commit its WAL row: got %d pending", len(pending))
	}

	if h.pendingWALID != 0 || h.pendingIntent != "" || h.pendingParent != "" {
		t.Fatalf("pending state must be cleared after fork init")
	}
}

// TestForkCrashBeforeInitOrphansWAL covers the recovery path: the bridge
// dies between staging the fork WAL row and CC's init event delivering the
// new UUID. The pending row must survive the crash and be marked orphaned
// on the next bridge boot — no spurious kind='fork' rollout should appear.
func TestForkCrashBeforeInitOrphansWAL(t *testing.T) {
	h := newTestHarness(t, "bsid-1")

	// Cold-start landed in a previous bridge process.
	stageStart(t, h)
	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	// Stage a fork, then "crash" — drop the harness without ever calling
	// recordChainOnInit. The pending WAL row stays in state.db.
	stageFork(t, h, "uuid-a")

	// Simulate the next bridge boot.
	if err := recoverOrphansOnBoot(h.state); err != nil {
		t.Fatalf("recoverOrphansOnBoot: %v", err)
	}

	// No more pending rows.
	if pending, err := h.state.ListPendingWAL(); err != nil {
		t.Fatalf("ListPendingWAL: %v", err)
	} else if len(pending) != 0 {
		t.Fatalf("orphan recovery must clear pending: got %d", len(pending))
	}

	// Rollouts unchanged — still just the seed start row.
	rs, err := h.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 1 || rs[0].Kind != "start" {
		t.Fatalf("orphaned fork must not produce a rollout: got %+v", rs)
	}

	// current_harness_id stays on uuid-a; the failed fork did not rotate.
	row, err := h.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "uuid-a" {
		t.Fatalf("orphaned fork must not rotate current_harness_id: got %q", row.CurrentHarnessID)
	}
}

// TestRecordChainOnInit_ResumeWithEmptyParentTouchesOnly covers the corner
// where pendingParent ended up empty (e.g. resume staged before any prior
// session was recorded). Must still bump updated_at, not add a rollout.
func TestRecordChainOnInit_ResumeWithEmptyParentTouchesOnly(t *testing.T) {
	h := newTestHarness(t, "bsid-1")
	if err := h.state.UpsertSession("bsid-1", "uuid-a"); err != nil {
		t.Fatalf("seed UpsertSession: %v", err)
	}

	stageResume(h, "")
	h.recordChainOnInit("uuid-a", "/tmp/a.jsonl")

	rs, err := h.state.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 0 {
		t.Fatalf("empty-parent resume must not add a rollout, got %d", len(rs))
	}
	row, err := h.state.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.CurrentHarnessID != "uuid-a" {
		t.Fatalf("current_harness_id should remain uuid-a, got %q", row.CurrentHarnessID)
	}
}
