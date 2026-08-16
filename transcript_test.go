package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTranscript lays down a conversation file the way Claude Code does:
// under <config>/projects/<cwd with "/" replaced by "-">/<uuid>.jsonl, with the
// real cwd recorded inside it.
func writeTranscript(t *testing.T, root, cwd, uuid string, firstLines ...string) string {
	t.Helper()
	slug := ""
	for _, r := range cwd {
		if r == '/' {
			slug += "-"
		} else {
			slug += string(r)
		}
	}
	dir := filepath.Join(root, "projects", slug)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := ""
	for _, l := range firstLines {
		body += l + "\n"
	}
	if len(firstLines) == 0 {
		body = `{"type":"user","cwd":"` + cwd + `","message":{"role":"user"}}` + "\n"
	}
	p := filepath.Join(dir, uuid+".jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return p
}

func TestTranscriptWorkingDir_FoundByScan(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	writeTranscript(t, root, "/", "0d69dd7d-7918-4380-8cce-074498d42b33")

	dir, ok := transcriptWorkingDir(nil, "0d69dd7d-7918-4380-8cce-074498d42b33")
	if !ok {
		t.Fatal("transcript not found by scan")
	}
	if dir != "/" {
		t.Fatalf("working dir = %q; want %q", dir, "/")
	}
}

// The regression this whole file exists for. A conversation created when the
// harness inherited "/" must still be found once the harness is started in the
// instance's directory instead — the transcript did not move.
func TestTranscriptWorkingDir_SurvivesTheHarnessBeingMoved(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	const uuid = "11111111-2222-3333-4444-555555555555"
	writeTranscript(t, root, "/", uuid)

	// The caller now asks for /home/kayushkincom/repos. The answer must still
	// be "/", because that is where Claude Code can find this conversation.
	dir, ok := transcriptWorkingDir(nil, uuid)
	if !ok || dir != "/" {
		t.Fatalf("got (%q, %v); want (\"/\", true)", dir, ok)
	}
}

// The directory NAME is lossy: "/home/my-dir" and "/home/my/dir" both encode to
// "-home-my-dir". Decoding it back would hand Claude Code a directory the
// conversation is not in, so the cwd is read from inside the file instead.
func TestTranscriptWorkingDir_PrefersRecordedCwdOverTheLossyDirName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	const uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	// Slug "-home-my-dir" — ambiguous. The file says which one it really was.
	writeTranscript(t, root, "/home/my-dir", uuid)

	dir, ok := transcriptWorkingDir(nil, uuid)
	if !ok {
		t.Fatal("not found")
	}
	if dir != "/home/my-dir" {
		t.Fatalf("working dir = %q; want %q — decoded from the directory name instead of read from the file", dir, "/home/my-dir")
	}
}

// A transcript whose opening records carry no cwd reports not-found rather than
// guessing. The caller's configured directory is a better answer than a wrong
// one, and a wrong one resumes nothing.
func TestTranscriptWorkingDir_NoRecordedCwdIsNotFound(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	const uuid = "99999999-8888-7777-6666-555555555555"
	writeTranscript(t, root, "/somewhere", uuid,
		`{"type":"summary","summary":"no cwd here"}`,
		`{"type":"meta","note":"still none"}`,
	)

	if dir, ok := transcriptWorkingDir(nil, uuid); ok {
		t.Fatalf("got (%q, true); want not-found", dir)
	}
}

func TestTranscriptWorkingDir_UnknownSessionIsNotFound(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	writeTranscript(t, root, "/", "11111111-1111-1111-1111-111111111111")

	if dir, ok := transcriptWorkingDir(nil, "22222222-2222-2222-2222-222222222222"); ok {
		t.Fatalf("got (%q, true); want not-found for a uuid with no transcript", dir)
	}
}

// state.db is the fast path: it already records every rollout this harness has
// opened, so the usual answer is one indexed lookup rather than a walk of every
// directory Claude Code has ever run in.
func TestTranscriptWorkingDir_UsesStateDBPathWhenRecorded(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	const uuid = "abcdefab-cdef-abcd-efab-cdefabcdefab"
	path := writeTranscript(t, root, "/recorded/dir", uuid)

	st, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()
	if err := st.UpsertSession("br_test", uuid); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: uuid, BridgeSessionID: "br_test",
		RolloutPath: path, Sequence: 1, Kind: "start",
	}); err != nil {
		t.Fatalf("insert rollout: %v", err)
	}

	if got, err := st.RolloutPathFor(uuid); err != nil || got != path {
		t.Fatalf("RolloutPathFor = (%q, %v); want %q", got, err, path)
	}
	dir, ok := transcriptWorkingDir(st, uuid)
	if !ok || dir != "/recorded/dir" {
		t.Fatalf("got (%q, %v); want (\"/recorded/dir\", true)", dir, ok)
	}
}

// A recorded path that no longer exists must not stop the scan finding the
// file. Transcripts get moved and pruned; the row outlives them.
func TestTranscriptWorkingDir_StaleStateDBPathFallsBackToScan(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", root)
	const uuid = "12341234-1234-1234-1234-123412341234"
	writeTranscript(t, root, "/real/dir", uuid)

	st, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()
	if err := st.UpsertSession("br_test", uuid); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: uuid, BridgeSessionID: "br_test",
		RolloutPath: "/gone/nowhere/" + uuid + ".jsonl", Sequence: 1, Kind: "start",
	}); err != nil {
		t.Fatalf("insert rollout: %v", err)
	}

	dir, ok := transcriptWorkingDir(st, uuid)
	if !ok || dir != "/real/dir" {
		t.Fatalf("got (%q, %v); want (\"/real/dir\", true) via the scan fallback", dir, ok)
	}
}

func TestRolloutPathFor_EmptyWhenOnlyBlankPathsRecorded(t *testing.T) {
	const uuid = "55555555-5555-5555-5555-555555555555"
	st, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("open state: %v", err)
	}
	defer st.Close()
	if err := st.UpsertSession("br_test", uuid); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// 470 of the 7,838 live rows look exactly like this.
	if err := st.InsertRollout(RolloutRow{
		HarnessSessionID: uuid, BridgeSessionID: "br_test", Sequence: 1, Kind: "start",
	}); err != nil {
		t.Fatalf("insert rollout: %v", err)
	}

	got, err := st.RolloutPathFor(uuid)
	if err != nil {
		t.Fatalf("RolloutPathFor errored on a blank-path row: %v", err)
	}
	if got != "" {
		t.Fatalf("RolloutPathFor = %q; want empty", got)
	}
}

// The scanner ceiling in transcriptWorkingDir, pinned from both sides one byte
// apart. Nothing held this value before: the 194th nightly pass examined it and
// left it unpinned, so the cap could be raised or lowered with a green suite.
//
// The straddle is deliberately tighter than any plausible drift. 8388607 is the
// longest opening line a transcript may carry and still be read; 8388608 is the
// first that is not. The literal here is written out independently of
// transcript.go's own expression so that moving one and not the other fails.
//
// Both cases put the long line BEFORE the line carrying the cwd, because that
// is the only order in which the ceiling is reachable at all — a cwd on line 1
// returns before the scanner ever meets the long line.
func TestTranscriptWorkingDir_ScannerCeilingStraddle(t *testing.T) {
	const ceiling = 8 * 1024 * 1024 // must match transcript.go's sc.Buffer max

	// padLine returns a well-formed JSONL record with no cwd, exactly n bytes long.
	padLine := func(t *testing.T, n int) string {
		t.Helper()
		const prefix, suffix = `{"type":"meta","pad":"`, `"}`
		line := prefix + strings.Repeat("x", n-len(prefix)-len(suffix)) + suffix
		if len(line) != n {
			t.Fatalf("pad line is %d bytes; want %d", len(line), n)
		}
		return line
	}

	cases := []struct {
		name    string
		padTo   int
		wantOK  bool
		wantDir string
	}{
		{"longest readable opening line", ceiling - 1, true, "/tmp/straddle"},
		{"one byte over the ceiling", ceiling, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", root)
			const uuid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
			writeTranscript(t, root, "/tmp/straddle", uuid,
				padLine(t, tc.padTo),
				`{"type":"user","cwd":"/tmp/straddle","message":{"role":"user"}}`,
			)

			dir, ok := transcriptWorkingDir(nil, uuid)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v; want %v (pad line %d bytes, ceiling %d)", ok, tc.wantOK, tc.padTo, ceiling)
			}
			if dir != tc.wantDir {
				t.Fatalf("dir = %q; want %q", dir, tc.wantDir)
			}
		})
	}
}
