package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestTranslateHook_Started(t *testing.T) {
	raw := json.RawMessage(`{"type":"system","subtype":"hook_started","hook_id":"abc","hook_name":"PreToolUse:0","hook_event":"PreToolUse","tool_name":"Bash","session_id":"s1"}`)
	events := translateEvent(raw, "s1", &UsageAggregator{}, nil)
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
	events := translateEvent(raw, "s1", &UsageAggregator{}, nil)
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
	events := translateEvent(raw, "s1", &UsageAggregator{}, nil)
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
	events := translateEvent(raw, "s1", &UsageAggregator{}, nil)
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

func TestTranslateAssistant_TextBlock(t *testing.T) {
	raw := json.RawMessage(`{"type":"assistant","session_id":"s1","message":{"id":"msg_abc","role":"assistant","content":[{"type":"text","text":"hello world"}],"usage":{"input_tokens":1,"output_tokens":2}}}`)
	events := translateEvent(raw, "s1", &UsageAggregator{}, nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	e := events[0]
	if e.Type != msg.EventBlock {
		t.Fatalf("expected EventBlock, got %s", e.Type)
	}
	if e.Block == nil || e.Block.Block == nil {
		t.Fatal("Block payload missing")
	}
	if e.Block.MessageID != "msg_abc" {
		t.Errorf("MessageID = %q, want msg_abc", e.Block.MessageID)
	}
	if e.Block.Block.Type != msg.BlockText {
		t.Errorf("Block.Type = %q, want text", e.Block.Block.Type)
	}
	if e.Block.Block.Text == nil || e.Block.Block.Text.Text != "hello world" {
		t.Errorf("Text = %+v, want %q", e.Block.Block.Text, "hello world")
	}
}

func TestTranslateAssistant_ThinkingBlock(t *testing.T) {
	raw := json.RawMessage(`{"type":"assistant","session_id":"s1","message":{"id":"msg_def","role":"assistant","content":[{"type":"thinking","thinking":"reasoning text","signature":"sigblob"}],"usage":{"input_tokens":1,"output_tokens":2}}}`)
	events := translateEvent(raw, "s1", &UsageAggregator{}, nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event (no fan-out), got %d", len(events))
	}
	e := events[0]
	if e.Type != msg.EventBlock {
		t.Fatalf("expected EventBlock, got %s", e.Type)
	}
	if e.Block == nil || e.Block.Block == nil || e.Block.Block.Thinking == nil {
		t.Fatal("Thinking block payload missing")
	}
	if e.Block.Block.Type != msg.BlockThinking {
		t.Errorf("Block.Type = %q, want thinking", e.Block.Block.Type)
	}
	if e.Block.Block.Thinking.Text != "reasoning text" {
		t.Errorf("Thinking.Text = %q, want %q", e.Block.Block.Thinking.Text, "reasoning text")
	}
	if e.Block.Block.Thinking.Signature != "sigblob" {
		t.Errorf("Thinking.Signature = %q, want sigblob", e.Block.Block.Thinking.Signature)
	}
}

func TestTranslateAssistant_ToolUseAndResultUnchanged(t *testing.T) {
	raw := json.RawMessage(`{"type":"assistant","session_id":"s1","message":{"id":"msg_ghi","role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"cmd":"ls"}}],"usage":{"input_tokens":1,"output_tokens":2}}}`)
	events := translateEvent(raw, "s1", &UsageAggregator{}, nil)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != msg.EventToolCall {
		t.Errorf("expected EventToolCall, got %s", events[0].Type)
	}
}
