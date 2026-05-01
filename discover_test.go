package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeJSONL drops a minimal CC session .jsonl file at path. The single user
// line is enough for parseSessionHead to recover prompt + ts + a turn count.
func writeJSONL(t *testing.T, path, prompt string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := `{"type":"user","timestamp":"2026-04-30T12:00:00Z","message":{"content":"` + prompt + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// withCCHome points HOME at a temp dir, ensures ~/.claude/projects exists,
// and returns the projects dir + the temp HOME path.
func withCCHome(t *testing.T) (projectsDir, home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	projectsDir = filepath.Join(home, ".claude", "projects")
	if err := os.MkdirAll(projectsDir, 0o755); err != nil {
		t.Fatalf("mkdir projects: %v", err)
	}
	return projectsDir, home
}

// TestDiscover_ColdImportIdempotent confirms cold-import inserts one
// synthetic session per .jsonl on first call and zero new rows on the
// second.
func TestDiscover_ColdImportIdempotent(t *testing.T) {
	projectsDir, _ := withCCHome(t)

	// Two pre-existing CC sessions in different projects.
	writeJSONL(t, filepath.Join(projectsDir, "-tmp-projA", "uuid-a.jsonl"), "hello A")
	writeJSONL(t, filepath.Join(projectsDir, "-tmp-projB", "uuid-b.jsonl"), "hello B")

	got, err := discoverSessions("")
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("first call: want 2 sessions, got %d (%+v)", len(got), got)
	}

	// IDs are bridge_session_ids; cold-import sets them = harness UUID.
	ids := map[string]bool{}
	for _, s := range got {
		ids[s.ID] = true
	}
	if !ids["uuid-a"] || !ids["uuid-b"] {
		t.Fatalf("want both uuid-a and uuid-b in IDs, got %v", ids)
	}

	got2, err := discoverSessions("")
	if err != nil {
		t.Fatalf("second discoverSessions: %v", err)
	}
	if len(got2) != 2 {
		t.Fatalf("second call must be idempotent: want 2 sessions, got %d", len(got2))
	}
}

// TestDiscover_BridgeSessionUsesBridgeID confirms that a session opened via
// the bridge surfaces its bridge_session_id as StoredSession.ID — not the
// harness UUID.
func TestDiscover_BridgeSessionUsesBridgeID(t *testing.T) {
	projectsDir, home := withCCHome(t)

	// Pre-seed state.db with a bridge-spawned session: bridge_id "bsid-1"
	// chained to harness UUID "uuid-x".
	statePath := filepath.Join(home, ".local", "share", "llm-bridge-claudecode", "state.db")
	st, err := OpenState(statePath)
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	rolloutPath := filepath.Join(projectsDir, "-tmp-bridge", "uuid-x.jsonl")
	writeJSONL(t, rolloutPath, "bridge prompt")
	if err := st.UpsertSession("bsid-1", "uuid-x"); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: "uuid-x",
		BridgeSessionID:  "bsid-1",
		RolloutPath:      rolloutPath,
		Sequence:         0,
		Kind:             "start",
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertRollout: %v", err)
	}
	st.Close()

	got, err := discoverSessions("")
	if err != nil {
		t.Fatalf("discoverSessions: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 session (no cold-import double), got %d (%+v)", len(got), got)
	}
	if got[0].ID != "bsid-1" {
		t.Fatalf("ID must be bridge_session_id, got %q", got[0].ID)
	}
	if got[0].Path != rolloutPath {
		t.Fatalf("path mismatch: want %q got %q", rolloutPath, got[0].Path)
	}
	if got[0].Prompt != "bridge prompt" {
		t.Fatalf("prompt should come from latest rollout file, got %q", got[0].Prompt)
	}
}

// TestDiscover_ProjectFilter confirms the project filter restricts to
// sessions whose latest rollout lives under ~/.claude/projects/<encoded>/.
func TestDiscover_ProjectFilter(t *testing.T) {
	projectsDir, _ := withCCHome(t)

	writeJSONL(t, filepath.Join(projectsDir, "-tmp-projA", "uuid-a.jsonl"), "A1")
	writeJSONL(t, filepath.Join(projectsDir, "-tmp-projB", "uuid-b.jsonl"), "B1")

	gotA, err := discoverSessions("/tmp/projA")
	if err != nil {
		t.Fatalf("discoverSessions A: %v", err)
	}
	if len(gotA) != 1 {
		t.Fatalf("project filter A: want 1 session, got %d (%+v)", len(gotA), gotA)
	}
	if !strings.HasSuffix(gotA[0].Path, "/-tmp-projA/uuid-a.jsonl") {
		t.Fatalf("filter let through wrong session: %s", gotA[0].Path)
	}

	gotB, err := discoverSessions("/tmp/projB")
	if err != nil {
		t.Fatalf("discoverSessions B: %v", err)
	}
	if len(gotB) != 1 {
		t.Fatalf("project filter B: want 1 session, got %d (%+v)", len(gotB), gotB)
	}
	if gotB[0].ID != "uuid-b" {
		t.Fatalf("filter B wrong id: %s", gotB[0].ID)
	}

	gotEmpty, err := discoverSessions("")
	if err != nil {
		t.Fatalf("discoverSessions empty: %v", err)
	}
	if len(gotEmpty) != 2 {
		t.Fatalf("empty filter must return all: got %d", len(gotEmpty))
	}
}

// TestDiscover_MissingProjectsDirNoError covers the fresh-install case:
// ~/.claude/projects does not exist, but state.db may still have rows. The
// caller should get whatever's in state.db without an error.
func TestDiscover_MissingProjectsDirNoError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Don't create ~/.claude/projects.

	got, err := discoverSessions("")
	if err != nil {
		t.Fatalf("discoverSessions on empty home: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("empty home should return 0 sessions, got %d", len(got))
	}
}

// TestColdImport_SkipsAlreadyKnown confirms a UUID present in state.db
// rollouts is NOT re-imported as a synthetic row. This guards the "session
// opened by the bridge, then discover called" sequence.
func TestColdImport_SkipsAlreadyKnown(t *testing.T) {
	projectsDir, home := withCCHome(t)

	// Seed state.db: bsid-1 → uuid-x (bridge-spawned chain).
	statePath := filepath.Join(home, ".local", "share", "llm-bridge-claudecode", "state.db")
	st, err := OpenState(statePath)
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	rolloutPath := filepath.Join(projectsDir, "-tmp-proj", "uuid-x.jsonl")
	writeJSONL(t, rolloutPath, "x")
	if err := st.UpsertSession("bsid-1", "uuid-x"); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: "uuid-x",
		BridgeSessionID:  "bsid-1",
		RolloutPath:      rolloutPath,
		Sequence:         0,
		Kind:             "start",
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertRollout: %v", err)
	}

	if err := coldImportRollouts(st, projectsDir); err != nil {
		t.Fatalf("coldImportRollouts: %v", err)
	}

	all, err := st.AllSessions()
	if err != nil {
		t.Fatalf("AllSessions: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("known UUID must NOT trigger a synthetic session: got %d sessions", len(all))
	}
	if all[0].BridgeSessionID != "bsid-1" {
		t.Fatalf("session should remain bsid-1, got %q", all[0].BridgeSessionID)
	}
	st.Close()
}

// TestColdImport_BackfillPathOnEmpty confirms that when a state.db rollout
// has an empty rollout_path (e.g. init arrived before CC wrote the file),
// buildStoredSession backfills via findRolloutForUUID.
func TestColdImport_BackfillPathOnEmpty(t *testing.T) {
	projectsDir, home := withCCHome(t)

	statePath := filepath.Join(home, ".local", "share", "llm-bridge-claudecode", "state.db")
	st, err := OpenState(statePath)
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	defer st.Close()

	// Bridge-spawned session: rollout row recorded with empty path.
	if err := st.UpsertSession("bsid-1", "uuid-x"); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: "uuid-x",
		BridgeSessionID:  "bsid-1",
		RolloutPath:      "", // empty — to be backfilled
		Sequence:         0,
		Kind:             "start",
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("InsertRollout: %v", err)
	}

	// File appears later on disk under the encoded project dir.
	rolloutPath := filepath.Join(projectsDir, "-tmp-proj", "uuid-x.jsonl")
	writeJSONL(t, rolloutPath, "later")

	rs, err := st.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	row, err := st.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	ss := buildStoredSession(*row, rs)
	if ss.Path != rolloutPath {
		t.Fatalf("path should be backfilled to %q, got %q", rolloutPath, ss.Path)
	}
	if ss.Prompt != "later" {
		t.Fatalf("prompt should come from backfilled file, got %q", ss.Prompt)
	}
}
