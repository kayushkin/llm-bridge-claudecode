package main

import (
	"os"
	"testing"
)

// A check against the REAL ~/.claude and state.db on this box, for the session
// that actually aborted on 2026-08-03 when llm-bridge-server started honouring
// the instance's working_dir. Skips everywhere the data is absent, so it never
// fails on a machine that simply does not have it.
func TestLive_DeadSessionResolvesToItsOwnDirectory(t *testing.T) {
	const uuid = "0d69dd7d-7918-4380-8cce-074498d42b33"
	if _, err := os.Stat(projectsRoot()); err != nil {
		t.Skip("no live ~/.claude/projects")
	}
	st, err := OpenState(DefaultStatePath())
	if err != nil {
		t.Skip("no live state.db")
	}
	defer st.Close()

	p, _ := st.RolloutPathFor(uuid)
	t.Logf("state.db rollout_path = %q (empty is expected for this one)", p)
	t.Logf("transcriptPath        = %q", transcriptPath(st, uuid))

	dir, ok := transcriptWorkingDir(st, uuid)
	if !ok {
		// The transcript is a real file on a real box; it can be pruned. Skip
		// rather than fail, so this stays a check on live data and never
		// becomes a test that depends on one machine's housekeeping.
		t.Skip("live: that conversation is no longer on disk")
	}
	if dir != "/" {
		t.Fatalf("live: resolved %q; want \"/\" — the directory it was created in", dir)
	}
	t.Logf("RESOLVED: resume would run from %q instead of the instance's dir", dir)
}
