package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// StoredSession.ID is always the bridge_session_id, never the
// harness_session_id. Frontend uses this to start sessions; bridge-server
// looks up the chain in state.db on resume.
//
// projectDir filters the result to sessions whose latest rollout is under
// `~/.claude/projects/<encoded(projectDir)>/`. Empty projectDir returns
// every session.
func discoverSessions(projectDir string) ([]msg.StoredSession, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	projectsDir := filepath.Join(home, ".claude", "projects")

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
		ID:        sess.BridgeSessionID,
		Harness:   msg.HarnessClaudeCode,
		CreatedAt: sess.CreatedAt,
		UpdatedAt: sess.UpdatedAt,
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
		// Project is encoded into the parent directory name.
		out.Project = ccProjectToPath(filepath.Base(filepath.Dir(path)))
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

// parseSessionHead scans a CC session JSONL file to extract the first user
// prompt, timestamp, and turn count.
func parseSessionHead(path string) (prompt string, ts time.Time, turns int) {
	f, err := os.Open(path)
	if err != nil {
		return "", time.Time{}, 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
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
					prompt = truncate(prompt, 200)
				}
				if ts.IsZero() && entry.Timestamp != "" {
					ts, _ = time.Parse(time.RFC3339Nano, entry.Timestamp)
				}
			}
		}
	}

	return prompt, ts, turns
}

// findRolloutForUUID does a best-effort scan of ~/.claude/projects/*/<uuid>.jsonl
// for a file matching the given Claude Code session UUID. Returns "" if not
// found — caller treats that as "rollout file not yet on disk" and proceeds
// without the path. The path can be backfilled later by re-globbing.
func findRolloutForUUID(uuid string) string {
	if uuid == "" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	projectsDir := filepath.Join(home, ".claude", "projects")
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

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
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
