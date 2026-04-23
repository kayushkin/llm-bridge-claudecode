package main

import (
	"encoding/json"
	"time"

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
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// translateEvent converts a raw CC stream-json event into canonical msg.Event(s).
// Returns nil for events that should be consumed internally (keep_alive, control_response).
func translateEvent(raw json.RawMessage, sessionID string, agg *UsageAggregator) []msg.Event {
	var ev ccStreamEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		return nil
	}

	// Use CC's session_id if available, fall back to ours.
	sid := ev.SessionID
	if sid == "" {
		sid = sessionID
	}

	switch ev.Type {
	case "system":
		return translateSystem(ev, sid, raw)
	case "assistant":
		return translateAssistant(ev, sid, raw, agg)
	case "result":
		return translateResult(ev, sid, raw, agg)
	case "rate_limit_event":
		return translateRateLimit(sid, raw)
	case "tool_progress":
		return translateToolProgress(sid, raw)
	case "control_response", "keep_alive":
		return nil // consumed internally
	case "user":
		return nil // echo of user messages (e.g. interrupt synthetic), skip
	default:
		// Forward unknown events as system events for visibility.
		return []msg.Event{makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: ev.Type, Message: string(raw)}
		})}
	}
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

func translateSystem(ev ccStreamEvent, sid string, raw json.RawMessage) []msg.Event {
	var events []msg.Event

	switch ev.Subtype {
	case "init":
		// Emit both a state change and a system event.
		events = append(events, makeEvent(sid, msg.EventSessionState, raw, func(e *msg.Event) {
			e.State = &msg.StateEvent{State: msg.SessionRunning, Previous: msg.SessionIdle}
		}))
		// Extract model from init for the system event message.
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

func translateAssistant(ev ccStreamEvent, sid string, raw json.RawMessage, agg *UsageAggregator) []msg.Event {
	var am ccAssistantMessage
	if err := json.Unmarshal(ev.Message, &am); err != nil {
		return nil
	}

	agg.AddAPICall(am.Usage)

	var events []msg.Event
	for i, block := range am.Content {
		switch block.Type {
		case "text":
			events = append(events, makeEvent(sid, msg.EventStream, raw, func(e *msg.Event) {
				e.Stream = &msg.HarnessStream{
					Delta: &msg.BlockDelta{
						Index: i,
						Type:  msg.DeltaText,
						Text:  block.Text,
					},
					MessageID: am.ID,
				}
			}))

		case "thinking":
			events = append(events, makeEvent(sid, msg.EventThinking, raw, func(e *msg.Event) {
				e.Thinking = &msg.ThinkingEvent{Text: block.Thinking}
			}))
			events = append(events, makeEvent(sid, msg.EventStream, raw, func(e *msg.Event) {
				e.Stream = &msg.HarnessStream{
					Delta: &msg.BlockDelta{
						Index:    i,
						Type:     msg.DeltaThinking,
						Thinking: block.Thinking,
					},
					MessageID: am.ID,
				}
			}))

		case "tool_use":
			agg.AddToolCall()
			events = append(events, makeEvent(sid, msg.EventToolCall, raw, func(e *msg.Event) {
				e.ToolCall = &msg.ToolCallEvent{
					ToolID:    block.ID,
					Name:      block.Name,
					Input:     block.Input,
					MessageID: am.ID,
				}
			}))

		case "tool_result":
			events = append(events, makeEvent(sid, msg.EventToolResult, raw, func(e *msg.Event) {
				e.ToolResult = &msg.ToolResultEvent{
					ToolID:    block.ID,
					Name:      block.Name,
					Output:    block.Content,
					IsError:   block.IsError,
					MessageID: am.ID,
				}
			}))
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
		events = append(events, makeEvent(sid, msg.EventSessionState, raw, func(e *msg.Event) {
			e.State = &msg.StateEvent{State: msg.SessionError, Previous: msg.SessionRunning, Reason: ev.Subtype}
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
		events = append(events, makeEvent(sid, msg.EventSessionState, raw, func(e *msg.Event) {
			e.State = &msg.StateEvent{State: msg.SessionIdle, Previous: msg.SessionRunning}
		}))
	}

	// Surface permission denials as approval events.
	for _, d := range ev.PermissionDenials {
		var denial struct {
			Tool    string `json:"tool"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(d, &denial)
		events = append(events, makeEvent(sid, msg.EventApproval, raw, func(e *msg.Event) {
			e.Approval = &msg.ApprovalEvent{
				Action:   "tool_use",
				Status:   "denied",
				ToolName: denial.Tool,
				Detail:   denial.Message,
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

// makeEvent builds a canonical msg.Event with common fields set.
func makeEvent(sessionID string, eventType msg.EventType, raw json.RawMessage, fill func(*msg.Event)) msg.Event {
	e := msg.Event{
		Type:      eventType,
		Harness:   harness,
		SessionID: sessionID,
		Timestamp: time.Now(),
		Raw:       raw,
	}
	fill(&e)
	return e
}
