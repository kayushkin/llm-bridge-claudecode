package main

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// CCProcess.done used to carry two meanings on one channel — "the process has
// exited" and "the telemetry receiver has been drained" — because the exit
// goroutine closed it only after the OTel flush window. Alive reads that
// channel, so for two seconds after every death it answered true, and the three
// handlers that branch on Alive to choose respawn-vs-write all took the wrong
// branch. handleResume took the worst one: it returned success having respawned
// nothing.
//
// These canaries run the real spawn path with a real OTLP receiver attached,
// because that is the only configuration where the window exists at all. The
// existing watchdog canaries pass nil for the receiver (spawnCanary), so a test
// written against that helper would be green on the broken code for the wrong
// reason.

// otelFlushWindow is the sleep spawnClaudeCode's exit goroutine takes before
// tearing the receiver down. The assertions below are written relative to it so
// they stay honest if it is ever retuned.
const otelFlushWindow = 2 * time.Second

// spawnWithTelemetry starts the fake claude through the real spawnClaudeCode
// with a live OTel receiver wired in, the way handleStart always does —
// "telemetry is always on for claudecode harnesses".
func spawnWithTelemetry(t *testing.T, mode string, emit func(msg.Event)) (*CCProcess, *OTelReceiver) {
	t.Helper()
	path := fakeClaude(t)

	t.Setenv("FAKECC_MODE", mode)
	t.Setenv("FAKECC_UUID", canaryUUID)
	t.Setenv("FAKECC_ARGV_LOG", filepath.Join(t.TempDir(), "argv"))

	if emit == nil {
		emit = func(msg.Event) {}
	}
	recv, err := NewOTelReceiver(emit)
	if err != nil {
		t.Fatalf("new otel receiver: %v", err)
	}
	recv.Start()

	proc, err := spawnClaudeCode(&Config{ClaudePath: path}, canaryUUID, nil, recv)
	if err != nil {
		t.Fatalf("spawn fake claude: %v", err)
	}
	t.Cleanup(func() { _ = proc.Kill() })

	// Rig guard. With no receiver there is no flush window, so every
	// assertion below would hold on the broken code too.
	if proc.otelRecv == nil {
		t.Fatal("rig is wrong: the process carries no OTel receiver, so the window under test does not exist")
	}
	return proc, recv
}

// waitPidGone blocks until the OS says the pid is reaped. It asks the kernel,
// not CCProcess.Alive — Alive is the thing under test here.
//
// The pid-only signature is the point, not an accident: a helper that cannot
// see a *CCProcess structurally cannot reach .Alive(), so no caller can pick
// the wrong instrument. That is the enforcement pidAlive's comment asks for
// and could not supply on its own — see the note above pidAlive.
//
// what names the process's role, so a failure says which one outlived its
// killer. The kill is not always an explicit Kill() call: the watchdog canaries
// reach here after the watchdog killed the process for them.
func waitPidGone(t *testing.T, what string, pid int, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for pidAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) was still running %s after it should have died", what, pid, limit)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestAliveGoesFalseWhenTheProcessDies is the defect, measured. Alive must
// answer for the process, not for the telemetry receiver, so it has to flip
// well inside the flush window rather than at the end of it.
func TestAliveGoesFalseWhenTheProcessDies(t *testing.T) {
	proc, _ := spawnWithTelemetry(t, "wedge", nil)
	pid := proc.cmd.Process.Pid
	if !proc.Alive() {
		t.Fatal("fake claude was not alive before the kill")
	}

	killedAt := time.Now()
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitPidGone(t, "the killed process", pid, 5*time.Second)

	// A quarter of the window: comfortably longer than cmd.Wait needs to reap
	// a killed child, and far short of the two seconds the old code took.
	limit := otelFlushWindow / 4
	deadline := time.Now().Add(limit)
	for proc.Alive() {
		if time.Now().After(deadline) {
			t.Fatalf("Alive still reported the process running %s after it exited "+
				"(the OS agrees it is gone); done is gated behind the %s telemetry "+
				"flush, so Alive is answering for the receiver, not the process",
				time.Since(killedAt).Round(time.Millisecond), otelFlushWindow)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestDoneClosesBeforeTheTelemetryFlush pins the other half of the split, and
// is what stops the fix being "delete the sleep". The flush window still has to
// happen — CC's exporter batches on a 1s interval, and trailing batches are the
// auxiliary API calls stream-json never shows — so the receiver must still be
// answering after Done is closed.
func TestDoneClosesBeforeTheTelemetryFlush(t *testing.T) {
	var mu sync.Mutex
	var got []msg.Event
	proc, recv := spawnWithTelemetry(t, "wedge", func(e msg.Event) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	})

	pid := proc.cmd.Process.Pid
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitPidGone(t, "the killed process", pid, 5*time.Second)

	select {
	case <-proc.Done():
	case <-time.After(otelFlushWindow / 4):
		t.Fatalf("Done did not close within %s of the process exiting", otelFlushWindow/4)
	}

	// A batch that lands after the exit, which is the case the window exists
	// for. It must still be translated and emitted.
	payload := `{
		"resourceLogs": [{
			"scopeLogs": [{
				"logRecords": [{
					"timeUnixNano": "1778782946151000000",
					"body": {"stringValue": "claude_code.internal_error"},
					"attributes": [
						{"key": "event.name", "value": {"stringValue": "internal_error"}},
						{"key": "message",    "value": {"stringValue": "a trailing batch"}}
					]
				}]
			}]
		}]
	}`
	resp, err := http.Post(recv.EndpointURL()+"/v1/logs", "application/json", bytes.NewBufferString(payload))
	if err != nil {
		t.Fatalf("the receiver was already torn down when Done closed, so the "+
			"flush window is gone and trailing telemetry is dropped: %v", err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	for _, e := range got {
		if e.Type == msg.EventError && e.Error != nil && e.Error.Message == "a trailing batch" {
			return
		}
	}
	t.Fatalf("the trailing batch was accepted but never emitted; got %d events", len(got))
}

// waitArgvLines waits for the fake's argv log to hold want spawn records and
// returns them. Each fake logs its argv as its first act, so "how many lines"
// is "how many claude processes were started".
func waitArgvLines(t *testing.T, path string, want int, limit time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(limit)
	var lines []string
	for {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("read argv log: %v", err)
		}
		if trimmed := strings.TrimSpace(string(raw)); trimmed != "" {
			lines = strings.Split(trimmed, "\n")
		} else {
			lines = nil
		}
		if len(lines) >= want {
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected %d claude spawn(s) within %s, got %d: %q\n"+
				"a resume inside the telemetry flush window read the dead process as "+
				"alive and returned success having done nothing", want, limit, len(lines), lines)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHandleResumeRespawnsAfterADeath is the consequence the todo is named for.
// A resume arriving just after a death used to hit `if h.proc.Alive() { return
// nil }` and report success without starting anything — fail-silent, which
// CLAUDE.md forbids by name. The assertion is on the spawn, not on the return
// value, precisely because the broken path returns nil too.
func TestHandleResumeRespawnsAfterADeath(t *testing.T) {
	_, restore := swapEmit()
	defer restore()

	path := fakeClaude(t)
	argvLog := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKECC_MODE", "prompt")
	t.Setenv("FAKECC_UUID", canaryUUID)
	t.Setenv("FAKECC_ARGV_LOG", argvLog)

	// Watchdog off: this canary is about the death, not about a stall.
	h := NewHarness(&Config{ClaudePath: path})
	t.Cleanup(func() {
		if h.proc != nil {
			_ = h.proc.Kill()
		}
		h.cancel()
	})

	if err := h.handleStart(StartParams{
		BridgeSessionID: "canary-bridge",
		SessionID:       "canary-bridge",
		Prompt:          "hello",
	}); err != nil {
		t.Fatalf("handleStart: %v", err)
	}
	if h.sessionID != canaryUUID {
		t.Fatalf("harness did not adopt the UUID from init: %q", h.sessionID)
	}

	// The process dies the way a crash, an OOM or the turn watchdog's own Kill
	// leaves it: gone, with the resume arriving very soon afterwards.
	pid := h.proc.cmd.Process.Pid
	if err := h.proc.Kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitPidGone(t, "the process that died", pid, 5*time.Second)
	// Long enough for cmd.Wait to reap the child, an order of magnitude short
	// of the flush window the old code hid the death behind. Sleeping until
	// Alive goes false instead would make this test assert nothing: it would
	// wait out the very window that is the bug.
	time.Sleep(otelFlushWindow / 10)

	if err := h.handleResume(); err != nil {
		t.Fatalf("handleResume after a death: %v", err)
	}

	// A resume carries no prompt, so handleStart returns as soon as the spawn
	// succeeds and does not drain — the new child may not have reached its
	// first line yet. Poll rather than read once: reading immediately makes a
	// working resume look like a silent no-op.
	lines := waitArgvLines(t, argvLog, 2, 5*time.Second)
	if !strings.Contains(lines[1], "--resume "+canaryUUID) {
		t.Fatalf("respawn did not --resume the minted UUID: %q", lines[1])
	}
}
