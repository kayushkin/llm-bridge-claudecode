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

// ──────────────────────────────────────────────────────────────────────────────
// Subagent demux key (TEAM-ORCHESTRATION.md §21.4)
// ──────────────────────────────────────────────────────────────────────────────

// subagentAssistantFrame is a verbatim subagent frame from a live
// `claude -p --output-format stream-json` capture (CC 2.1.x), trimmed to one
// content block. Note that session_id is the PARENT's — Claude Code stamps
// every frame it emits with the parent process's session id, which is exactly
// why parent_tool_use_id has to be the discriminator.
const subagentAssistantFrame = `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_011CdUvE9NrBfik9EV8vKZng","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":10,"output_tokens":4}},"parent_tool_use_id":"toolu_01C6u17fXLPviJJdM4tRLSjd","session_id":"5c3112cf-ac7b-4215-92de-991dd4628667","subagent_type":"Explore","task_description":"tiny-probe"}`

// parentAssistantFrame is the same shape without the discriminator — the
// parent's own work.
const parentAssistantFrame = `{"type":"assistant","message":{"model":"claude-haiku-4-5-20251001","id":"msg_0199parent","type":"message","role":"assistant","content":[{"type":"text","text":"spawning"}],"usage":{"input_tokens":10,"output_tokens":4}},"parent_tool_use_id":null,"session_id":"5c3112cf-ac7b-4215-92de-991dd4628667"}`

// TestTranslateEventCarriesSubagentParentID pins the demux key. Dropping
// parent_tool_use_id is what collapsed every harness subagent onto its parent's
// session and left the subagent rows with no link to what spawned them; the
// adapter is the only layer that ever sees the field.
func TestTranslateEventCarriesSubagentParentID(t *testing.T) {
	events := translateEvent(json.RawMessage(subagentAssistantFrame), "fallback-session", &UsageAggregator{}, nil)
	if len(events) == 0 {
		t.Fatal("subagent assistant frame produced no events")
	}
	const want = "toolu_01C6u17fXLPviJJdM4tRLSjd"
	for i, ev := range events {
		if ev.HarnessParentID != want {
			t.Errorf("event %d (%s): harness_parent_id = %q, want %q", i, ev.Type, ev.HarnessParentID, want)
		}
	}
}

// TestTranslateEventLeavesParentFramesUnstamped is the negative guard: if the
// parent's own frames picked up a harness_parent_id, the demux would route the
// parent's work into a subagent session.
func TestTranslateEventLeavesParentFramesUnstamped(t *testing.T) {
	events := translateEvent(json.RawMessage(parentAssistantFrame), "fallback-session", &UsageAggregator{}, nil)
	if len(events) == 0 {
		t.Fatal("parent assistant frame produced no events")
	}
	for i, ev := range events {
		if ev.HarnessParentID != "" {
			t.Errorf("event %d (%s): parent frame got harness_parent_id = %q, want empty", i, ev.Type, ev.HarnessParentID)
		}
	}
}

// TestTranslateEventStampsTaskNarrationOnParent locks in which side of the
// split the narration falls on: task_started describes a spawn the PARENT
// performed, so it must stay on the parent's session even though it names the
// subagent. The live capture confirms CC emits it with no parent_tool_use_id.
func TestTranslateEventStampsTaskNarrationOnParent(t *testing.T) {
	const frame = `{"type":"system","subtype":"task_started","task_id":"a13cb6a91d4c5d3dc","tool_use_id":"toolu_01C6u17fXLPviJJdM4tRLSjd","description":"tiny-probe","subagent_type":"Explore","session_id":"5c3112cf-ac7b-4215-92de-991dd4628667"}`
	events := translateEvent(json.RawMessage(frame), "fallback-session", &UsageAggregator{}, nil)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.HarnessParentID != "" {
		t.Errorf("task_started got harness_parent_id = %q; it belongs to the parent that spawned the task", ev.HarnessParentID)
	}
	// The demux needs both ids off this frame to build the subagent's dedupe key.
	if ev.System == nil || ev.System.TaskID != "a13cb6a91d4c5d3dc" || ev.System.ToolUseID != "toolu_01C6u17fXLPviJJdM4tRLSjd" {
		t.Fatalf("task_started did not surface both correlator ids: %+v", ev.System)
	}
}

// subagentPromptFrame is the verbatim frame Claude Code emits for a Task
// subagent under the flags this bridge runs it with:
//
//	claude -p --input-format stream-json --output-format stream-json --verbose
//
// Measured, not assumed. In that mode it is the ONLY frame the subagent
// contributes to the parent's stream — the subagent's assistant turns never
// appear inline, they only reach its own rollout file on disk. (Plain `-p` with
// a text prompt behaves differently and DOES inline the assistant turns, which
// is what an earlier capture recorded; the bridge does not run that way.)
const subagentPromptFrame = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"reply with the single word ping"}]},"parent_tool_use_id":"toolu_01QSYM3UeBGpKU28zdnpuftA","session_id":"c8939508-e6ea-4341-b614-c0adbe9ace1c","subagent_type":"Explore","task_description":"probe2"}`

// operatorEchoFrame is the shape that must still be dropped: a user frame with
// no parent_tool_use_id is an echo of a message the operator already sent.
const operatorEchoFrame = `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"do the thing"}]},"parent_tool_use_id":null,"session_id":"c8939508-e6ea-4341-b614-c0adbe9ace1c"}`

// TestTranslateEventKeepsSubagentPromptFrame is the regression for the bug the
// live e2e caught: every `user` frame was dropped as an echo, which threw away
// the one frame carrying parent_tool_use_id. The demux upstream was correct and
// still never fired, because nothing ever reached it.
func TestTranslateEventKeepsSubagentPromptFrame(t *testing.T) {
	events := translateEvent(json.RawMessage(subagentPromptFrame), "fallback-session", &UsageAggregator{}, nil)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 — the subagent's only live frame must survive translation", len(events))
	}
	ev := events[0]
	if ev.Type != msg.EventUserMessage {
		t.Errorf("type = %q, want user_message", ev.Type)
	}
	if ev.HarnessParentID != "toolu_01QSYM3UeBGpKU28zdnpuftA" {
		t.Errorf("harness_parent_id = %q; without it bridge-server cannot route the frame", ev.HarnessParentID)
	}
	if ev.Result == nil || ev.Result.Text != "reply with the single word ping" {
		t.Errorf("prompt text did not survive: %+v", ev.Result)
	}
}

func TestTranslateEventStillDropsOperatorEcho(t *testing.T) {
	if events := translateEvent(json.RawMessage(operatorEchoFrame), "fallback-session", &UsageAggregator{}, nil); len(events) != 0 {
		t.Fatalf("operator echo produced %d events, want 0", len(events))
	}
}

// taskToolResultFrame is the frame that closes a Task call out, verbatim from a
// live rollout. Two details killed it before: the call is named by
// `tool_use_id` (not `id`), and the content is an ARRAY of blocks rather than a
// bare string. A string-typed Content field fails to unmarshal this exact
// shape, so the frames reporting a subagent were the ones most reliably lost.
// parent_tool_use_id is absent because the result is addressed to the PARENT.
const taskToolResultFrame = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01EY4GRJgffcgazw4KotQuwu","type":"tool_result","content":[{"type":"text","text":"Async agent launched successfully."}]}]},"session_id":"c8939508-e6ea-4341-b614-c0adbe9ace1c"}`

// bashToolResultFrame is the ordinary shape: a parent-session tool reporting
// back with string content. It has no parent_tool_use_id either, which is why
// the blanket drop took every tool's output with it, not just Task's.
const bashToolResultFrame = `{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_01Bash","type":"tool_result","content":"stdout: ok"}]},"session_id":"c8939508-e6ea-4341-b614-c0adbe9ace1c"}`

// TestTranslateUserFrameEmitsTaskToolResult is the regression for the reported
// bug: a Task block spun forever because the only event that could resolve it
// was discarded. The UI pairs strictly on tool_id, so an event with an empty
// ToolID is no better than no event at all.
func TestTranslateUserFrameEmitsTaskToolResult(t *testing.T) {
	events := translateEvent(json.RawMessage(taskToolResultFrame), "fallback-session", &UsageAggregator{}, nil)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 — the Task result must survive translation", len(events))
	}
	ev := events[0]
	if ev.Type != msg.EventToolResult {
		t.Fatalf("type = %q, want tool_result", ev.Type)
	}
	if ev.ToolResult.ToolID != "toolu_01EY4GRJgffcgazw4KotQuwu" {
		t.Errorf("tool_id = %q; without it nothing can pair the result to its Task call", ev.ToolResult.ToolID)
	}
	if ev.ToolResult.Output != "Async agent launched successfully." {
		t.Errorf("output = %q, want the array content flattened to text", ev.ToolResult.Output)
	}
}

func TestTranslateUserFrameEmitsOrdinaryToolResult(t *testing.T) {
	events := translateEvent(json.RawMessage(bashToolResultFrame), "fallback-session", &UsageAggregator{}, nil)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	tr := events[0].ToolResult
	if tr.ToolID != "toolu_01Bash" || tr.Output != "stdout: ok" || tr.IsError {
		t.Errorf("tool_result = %+v", tr)
	}
}

// A subagent's frame can carry its prompt and a tool result at once. Emitting
// only one shape per frame would silently drop the other.
func TestTranslateUserFrameEmitsBothResultAndPrompt(t *testing.T) {
	frame := `{"type":"user","message":{"role":"user","content":[` +
		`{"tool_use_id":"toolu_01Sub","type":"tool_result","content":"done","is_error":true},` +
		`{"type":"text","text":"now do the next thing"}` +
		`]},"parent_tool_use_id":"toolu_01Parent","session_id":"s"}`
	events := translateEvent(json.RawMessage(frame), "fallback-session", &UsageAggregator{}, nil)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (tool_result + user_message)", len(events))
	}
	if events[0].Type != msg.EventToolResult || !events[0].ToolResult.IsError {
		t.Errorf("first event = %+v, want a failing tool_result", events[0])
	}
	if events[1].Type != msg.EventUserMessage || events[1].Result.Text != "now do the next thing" {
		t.Errorf("second event = %+v, want the subagent prompt", events[1])
	}
	for _, ev := range events {
		if ev.HarnessParentID != "toolu_01Parent" {
			t.Errorf("harness_parent_id = %q on %s; both events belong to the subagent", ev.HarnessParentID, ev.Type)
		}
	}
}

func TestDecodeToolResultContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"bare string", `"stdout"`, "stdout"},
		{"block array", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{"image block ignored", `[{"type":"image"}]`, ""},
		{"empty", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeToolResultContent(json.RawMessage(tc.content)); got != tc.want {
				t.Errorf("decodeToolResultContent(%s) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

func TestExtractUserText(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"bare string", `"hello"`, "hello"},
		{"text blocks", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, "a\nb"},
		{"tool result only", `[{"type":"tool_result","content":"x"}]`, ""},
		{"empty", ``, ""},
		{"malformed", `{"nope":1}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractUserText(json.RawMessage(tc.content)); got != tc.want {
				t.Errorf("extractUserText(%s) = %q, want %q", tc.content, got, tc.want)
			}
		})
	}
}

// TestTranslateSystemSurfacesTaskTerminalIDs is the second regression the live
// e2e caught. task_updated and task_notification fell through to the catch-all
// system branch, which sets only Subtype — so the frames that report a
// subagent finished arrived without naming the subagent, and every promoted
// subagent session sat at "running" forever. Both frames are verbatim from a
// live capture.
func TestTranslateSystemSurfacesTaskTerminalIDs(t *testing.T) {
	cases := []struct {
		name    string
		frame   string
		subtype string
	}{
		{
			"task_updated",
			`{"type":"system","subtype":"task_updated","task_id":"a13cb6a91d4c5d3dc","patch":{"status":"completed","end_time":1785262846220},"session_id":"5c3112cf"}`,
			"task_updated",
		},
		{
			"task_notification",
			`{"type":"system","subtype":"task_notification","task_id":"a13cb6a91d4c5d3dc","tool_use_id":"toolu_01C6u17fXLPviJJdM4tRLSjd","status":"completed","session_id":"5c3112cf"}`,
			"task_notification",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := translateEvent(json.RawMessage(tc.frame), "fallback-session", &UsageAggregator{}, nil)
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			sys := events[0].System
			if sys == nil || sys.Subtype != tc.subtype {
				t.Fatalf("subtype = %+v, want %s", sys, tc.subtype)
			}
			if sys.TaskID != "a13cb6a91d4c5d3dc" {
				t.Errorf("task_id = %q; without it nothing can tell which subagent this closes", sys.TaskID)
			}
			// The status stays on Raw — there is no canonical field for it —
			// so Raw must survive translation.
			if len(events[0].Raw) == 0 {
				t.Error("Raw was dropped; the terminal status is only readable there")
			}
		})
	}
}

// TestTranslateSystemNormalizesTaskStatus pins the field that actually settles
// a subagent. Claude Code spells the status two different ways — task_updated
// nests it under "patch", task_notification puts it at the top level — and it
// used to be surfaced as neither, left on Raw with a comment saying so. That
// pushed the difference onto every consumer: bridge-server re-parsed the raw
// frame to recover it, a second parser of CC's wire format that would have
// broken silently the day CC renamed the field.
//
// Frames are verbatim from a live capture (CC 2.1.220, the flags the bridge
// actually runs: -p --input-format stream-json --output-format stream-json).
func TestTranslateSystemNormalizesTaskStatus(t *testing.T) {
	cases := []struct {
		name       string
		frame      string
		wantStatus string
		wantSummry string
		wantOutput string
	}{
		{
			name:       "task_updated nests status under patch",
			frame:      `{"type":"system","subtype":"task_updated","task_id":"a66ecc868c9c5c8db","patch":{"status":"completed","end_time":1785969393340},"session_id":"741b7725"}`,
			wantStatus: "completed",
		},
		{
			name: "task_notification puts status at top level, with the report",
			// Interpreted literal, not raw: the real summary is markdown and
			// contains code fences, which would close a raw string.
			frame:      "{\"type\":\"system\",\"subtype\":\"task_notification\",\"task_id\":\"a66ecc868c9c5c8db\",\"tool_use_id\":\"toolu_01PdEBPefCBfgx62jXmkfBvP\",\"status\":\"completed\",\"output_file\":\"/tmp/claude-1000/x/tasks/a66ecc868c9c5c8db.output\",\"summary\":\"The command's exact stdout was:\\n\\n```\\nhi\\n```\",\"session_id\":\"741b7725\"}",
			wantStatus: "completed",
			wantSummry: "The command's exact stdout was:\n\n```\nhi\n```",
			wantOutput: "/tmp/claude-1000/x/tasks/a66ecc868c9c5c8db.output",
		},
		{
			name:       "a non-terminal status is carried through, not swallowed",
			frame:      `{"type":"system","subtype":"task_updated","task_id":"a66ecc868c9c5c8db","patch":{"status":"in_progress"},"session_id":"741b7725"}`,
			wantStatus: "in_progress",
		},
		// The next two frames are the SAME task (bmmxgu8ao), verbatim from the
		// event store: CC reports it "stopped" and then patches it to "killed".
		// Both are the canonical cancelled. Until they were mapped,
		// TaskStatusIsTerminal recognized neither and the task never settled.
		{
			name:       "stopped is cancelled",
			frame:      `{"type":"system","subtype":"task_notification","task_id":"bmmxgu8ao","tool_use_id":"toolu_014vDBbaWGQJdmGU6QhVH6fh","status":"stopped","output_file":"","summary":"wait for ingest curl to finish","session_id":"fbb3090f"}`,
			wantStatus: msg.TaskStatusCancelled,
			wantSummry: "wait for ingest curl to finish",
		},
		{
			name:       "killed is cancelled too — same task, patched a moment later",
			frame:      `{"type":"system","subtype":"task_updated","task_id":"bmmxgu8ao","patch":{"status":"killed","end_time":1776671433547},"session_id":"fbb3090f"}`,
			wantStatus: msg.TaskStatusCancelled,
		},
		{
			name: "a status with no mapping passes through verbatim",
			// Unknown must not become "" and must not be guessed at: an empty
			// status says nothing, and TaskStatusIsTerminal already declines to
			// settle a word it does not know. Carrying it makes the new status
			// visible to whoever reads the event.
			frame:      `{"type":"system","subtype":"task_updated","task_id":"a66ecc868c9c5c8db","patch":{"status":"quiesced"},"session_id":"741b7725"}`,
			wantStatus: "quiesced",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := translateEvent(json.RawMessage(tc.frame), "fallback-session", &UsageAggregator{}, nil)
			if len(events) != 1 {
				t.Fatalf("got %d events, want 1", len(events))
			}
			sys := events[0].System
			if sys == nil {
				t.Fatal("no SystemEvent")
			}
			if sys.TaskStatus != tc.wantStatus {
				t.Errorf("TaskStatus = %q, want %q — without it nothing can settle the subagent", sys.TaskStatus, tc.wantStatus)
			}
			if sys.TaskSummary != tc.wantSummry {
				t.Errorf("TaskSummary = %q, want %q", sys.TaskSummary, tc.wantSummry)
			}
			if sys.TaskOutputFile != tc.wantOutput {
				t.Errorf("TaskOutputFile = %q, want %q", sys.TaskOutputFile, tc.wantOutput)
			}
		})
	}
}
