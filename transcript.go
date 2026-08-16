package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
)

// Where Claude Code keeps a conversation, and which directory it has to be
// resumed from.
//
// Claude Code writes each conversation to
// <config>/projects/<cwd with every "/" replaced by "-">/<uuid>.jsonl, and
// `--resume <uuid>` looks ONLY under the directory matching the process's
// current working directory. A conversation is therefore reachable from the
// directory it was created in and from nowhere else.
//
// That is a Claude Code implementation detail and it stops here. llm-bridge
// asks for a session to be resumed; how this harness finds the conversation is
// its own business, and no layer above it should carry a path under ~/.claude.
//
// It became load-bearing when llm-bridge-server started honouring an
// instance's working_dir (db47bc7). Local harnesses had been inheriting
// bridge-server's own directory ("/"); they began getting the instance's
// instead, and every conversation created before that became unreachable —
// the transcript still on disk, looked for in a directory it was never in.
// The fix is not to pin the instance's configuration: it is for the resume to
// go where the conversation actually is, which only this package can know.

// claudeConfigDir returns the root Claude Code keeps its state under.
// CLAUDE_CONFIG_DIR overrides it; otherwise ~/.claude.
func claudeConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// projectsRoot is the directory holding one sub-directory per working
// directory Claude Code has ever run in.
func projectsRoot() string {
	return filepath.Join(claudeConfigDir(), "projects")
}

// transcriptPath returns the on-disk conversation file for a Claude Code
// session UUID, or "" if there is none.
//
// state.db is asked first because this harness already records it: every
// rollout it has ever opened is stored with its rollout_path, so the usual
// answer costs one indexed lookup and no filesystem walk. The walk is the
// fallback for the rollouts written before that column was populated —
// currently 470 of 7,838 rows — and for conversations this harness never
// opened itself.
func transcriptPath(st *State, uuid string) string {
	if st != nil {
		if p, err := st.RolloutPathFor(uuid); err == nil && p != "" {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	entries, err := os.ReadDir(projectsRoot())
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(projectsRoot(), e.Name(), uuid+".jsonl")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// transcriptWorkingDir returns the directory `--resume uuid` must run in, and
// whether one was found.
//
// The answer is read from the `cwd` Claude Code records inside the transcript
// rather than decoded back out of the directory name. The directory name is
// lossy — it replaces every "/" with "-", so "/home/my-dir" and "/home/my/dir"
// encode identically — and there is no need to guess when the file states it.
//
// A transcript with no cwd on any of its opening lines reports not-found
// rather than a guess: a wrong directory resumes nothing, and a configured
// directory is a better answer than a decoded one.
//
// ⚠️ That argument is about handler.go, and it does not carry to the other
// caller. Not-found means different things at the two call sites:
//
//   - handler.go leaves h.workDir at the directory its caller configured. If
//     that is not where the conversation lives, Claude Code aborts with "No
//     conversation found with session ID" — wrong, but loud.
//   - main.go's pty path (execClaudePTY) has no configured directory to fall
//     back to. It skips the chdir silently and execs claude in whatever cwd
//     bridge-server handed the child, which per its own comment "opens a blank
//     session instead of the one the user asked for" — wrong, and quiet.
//
// So do not read a not-found here as harmless. It also has several producers —
// no transcript file, an unopenable one, an opening line past the scanner
// ceiling below, or genuinely no recorded cwd — and the pty caller cannot tell
// them apart, because none of them says anything.
func transcriptWorkingDir(st *State, uuid string) (string, bool) {
	p := transcriptPath(st, uuid)
	if p == "" {
		return "", false
	}
	f, err := os.Open(p)
	if err != nil {
		return "", false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	// The cwd appears on the first record in practice. Read a bounded number of
	// lines anyway so a leading summary or meta record cannot hide it, and stop
	// well short of walking a transcript that can run to tens of megabytes.
	for i := 0; i < 50 && sc.Scan(); i++ {
		var row struct {
			Cwd string `json:"cwd"`
		}
		if err := json.Unmarshal(sc.Bytes(), &row); err != nil {
			continue
		}
		if row.Cwd != "" {
			return row.Cwd, true
		}
	}
	return "", false
}
