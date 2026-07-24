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

	done := make(chan struct{})
	go func() { h.drainUntilResult(); close(done) }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainUntilResult did not return — watchdog failed to fire")
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

	done := make(chan struct{})
	go func() { h.drainUntilResult(); close(done) }()

	// Deliver the OTel-only final message after the turn has begun.
	time.Sleep(15 * time.Millisecond)
	h.emit(textBlockEvent("End-to-end verified. Want me to (a) write the doc?", true))

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drainUntilResult did not return")
	}

	rec := countRecovered(get())
	if len(rec) != 1 || rec[0] != "End-to-end verified. Want me to (a) write the doc?" {
		t.Fatalf("expected the OTel final message recovered, got %v", rec)
	}
}
