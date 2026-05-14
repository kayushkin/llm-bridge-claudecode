package main

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// Each test exercises translateRolloutEntry against a JSONL entry shape
// CC actually writes to disk. The fixtures are minimal but reflect the
// structures observed in real rollout files (see import_history.go's
// ccStoredEvent / ccStoredMessage).

func storedFrom(t *testing.T, line string) ccStoredEvent {
	t.Helper()
	var s ccStoredEvent
	if err := json.Unmarshal([]byte(line), &s); err != nil {
		t.Fatalf("fixture parse: %v\nline=%s", err, line)
	}
	return s
}

func TestTranslateRolloutEntry_UserText(t *testing.T) {
	stored := storedFrom(t, `{
		"type": "user",
		"uuid": "u-1",
		"sessionId": "sess-aaa",
		"timestamp": "2026-05-14T22:30:00Z",
		"message": {"role": "user", "content": [{"type": "text", "text": "hello there"}]}
	}`)
	out := translateRolloutEntry(stored)
	if len(out) != 1 {
		t.Fatalf("expected 1 event, got %d", len(out))
	}
	ev := out[0]
	if ev.Type != msg.EventUserMessage {
		t.Errorf("type = %q; want user_message", ev.Type)
	}
	if ev.Result == nil || ev.Result.Text != "hello there" {
		t.Errorf("text = %+v", ev.Result)
	}
	if string(ev.Extensions["source"]) != `"rollout"` {
		t.Errorf("missing rollout provenance tag: %v", ev.Extensions)
	}
	if ev.HarnessSessionID != "sess-aaa" {
		t.Errorf("HarnessSessionID = %q", ev.HarnessSessionID)
	}
}

func TestTranslateRolloutEntry_UserToolResult(t *testing.T) {
	stored := storedFrom(t, `{
		"type": "user",
		"uuid": "u-2",
		"sessionId": "sess-bbb",
		"timestamp": "2026-05-14T22:30:01Z",
		"message": {"role": "user", "content": [
			{"type": "tool_result", "id": "toolu_abc", "content": "stdout: ok", "is_error": false},
			{"type": "tool_result", "id": "toolu_def", "content": "ENOENT", "is_error": true}
		]}
	}`)
	out := translateRolloutEntry(stored)
	if len(out) != 2 {
		t.Fatalf("expected 2 tool_result events, got %d", len(out))
	}
	if out[0].Type != msg.EventToolResult || out[0].ToolResult == nil ||
		out[0].ToolResult.ToolID != "toolu_abc" ||
		out[0].ToolResult.Output != "stdout: ok" || out[0].ToolResult.IsError {
		t.Errorf("first tool_result: %+v", out[0].ToolResult)
	}
	if out[1].Type != msg.EventToolResult || out[1].ToolResult == nil ||
		out[1].ToolResult.ToolID != "toolu_def" ||
		out[1].ToolResult.Output != "ENOENT" || !out[1].ToolResult.IsError {
		t.Errorf("second tool_result: %+v", out[1].ToolResult)
	}
}

func TestTranslateRolloutEntry_AssistantMixedBlocks(t *testing.T) {
	// Real claude assistant turn that thinks, says something, then
	// invokes a tool. Translator should emit one event per block, in
	// order, with the harness UUID propagated so downstream consumers
	// can group them into a single chat bubble.
	stored := storedFrom(t, `{
		"type": "assistant",
		"uuid": "msg_xyz",
		"sessionId": "sess-ccc",
		"timestamp": "2026-05-14T22:30:05Z",
		"message": {
			"role": "assistant",
			"model": "claude-opus-4-7",
			"content": [
				{"type": "thinking", "thinking": "Let me check that file."},
				{"type": "text", "text": "I'll read it for you."},
				{"type": "tool_use", "id": "toolu_ghi", "name": "Read", "input": {"path": "/etc/hostname"}}
			],
			"stop_reason": "tool_use"
		}
	}`)
	out := translateRolloutEntry(stored)
	if len(out) != 3 {
		t.Fatalf("expected 3 events (thinking + text + tool_use), got %d", len(out))
	}

	if out[0].Type != msg.EventThinking || out[0].Thinking == nil ||
		out[0].Thinking.Text != "Let me check that file." {
		t.Errorf("thinking: %+v", out[0].Thinking)
	}
	if out[0].HarnessMessageID != "msg_xyz" {
		t.Errorf("thinking missing harness_message_id: %q", out[0].HarnessMessageID)
	}

	if out[1].Type != msg.EventBlock || out[1].Block == nil ||
		out[1].Block.Block == nil || out[1].Block.Block.Text == nil ||
		out[1].Block.Block.Text.Text != "I'll read it for you." {
		t.Errorf("text block: %+v", out[1].Block)
	}
	if out[1].Block.Index != 1 {
		t.Errorf("text block index = %d; want 1", out[1].Block.Index)
	}

	if out[2].Type != msg.EventToolCall || out[2].ToolCall == nil ||
		out[2].ToolCall.ToolID != "toolu_ghi" ||
		out[2].ToolCall.Name != "Read" {
		t.Errorf("tool_call: %+v", out[2].ToolCall)
	}
	// Input round-trips as raw JSON.
	var inputCheck map[string]any
	if err := json.Unmarshal(out[2].ToolCall.Input, &inputCheck); err != nil {
		t.Errorf("tool_call input unmarshal: %v", err)
	}
	if inputCheck["path"] != "/etc/hostname" {
		t.Errorf("tool_call input.path = %v; want /etc/hostname", inputCheck["path"])
	}

	// All events tagged.
	for i, ev := range out {
		if string(ev.Extensions["source"]) != `"rollout"` {
			t.Errorf("event[%d] missing rollout source: %v", i, ev.Extensions)
		}
	}
}

func TestTranslateRolloutEntry_EmptyContentSkipped(t *testing.T) {
	// A user "text" block with empty text shouldn't emit a user_message
	// (would render as an empty bubble). Similarly for blank text/
	// thinking in assistant blocks.
	stored := storedFrom(t, `{
		"type": "user",
		"uuid": "u-empty",
		"timestamp": "2026-05-14T22:30:00Z",
		"message": {"role": "user", "content": [{"type": "text", "text": ""}]}
	}`)
	if out := translateRolloutEntry(stored); len(out) != 0 {
		t.Errorf("empty user text should emit nothing, got %d events", len(out))
	}

	stored2 := storedFrom(t, `{
		"type": "assistant",
		"uuid": "a-empty",
		"timestamp": "2026-05-14T22:30:00Z",
		"message": {"role": "assistant", "content": [
			{"type": "text", "text": ""},
			{"type": "thinking", "thinking": ""}
		]}
	}`)
	if out := translateRolloutEntry(stored2); len(out) != 0 {
		t.Errorf("empty assistant blocks should emit nothing, got %d events", len(out))
	}
}

func TestTranslateRolloutEntry_UnknownTypeSkipped(t *testing.T) {
	// CC writes a "summary" entry type at session start. Translator
	// should skip silently rather than fabricate a wrong-typed event.
	stored := storedFrom(t, `{
		"type": "summary",
		"uuid": "summary-1",
		"timestamp": "2026-05-14T22:30:00Z",
		"message": {}
	}`)
	if out := translateRolloutEntry(stored); len(out) != 0 {
		t.Errorf("unknown entry type should emit nothing, got %d events", len(out))
	}
}

func TestTranslateRolloutEntry_ToolUseEmptyInputDefaultsToObject(t *testing.T) {
	// CC may emit tool_use with no input (zero-arg tools). The
	// translator stamps "{}" so downstream ToolCallEvent.Input remains
	// valid JSON rather than nil — bridge-ui and log-store both
	// json.Unmarshal it.
	stored := storedFrom(t, `{
		"type": "assistant",
		"uuid": "msg_noinput",
		"timestamp": "2026-05-14T22:30:00Z",
		"message": {"role": "assistant", "content": [
			{"type": "tool_use", "id": "toolu_x", "name": "ListDir"}
		]}
	}`)
	out := translateRolloutEntry(stored)
	if len(out) != 1 || out[0].ToolCall == nil {
		t.Fatalf("expected one tool_call event, got %+v", out)
	}
	if string(out[0].ToolCall.Input) != "{}" {
		t.Errorf("Input = %q; want {}", string(out[0].ToolCall.Input))
	}
}
