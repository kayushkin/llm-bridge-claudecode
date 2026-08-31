package main

import (
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/identity"
	"github.com/kayushkin/llm-bridge/msg"
)

const harness = msg.HarnessClaudeCode

// ccStreamEvent is the top-level structure of a Claude Code stream-json line.
type ccStreamEvent struct {
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype,omitempty"`
	SessionID string          `json:"session_id,omitempty"`
	Message   json.RawMessage `json:"message,omitempty"`
	Result    string          `json:"result,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// ParentToolUseID is the tool_use_id of the Task/Agent call that spawned
	// the subagent this frame belongs to. Claude Code emits a subagent's own
	// frames inline on the parent process's stdout, all carrying the PARENT's
	// session_id, so session_id cannot tell them apart — this field is the only
	// discriminator. Null/absent on the parent's own frames.
	ParentToolUseID string `json:"parent_tool_use_id,omitempty"`

	// Result fields
	DurationMS        int64             `json:"duration_ms,omitempty"`
	DurationAPIMS     int64             `json:"duration_api_ms,omitempty"`
	NumTurns          int               `json:"num_turns,omitempty"`
	TotalCostUSD      float64           `json:"total_cost_usd,omitempty"`
	PermissionDenials []json.RawMessage `json:"permission_denials,omitempty"`
}

// ccAssistantMessage is a CC assistant event's message payload.
type ccAssistantMessage struct {
	ID      string             `json:"id"`
	Role    string             `json:"role"`
	Content []ccAssistantBlock `json:"content"`
	Usage   ccAssistantUsage   `json:"usage"`
}

// ccAssistantBlock is a content block within a CC assistant message.
type ccAssistantBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Signature string          `json:"signature,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// translateEvent converts a raw CC stream-json event into canonical msg.Event(s).
// Returns nil for events that should be consumed internally (keep_alive, control_response).
//
// tracker, when non-nil, is used to map CC's per-message id (am.ID from
// assistant_message events) to a canonical bridge MessageID — the
// adapter pre-stamps Event.MessageID so bridge-server doesn't need to
// own assignAssistantID (Phase III.B). nil tracker is the legacy path
// that puts the harness id in BlockEvent.MessageID and lets bridge-server
// do the mapping; both paths emit the same Event shape.
func translateEvent(raw json.RawMessage, sessionID string, agg *UsageAggregator, tracker *identity.Tracker) []msg.Event {
	var ev ccStreamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil
	}

	// Use CC's session_id if available, fall back to ours.
	sid := ev.SessionID
	if sid == "" {
		sid = sessionID
	}

	out := translateEventByType(ev, sid, raw, agg, tracker)

	// Carry the subagent discriminator through to the canonical event so
	// bridge-server can demux a harness subagent's frames into their own
	// session (TEAM-ORCHESTRATION.md §21.4).
	//
	// Measured against a live capture taken with the exact flags spawnClaudeCode
	// uses: every frame — parent and subagent alike — carries the PARENT's
	// session_id, so session_id discriminates nothing, and only a subagent's
	// frames carry parent_tool_use_id. The task_started/task_progress narration
	// leaves it null and so stays on the parent, which is right: the parent is
	// the one narrating that it spawned a task.
	//
	// How much a subagent contributes is flag-dependent — under these flags it
	// is one `user` frame; see the `case "user"` branch below.
	if ev.ParentToolUseID != "" {
		for i := range out {
			out[i].HarnessParentID = ev.ParentToolUseID
		}
	}
	return out
}

// translateEventByType dispatches one parsed stream-json frame to the
// per-type translator. Split out of translateEvent so the caller can stamp
// frame-wide fields (HarnessParentID) onto every event a frame produces
// without repeating the assignment at each translator's construction sites.
func translateEventByType(ev ccStreamEvent, sid string, raw json.RawMessage, agg *UsageAggregator, tracker *identity.Tracker) []msg.Event {
	switch ev.Type {
	case "system":
		return translateSystem(ev, sid, raw)
	case "assistant":
		return translateAssistant(ev, sid, raw, agg, tracker)
	case "result":
		out := translateResult(ev, sid, raw, agg)
		if tracker != nil {
			tracker.EndTurn()
		}
		return out
	case "rate_limit_event":
		return translateRateLimit(sid, raw)
	case "tool_progress":
		return translateToolProgress(sid, raw)
	case "control_response", "keep_alive":
		return nil // consumed internally
	case "user":
		return translateUserFrame(ev, sid, raw)
	default:
		// Forward unknown events as system events for visibility.
		return []msg.Event{makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: ev.Type, Message: string(raw)}
		})}
	}
}

// translateUserFrame converts a Claude Code `user` frame into canonical events.
// The frame type is overloaded three ways, and each needs different handling:
//
//   - Tool results. Every tool Claude Code runs reports back in a `user` frame
//     carrying tool_result blocks, because that is where the Anthropic wire
//     format requires them to live — a tool_result may only appear in a
//     user-role message. These frames are the ONLY live source of tool output
//     in stream-json mode. That covers the parent's own tools (which carry no
//     parent_tool_use_id) as much as a subagent's, and it includes the Task
//     tool's own result — the event that tells a Task block it finished.
//   - A subagent's prompt: a frame WITH parent_tool_use_id whose content is
//     text. That text is the instructions handed to the Task subagent, so it
//     becomes a user_message on the subagent's own session once bridge-server
//     routes it there by parent_tool_use_id.
//   - An echo of the operator's own message (e.g. the interrupt synthetic):
//     text with no parent_tool_use_id. Already recorded when it was sent, so
//     it is skipped.
//
// A single frame can carry both tool results and text, so the block walk emits
// each independently rather than choosing one shape for the whole frame.
func translateUserFrame(ev ccStreamEvent, sid string, raw json.RawMessage) []msg.Event {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(ev.Message, &m) != nil {
		return nil
	}

	var events []msg.Event
	for _, block := range parseUserBlocks(m.Content) {
		if block.Type != "tool_result" {
			continue
		}
		events = append(events, makeEvent(sid, msg.EventToolResult, raw, func(e *msg.Event) {
			e.ToolResult = &msg.ToolResultEvent{
				ToolID:  block.ToolUseID,
				Output:  decodeToolResultContent(block.Content),
				IsError: block.IsError,
			}
		}))
	}

	// Only a subagent's text is new information; the operator's own text was
	// recorded when it was sent.
	if ev.ParentToolUseID != "" {
		if text := extractUserText(m.Content); text != "" {
			events = append(events, makeEvent(sid, msg.EventUserMessage, raw, func(e *msg.Event) {
				e.Result = &msg.ResultEvent{Text: text}
			}))
		}
	}
	return events
}

// ccUserBlock is a content block inside a Claude Code `user` frame.
//
// A tool_result names its call with `tool_use_id`, NOT `id` — `id` is what a
// tool_use block uses. Reading `id` here yields an empty ToolID, which is
// indistinguishable from "unpairable" to every downstream consumer, so the
// result silently never reaches the tool row it belongs to.
type ccUserBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// parseUserBlocks decodes a user message's content into blocks. Content is
// either a bare string (which carries no blocks) or an array of them.
func parseUserBlocks(content json.RawMessage) []ccUserBlock {
	if len(content) == 0 {
		return nil
	}
	var blocks []ccUserBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	return blocks
}

// decodeToolResultContent renders a tool_result block's content as plain text.
// Claude Code writes it either as a bare string or as an array of content
// blocks — measured on live rollouts, both shapes occur, and the array shape is
// what the Task tool uses. Typing this field as a plain string therefore fails
// to unmarshal exactly the frames that report a subagent, taking the whole
// message down with it.
//
// Non-text blocks (images) contribute nothing.
func decodeToolResultContent(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(content, &asString) == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// extractUserText pulls the plain text out of a CC user message's content,
// which is either a bare string or an array of content blocks. Non-text blocks
// (tool results, images) contribute nothing.
func extractUserText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(content, &asString) == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// parseInitInfo extracts the session-metadata fields from a Claude Code
// `{"type":"system","subtype":"init",...}` event. The CC CLI is the canonical
// source for tools / slash_commands / agents / skills / MCP servers / model;
// fields that CC doesn't emit (system prompt, append system prompt) must be
// filled in by the caller from the StartParams the harness was given.
func parseInitInfo(raw json.RawMessage) *msg.SessionInfo {
	var init struct {
		Model          string `json:"model"`
		CWD            string `json:"cwd"`
		PermissionMode string `json:"permissionMode"`
		Tools          []string `json:"tools"`
		SlashCommands  []string `json:"slash_commands"`
		Agents         []string `json:"agents"`
		Skills         []string `json:"skills"`
		MCPServers     []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"mcp_servers"`
	}
	if err := json.Unmarshal(raw, &init); err != nil {
		return nil
	}
	info := &msg.SessionInfo{
		WorkingDir:     init.CWD,
		Model:          init.Model,
		PermissionMode: init.PermissionMode,
		SlashCommands:  init.SlashCommands,
		Agents:         init.Agents,
		Skills:         init.Skills,
	}
	for _, t := range init.Tools {
		info.Tools = append(info.Tools, msg.ToolInfo{Name: t})
	}
	for _, m := range init.MCPServers {
		info.MCPServers = append(info.MCPServers, msg.MCPServerInfo{Name: m.Name, Status: m.Status})
	}
	return info
}

// canonicalTaskStatus maps Claude Code's task status vocabulary onto the
// canonical one in msg. llm-bridge owns the vocabulary; this is where CC's
// words become it.
//
// "stopped" and "killed" are one outcome narrated twice for the same task:
// task_notification reports "stopped" with the summary, then task_updated
// patches the record to "killed" with an end_time. The bridge draws no
// distinction between the two, so both land on TaskStatusCancelled.
//
// Measured over the whole event store: completed 7931, failed 256, stopped 105,
// killed 90. Neither stopped nor killed was recognized, and TaskStatusIsTerminal
// rejects what it does not know, so roughly 195 tasks stayed "running" forever —
// their sessions never settled and the live status line never let them go. CC
// never says "cancelled", so the canonical value was unreachable from this
// harness.
var canonicalTaskStatus = map[string]string{
	"completed":   msg.TaskStatusCompleted,
	"failed":      msg.TaskStatusFailed,
	"stopped":     msg.TaskStatusCancelled,
	"killed":      msg.TaskStatusCancelled,
	"cancelled":   msg.TaskStatusCancelled,
	"in_progress": msg.TaskStatusInProgress,
}

// taskStatusOf picks the status off whichever field the subtype spells it in and
// returns the canonical name for it.
//
// A status with no mapping passes through verbatim rather than being dropped or
// guessed at: TaskStatusIsTerminal treats what it does not recognize as
// non-terminal, so an unknown word leaves the task open, which is the safe
// direction. It also keeps the unknown word visible to whoever reads the event,
// instead of hiding a new CC status behind an empty field.
func taskStatusOf(topLevel, patched string) string {
	status := topLevel
	if status == "" {
		status = patched
	}
	if canonical, ok := canonicalTaskStatus[status]; ok {
		return canonical
	}
	return status
}

func translateSystem(ev ccStreamEvent, sid string, raw json.RawMessage) []msg.Event {
	var events []msg.Event

	switch ev.Subtype {
	case "init":
		// Extract model from init for the system event message. Session
		// state on init is derived centrally by llm-bridge-server from
		// the raw event stream — harnesses no longer emit SessionState.
		var init struct {
			Model string `json:"model"`
			CWD   string `json:"cwd"`
		}
		_ = json.Unmarshal(raw, &init)
		events = append(events, makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: "init", Message: "model=" + init.Model + " cwd=" + init.CWD}
		}))

	case "api_retry":
		var retry struct {
			Attempt      int `json:"attempt"`
			MaxRetries   int `json:"max_retries"`
			RetryDelayMS int `json:"retry_delay_ms"`
			ErrorStatus  int `json:"error_status"`
		}
		_ = json.Unmarshal(raw, &retry)
		events = append(events, makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{
				Subtype:      "api_retry",
				Attempt:      retry.Attempt,
				MaxRetries:   retry.MaxRetries,
				RetryDelayMS: retry.RetryDelayMS,
				ErrorStatus:  retry.ErrorStatus,
			}
		}))

	case "task_started":
		// task_started opens a sub-agent task. Claude Code includes a
		// description ("Deploy to dash"), task_id, tool_use_id, and a
		// task_type tag — preserve them all so the UI can render the task
		// opener with context instead of a bare "Task" row.
		var ts struct {
			TaskID       string `json:"task_id"`
			ToolUseID    string `json:"tool_use_id"`
			Description  string `json:"description"`
			TaskType     string `json:"task_type"`
			SubagentType string `json:"subagent_type"`
		}
		_ = json.Unmarshal(raw, &ts)
		// task_started is the ONLY frame that says what kind of task this is.
		// Claude Code backgrounds a shell command through the same task frames
		// it uses for a subagent ("local_bash" vs "local_agent"), so without
		// the kind a consumer cannot tell an agent from `sleep 2` — and
		// bridge-server promoted both to sessions.
		events = append(events, makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{
				Subtype:      "task_started",
				ToolUseID:    ts.ToolUseID,
				TaskID:       ts.TaskID,
				Description:  ts.Description,
				TaskType:     ts.TaskType,
				SubagentType: ts.SubagentType,
			}
		}))

	case "task_progress":
		// Claude Code narrates in-flight tool calls with a task_progress
		// event that carries the tool_use_id of the tool it's reporting on.
		// Surface the correlator fields so the bridge server can resolve
		// this event back to its containing message bubble.
		var tp struct {
			TaskID       string `json:"task_id"`
			ToolUseID    string `json:"tool_use_id"`
			Description  string `json:"description"`
			LastToolName string `json:"last_tool_name"`
		}
		_ = json.Unmarshal(raw, &tp)
		events = append(events, makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{
				Subtype:      "task_progress",
				ToolUseID:    tp.ToolUseID,
				TaskID:       tp.TaskID,
				Description:  tp.Description,
				LastToolName: tp.LastToolName,
			}
		}))

	case "task_updated", "task_notification":
		// The frames that close a subagent task out. Both name the task they
		// report on, and without that id surfaced they arrive as anonymous
		// "something changed" events — a consumer cannot tell WHICH subagent
		// finished, which is what left promoted subagent sessions stuck at
		// running.
		//
		// Claude Code spells the status differently per subtype: task_updated
		// nests it under "patch", task_notification puts it at the top level.
		// That difference is CC's, not the bridge's, so it is normalized here
		// onto SystemEvent.TaskStatus rather than left on Raw for every
		// consumer to rediscover. It used to be left on Raw, and bridge-server
		// duly re-parsed the frame to get it back. taskStatusOf settles both
		// the spelling and the vocabulary — see the map above it.
		//
		// Only task_notification carries the summary and the transcript path.
		// task_updated is the state change; task_notification is the report.
		var tu struct {
			TaskID     string `json:"task_id"`
			ToolUseID  string `json:"tool_use_id"`
			Status     string `json:"status"`
			Summary    string `json:"summary"`
			OutputFile string `json:"output_file"`
			Patch      struct {
				Status string `json:"status"`
			} `json:"patch"`
		}
		_ = json.Unmarshal(raw, &tu)
		status := taskStatusOf(tu.Status, tu.Patch.Status)
		events = append(events, makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{
				Subtype:        ev.Subtype,
				TaskID:         tu.TaskID,
				ToolUseID:      tu.ToolUseID,
				TaskStatus:     status,
				TaskSummary:    tu.Summary,
				TaskOutputFile: tu.OutputFile,
			}
		}))

	case "hook_started", "hook_progress", "hook_response":
		events = append(events, translateHook(ev, sid, raw)...)

	default:
		// Forward all other system subtypes (compact_boundary, task_*, status, etc.)
		events = append(events, makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: ev.Subtype}
		}))
	}

	return events
}

// ccHookEvent is the Claude Code stream-json payload for hook lifecycle events
// emitted when the process is run with --include-hook-events. CC emits three
// subtypes: hook_started (fires when a hook begins), hook_progress (optional
// intermediate update), and hook_response (fires when the hook returns).
type ccHookEvent struct {
	HookID    string `json:"hook_id"`
	HookName  string `json:"hook_name"`  // "<Event>:<index-or-name>"
	HookEvent string `json:"hook_event"` // "PreToolUse", "SessionStart", etc.
	ToolName  string `json:"tool_name,omitempty"`

	// Populated on hook_response only.
	Output   string `json:"output,omitempty"`
	Stdout   string `json:"stdout,omitempty"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Outcome  string `json:"outcome,omitempty"` // "success", "timeout", "error", ...
}

// translateHook converts a CC hook lifecycle event into a canonical HookEvent.
// HookID on the emitted msg.HookEvent is left empty — it is the bridge's
// hook-store registry id, which only applies when the hook was registered via
// the bridge server. For observation (CC-native hooks defined in settings.json
// outside the bridge), there is no registry id to report.
func translateHook(ev ccStreamEvent, sid string, raw json.RawMessage) []msg.Event {
	var h ccHookEvent
	_ = json.Unmarshal(raw, &h)

	hook := &msg.HookEvent{
		Event:    h.HookEvent,
		ToolName: h.ToolName,
	}

	switch ev.Subtype {
	case "hook_started":
		hook.Phase = "started"
	case "hook_progress":
		hook.Phase = "progress"
	case "hook_response":
		hook.Phase = "completed"
		hook.ExitCode = h.ExitCode

		// Prefer stdout for the hook's JSON response (CC's hook protocol reads
		// the hook's structured decision from stdout). Fall back to `output`.
		responseText := h.Stdout
		if responseText == "" {
			responseText = h.Output
		}
		if responseText != "" && json.Valid([]byte(responseText)) {
			hook.Output = json.RawMessage(responseText)
			var dec struct {
				Decision string `json:"decision"`
			}
			_ = json.Unmarshal([]byte(responseText), &dec)
			hook.Decision = dec.Decision
		}

		if h.Outcome != "" && h.Outcome != "success" {
			hook.Error = h.Outcome
			if h.Stderr != "" {
				hook.Error = h.Outcome + ": " + h.Stderr
			}
		}
	default:
		return nil
	}

	return []msg.Event{makeEvent(sid, msg.EventHook, raw, func(e *msg.Event) {
		e.Hook = hook
	})}
}

func translateAssistant(ev ccStreamEvent, sid string, raw json.RawMessage, agg *UsageAggregator, tracker *identity.Tracker) []msg.Event {
	var am ccAssistantMessage
	if err := json.Unmarshal(ev.Message, &am); err != nil {
		return nil
	}

	agg.AddAPICall(am.Usage)

	// blockMessageID is what we put in BlockEvent.MessageID and
	// ToolCall/ToolResult.MessageID. With the Tracker (Phase III.B), this
	// is the canonical bridge MessageID — pre-stamped here so bridge-server
	// honors it instead of re-running assignAssistantID. Without the
	// Tracker (legacy path), we leave the harness id in place and let
	// bridge-server do the mapping. Both paths produce the same wire shape;
	// only the value of the id field differs.
	blockMessageID := am.ID
	var bridgeMessageID string
	if tracker != nil {
		assigned, err := tracker.AssignMessageID(am.ID)
		if err != nil {
			// Tracker failure is local to this adapter's state.db; log and
			// fall back to the harness id so bridge-server's legacy
			// assignAssistantID can still produce a working bubble.
			log.Printf("[claudecode] tracker assign for am.id=%s: %v", am.ID, err)
		} else {
			blockMessageID = assigned
			bridgeMessageID = assigned
		}
	}

	stamp := func(e *msg.Event) {
		// Pre-stamping Event.MessageID lets bridge-server's
		// assignAssistantID short-circuit (commit e743f86). Bare am.ID
		// in HarnessMessageID preserves the harness-side correlation
		// for diagnostics and for any consumer that still looks at it.
		if bridgeMessageID != "" {
			e.MessageID = bridgeMessageID
		}
		e.HarnessMessageID = am.ID
	}

	var events []msg.Event
	for i, block := range am.Content {
		switch block.Type {
		case "text":
			events = append(events, makeEvent(sid, msg.EventBlock, raw, func(e *msg.Event) {
				stamp(e)
				e.Block = &msg.BlockEvent{
					Index:     i,
					MessageID: blockMessageID,
					Block: &msg.ContentBlock{
						Type: msg.BlockText,
						Text: &msg.TextBlock{Text: block.Text},
					},
				}
			}))

		case "thinking":
			events = append(events, makeEvent(sid, msg.EventBlock, raw, func(e *msg.Event) {
				stamp(e)
				e.Block = &msg.BlockEvent{
					Index:     i,
					MessageID: blockMessageID,
					Block: &msg.ContentBlock{
						Type: msg.BlockThinking,
						Thinking: &msg.ThinkingBlock{
							Text:      block.Thinking,
							Signature: block.Signature,
						},
					},
				}
			}))

		case "tool_use":
			agg.AddToolCall()
			events = append(events, makeEvent(sid, msg.EventToolCall, raw, func(e *msg.Event) {
				stamp(e)
				e.ToolCall = &msg.ToolCallEvent{
					ToolID:    block.ID,
					Name:      block.Name,
					Input:     block.Input,
					MessageID: blockMessageID,
				}
			}))

			// No `tool_result` case: an assistant frame never carries one. The
			// Anthropic wire format only permits tool_result in a user-role
			// message, and Claude Code follows it. A branch here reads as
			// "tool results are handled" while never running once; see
			// translateUserFrame for where they actually arrive.
		}
	}

	return events
}

func translateResult(ev ccStreamEvent, sid string, raw json.RawMessage, agg *UsageAggregator) []msg.Event {
	var events []msg.Event

	usage, cost := agg.Finalize(raw)

	// Extract model from modelUsage keys.
	var resultMeta struct {
		ModelUsage map[string]json.RawMessage `json:"modelUsage"`
	}
	_ = json.Unmarshal(raw, &resultMeta)
	var model string
	for k := range resultMeta.ModelUsage {
		model = k
		break
	}

	if ev.IsError {
		// Extract error details.
		var errResult struct {
			Errors []string `json:"errors"`
		}
		_ = json.Unmarshal(raw, &errResult)
		errMsg := "Claude Code error"
		if len(errResult.Errors) > 0 {
			errMsg = errResult.Errors[0]
		}

		events = append(events, makeEvent(sid, msg.EventError, raw, func(e *msg.Event) {
			e.Error = &msg.ErrorEvent{
				Code:    "EXECUTION_ERROR",
				Message: errMsg,
			}
		}))
	} else {
		events = append(events, makeEvent(sid, msg.EventResult, raw, func(e *msg.Event) {
			e.Result = &msg.ResultEvent{
				Text:          ev.Result,
				IsError:       false,
				Usage:         usage,
				Cost:          cost,
				DurationMS:    ev.DurationMS,
				DurationAPIMS: ev.DurationAPIMS,
				NumTurns:      ev.NumTurns,
				APICalls:      len(agg.APICallUsages()),
				Model:         model,
				APICallUsages: agg.APICallUsages(),
			}
		}))
	}

	// Surface permission denials CC reports post-hoc in its result event as
	// completed permission_prompt hooks. These are denials CC made on its
	// own (e.g. tools blocked by --allowed-tools or --disallowed-tools) —
	// the bridge never had a chance to consult its UI. Emitting them as
	// HookEvent{phase:completed, decision:deny} keeps the canonical surface
	// uniform with awaiting_resolution → completed flows initiated through
	// the embedded MCP.
	for _, d := range ev.PermissionDenials {
		var denial struct {
			Tool    string `json:"tool"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(d, &denial)
		events = append(events, makeEvent(sid, msg.EventHook, raw, func(e *msg.Event) {
			e.Hook = &msg.HookEvent{
				Source:   "permission_prompt",
				Event:    "PreToolUse",
				ToolName: denial.Tool,
				Phase:    "completed",
				Decision: "deny",
				Resolution: &msg.HookResolution{
					Behavior:   "deny",
					Message:    denial.Message,
					ResolvedBy: "auto",
				},
			}
		}))
	}

	agg.Reset()
	return events
}

func translateRateLimit(sid string, raw json.RawMessage) []msg.Event {
	return []msg.Event{makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "rate_limit"}
	})}
}

func translateToolProgress(sid string, raw json.RawMessage) []msg.Event {
	return []msg.Event{makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
		e.System = &msg.SystemEvent{Subtype: "tool_progress"}
	})}
}

// makeEvent builds a canonical msg.Event with common fields set. sessionID is
// the harness-native id (Claude Code's session UUID); BridgeSessionID is
// stamped later by Harness.emit, which has access to the stable bridge id.
func makeEvent(sessionID string, eventType msg.EventType, raw json.RawMessage, fill func(*msg.Event)) msg.Event {
	e := msg.Event{
		Type:             eventType,
		Harness:          harness,
		HarnessSessionID: sessionID,
		Timestamp:        time.Now(),
		Raw:              raw,
	}
	fill(&e)
	return e
}
