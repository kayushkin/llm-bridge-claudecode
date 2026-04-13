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
	Role       string                `json:"role"`
	Content    []ccStoredContentBlock `json:"content"`
	Model      string                `json:"model,omitempty"`
	Usage      ccStoredUsage         `json:"usage,omitempty"`
	StopReason string                `json:"stop_reason,omitempty"`
}

// ccStoredContentBlock is a content block in a stored message.
type ccStoredContentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Content  string          `json:"content,omitempty"`
	IsError  bool            `json:"is_error,omitempty"`
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

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var stored ccStoredEvent
		if err := json.Unmarshal(line, &stored); err != nil {
			continue
		}

		events := translateStoredEvent(stored, sessionID)
		for _, ev := range events {
			enc.Encode(ev)
		}
	}

	return scanner.Err()
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

// translateUserEvent converts a CC user event to a user_message msg.Event.
func translateUserEvent(stored ccStoredEvent, sessionID string, ts time.Time) []msg.Event {
	var message ccStoredMessage
	if err := json.Unmarshal(stored.Message, &message); err != nil {
		return nil
	}

	// Extract text content
	var text string
	for _, block := range message.Content {
		if block.Type == "text" {
			text += block.Text
		}
	}

	if text == "" {
		return nil
	}

	return []msg.Event{{
		Type:      msg.EventType("user_message"),
		Harness:   harness,
		SessionID: sessionID,
		Timestamp: ts,
		Result: &msg.ResultEvent{
			Text: text,
		},
		Raw: stored.Message,
	}}
}

// translateAssistantEvent converts a CC assistant event to a result msg.Event.
func translateAssistantEvent(stored ccStoredEvent, sessionID string, ts time.Time) []msg.Event {
	var message ccStoredMessage
	if err := json.Unmarshal(stored.Message, &message); err != nil {
		return nil
	}

	// Extract text and thinking content
	var text, thinking string
	var tools []msg.ToolSummary

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
		case "tool_result":
			// Find matching tool and add result
			for i := len(tools) - 1; i >= 0; i-- {
				if tools[i].Output == "" {
					tools[i].Output = block.Content
					if block.IsError {
						tools[i].Error = "tool error"
					}
					break
				}
			}
		}
	}

	// Skip empty assistant messages
	if text == "" && thinking == "" && len(tools) == 0 {
		return nil
	}

	ev := msg.Event{
		Type:      msg.EventResult,
		Harness:   harness,
		SessionID: sessionID,
		Timestamp: ts,
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

	return []msg.Event{ev}
}
