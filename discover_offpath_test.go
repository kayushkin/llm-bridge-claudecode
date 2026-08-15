package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The tests in this file pin the numbers in discover.go that sit OFF the
// truncation path: the two index offsets and two bounds in
// classifySubagentPath, the "latest rollout" index in buildStoredSession, and
// the scanner's line ceiling in parseSessionHead.
//
// They exist because scripts/sabotage-offpath.py scored those five at
// UNNOTICED. discover_test.go already drives both subagent layouts with exact
// assertions, so the mechanisms were covered; what nothing asked was what the
// numbers ARE. Every fixture here writes its own literals rather than deriving
// them from the production expression, so a mutation that moves the production
// number cannot move the fixture with it.

// TestClassifySubagentPathShallowestPathTheGuardAccepts pins the lower bound of
// `subIdx < 2` from above: two segments before `subagents` is the shallowest
// path the guard lets through, and at that depth the project degenerates to "/"
// because the segment it reads is the empty string before the leading slash.
//
// This asserts what the function does at the boundary, not what it ought to do.
// Whether a path this shallow should be classified at all is a separate
// question; the point here is that moving the guard to `< 3` changes the answer
// and nothing noticed.
func TestClassifySubagentPathShallowestPathTheGuardAccepts(t *testing.T) {
	// parts: ["", "proj", "subagents", "agent-x.jsonl"] -> subIdx == 2.
	source, project, parent, ok := classifySubagentPath("/proj/subagents/agent-x.jsonl")
	if !ok {
		t.Fatalf("ok = false at subIdx == 2; two segments above `subagents` is the shallowest depth the guard accepts, and raising it silently drops a whole layout")
	}
	if source != "subagent" {
		t.Errorf("source = %q, want %q", source, "subagent")
	}
	if parent != "proj" {
		t.Errorf("parent = %q, want %q — the segment directly above `subagents`", parent, "proj")
	}
	if project != "/" {
		t.Errorf("project = %q, want %q — at this depth the project segment is the empty string before the leading slash", project, "/")
	}
}

// TestClassifySubagentPathRejectsAPathWithNoRoomForTheProject pins the same
// guard from below. One segment above `subagents` leaves nothing for the
// project to be read from, and `parts[subIdx-2]` would index -1.
//
// A Go slice index of -1 panics, so relaxing the guard to `< 1` does not return
// a wrong answer — it takes the process down. `/subagents/agent-x.jsonl` is a
// perfectly well-formed absolute path, which is what makes this worth a test
// rather than a comment.
func TestClassifySubagentPathRejectsAPathWithNoRoomForTheProject(t *testing.T) {
	// parts: ["", "subagents", "agent-x.jsonl"] -> subIdx == 1.
	source, project, parent, ok := classifySubagentPath("/subagents/agent-x.jsonl")
	if ok {
		t.Fatalf("ok = true at subIdx == 1; there is no segment for the project here and reading one indexes past the start of the slice")
	}
	if source != "" || project != "" || parent != "" {
		t.Errorf("a rejected path must report nothing, got source=%q project=%q parent=%q", source, project, parent)
	}
}

// TestClassifySubagentPathBoundsTheWorkflowsLookahead drives the one input the
// lookahead's bounds check exists for: a path whose LAST segment is
// `subagents`, so there is no segment after it to compare against "workflows".
//
// `subIdx+1 < len(parts)` is the whole protection. Widening it to `<=` reads
// one past the end of the slice and panics. No fixture in discover_test.go ends
// at `subagents` — every one of them has an agent file below it — so the guard
// shipped with nothing exercising it.
func TestClassifySubagentPathBoundsTheWorkflowsLookahead(t *testing.T) {
	// parts: ["", "proj", "uuid", "subagents"] -> subIdx == 3 == len(parts)-1.
	source, project, parent, ok := classifySubagentPath("/proj/uuid/subagents")
	if !ok {
		t.Fatalf("ok = false; the path carries a `subagents` segment deep enough to classify")
	}
	if source != "subagent" {
		t.Errorf("source = %q, want %q — there is no segment after `subagents`, so the workflow layout cannot match", source, "subagent")
	}
	if project != "/proj" {
		t.Errorf("project = %q, want %q", project, "/proj")
	}
	if parent != "uuid" {
		t.Errorf("parent = %q, want %q", parent, "uuid")
	}
}

// TestBuildStoredSessionReadsTheLatestRolloutNotTheFirst pins the index behind
// the word "latest". ListRollouts returns rows ordered by sequence ASC, so the
// newest is the LAST element, and every existing test supplies exactly one
// rollout — a one-element slice cannot tell rollouts[0] from
// rollouts[len-1].
//
// The failure this protects against is quiet: a resumed session would show the
// prompt and path of the conversation it forked from, which reads like stale
// data rather than like a bug.
func TestBuildStoredSessionReadsTheLatestRolloutNotTheFirst(t *testing.T) {
	projectsDir, home := withCCHome(t)

	st, err := OpenState(filepath.Join(home, ".local", "share", "llm-bridge-claudecode", "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	defer st.Close()

	if err := st.UpsertSession("bsid-1", "uuid-second"); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	firstPath := filepath.Join(projectsDir, "-tmp-proj", "uuid-first.jsonl")
	secondPath := filepath.Join(projectsDir, "-tmp-proj", "uuid-second.jsonl")
	writeJSONL(t, firstPath, "the earlier conversation")
	writeJSONL(t, secondPath, "the resumed conversation")

	// Inserted in sequence order: 0 is the original, 1 is the resume.
	for i, r := range []RolloutRow{
		{HarnessSessionID: "uuid-first", BridgeSessionID: "bsid-1", RolloutPath: firstPath, Sequence: 0, Kind: "start"},
		{HarnessSessionID: "uuid-second", BridgeSessionID: "bsid-1", RolloutPath: secondPath, Sequence: 1, Kind: "resume"},
	} {
		r.CreatedAt = time.Now().UTC()
		if err := st.InsertRollout(r); err != nil {
			t.Fatalf("InsertRollout %d: %v", i, err)
		}
	}

	rs, err := st.ListRollouts("bsid-1")
	if err != nil {
		t.Fatalf("ListRollouts: %v", err)
	}
	if len(rs) != 2 {
		t.Fatalf("want 2 rollouts to distinguish first from latest, got %d", len(rs))
	}
	row, err := st.GetSession("bsid-1")
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}

	ss := buildStoredSession(*row, rs)
	if ss.Path != secondPath {
		t.Errorf("path = %q, want the LATEST rollout %q", ss.Path, secondPath)
	}
	if ss.Prompt != "the resumed conversation" {
		t.Errorf("prompt = %q, want the latest rollout's; the earlier one is %q", ss.Prompt, "the earlier conversation")
	}
}

// jsonlLineOfExactly returns one CC user line whose length in bytes is exactly
// n, padding the prompt to make up the difference. The caller gets back the
// line without its newline, which is the token bufio.Scanner measures against
// its ceiling.
func jsonlLineOfExactly(t *testing.T, n int) string {
	t.Helper()
	const prefix = `{"type":"user","timestamp":"2026-04-30T12:00:00Z","message":{"content":"`
	const suffix = `"}}`
	pad := n - len(prefix) - len(suffix)
	if pad < 1 {
		t.Fatalf("cannot build a %d-byte line: the envelope alone is %d bytes", n, len(prefix)+len(suffix))
	}
	line := prefix + strings.Repeat("a", pad) + suffix
	if len(line) != n {
		t.Fatalf("built a %d-byte line, want %d", len(line), n)
	}
	return line
}

// TestParseSessionHeadReadsTheLongestLineItsCeilingAllows straddles the
// scanner's line ceiling by one byte on each side.
//
// The straddle has to be this tight to be worth anything. bufio.Scanner rejects
// a token of exactly max bytes and accepts max-1 (measured directly against
// bufio, not inferred), so with the shipped 1 MB ceiling the last readable line
// is 1048575 bytes. A fixture anywhere below that passes for every ceiling from
// its own length upward, which is how a 1 MB cap can be moved to 300 KB with a
// green suite.
//
// Both literals here are written out rather than derived from the production
// expression, so a mutation that moves the ceiling cannot move the fixture with
// it.
//
// The over-ceiling half pins the drop itself: a line at or over the ceiling is
// not parsed and does not count as a turn. The drop is no longer SILENT — this
// comment said it was, which stopped being true when parseSessionHead started
// reading scanner.Err() and reporting it. What is dropped has not changed, only
// whether anything says so, and that half is pinned by
// TestParseSessionHeadReportsAnOverLongLineInsteadOfDroppingItInSilence.
func TestParseSessionHeadReadsTheLongestLineItsCeilingAllows(t *testing.T) {
	const (
		longestAccepted = 1048575 // one below the shipped 1 MB ceiling
		firstRejected   = 1048576 // exactly the ceiling, which bufio refuses
	)

	for _, tc := range []struct {
		name      string
		lineBytes int
		wantTurns int
	}{
		{"one byte under the ceiling is read", longestAccepted, 1},
		{"a line at the ceiling is dropped", firstRejected, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(jsonlLineOfExactly(t, tc.lineBytes)+"\n"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			prompt, _, turns := parseSessionHead(path)
			if turns != tc.wantTurns {
				t.Fatalf("turns = %d, want %d for a %d-byte line", turns, tc.wantTurns, tc.lineBytes)
			}
			if tc.wantTurns == 0 {
				if prompt != "" {
					t.Errorf("prompt = %q, want empty: the line was over the ceiling and never parsed", prompt)
				}
				return
			}
			if prompt == "" {
				t.Errorf("prompt is empty for a %d-byte line, which is within the ceiling", tc.lineBytes)
			}
		})
	}
}
