package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kayushkin/llm-bridge/msg"
)

// discoverSessions returns one msg.StoredSession per bridge_session_id known
// to state.db.
//
// Per the session-identity contract (ARCHITECTURE.md "Session Identity &
// Resumption") state.db is the source of truth. The on-disk
// `~/.claude/projects/<dir>/<uuid>.jsonl` tree is only used in two ways:
//
//  1. Cold import — any rollout file whose harness_session_id is NOT yet in
//     state.db.rollouts (legacy data, or sessions started outside the
//     bridge) is imported as a synthetic single-rollout session with
//     bridge_session_id = harness_session_id, sequence=0, kind='start'.
//     Lazy + idempotent: a second discover call sees the rows already
//     present and skips them.
//  2. Metadata extraction — for each emitted session we read the LATEST
//     rollout's on-disk file to populate prompt / turns / created_at.
//
// StoredSession.HarnessSessionID is the harness UUID (sessions.current_harness_id)
// — the field bridge-server dedupes on. StoredSession.BridgeSessionID is the
// chain head (sessions.bridge_session_id); for cold-imported sessions it
// equals the harness UUID, for bridge-spawned sessions it's the `br_*` id
// minted by bridge-server.
//
// projectDir filters the result to sessions whose latest rollout is under
// `<projectsRoot()>/<encoded(projectDir)>/` — that is, under
// `$CLAUDE_CONFIG_DIR/projects` when the variable is set and
// `~/.claude/projects` otherwise. Empty projectDir returns every session.
func discoverSessions(projectDir string) ([]msg.StoredSession, error) {
	projectsDir := projectsRoot()

	st, err := OpenState(DefaultStatePath())
	if err != nil {
		return nil, err
	}
	defer st.Close()

	if err := coldImportRollouts(st, projectsDir); err != nil {
		return nil, err
	}

	all, err := st.AllSessions()
	if err != nil {
		return nil, err
	}

	var projectPrefix string
	if projectDir != "" {
		projectPrefix = filepath.Join(projectsDir, pathToCCProject(projectDir)) + string(filepath.Separator)
	}

	out := make([]msg.StoredSession, 0, len(all))
	for _, sess := range all {
		rollouts, err := st.ListRollouts(sess.BridgeSessionID)
		if err != nil {
			return nil, err
		}
		ss := buildStoredSession(sess, rollouts)
		if projectPrefix != "" {
			if ss.Path == "" || !strings.HasPrefix(ss.Path, projectPrefix) {
				continue
			}
		}
		out = append(out, ss)
	}
	return out, nil
}

// coldImportRollouts walks projectsDir (CC's per-project session tree) and
// inserts a synthetic session + rollout row for every .jsonl file whose
// harness_session_id is not already in state.db.rollouts. Idempotent:
// re-running on the same tree produces no new rows.
//
// A missing or unreadable projectsDir is not an error — it just means there
// is nothing to cold-import (fresh install, or no claude CLI history yet).
func coldImportRollouts(st *State, projectsDir string) error {
	known, err := loadKnownHarnessIDs(st)
	if err != nil {
		return err
	}

	walkErr := filepath.WalkDir(projectsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Permission errors on subdirs shouldn't kill the whole import;
			// keep walking.
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
				return nil
			}
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}

		id := strings.TrimSuffix(d.Name(), ".jsonl")
		if id == "" {
			return nil
		}
		if _, ok := known[id]; ok {
			return nil
		}

		info, _ := d.Info()
		_, ts, _ := parseSessionHead(path)
		created := ts
		if created.IsZero() && info != nil {
			created = info.ModTime()
		}

		// Synthetic chain: bridge_session_id = harness_session_id, single
		// rollout at sequence 0 with kind 'start' and no parent. When the
		// user later resumes via the bridge, recordChainOnInit will append
		// a kind='resume' rollout (under the defensive UUID-rotation guard)
		// or simply touch updated_at if CC keeps the same UUID.
		if err := st.UpsertSession(id, id); err != nil {
			return err
		}
		if err := st.InsertRollout(RolloutRow{
			HarnessSessionID: id,
			BridgeSessionID:  id,
			RolloutPath:      path,
			Sequence:         0,
			Kind:             "start",
			CreatedAt:        created,
		}); err != nil {
			return err
		}
		known[id] = struct{}{}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.ErrNotExist) {
		return walkErr
	}
	return nil
}

// loadKnownHarnessIDs returns the set of harness_session_ids already present
// in state.db.rollouts across all sessions.
func loadKnownHarnessIDs(st *State) (map[string]struct{}, error) {
	known := map[string]struct{}{}
	all, err := st.AllSessions()
	if err != nil {
		return nil, err
	}
	for _, sess := range all {
		rs, err := st.ListRollouts(sess.BridgeSessionID)
		if err != nil {
			return nil, err
		}
		for _, r := range rs {
			known[r.HarnessSessionID] = struct{}{}
		}
	}
	return known, nil
}

// buildStoredSession projects a (session, rollouts) pair into a
// msg.StoredSession. Metadata (prompt, turns, project, created_at, path)
// comes from the LATEST rollout's on-disk file when available; if the file
// is missing or rollouts are empty the StoredSession still ships with
// whatever the state.db rows themselves carry.
func buildStoredSession(sess SessionRow, rollouts []RolloutRow) msg.StoredSession {
	out := msg.StoredSession{
		HarnessSessionID: sess.CurrentHarnessID,
		BridgeSessionID:  sess.BridgeSessionID,
		Harness:          msg.HarnessClaudeCode,
		CreatedAt:        sess.CreatedAt,
		UpdatedAt:        sess.UpdatedAt,
	}

	if len(rollouts) == 0 {
		return out
	}

	// rollouts is ordered by sequence ASC (per ListRollouts), so the latest
	// is the last element.
	latest := rollouts[len(rollouts)-1]

	// Backfill the rollout path if state.db has it empty (e.g. when init
	// arrived before CC had created the .jsonl on disk).
	path := latest.RolloutPath
	if path == "" {
		path = findRolloutForUUID(latest.HarnessSessionID)
	}

	if path != "" {
		out.Path = path
		if source, project, parent, ok := classifySubagentPath(path); ok {
			// Structurally-spawned subagent. Tag it so bridge-server buckets it
			// via SOURCE_FOLDERS instead of surfacing it as a top-level chat,
			// and report the parent CC recorded in the path so bridge-server
			// can resolve it to a session and write the lineage link.
			out.Source = source
			out.Project = project
			out.ParentHarnessSessionID = parent
		} else {
			// Project is encoded into the parent directory name.
			out.Project = ccProjectToPath(filepath.Base(filepath.Dir(path)))
		}
		if info, err := os.Stat(path); err == nil {
			out.UpdatedAt = info.ModTime()
		}
		if prompt, ts, turns := parseSessionHead(path); prompt != "" {
			out.Prompt = prompt
			out.TurnCount = turns
			if !ts.IsZero() {
				out.CreatedAt = ts
			}
		}
	}

	return out
}

// classifySubagentPath inspects a rollout file path for Claude Code's
// subagent layout and, if matched, returns the structural source tag, the
// project root, and the parent session's harness id. Two layouts exist:
//
//	<projects>/<proj>/<parent-uuid>/subagents/agent-*.jsonl                   → "subagent"
//	<projects>/<proj>/<parent-uuid>/subagents/workflows/<wf-id>/agent-*.jsonl → "workflow-subagent"
//
// The match keys on a `subagents` path segment at ANY depth, not just the
// immediate parent, so the deeper Workflow-tool layout is classified too —
// the original check only matched the one-level Task() layout, which left
// workflow subagents untagged (empty type/purpose, origin falling back to the
// harness name). The project is the directory two levels above `subagents`
// (`<projects>/<proj>/<parent-uuid>/subagents` → `<proj>`), so it is correct
// regardless of how deeply the agent file nests below `subagents`. Returns
// ok=false when the path carries no `subagents` segment or is too shallow to
// hold the `<proj>/<parent-uuid>/subagents` prefix.
//
// parent is the segment directly above `subagents`, which is the parent
// session's own UUID — the same id its top-level rollout file is named for.
// This function used to read parts[subIdx-2] for the project and simply skip
// parts[subIdx-1], discarding the one piece of lineage Claude Code records on
// disk; every discovered subagent then landed with no link to what spawned it.
func classifySubagentPath(path string) (source, project, parent string, ok bool) {
	parts := strings.Split(path, string(filepath.Separator))
	subIdx := -1
	for i, p := range parts {
		if p == "subagents" {
			subIdx = i
			break
		}
	}
	if subIdx < 2 {
		return "", "", "", false
	}
	source = "subagent"
	if subIdx+1 < len(parts) && parts[subIdx+1] == "workflows" {
		source = "workflow-subagent"
	}
	return source, ccProjectToPath(parts[subIdx-2]), parts[subIdx-1], true
}

// parseSessionHead scans a CC session JSONL file to extract the first user
// prompt, timestamp, and turn count.
//
// A line over the scanner's ceiling ends the scan where it sits, so the counts
// returned describe the file up to that line and nothing after it. That is
// reported, not propagated — see the end of the function for why.
func parseSessionHead(path string) (prompt string, ts time.Time, turns int) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	// The largest line this accepts is 1024*1024 - 1 bytes, not 1024*1024. A
	// line of exactly the cap fills the buffer with no newline in it, so the
	// token-too-long check fires before the split can succeed. The two spellings
	// are the starting buffer and the cap, not two separate ceilings — the
	// effective one is the larger of the two, and the zero-length starting slice
	// only sets how much is allocated up front. Pinned from both sides by
	// TestParseSessionHeadReadsTheLongestLineItsCeilingAllows.
	scanner.Buffer(make([]byte, 0, 256*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var entry struct {
			Type      string          `json:"type"`
			Timestamp string          `json:"timestamp"`
			Message   json.RawMessage `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil {
			continue
		}

		if entry.Type == "user" {
			turns++
			// Extract first user message as prompt
			if prompt == "" {
				prompt = extractUserContent(entry.Message)
				if prompt != "" {
					prompt = truncateAtRuneBoundary(prompt, 200)
				}
				if ts.IsZero() && entry.Timestamp != "" {
					ts, _ = time.Parse(time.RFC3339Nano, entry.Timestamp)
				}
			}
		}
	}

	// bufio.Scanner ends a scan on ErrTooLong exactly as it ends one at EOF —
	// Scan() returns false — so without this read an over-long line is
	// indistinguishable from a clean end of file, and a session with 500 user
	// turns behind one long line reports the turns it managed to reach as if
	// that were the whole file.
	//
	// Deliberately NOT propagated. This function has no error return, and the
	// two callers cannot agree on what one would mean: coldImportRollouts calls
	// it from inside a filepath.WalkDir callback, where a returned error aborts
	// the entire import and drops every session after this one. Losing a turn
	// count is worth reporting; losing the rest of the walk is not. Whether an
	// over-ceiling line should be skipped or should kill the read is the open
	// fleet question 6fbf83b3 — this changes nothing there, it only stops the
	// failure being silent.
	if err := scanner.Err(); err != nil {
		log.Printf("parseSessionHead: %s: scan stopped early: %v; "+
			"reporting the first %d turn(s) only — any turn after the over-long line is not counted",
			path, err, turns)
	}

	return prompt, ts, turns
}

// findRolloutForUUID does a best-effort scan of <projectsRoot()>/*/<uuid>.jsonl
// for a file matching the given Claude Code session UUID. Returns "" if not
// found — caller treats that as "rollout file not yet on disk" and proceeds
// without the path. The path can be backfilled later by re-globbing.
func findRolloutForUUID(uuid string) string {
	if uuid == "" {
		return ""
	}
	projectsDir := projectsRoot()
	target := uuid + ".jsonl"
	var found string
	_ = filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == target {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// pathToCCProject converts a filesystem path to Claude Code's project directory name.
// /home/user/repos → -home-user-repos
func pathToCCProject(path string) string {
	return strings.ReplaceAll(path, "/", "-")
}

// ccProjectToPath converts a CC project directory name back to a filesystem path.
// -home-user-repos → /home/user/repos
func ccProjectToPath(name string) string {
	if name == "" || name == "-" {
		return "/"
	}
	// CC format: leading dash is the root /, subsequent dashes are path separators
	return "/" + strings.ReplaceAll(strings.TrimPrefix(name, "-"), "-", "/")
}

// truncateAtRuneBoundary returns the longest prefix of s that is no longer than
// maxBytes and does not end part-way through a multi-byte UTF-8 sequence.
//
// Cutting a Go string at a fixed byte offset splits whatever rune straddles that
// offset, and the result is not valid UTF-8. Nothing reports it: encoding/json
// substitutes U+FFFD rather than failing, so the reader sees a replacement
// character and no error is raised anywhere along the way. The one caller here
// cuts a discovered session's first user message down to a label, and that label
// is encoded into msg.StoredSession.Prompt and crosses to bridge-server — so a
// split rune survives the request and a reload does not fix it.
//
// The walk-back costs at most three byte comparisons and allocates nothing,
// which is why it is preferred here over converting to []rune.
func truncateAtRuneBoundary(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	// s[cut] is the first byte past the prefix. While it is a continuation
	// byte, a rune straddles the cut, so move the cut earlier.
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// extractUserContent extracts text from a CC user message.
// CC stores user message.content as a plain string, not structured blocks.
func extractUserContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	// First, try to unmarshal the message object and get content field
	var msgObj struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msgObj) == nil && len(msgObj.Content) > 0 {
		// Content field exists, try as plain string first
		var str string
		if json.Unmarshal(msgObj.Content, &str) == nil {
			return str
		}

		// Try as array of content blocks
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(msgObj.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "text" && b.Text != "" {
					return b.Text
				}
			}
		}
	}

	return ""
}
