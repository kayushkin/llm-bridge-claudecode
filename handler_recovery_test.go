package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// swapEmit replaces the package emitEvent sink with a collector for the life of
// the returned restore func, and returns a snapshot getter.
func swapEmit() (get func() []msg.Event, restore func()) {
	var (
		mu   sync.Mutex
		seen []msg.Event
	)
	prev := emitEvent
	emitEvent = func(ev any) {
		mu.Lock()
		defer mu.Unlock()
		if e, ok := ev.(msg.Event); ok {
			seen = append(seen, e)
		}
	}
	get = func() []msg.Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]msg.Event, len(seen))
		copy(out, seen)
		return out
	}
	return get, func() { emitEvent = prev }
}

func textBlockEvent(text string, otel bool) msg.Event {
	e := msg.Event{
		Type: msg.EventBlock,
		Block: &msg.BlockEvent{
			Block: &msg.ContentBlock{Type: msg.BlockText, Text: &msg.TextBlock{Text: text}},
		},
	}
	if otel {
		tagOTelSource(&e)
	}
	return e
}

// otelTextFrom is an OTel assistant_response copy stamped with the
// query_source Claude Code put on it — "sdk" for the conversation, anything
// else for one of its own side calls.
func otelTextFrom(text, querySource string) msg.Event {
	e := textBlockEvent(text, true)
	e.Extensions[otelQuerySourceExtension] = json.RawMessage(`"` + querySource + `"`)
	return e
}

func countRecovered(events []msg.Event) []string {
	var out []string
	for _, e := range events {
		if isAssistantTextEvent(e) && string(e.Extensions["recovered"]) == "true" {
			out = append(out, e.Block.Block.Text.Text)
		}
	}
	return out
}

// TestRecovery_FlushesOTelWhenStreamJSONSilent covers the wedged-turn case: the
// only assistant text arrived via OTel, so flushRecoveredAssistant must surface
// it (tagged recovered) instead of the message vanishing.
func TestRecovery_FlushesOTelWhenStreamJSONSilent(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := &Harness{cfg: &Config{}}
	h.beginTurn()
	h.emit(textBlockEvent("first segment", true))
	h.emit(textBlockEvent("Want me to (a) write the doc?", true))
	h.flushRecoveredAssistant()

	rec := countRecovered(get())
	if len(rec) != 2 || rec[0] != "first segment" || rec[1] != "Want me to (a) write the doc?" {
		t.Fatalf("expected 2 recovered segments in order, got %v", rec)
	}
}

// TestRecovery_NoDoubleRenderOnHealthyTurn covers the healthy path: stream-json
// carried the assistant text, so the buffered OTel copy must be dropped — no
// duplicate bubble.
func TestRecovery_NoDoubleRenderOnHealthyTurn(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := &Harness{cfg: &Config{}}
	h.beginTurn()
	h.emit(textBlockEvent("the real answer", false)) // stream-json, forwarded + marks turn
	h.emit(textBlockEvent("the real answer", true))  // OTel dup, buffered
	h.flushRecoveredAssistant()

	events := get()
	if rec := countRecovered(events); len(rec) != 0 {
		t.Fatalf("healthy turn must not emit recovered blocks, got %v", rec)
	}
	// Exactly the one stream-json block should have been forwarded.
	var forwarded int
	for _, e := range events {
		if isAssistantTextEvent(e) {
			forwarded++
		}
	}
	if forwarded != 1 {
		t.Fatalf("expected exactly 1 forwarded assistant block, got %d", forwarded)
	}
}

// TestWatchdog_UnblocksWedgedTurn covers the hang fix: a live process that
// produces no stream-json result must not block forever. The watchdog surfaces
// a TURN_IDLE_TIMEOUT error and returns.
func TestWatchdog_UnblocksWedgedTurn(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Harness{
		cfg:    &Config{TurnIdleTimeout: 60 * time.Millisecond},
		events: make(chan json.RawMessage), // never fed, never closed
		proc:   &CCProcess{done: make(chan struct{})},
		ctx:    ctx,
	}

	h.beginTurn()
	waiter := h.registerTurnWaiter()
	done := make(chan struct{})
	go func() { h.awaitTurnEnd(waiter); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("awaitTurnEnd did not return — watchdog failed to fire")
	}

	var got *msg.ErrorEvent
	for _, e := range get() {
		if e.Type == msg.EventError && e.Error != nil && e.Error.Code == "TURN_IDLE_TIMEOUT" {
			got = e.Error
		}
	}
	if got == nil {
		t.Fatal("expected a TURN_IDLE_TIMEOUT error event")
	}
}

// TestWatchdog_RecoversFinalMessageOnStall is the end-to-end of the reported
// bug: the process wedges with the final message only on OTel; the watchdog
// must both surface that message and unblock.
func TestWatchdog_RecoversFinalMessageOnStall(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &Harness{
		cfg:    &Config{TurnIdleTimeout: 60 * time.Millisecond},
		events: make(chan json.RawMessage),
		proc:   &CCProcess{done: make(chan struct{})},
		ctx:    ctx,
	}

	h.beginTurn()
	waiter := h.registerTurnWaiter()
	done := make(chan struct{})
	go func() { h.awaitTurnEnd(waiter); close(done) }()

	// Deliver the OTel-only final message after the turn has begun.
	time.Sleep(15 * time.Millisecond)
	h.emit(textBlockEvent("End-to-end verified. Want me to (a) write the doc?", true))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("awaitTurnEnd did not return")
	}

	rec := countRecovered(get())
	if len(rec) != 1 || rec[0] != "End-to-end verified. Want me to (a) write the doc?" {
		t.Fatalf("expected the OTel final message recovered, got %v", rec)
	}
}

// The case recovery exists for, and the one it refused for a month: the turn
// narrates once on stream-json, then stream-json drops the final answer, which
// arrives on OTel only. The narration must not double-render and the final
// answer must surface. 19 of the 20 idle-timeout turns in log-store had this
// shape and recovered nothing.
func TestRecovery_RecoversOnlyTheSegmentStreamJSONDropped(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := &Harness{cfg: &Config{}}
	h.beginTurn()
	h.emit(textBlockEvent("Let me check the journal first.", false))
	h.emit(textBlockEvent("Let me check the journal first.\n", true)) // trailing newline: same segment
	h.emit(textBlockEvent("Found it: the push is dropped on a refused connection.", true))
	h.flushRecoveredAssistant()

	rec := countRecovered(get())
	if len(rec) != 1 || rec[0] != "Found it: the push is dropped on a refused connection." {
		t.Fatalf("want exactly the undelivered final answer recovered, got %v", rec)
	}
}

// Claude Code's exporter batches for about a second, so the OTel copy of a
// turn's last segment routinely lands after that turn's result — in the next
// turn. If the next turn then wedges, that late copy is already-delivered
// text from the previous turn, not a lost answer, and must not be recovered.
func TestRecovery_LateOTelCopyOfThePreviousTurnIsNotRecovered(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := &Harness{cfg: &Config{}}
	h.beginTurn()
	h.emit(textBlockEvent("Done, pushed as abc123.", false))
	h.flushRecoveredAssistant() // turn 1 ends on its result; the OTel copy has not arrived
	h.beginTurn()
	h.emit(textBlockEvent("Done, pushed as abc123.", true)) // ...and lands here, in turn 2
	h.flushRecoveredAssistant()                             // turn 2 wedges with nothing on stream-json

	if rec := countRecovered(get()); len(rec) != 0 {
		t.Fatalf("a late copy of delivered text was recovered as a new answer: %v", rec)
	}
}

// A turn that loses its answer while the previous turn's late copy is also in
// the buffer recovers the answer and not the copy.
func TestRecovery_TellsALostAnswerFromALateCopy(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := &Harness{cfg: &Config{}}
	h.beginTurn()
	h.emit(textBlockEvent("First answer.", false))
	h.flushRecoveredAssistant()
	h.beginTurn()
	h.emit(textBlockEvent("First answer.", true))  // late copy of turn 1
	h.emit(textBlockEvent("Second answer.", true)) // turn 2's answer, dropped by stream-json
	h.flushRecoveredAssistant()

	rec := countRecovered(get())
	if len(rec) != 1 || rec[0] != "Second answer." {
		t.Fatalf("want only the lost second answer recovered, got %v", rec)
	}
}

// The same sentence said in two consecutive turns, with the second one lost:
// each delivery is consumed once, so the second is still recovered.
func TestRecovery_ARepeatedSentenceIsMatchedOncePerDelivery(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := &Harness{cfg: &Config{}}
	h.beginTurn()
	h.emit(textBlockEvent("Done.", false))
	h.emit(textBlockEvent("Done.", true))
	h.flushRecoveredAssistant()
	h.beginTurn()
	h.emit(textBlockEvent("Done.", true)) // turn 2 said it again; stream-json dropped it
	h.flushRecoveredAssistant()

	if rec := countRecovered(get()); len(rec) != 1 || rec[0] != "Done." {
		t.Fatalf("want the second, undelivered \"Done.\" recovered, got %v", rec)
	}
}

// Claude Code's own side calls — the session title here — are
// assistant_response events too, and nothing on stream-json ever matches them.
// One of the five all-time recoveries on this box surfaced
// `{"title": "Failed to fetch error on new chat startup"}` as the model's final
// answer. Skipped by query_source; a copy with no query_source at all is still
// the conversation.
func TestRecovery_SkipsSideCallResponses(t *testing.T) {
	get, restore := swapEmit()
	defer restore()

	h := &Harness{cfg: &Config{}}
	h.beginTurn()
	h.emit(otelTextFrom(`{"title": "Failed to fetch error on new chat startup"}`, "generate_session_title"))
	h.emit(otelTextFrom("The real final answer.", conversationQuerySource))
	h.emit(textBlockEvent("An answer from a CLI that stamps no query_source.", true))
	h.flushRecoveredAssistant()

	rec := countRecovered(get())
	if len(rec) != 2 || rec[0] != "The real final answer." || rec[1] != "An answer from a CLI that stamps no query_source." {
		t.Fatalf("want the conversation's two answers and not the title call, got %v", rec)
	}
}
