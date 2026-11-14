package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// Stored Claude Code session lines, in the shape `~/.claude/projects/*.jsonl`
// holds them: the assistant message carries the tool_use, and the tool_result
// arrives on the next line, in a user message with no text.
const (
	storedAssistantToolUse  = `{"type":"assistant","sessionId":"s1","timestamp":"2026-08-17T10:00:00Z","message":{"model":"claude-x","content":[{"type":"text","text":"Listing."},{"type":"tool_use","id":"toolu_A","name":"Bash","input":{"command":"ls"}}],"usage":{"input_tokens":3,"output_tokens":4}}}`
	storedUserToolResult    = `{"type":"user","sessionId":"s1","timestamp":"2026-08-17T10:00:01Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_A","content":[{"type":"text","text":"a.go\nb.go"}]}]}}`
	storedUserToolError     = `{"type":"user","sessionId":"s1","timestamp":"2026-08-17T10:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_B","content":"exit 1","is_error":true}]}}`
	storedUserText          = `{"type":"user","sessionId":"s1","timestamp":"2026-08-17T10:00:03Z","message":{"role":"user","content":"thanks"}}`
	storedUserResultAndText = `{"type":"user","sessionId":"s1","timestamp":"2026-08-17T10:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_C","content":""},{"type":"text","text":"and also"}]}}`
)

func translateStoredLine(t *testing.T, line string) []msg.Event {
	t.Helper()
	var stored ccStoredEvent
	if err := json.Unmarshal([]byte(line), &stored); err != nil {
		t.Fatalf("fixture does not parse: %v", err)
	}
	return translateStoredEvent(stored, "fallback-session")
}

func TestImportEmitsToolCallWithItsID(t *testing.T) {
	events := translateStoredLine(t, storedAssistantToolUse)
	if len(events) != 2 {
		t.Fatalf("got %d events, want result + tool_call", len(events))
	}
	if events[0].Type != msg.EventResult || events[0].Result.Text != "Listing." {
		t.Errorf("first event = %q %+v, want the result with the text", events[0].Type, events[0].Result)
	}
	call := events[1]
	if call.Type != msg.EventToolCall {
		t.Fatalf("second event type = %q, want tool_call", call.Type)
	}
	if call.ToolCall.ToolID != "toolu_A" || call.ToolCall.Name != "Bash" {
		t.Errorf("tool_call = %+v, want id toolu_A name Bash", call.ToolCall)
	}
	if string(call.ToolCall.Input) != `{"command":"ls"}` {
		t.Errorf("tool_call input = %s", call.ToolCall.Input)
	}
	if call.HarnessSessionID != "s1" {
		t.Errorf("session = %q, want the stored session id s1", call.HarnessSessionID)
	}
}

// A user message made only of tool results has no text. It used to produce no
// event at all, which dropped the output of every tool in an imported session.
func TestImportEmitsToolResultFromUserMessage(t *testing.T) {
	events := translateStoredLine(t, storedUserToolResult)
	if len(events) != 1 {
		t.Fatalf("got %d events, want one tool_result", len(events))
	}
	ev := events[0]
	if ev.Type != msg.EventToolResult {
		t.Fatalf("type = %q, want tool_result", ev.Type)
	}
	if ev.ToolResult.ToolID != "toolu_A" {
		t.Errorf("tool_id = %q, want toolu_A, the id of the call it answers", ev.ToolResult.ToolID)
	}
	if ev.ToolResult.Output != "a.go\nb.go" {
		t.Errorf("output = %q", ev.ToolResult.Output)
	}
	if ev.ToolResult.IsError {
		t.Error("is_error set on a result that did not fail")
	}
	if got := ev.Timestamp.Format("15:04:05"); got != "10:00:01" {
		t.Errorf("timestamp = %s, want the stored 10:00:01", got)
	}
}

func TestImportCarriesToolResultError(t *testing.T) {
	events := translateStoredLine(t, storedUserToolError)
	if len(events) != 1 || events[0].Type != msg.EventToolResult {
		t.Fatalf("got %+v, want one tool_result", events)
	}
	r := events[0].ToolResult
	if r.ToolID != "toolu_B" || r.Output != "exit 1" || !r.IsError {
		t.Errorf("tool_result = %+v, want toolu_B, \"exit 1\", is_error", r)
	}
}

func TestImportKeepsUserText(t *testing.T) {
	events := translateStoredLine(t, storedUserText)
	if len(events) != 1 || events[0].Type != msg.EventType("user_message") {
		t.Fatalf("got %+v, want one user_message", events)
	}
	if events[0].Result.Text != "thanks" {
		t.Errorf("text = %q", events[0].Result.Text)
	}
}

// One message can carry a tool result and text. Each is emitted; an empty
// result is still a result, not a missing one.
func TestImportEmitsBothResultAndText(t *testing.T) {
	events := translateStoredLine(t, storedUserResultAndText)
	if len(events) != 2 {
		t.Fatalf("got %d events, want tool_result + user_message", len(events))
	}
	if events[0].Type != msg.EventToolResult || events[0].ToolResult.ToolID != "toolu_C" || events[0].ToolResult.Output != "" {
		t.Errorf("first = %q %+v, want the empty tool_result for toolu_C", events[0].Type, events[0].ToolResult)
	}
	if events[1].Type != msg.EventType("user_message") || events[1].Result.Text != "and also" {
		t.Errorf("second = %q %+v, want the user_message", events[1].Type, events[1].Result)
	}
}
