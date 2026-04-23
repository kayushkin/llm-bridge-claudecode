package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestTranslateHook_Started(t *testing.T) {
	raw := json.RawMessage(`{"type":"system","subtype":"hook_started","hook_id":"abc","hook_name":"PreToolUse:0","hook_event":"PreToolUse","tool_name":"Bash","session_id":"s1"}`)
	events := translateEvent(raw, "s1", &UsageAggregator{})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Type != msg.EventHook {
		t.Fatalf("expected EventHook, got %s", e.Type)
	}
	if e.Hook == nil {
		t.Fatal("Hook payload missing")
	}
	if e.Hook.Phase != "started" {
		t.Errorf("phase = %q, want started", e.Hook.Phase)
	}
	if e.Hook.Event != "PreToolUse" {
		t.Errorf("event = %q, want PreToolUse", e.Hook.Event)
	}
	if e.Hook.ToolName != "Bash" {
		t.Errorf("tool_name = %q, want Bash", e.Hook.ToolName)
	}
	if e.Hook.HookID != "" {
		t.Errorf("HookID = %q, want empty (observed hook)", e.Hook.HookID)
	}
}

func TestTranslateHook_ResponseWithJSONDecision(t *testing.T) {
	raw := json.RawMessage(`{"type":"system","subtype":"hook_response","hook_id":"abc","hook_name":"PreToolUse:0","hook_event":"PreToolUse","stdout":"{\"decision\":\"deny\",\"reason\":\"nope\"}","stderr":"","exit_code":0,"outcome":"success","session_id":"s1"}`)
	events := translateEvent(raw, "s1", &UsageAggregator{})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	h := events[0].Hook
	if h == nil {
		t.Fatal("Hook payload missing")
	}
	if h.Phase != "completed" {
		t.Errorf("phase = %q, want completed", h.Phase)
	}
	if h.Decision != "deny" {
		t.Errorf("decision = %q, want deny", h.Decision)
	}
	if h.ExitCode != 0 {
		t.Errorf("exit_code = %d, want 0", h.ExitCode)
	}
	if string(h.Output) != `{"decision":"deny","reason":"nope"}` {
		t.Errorf("output = %s, want full JSON", h.Output)
	}
	if h.Error != "" {
		t.Errorf("error = %q, want empty", h.Error)
	}
}

func TestTranslateHook_ResponseWithFailureOutcome(t *testing.T) {
	raw := json.RawMessage(`{"type":"system","subtype":"hook_response","hook_id":"abc","hook_name":"PostToolUse:0","hook_event":"PostToolUse","stdout":"","stderr":"command not found","exit_code":127,"outcome":"error","session_id":"s1"}`)
	events := translateEvent(raw, "s1", &UsageAggregator{})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	h := events[0].Hook
	if h.Phase != "completed" {
		t.Errorf("phase = %q, want completed", h.Phase)
	}
	if h.ExitCode != 127 {
		t.Errorf("exit_code = %d, want 127", h.ExitCode)
	}
	if h.Error == "" {
		t.Error("expected Error to be set for non-success outcome")
	}
}

func TestTranslateHook_NonJSONStdout(t *testing.T) {
	raw := json.RawMessage(`{"type":"system","subtype":"hook_response","hook_id":"abc","hook_name":"SessionStart:startup","hook_event":"SessionStart","stdout":"started\n","stderr":"","exit_code":0,"outcome":"success","session_id":"s1"}`)
	events := translateEvent(raw, "s1", &UsageAggregator{})
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	h := events[0].Hook
	if len(h.Output) != 0 {
		t.Errorf("Output = %s, want empty for non-JSON stdout", h.Output)
	}
	if h.Decision != "" {
		t.Errorf("Decision = %q, want empty for non-JSON stdout", h.Decision)
	}
}
