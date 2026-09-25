package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// liveClaudeCanaryVariable turns on the canaries that run the real claude
// binary. They cost a model call and about two minutes, so plain `go test`
// skips them; scheduler job "claude-code notice-hold canary" runs them weekly.
const liveClaudeCanaryVariable = "LLM_BRIDGE_CLAUDECODE_LIVE_CLAUDE_CANARY"

// TestLiveClaudeCodeStillHoldsNoticeBehindFileWatcher checks, against the real
// claude binary, the two facts finished_task_waiters.go rests on:
//
//  1. Claude Code still holds a background task's notice while the owning
//     subagent is blocked in a foreground command that watches the task's
//     output file. If this fails, Claude Code now delivers the notice (or
//     ends the watcher) by itself, and finishedTaskWaiterStopper may be
//     unnecessary: remove it and this canary together.
//  2. A stop_task control request still frees the watcher. If this fails,
//     the stopper no longer works and must be fixed.
func TestLiveClaudeCodeStillHoldsNoticeBehindFileWatcher(t *testing.T) {
	if os.Getenv(liveClaudeCanaryVariable) != "1" {
		t.Skipf("set %s=1 to run against the real claude binary", liveClaudeCanaryVariable)
	}
	claudePath, err := exec.LookPath("claude")
	if err != nil {
		t.Fatalf("claude binary not found: %v", err)
	}
	versionOutput, err := exec.Command(claudePath, "--version").Output()
	if err != nil {
		t.Fatalf("claude --version: %v", err)
	}
	claudeVersion := strings.TrimSpace(string(versionOutput))
	t.Logf("claude %s", claudeVersion)

	proc, err := spawnClaudeCode(&Config{ClaudePath: claudePath, WorkDir: t.TempDir()}, "", nil, nil,
		"--model", "sonnet", "--permission-mode", "bypassPermissions")
	if err != nil {
		t.Fatalf("spawn claude: %v", err)
	}
	defer proc.Kill()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := proc.ReadEvents(ctx)

	const prompt = "Harness test. Call the Agent tool once, subagent_type general-purpose, run_in_background false. " +
		"Its exact task: 'Step 1: run Bash `sleep 15; echo FINISHED-MARKER` with run_in_background true and note the output file path it gives you. " +
		"Step 2: immediately run Bash `timeout 150 tail -n +1 -f <that output file path>` in the foreground with the timeout parameter 200000. " +
		"Step 3: report what step 2 returned.' Then relay its report."
	if err := proc.WriteMessage(prompt); err != nil {
		t.Fatalf("write prompt: %v", err)
	}

	// Phase 1: wait for the background task to finish while a foreground
	// watcher of its output file is running.
	stopper := newFinishedTaskWaiterStopper()
	watcherToolUseIDByTask := map[string]string{}
	var watcherTaskID, watcherToolUseID string
	setupDeadline := time.After(3 * time.Minute)
	for watcherTaskID == "" {
		select {
		case raw, open := <-events:
			if !open {
				t.Fatalf("claude exited before the scenario was set up")
			}
			var started streamFrameForTaskWaiters
			if json.Unmarshal(raw, &started) == nil && started.Subtype == "task_started" {
				watcherToolUseIDByTask[started.TaskID] = started.ToolUseID
			}
			if stopped := stopper.observe(raw); len(stopped) == 1 {
				watcherTaskID = stopped[0]
				watcherToolUseID = watcherToolUseIDByTask[watcherTaskID]
			}
		case <-setupDeadline:
			t.Fatalf("INCONCLUSIVE on claude %s: within 3 minutes the model did not background a command and "+
				"then watch its output file in the foreground, so nothing was measured. Rerun, or reword the prompt.", claudeVersion)
		}
	}
	t.Logf("background task finished while task %s (tool_use %s) watches its output file", watcherTaskID, watcherToolUseID)

	// Phase 2: for 30 seconds, the watcher must stay blocked and the notice
	// must stay undelivered. The watcher's own timeout is 150s, so anything
	// that ends it sooner is Claude Code's doing.
	holdWindow := time.After(30 * time.Second)
	for holding := true; holding; {
		select {
		case raw, open := <-events:
			if !open {
				t.Fatalf("claude exited while the watcher should have been blocked")
			}
			if frameAnswersToolUse(raw, watcherToolUseID) {
				t.Fatalf("CLAUDE CODE CHANGED (claude %s): the watcher's tool call returned within 30s of the "+
					"background task finishing, without stop_task. Claude Code no longer holds the notice behind a "+
					"file watcher, so finishedTaskWaiterStopper (finished_task_waiters.go) may be unnecessary. "+
					"Check, then remove it and this canary.", claudeVersion)
			}
		case <-holdWindow:
			holding = false
		}
	}
	t.Logf("claude %s still holds the notice: the watcher is blocked 30s after the task finished", claudeVersion)

	// Phase 3: stop_task must free the watcher.
	stopFinishedTaskWaiter(proc, watcherTaskID)
	freeDeadline := time.After(20 * time.Second)
	for {
		select {
		case raw, open := <-events:
			if !open {
				t.Fatalf("claude exited after stop_task")
			}
			if requestID, refusal, refused := stopTaskRefusal(raw); refused {
				t.Fatalf("STOPPER BROKEN (claude %s): stop_task %s refused: %s", claudeVersion, requestID, refusal)
			}
			if frameAnswersToolUse(raw, watcherToolUseID) {
				t.Logf("stop_task freed the watcher on claude %s", claudeVersion)
				return
			}
		case <-freeDeadline:
			t.Fatalf("STOPPER BROKEN (claude %s): the watcher's tool call did not return within 20s of stop_task. "+
				"finishedTaskWaiterStopper no longer frees blocked watchers.", claudeVersion)
		}
	}
}

// frameAnswersToolUse reports whether a stream-json frame carries the
// tool_result for toolUseID.
func frameAnswersToolUse(raw json.RawMessage, toolUseID string) bool {
	var frame streamFrameForTaskWaiters
	if json.Unmarshal(raw, &frame) != nil || frame.Type != "user" {
		return false
	}
	var blocks []contentBlockForTaskWaiters
	if json.Unmarshal(frame.Message.Content, &blocks) != nil {
		return false
	}
	for _, block := range blocks {
		if block.Type == "tool_result" && block.ToolUseID == toolUseID {
			return true
		}
	}
	return false
}
