package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// runRolloutTailer follows claude's per-session JSONL rollout file and
// emits granular msg.Events for each entry. PTY-mode counterpart to the
// OTel pipeline: OTel gives us metadata (api_call, tool_decision, etc.),
// the rollout gives us the actual conversation content (user text,
// assistant text, thinking, tool args + outputs) that's otherwise
// invisible because claude renders straight to the pty fd.
//
// Lifecycle:
//   - Looks up (or polls for) the session's .jsonl in
//     ~/.claude/projects/<encoded(cwd)>/. resumeID short-circuits to a
//     known filename; otherwise we wait for a new file to appear after
//     the sidecar started.
//   - Tails the file: reads to EOF, sleeps, repeats. When `done` closes
//     the loop exits cleanly.
//   - Translation is granular: one msg.Event per content block so
//     bridge-ui's chat renderer sees text/thinking/tool_call/tool_result
//     rows the same way it does for -p mode sessions.
//
// All emitted events are tagged Extensions["source"]="rollout" so
// consumers can dedupe against the OTel-derived equivalents (which
// only carry metadata).
func runRolloutTailer(emit func(msg.Event), cwd, resumeID string, done <-chan struct{}) {
	if emit == nil {
		log.Printf("[rollout] emit is nil; skipping tailer")
		return
	}

	home, err := os.UserHomeDir()
	if err != nil {
		log.Printf("[rollout] cannot resolve $HOME: %v", err)
		return
	}
	if cwd == "" {
		cwd = "/"
	}
	projectsDir := filepath.Join(home, ".claude", "projects", pathToCCProject(cwd))

	start := time.Now()
	target := locateRolloutFile(projectsDir, resumeID, start, done)
	if target == "" {
		log.Printf("[rollout] no rollout file located in %s within startup window", projectsDir)
		return
	}
	log.Printf("[rollout] tailing %s", target)

	tailRolloutFile(target, emit, done)
}

// locateRolloutFile returns the path of claude's rollout for this
// session. Resume case is trivial — the file name is the harness UUID.
// Fresh case polls the projects directory until a .jsonl file modified
// after `since` appears (claude creates the file as soon as the TUI
// boots). Caller-controlled `done` cancels the wait.
func locateRolloutFile(projectsDir, resumeID string, since time.Time, done <-chan struct{}) string {
	if resumeID != "" {
		return filepath.Join(projectsDir, resumeID+".jsonl")
	}

	const (
		pollEvery = 250 * time.Millisecond
		// 30s startup window — claude takes a few seconds to boot the
		// TUI and write the first entry. Anything longer than that and
		// either claude failed or we're attached to the wrong dir.
		maxWait = 30 * time.Second
	)
	deadline := time.Now().Add(maxWait)

	for time.Now().Before(deadline) {
		if path := newestJSONLAfter(projectsDir, since); path != "" {
			return path
		}
		select {
		case <-done:
			return ""
		case <-time.After(pollEvery):
		}
	}
	return ""
}

// newestJSONLAfter returns the path of the newest .jsonl file in dir
// whose mtime is at or after `since`. Returns "" if no such file
// exists yet. Errors reading the directory return "" — caller polls.
func newestJSONLAfter(dir string, since time.Time) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var best string
	var bestMtime time.Time
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if mt.Before(since) {
			continue
		}
		if mt.After(bestMtime) {
			best = filepath.Join(dir, e.Name())
			bestMtime = mt
		}
	}
	return best
}

// tailRolloutFile opens path, reads lines past EOF until `done` closes.
// Each line is a CC rollout JSONL entry; translate to one or more
// msg.Events and hand to emit.
//
// Partial-line handling: bufio.Reader.ReadBytes returns whatever bytes
// it has plus EOF when it hits end-of-file before the delimiter. We
// accumulate those bytes across iterations and process them as a single
// line once the newline arrives — without this, claude's mid-write
// state would corrupt every entry it tails.
//
// Known limitations (first cut, follow-up work): no compaction/fork
// support — if claude rotates to a new .jsonl mid-session this tailer
// keeps following the old file forever. Resume case (LLMBRIDGE_PTY_RESUME_ID)
// starts from byte 0 and re-emits the whole transcript, which the
// downstream pipeline dedups by event_id but the harness wastes CPU on.
func tailRolloutFile(path string, emit func(msg.Event), done <-chan struct{}) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("[rollout] open %s: %v", path, err)
		return
	}
	defer f.Close()

	br := bufio.NewReader(f)
	var pending []byte

	for {
		select {
		case <-done:
			return
		default:
		}

		chunk, err := br.ReadBytes('\n')
		pending = append(pending, chunk...)

		switch err {
		case nil:
			processRolloutLine(pending, emit)
			pending = pending[:0]
		case io.EOF:
			// Wait for more data. pending holds any partial line so the
			// next chunk completes it.
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
		default:
			log.Printf("[rollout] read %s: %v", path, err)
			return
		}
	}
}

func processRolloutLine(b []byte, emit func(msg.Event)) {
	// Trim trailing newline / CR.
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return
	}
	var stored ccStoredEvent
	if err := json.Unmarshal(b, &stored); err != nil {
		log.Printf("[rollout] parse line: %v (len=%d)", err, len(b))
		return
	}
	for _, ev := range translateRolloutEntry(stored) {
		emit(ev)
	}
}

// translateRolloutEntry converts one CC rollout JSONL entry into one or
// more msg.Events. Distinct from import_history.go's translator: this
// emits per-block events so the chat UI renders text / thinking / tool
// rows separately, matching what stream-json mode produces.
//
// Stamps Extensions["source"]="rollout" on every emitted event so
// consumers can dedupe against OTel-derived equivalents (the deduper
// for "user_prompt vs rollout user_message" is a future concern).
func translateRolloutEntry(stored ccStoredEvent) []msg.Event {
	ts, _ := time.Parse(time.RFC3339Nano, stored.Timestamp)
	if ts.IsZero() {
		ts = time.Now()
	}

	var message ccStoredMessage
	if err := json.Unmarshal(stored.Message, &message); err != nil {
		return nil
	}

	var out []msg.Event

	switch stored.Type {
	case "user":
		for _, b := range message.Content {
			switch b.Type {
			case "text":
				if b.Text == "" {
					continue
				}
				out = append(out, msg.Event{
					Type:             msg.EventUserMessage,
					HarnessSessionID: stored.SessionID,
					Timestamp:        ts,
					Result:           &msg.ResultEvent{Text: b.Text},
				})
			case "tool_result":
				out = append(out, msg.Event{
					Type:             msg.EventToolResult,
					HarnessSessionID: stored.SessionID,
					Timestamp:        ts,
					ToolResult: &msg.ToolResultEvent{
						ToolID:  b.ID,
						Output:  b.Content,
						IsError: b.IsError,
					},
				})
			}
		}
	case "assistant":
		var finalText strings.Builder
		for i, b := range message.Content {
			switch b.Type {
			case "text":
				finalText.WriteString(b.Text)
				if b.Text == "" {
					continue
				}
				out = append(out, msg.Event{
					Type:             msg.EventBlock,
					HarnessSessionID: stored.SessionID,
					HarnessMessageID: stored.UUID,
					Timestamp:        ts,
					Block: &msg.BlockEvent{
						Index:     i,
						MessageID: stored.UUID,
						Block: &msg.ContentBlock{
							Type: msg.BlockText,
							Text: &msg.TextBlock{Text: b.Text},
						},
					},
				})
			case "thinking":
				if b.Thinking == "" {
					continue
				}
				out = append(out, msg.Event{
					Type:             msg.EventThinking,
					HarnessSessionID: stored.SessionID,
					HarnessMessageID: stored.UUID,
					Timestamp:        ts,
					Thinking:         &msg.ThinkingEvent{Text: b.Thinking},
				})
			case "tool_use":
				input := b.Input
				if len(input) == 0 {
					input = json.RawMessage("{}")
				}
				out = append(out, msg.Event{
					Type:             msg.EventToolCall,
					HarnessSessionID: stored.SessionID,
					HarnessMessageID: stored.UUID,
					Timestamp:        ts,
					ToolCall: &msg.ToolCallEvent{
						ToolID:    b.ID,
						Name:      b.Name,
						Input:     input,
						MessageID: stored.UUID,
					},
				})
			}
		}

		// Synthesize the per-turn EventResult terminator. bridge-server's
		// derivation pipeline keys off EventResult for UsageTotal,
		// TurnComplete, and the turn→idle SessionState transition; without
		// it PTY turns never close. An assistant message with
		// stop_reason=tool_use is NOT terminal — the model wants a tool
		// and the turn continues with a tool_result + another assistant
		// message. Only end_turn / stop_sequence / max_tokens end the turn.
		//
		// Usage here is the final API call's only (CC writes per-call
		// usage on each assistant entry, not a turn cumulative), so it
		// under-counts multi-roundtrip turns. That's acceptable: the
		// canonical cost signal for PTY is OTel api_call / api_spend_total;
		// this EventResult exists to drive the state machine, not billing.
		if isTerminalStopReason(message.StopReason) {
			out = append(out, msg.Event{
				Type:             msg.EventResult,
				HarnessSessionID: stored.SessionID,
				HarnessMessageID: stored.UUID,
				Timestamp:        ts,
				Result: &msg.ResultEvent{
					Text:  finalText.String(),
					Model: message.Model,
					Usage: msg.TokenUsage{
						InputTokens:  message.Usage.InputTokens,
						OutputTokens: message.Usage.OutputTokens,
						TotalTokens:  message.Usage.InputTokens + message.Usage.OutputTokens,
					},
				},
			})
		}
	}

	for i := range out {
		if out[i].Extensions == nil {
			out[i].Extensions = make(map[string]json.RawMessage, 1)
		}
		out[i].Extensions["source"] = json.RawMessage(`"rollout"`)
	}

	return out
}

// isTerminalStopReason reports whether an assistant message's stop_reason
// ends the turn. tool_use means the model wants a tool and the turn
// continues (tool_result → another assistant message); the empty string
// is treated as non-terminal (defensive — shouldn't appear on a
// finalized rollout entry).
func isTerminalStopReason(sr string) bool {
	switch sr {
	case "end_turn", "stop_sequence", "max_tokens":
		return true
	default:
		return false
	}
}
