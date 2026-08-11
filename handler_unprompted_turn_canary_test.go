package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Claude Code does not only speak when spoken to. When a background Task
// finishes, CC injects a <task-notification> into its own conversation and runs
// a turn on it — with nothing written to its stdin first.
//
// The harness used to have no reader running at that moment. h.events
// (process.go ReadEvents) had exactly one consumer, and it was called only
// from handleStart, handleMessage and handleCompact — all handlers for a
// request the harness itself initiated. Between turns nobody read.
//
// readStreamJSON now reads for the life of the process and awaitTurnEnd only
// waits, which is what these canaries pin.
//
// So an unprompted turn's stream-json frames sit in the channel. The next
// harness-initiated turn drains them, returns on the *stale* result, and leaves
// its own frames behind. From then on every answer is delivered one turn late,
// which is what a user sees as "the reply only shows up when I send the next
// message".
//
// Measured on session br_1786411624543689771 on 2026-08-11: the opening prompt
// streamed live and correctly, the first <task-notification> landed 12 seconds
// after it finished, and every prompt after that was one behind — same harness
// pid and same claude child throughout, no respawn, no errors. See noteboard
// todo 60b7f5e2.
//
// These canaries reuse the rig in handler_watchdog_canary_test.go: a real
// subprocess through the real spawnClaudeCode, read through the real
// ReadEvents. Only the binary on the far end of the pipe is fake, and its
// "unprompted" mode emits a turn nobody asked for.

// resultTexts returns the `result` string of every EventResult, in order. The
// fake names each turn's result after the turn, so this answers "which turn's
// answer did the harness deliver", not merely "did one arrive".
func resultTexts(events []msg.Event) []string {
	var out []string
	for _, e := range events {
		if e.Type == msg.EventResult && e.Result != nil {
			out = append(out, e.Result.Text)
		}
	}
	return out
}

// assistantTexts returns the text of every assistant text block, in order.
func assistantTexts(events []msg.Event) []string {
	var out []string
	for _, e := range events {
		if isAssistantTextEvent(e) {
			out = append(out, e.Block.Block.Text.Text)
		}
	}
	return out
}

func containsText(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// messageWithin sends a follow-up and fails if the call does not return in the
// window. The watchdog is disabled in these canaries — they are about the
// reader, not the stall detector — so a drain that blocks would otherwise hang
// the suite instead of reporting.
func messageWithin(t *testing.T, h *Harness, content string, limit time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- h.handleMessage(MessageParams{Content: content}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("handleMessage(%q): %v", content, err)
		}
	case <-time.After(limit):
		t.Fatalf("handleMessage(%q) did not return within %s", content, limit)
	}
}

// startUnpromptedCanary spawns the fake in "unprompted" mode and drains the one
// turn the harness did ask for, leaving the fake about to emit a turn nobody
// asked for. Returns the harness and the emitted-event snapshot function.
//
// The watchdog is off (idle timeout 0): these canaries must fail on the reader,
// not be rescued or killed by the stall detector.
func startUnpromptedCanary(t *testing.T) (*Harness, func() []msg.Event) {
	t.Helper()
	get, restore := swapEmit()
	t.Cleanup(restore)

	t.Setenv("FAKECC_UNPROMPTED_DELAY_SEC", "1")
	h, waiter := spawnCanary(t, "unprompted", 0)

	// Turn 1 is harness-initiated, so it completes normally either way. If
	// this fails the rig is broken, not the behaviour under test.
	turnEndsWithin(t, h, waiter, 20*time.Second)
	if !containsText(resultTexts(get()), "r-first") {
		t.Fatalf("the harness-initiated turn never completed; rig problem, not the defect. results=%q", resultTexts(get()))
	}
	return h, get
}

// TestCanaryUnpromptedTurnReachesTheUser is the mechanism. A turn Claude Code
// starts by itself must reach the user when it happens, not whenever the user
// next happens to type.
func TestCanaryUnpromptedTurnReachesTheUser(t *testing.T) {
	_, get := startUnpromptedCanary(t)

	// The fake now emits a full turn with nothing written to its stdin. No
	// further request is made of the harness — that is the condition under
	// test, so polling is the only honest way to wait.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if containsText(assistantTexts(get()), "answered-unprompted") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("an unprompted turn was never delivered. The harness reads stdout only while servicing a request it started, so a turn Claude Code begins on its own sits unread in h.events. delivered=%q", assistantTexts(get()))
}

// TestCanaryTurnAfterAnUnpromptedTurnAnswersTheNewMessage is the user-visible
// bug: the one-turn lag. After a turn nobody asked for, the next message must
// still be answered with ITS OWN answer.
//
// This is the discriminating assertion. It fails today even though the previous
// canary's fix would also fix it, because it names the wrong answer rather than
// a missing one — the reply the user gets is real, complete, and about the
// wrong prompt.
func TestCanaryTurnAfterAnUnpromptedTurnAnswersTheNewMessage(t *testing.T) {
	h, get := startUnpromptedCanary(t)

	// Let the unprompted turn reach the channel before the next message, so
	// the test pins the lag rather than racing it.
	time.Sleep(3 * time.Second)

	before := len(get())
	messageWithin(t, h, "second", 20*time.Second)

	got := resultTexts(get()[before:])
	if len(got) == 0 {
		t.Fatalf("the turn for message 2 produced no result at all; emitted=%q", assistantTexts(get()[before:]))
	}
	if got[0] != "r-next" {
		t.Fatalf("message 2 was answered with %q, want \"r-next\". The drain returned on the unprompted turn's stale result, so this answer belongs to a turn the user never asked for and every turn from here is one behind. results=%q", got[0], got)
	}
}

// TestFakeClaudeUnpromptedModeEmitsBothTurns guards the new rig mode. Both
// canaries above read a silent fake as a behavioural result, so a mode that
// emitted nothing would fail them for the wrong reason — and the second canary
// would then be reporting a defect that its own fixture invented.
func TestFakeClaudeUnpromptedModeEmitsBothTurns(t *testing.T) {
	path := fakeClaude(t)
	cmd := exec.Command(path)
	cmd.Env = append(os.Environ(),
		"FAKECC_MODE=unprompted",
		"FAKECC_UUID="+canaryUUID,
		"FAKECC_ARGV_LOG="+filepath.Join(t.TempDir(), "argv"),
		"FAKECC_UNPROMPTED_DELAY_SEC=0",
	)
	// One line then EOF: the initial prompt, and no second message.
	cmd.Stdin = strings.NewReader(`{"type":"user"}` + "\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("fake claude unprompted mode is not runnable on this host: %v", err)
	}
	for _, want := range []string{"answered-first", "r-first", "answered-unprompted", "r-unprompted"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("unprompted mode did not emit %q; output=%q", want, out)
		}
	}
	// Nothing was sent after the prompt, so there must be no third turn — if
	// there were, the second canary could pass on an answer the fake produced
	// for free rather than in reply to the message.
	if strings.Contains(string(out), "answered-next") {
		t.Fatalf("unprompted mode emitted a follow-up turn with no message sent; output=%q", out)
	}
}
