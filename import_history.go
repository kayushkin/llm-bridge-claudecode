package main

import (
	"bufio"
	"encoding/json"
	"os"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// ccStoredEvent is the format of events in CC's on-disk session .jsonl files.
type ccStoredEvent struct {
	Type      string          `json:"type"`
	UUID      string          `json:"uuid"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	Message   json.RawMessage `json:"message"`
}

// ccStoredMessage is the message payload in stored events.
type ccStoredMessage struct {
	Role       string          `json:"role"`
	Content    ccStoredContent `json:"content"`
	Model      string          `json:"model,omitempty"`
	Usage      ccStoredUsage   `json:"usage,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
}

// ccStoredContent is a stored message's content. Claude Code writes it in two
// forms: an array of blocks, and a bare string for a prompt typed into its own
// screen. Decoding only the array made every typed prompt fail to unmarshal,
// and the readers drop a message that fails, so the pty sidecar never showed
// a prompt typed in the terminal and imported histories lost them too (433 of
// 1,472 user entries in 400 transcripts on 2026-10-20). A string becomes one
// text block.
type ccStoredContent []ccStoredContentBlock

func (c *ccStoredContent) UnmarshalJSON(b []byte) error {
	var text string
	if err := json.Unmarshal(b, &text); err == nil {
		*c = ccStoredContent{{Type: "text", Text: text}}
		return nil
	}
	var blocks []ccStoredContentBlock
	if err := json.Unmarshal(b, &blocks); err != nil {
		return err
	}
	*c = blocks
	return nil
}

// ccStoredContentBlock is a content block in a stored message.
//
// ToolUseID, not ID, is what a tool_result block uses to name the call it
// answers; ID belongs to a tool_use block. Content is raw because Claude Code
// writes a tool_result's content either as a bare string or as an array of
// content blocks — typing it as a string fails to unmarshal the array shape and
// takes the whole enclosing message down with it. Use decodeToolResultContent
// to render it.
type ccStoredContentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// ccStoredUsage is the usage info in stored assistant messages.
type ccStoredUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// importHistory reads a CC session .jsonl file and outputs translated msg.Event
// as NDJSON to stdout. These can be piped to log-store.
func importHistory(sessionID, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	enc := json.NewEncoder(os.Stdout)
	translator := newStoredTranscriptTranslator(sessionID)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var stored ccStoredEvent
		if err := json.Unmarshal(line, &stored); err != nil {
			continue
		}

		for _, ev := range translator.translate(stored) {
			if err := enc.Encode(ev); err != nil {
				return err
			}
		}
	}

	return scanner.Err()
}

// structuredOutputToolName is the tool Claude Code gives a run started with
// --json-schema, which is how -oneshot asks for an answer in a schema. The
// model answers by calling it, and Claude Code ends the run as soon as a call
// succeeds: the transcript's last line is that call's tool_result, and no
// assistant message follows it. Of the 5,877 newest such transcripts on one
// host on 2026-11-24, 5,876 ended there.
const structuredOutputToolName = "StructuredOutput"

// storedTranscriptTranslator translates one transcript's lines in order. It
// carries what a single line cannot say: which tool calls were the run's
// structured answer, so that the result of one can be read as the end of the
// turn.
type storedTranscriptTranslator struct {
	sessionID string
	// structuredOutputCallModels maps the id of each StructuredOutput call
	// seen so far to the model that made it.
	structuredOutputCallModels map[string]string
}

func newStoredTranscriptTranslator(sessionID string) *storedTranscriptTranslator {
	return &storedTranscriptTranslator{
		sessionID:                  sessionID,
		structuredOutputCallModels: map[string]string{},
	}
}

// translate converts one stored line, and after a tool_result that answers a
// StructuredOutput call without an error it adds the result event that ends
// the turn.
//
// Without that event the bridge reads the transcript as "the tools have
// answered, the model is writing": a finished run stays model_generating for
// good. The bridge takes a session in that state with no process for one whose
// process died mid-turn and resumes it. On 2026-11-24 it resumed 113 finished
// one-shots that way, each as a full Claude Code process, and the host ran out
// of memory.
func (t *storedTranscriptTranslator) translate(stored ccStoredEvent) []msg.Event {
	events := translateStoredEvent(stored, t.sessionID)
	var message ccStoredMessage
	if err := json.Unmarshal(stored.Message, &message); err != nil {
		return events
	}
	switch stored.Type {
	case "assistant":
		for _, block := range message.Content {
			if block.Type == "tool_use" && block.Name == structuredOutputToolName {
				t.structuredOutputCallModels[block.ID] = message.Model
			}
		}
	case "user":
		for _, block := range message.Content {
			if block.Type != "tool_result" || block.IsError {
				continue
			}
			model, answersStructuredOutput := t.structuredOutputCallModels[block.ToolUseID]
			if !answersStructuredOutput {
				continue
			}
			events = append(events, structuredOutputTurnEnd(events, model, stored.Message))
			break
		}
	}
	return events
}

// structuredOutputTurnEnd is the result event that ends a turn whose answer
// was a StructuredOutput call. It takes its session and time from the events
// of the line that carried the tool_result, and has no text: the answer is the
// call's input, which the tool_call event already carries.
func structuredOutputTurnEnd(lineEvents []msg.Event, model string, raw json.RawMessage) msg.Event {
	last := lineEvents[len(lineEvents)-1]
	return msg.Event{
		Type:             msg.EventResult,
		Harness:          harness,
		HarnessSessionID: last.HarnessSessionID,
		Timestamp:        last.Timestamp,
		Result:           &msg.ResultEvent{Model: model},
		Raw:              raw,
	}
}

// translateStoredEvent converts a CC stored event to msg.Event(s).
func translateStoredEvent(stored ccStoredEvent, sessionID string) []msg.Event {
	ts, _ := time.Parse(time.RFC3339Nano, stored.Timestamp)
	if ts.IsZero() {
		ts = time.Now()
	}

	sid := sessionID
	if stored.SessionID != "" {
		sid = stored.SessionID
	}

	switch stored.Type {
	case "user":
		return translateUserEvent(stored, sid, ts)
	case "assistant":
		return translateAssistantEvent(stored, sid, ts)
	default:
		return nil
	}
}

// translateUserEvent converts a CC user event into canonical events: one
// tool_result per tool_result block, then a user_message for any text.
//
// Claude Code stores every tool result in a user message, because the
// Anthropic wire format allows a tool_result only in a user-role message. A
// message made only of tool results carries no text, so reading text alone
// drops the output of every tool the session ran. Each result names its call
// by tool_use_id, the same pairing translateUserFrame uses on the live path.
func translateUserEvent(stored ccStoredEvent, sessionID string, ts time.Time) []msg.Event {
	var message ccStoredMessage
	if err := json.Unmarshal(stored.Message, &message); err != nil {
		return nil
	}

	var events []msg.Event
	var text string
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "tool_result":
			events = append(events, msg.Event{
				Type:             msg.EventToolResult,
				Harness:          harness,
				HarnessSessionID: sessionID,
				Timestamp:        ts,
				ToolResult: &msg.ToolResultEvent{
					ToolID:  block.ToolUseID,
					Output:  decodeToolResultContent(block.Content),
					Content: decodeToolResultMedia(block.Content),
					IsError: block.IsError,
				},
				Raw: stored.Message,
			})
		}
	}

	if text != "" {
		events = append(events, msg.Event{
			Type:             msg.EventType("user_message"),
			Harness:          harness,
			HarnessSessionID: sessionID,
			Timestamp:        ts,
			Result: &msg.ResultEvent{
				Text: text,
			},
			Raw: stored.Message,
		})
	}
	return events
}

// translateAssistantEvent converts a CC assistant event to a result msg.Event,
// followed by one tool_call event per tool_use block.
func translateAssistantEvent(stored ccStoredEvent, sessionID string, ts time.Time) []msg.Event {
	var message ccStoredMessage
	if err := json.Unmarshal(stored.Message, &message); err != nil {
		return nil
	}

	// Extract text and thinking content
	var text, thinking string
	var tools []msg.ToolSummary
	var toolCalls []msg.Event

	for _, block := range message.Content {
		switch block.Type {
		case "text":
			text += block.Text
		case "thinking":
			thinking += block.Thinking
		case "tool_use":
			inputStr := ""
			if block.Input != nil {
				inputStr = string(block.Input)
			}
			tools = append(tools, msg.ToolSummary{
				Tool:  block.Name,
				Input: inputStr,
			})
			toolCalls = append(toolCalls, msg.Event{
				Type:             msg.EventToolCall,
				Harness:          harness,
				HarnessSessionID: sessionID,
				Timestamp:        ts,
				ToolCall: &msg.ToolCallEvent{
					ToolID: block.ID,
					Name:   block.Name,
					Input:  block.Input,
				},
				Raw: stored.Message,
			})
		}
		// No tool_result case: an assistant message never carries one, so
		// ToolEvents holds calls with no Output. Each call is also emitted as
		// a tool_call event carrying its id, and translateUserEvent emits the
		// matching tool_result event carrying the same id.
	}

	// Skip empty assistant messages
	if text == "" && thinking == "" && len(tools) == 0 {
		return nil
	}

	ev := msg.Event{
		Type:             msg.EventResult,
		Harness:          harness,
		HarnessSessionID: sessionID,
		Timestamp:        ts,
		Result: &msg.ResultEvent{
			Text: text,
			Usage: msg.TokenUsage{
				InputTokens:  message.Usage.InputTokens,
				OutputTokens: message.Usage.OutputTokens,
				TotalTokens:  message.Usage.InputTokens + message.Usage.OutputTokens,
			},
			Model:      message.Model,
			ToolEvents: tools,
		},
		Raw: stored.Message,
	}

	if thinking != "" {
		ev.Thinking = &msg.ThinkingEvent{Text: thinking}
	}

	return append([]msg.Event{ev}, toolCalls...)
}
