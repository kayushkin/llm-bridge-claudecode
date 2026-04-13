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

	default:
		// Forward all other system subtypes (compact_boundary, hook_*, task_*, status, etc.)
		events = append(events, makeEvent(sid, msg.EventSystem, raw, func(e *msg.Event) {
			e.System = &msg.SystemEvent{Subtype: ev.Subtype}
		}))
	}

	return events
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
					ToolID: block.ID,
					Name:   block.Name,
					Input:  block.Input,
				}
			}))

		case "tool_result":
			events = append(events, makeEvent(sid, msg.EventToolResult, raw, func(e *msg.Event) {
				e.ToolResult = &msg.ToolResultEvent{
					ToolID:  block.ID,
					Name:    block.Name,
					Output:  block.Content,
					IsError: block.IsError,
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
