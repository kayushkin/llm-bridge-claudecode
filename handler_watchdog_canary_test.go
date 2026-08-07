package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// The watchdog tests in handler_recovery_test.go drive drainUntilResult with a
// hand-built Harness: an event channel nobody feeds and a CCProcess with no
// cmd behind it. That proves the select arithmetic and nothing else. It cannot
// see whether a real subprocess wedges the way the bug report describes, whether
// Kill actually kills anything, or whether a turn that is slow-but-alive
// survives — which is the risk this watchdog carries, since it guards the live
// core turn loop.
//
// These canaries spawn a real subprocess through the real spawnClaudeCode,
// read it through the real ReadEvents, and drain it through the real
// drainUntilResult. The only fake part is the binary on the far end of the
// pipe, which is the one thing a test cannot supply honestly.

// fakeClaude writes a stand-in `claude` binary that speaks enough stream-json
// for the drain loop, and returns its path. Behaviour is chosen by the
// FAKECC_MODE environment variable the harness inherits at spawn:
//
//	wedge     — announce the session, then fall silent while staying alive.
//	            The reported hang: stream-json stops, the process does not.
//	keepalive — announce, then send CC's own `keep_alive` ping every tick for
//	            FAKECC_TICKS ticks, then finish the turn with a result.
//	            A slow but demonstrably live turn.
//	prompt    — announce, answer, and idle waiting for the next message, the
//	            way a healthy `claude -p --input-format stream-json` sits.
//
// Every invocation appends its argv to FAKECC_ARGV_LOG, so a test can assert
// what the harness asked for without reaching into the harness.
func fakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKECC_ARGV_LOG"
uuid="$FAKECC_UUID"
emit() { printf '%s\n' "$1"; }
# idle is how a real claude -p in stream-json mode waits for the next message:
# still running, holding stdout open, saying nothing. FAKECC_EXIT
# turns that off so the rig's own smoke test can just read the output and stop.
idle() { if [ -n "$FAKECC_EXIT" ]; then exit 0; fi; exec sleep 3600; }
emit "{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"$uuid\",\"model\":\"fake\",\"tools\":[]}"
result="{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"num_turns\":1,\"session_id\":\"$uuid\",\"result\":\"ok\"}"

case "$FAKECC_MODE" in
  wedge)
    # The turn never ends and the process never exits. exec so the thing
    # holding stdout open is the process the harness knows the pid of.
    exec sleep 3600
    ;;
  keepalive)
    i=0
    while [ "$i" -lt "$FAKECC_TICKS" ]; do
      sleep "$FAKECC_TICK_SEC"
      emit '{"type":"keep_alive"}'
      i=$((i+1))
    done
    emit "$result"
    idle
    ;;
  prompt)
    emit "{\"type\":\"assistant\",\"session_id\":\"$uuid\",\"message\":{\"id\":\"msg_fake\",\"role\":\"assistant\",\"model\":\"fake\",\"content\":[{\"type\":\"text\",\"text\":\"answered\"}]}}"
    emit "$result"
    idle
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	return path
}

// canaryUUID is a real Claude Code session UUID shape. isClaudeSessionUUID
// gates --resume on it, so a stand-in id would silently drop the flag and the
// resume canary would pass for the wrong reason.
const canaryUUID = "6a1c9f2e-8b41-4d0a-9f77-2c5e13ab7f40"

// spawnCanary starts the fake through the real spawn path and wires a Harness
// exactly as handleStart does, so the drain loop under test sees a genuine
// subprocess, pipe and reader goroutine.
func spawnCanary(t *testing.T, mode string, idle time.Duration, extraArgs ...string) *Harness {
	t.Helper()
	path := fakeClaude(t)
	argvLog := filepath.Join(t.TempDir(), "argv")

	t.Setenv("FAKECC_MODE", mode)
	t.Setenv("FAKECC_UUID", canaryUUID)
	t.Setenv("FAKECC_ARGV_LOG", argvLog)

	cfg := &Config{ClaudePath: path, TurnIdleTimeout: idle}
	h := NewHarness(cfg)
	h.bridgeSessionID = "canary-bridge"
	h.sessionID = canaryUUID

	proc, err := spawnClaudeCode(cfg, canaryUUID, nil, nil, extraArgs...)
	if err != nil {
		t.Fatalf("spawn fake claude: %v", err)
	}
	t.Cleanup(func() {
		_ = proc.Kill()
		h.cancel()
	})
	h.proc = proc
	h.events = proc.ReadEvents(h.ctx)
	if err := proc.WriteMessage("hello"); err != nil {
		t.Fatalf("write prompt: %v", err)
	}
	return h
}

// drainWithin runs one turn and fails if it does not end in the given window —
// the hang this watchdog exists to break.
func drainWithin(t *testing.T, h *Harness, limit time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { h.drainUntilResult(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("drainUntilResult did not return within %s", limit)
	}
}

func idleTimeouts(events []msg.Event) int {
	n := 0
	for _, e := range events {
		if e.Type == msg.EventError && e.Error != nil && e.Error.Code == "TURN_IDLE_TIMEOUT" {
			n++
		}
	}
	return n
}

func hasResult(events []msg.Event) bool {
	for _, e := range events {
		if e.Type == msg.EventResult {
			return true
		}
	}
	return false
}

// pidAlive asks the OS, not the harness. CCProcess.Alive reads a channel the
// spawn goroutine closes, and with an OTel receiver attached that close is
// deliberately delayed — so Alive is the wrong instrument for "did Kill kill
// it".
func pidAlive(pid int) bool {
	return syscall.Kill(pid, syscall.Signal(0)) == nil
}

// TestCanaryWatchdogKillsARealWedgedProcess is the reported bug, reproduced
// against a live subprocess rather than a stubbed channel: stream-json goes
// silent while `claude` keeps running. Asserts all three things the watchdog
// promises — the turn ends, the stall is reported, and the wedged process is
// actually gone.
func TestCanaryWatchdogKillsARealWedgedProcess(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := spawnCanary(t, "wedge", 2*time.Second)
	pid := h.proc.cmd.Process.Pid
	if !pidAlive(pid) {
		t.Fatal("fake claude was not running at the start of the turn")
	}

	drainWithin(t, h, 20*time.Second)

	if n := idleTimeouts(get()); n != 1 {
		t.Fatalf("expected exactly one TURN_IDLE_TIMEOUT, got %d", n)
	}
	// Kill is asynchronous at the OS level; give the signal a moment to land.
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatalf("wedged process %d survived the watchdog", pid)
	}
}

// TestCanaryWatchdogSparesATurnSendingKeepAlives is the other half of the
// contract, and the one no unit test could see: Claude Code's keep_alive is a
// liveness ping, so a turn sending them is the opposite of wedged. The turn
// must run to its result untouched.
func TestCanaryWatchdogSparesATurnSendingKeepAlives(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	t.Setenv("FAKECC_TICKS", "6")
	t.Setenv("FAKECC_TICK_SEC", "1")
	h := spawnCanary(t, "keepalive", 2*time.Second)
	pid := h.proc.cmd.Process.Pid

	drainWithin(t, h, 30*time.Second)

	events := get()
	if n := idleTimeouts(events); n != 0 {
		t.Fatalf("a turn pinging keep_alive every second was killed by the %s watchdog (%d TURN_IDLE_TIMEOUT)", 2*time.Second, n)
	}
	if !hasResult(events) {
		t.Fatal("expected the turn to end on its result event")
	}
	if !pidAlive(pid) {
		t.Fatalf("process %d was killed during a live turn", pid)
	}
}

// TestCanaryWedgedTurnRespawnsWithResume pins the claim the watchdog's comment
// makes about the work not being lost: after the kill, the next message
// respawns via --resume against the UUID CC minted, and that turn completes.
func TestCanaryWedgedTurnRespawnsWithResume(t *testing.T) {
	_, restore := swapEmit()
	defer restore()

	path := fakeClaude(t)
	argvLog := filepath.Join(t.TempDir(), "argv")
	t.Setenv("FAKECC_MODE", "wedge")
	t.Setenv("FAKECC_UUID", canaryUUID)
	t.Setenv("FAKECC_ARGV_LOG", argvLog)

	cfg := &Config{ClaudePath: path, TurnIdleTimeout: 2 * time.Second}
	h := NewHarness(cfg)
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
	// handleStart drains the wedged turn inline; the watchdog is what let it
	// return at all.
	firstPID := h.proc.cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(firstPID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if pidAlive(firstPID) {
		t.Fatalf("first process %d survived the watchdog", firstPID)
	}
	if h.sessionID != canaryUUID {
		t.Fatalf("harness did not adopt the UUID from init: %q", h.sessionID)
	}
	// handleMessage respawns only once CCProcess.Alive goes false, and that
	// lags the kill: with an OTel receiver attached the spawn goroutine waits
	// for a flush window before closing done. A real next message arrives
	// long after; this test has to wait for the same condition explicitly.
	for h.proc.Alive() && time.Now().Before(deadline.Add(5*time.Second)) {
		time.Sleep(50 * time.Millisecond)
	}

	// The next message must respawn. Point the fake at a healthy turn so the
	// only thing under test is the respawn itself.
	t.Setenv("FAKECC_MODE", "prompt")
	if err := h.handleMessage(MessageParams{Content: "still there?"}); err != nil {
		t.Fatalf("handleMessage after a wedged turn: %v", err)
	}

	argv, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(argv)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected two spawns, got %d: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "--resume") {
		t.Fatalf("the cold start should not have resumed: %q", lines[0])
	}
	if !strings.Contains(lines[1], "--resume "+canaryUUID) {
		t.Fatalf("respawn did not --resume the minted UUID: %q", lines[1])
	}
}

// TestFakeClaudeIsRunnable guards the rig itself. Every canary below reads a
// failure to launch as a behavioural result, so a fake that cannot execute on
// this host would turn the whole file green for the wrong reason.
func TestFakeClaudeIsRunnable(t *testing.T) {
	path := fakeClaude(t)
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(),
		"FAKECC_MODE=prompt",
		"FAKECC_UUID="+canaryUUID,
		"FAKECC_ARGV_LOG="+filepath.Join(t.TempDir(), "argv"),
		"FAKECC_EXIT=1",
	)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake claude is not runnable on this host: %v", err)
	}
	if !strings.Contains(string(out), `"subtype":"init"`) {
		t.Fatalf("fake claude did not announce a session: %q", out)
	}
}
