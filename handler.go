package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// Request is the JSON-RPC request format from llm-bridge.
type Request struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// StartParams are the parameters for the "start" method.
type StartParams struct {
	SessionID    string   `json:"session_id"`
	DisplayName  string   `json:"display_name"`
	AgentID      string   `json:"agent_id"`
	Prompt       string   `json:"prompt"`
	Resume       bool     `json:"resume"`
	Fork         string   `json:"fork"`
	AutoApprove  *bool    `json:"auto_approve,omitempty"`  // nil = true (default), false = restricted mode
	AllowedTools []string `json:"allowed_tools,omitempty"` // tools to allow when auto_approve is false
}

// MessageParams are the parameters for the "message" method.
type MessageParams struct {
	Content string `json:"content"`
}

// CompactParams are the parameters for the "compact" method.
type CompactParams struct {
	Summary string `json:"summary"`
}

// Harness holds the runtime state for a single harness session.
type Harness struct {
	cfg          *Config
	proc         *CCProcess
	events       <-chan json.RawMessage // single reader for the process lifetime
	sessionID    string
	autoApprove  *bool    // persisted across respawns
	allowedTools []string // persisted across respawns
	agg          UsageAggregator
	ctx          context.Context
	cancel       context.CancelFunc
}

// NewHarness creates a new harness instance.
func NewHarness(cfg *Config) *Harness {
	ctx, cancel := context.WithCancel(context.Background())
	return &Harness{cfg: cfg, ctx: ctx, cancel: cancel}
}

// HandleRequest dispatches a JSON-RPC request to the appropriate handler.
func (h *Harness) HandleRequest(req Request) error {
	switch req.Method {
	case "start":
		var params StartParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fmt.Errorf("parse start params: %w", err)
		}
		return h.handleStart(params)

	case "message":
		var params MessageParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fmt.Errorf("parse message params: %w", err)
		}
		return h.handleMessage(params)

	case "compact":
		var params CompactParams
		if len(req.Params) > 0 {
			_ = json.Unmarshal(req.Params, &params)
		}
		return h.handleCompact(params)

	case "resume":
		return h.handleResume()

	default:
		return fmt.Errorf("unknown method: %s", req.Method)
	}
}

// handleStart spawns a Claude Code process and begins streaming events.
func (h *Harness) handleStart(params StartParams) error {
	h.sessionID = params.SessionID

	// Persist permission config for respawns. Only update if explicitly set.
	if params.AutoApprove != nil {
		h.autoApprove = params.AutoApprove
	}
	if params.AllowedTools != nil {
		h.allowedTools = params.AllowedTools
	}

	var extraArgs []string

	if params.Resume {
		extraArgs = append(extraArgs, "--resume", params.SessionID)
	} else if params.Fork != "" {
		extraArgs = append(extraArgs, "--resume", params.Fork, "--fork-session")
	}

	// Don't pass bridge session IDs to Claude Code — CC requires UUIDs
	// and bridge IDs are timestamp-based. Let CC generate its own session ID.

	if h.cfg.Model != "" {
		extraArgs = append(extraArgs, "--model", h.cfg.Model)
	}

	proc, err := spawnClaudeCode(h.cfg, params.SessionID, h.autoApprove, h.allowedTools, extraArgs...)
	if err != nil {
		emitEvent(msg.Event{
			Type:      msg.EventError,
			Harness:   harness,
			SessionID: h.sessionID,
			Timestamp: time.Now(),
			Error: &msg.ErrorEvent{
				Code:    "SPAWN_ERROR",
				Message: err.Error(),
			},
		})
		return err
	}
	h.proc = proc

	// Start a single event reader for the lifetime of this process.
	h.events = proc.ReadEvents(h.ctx)

	// Send the initial prompt as a user message.
	if params.Prompt != "" {
		if err := proc.WriteMessage(params.Prompt); err != nil {
			log.Printf("failed to write initial prompt: %v", err)
			return err
		}
		// Stream events from CC stdout until the turn completes.
		h.drainUntilResult()
	}
	// If no prompt, just return — CC is ready and waiting for a message.
	return nil
}

// handleMessage sends a follow-up message to the running Claude Code process.
func (h *Harness) handleMessage(params MessageParams) error {
	if h.proc == nil || !h.proc.Alive() {
		// Process died or was never started. Respawn with --resume.
		return h.handleStart(StartParams{
			SessionID: h.sessionID,
			Prompt:    params.Content,
			Resume:    true,
		})
	}

	// Write user message to the existing CC process's stdin.
	if err := h.proc.WriteMessage(params.Content); err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	// Stream events for this turn.
	h.drainUntilResult()
	return nil
}

// handleCompact acknowledges a compact request. CC manages compaction internally.
func (h *Harness) handleCompact(params CompactParams) error {
	emitEvent(msg.Event{
		Type:      msg.EventSystem,
		Harness:   harness,
		SessionID: h.sessionID,
		Timestamp: time.Now(),
		System:    &msg.SystemEvent{Subtype: "compact_ack", Message: "compaction delegated to Claude Code"},
	})
	return nil
}

// handleResume respawns the Claude Code process with --resume.
func (h *Harness) handleResume() error {
	if h.proc != nil && h.proc.Alive() {
		// Already running, nothing to do.
		return nil
	}
	return h.handleStart(StartParams{
		SessionID: h.sessionID,
		Resume:    true,
	})
}

// Interrupt sends an interrupt to the running CC process.
func (h *Harness) Interrupt() {
	if h.proc == nil || !h.proc.Alive() {
		return
	}
	requestID := fmt.Sprintf("int-%d", time.Now().UnixMilli())
	if err := h.proc.WriteInterrupt(requestID); err != nil {
		log.Printf("interrupt write failed: %v, falling back to kill", err)
		h.proc.Kill()
	}
}

// Shutdown cleans up the Claude Code process.
func (h *Harness) Shutdown() {
	h.cancel()
	if h.proc != nil && h.proc.Alive() {
		h.Interrupt()
		// Give CC a moment to exit gracefully.
		select {
		case <-h.proc.Done():
		case <-time.After(3 * time.Second):
			h.proc.Kill()
		}
	}
}

// drainUntilResult reads events from the shared event channel until a result
// or error event is seen, indicating the current turn is complete.
// The channel persists across turns — only one goroutine reads from it.
func (h *Harness) drainUntilResult() {
	for raw := range h.events {
		translated := translateEvent(raw, h.sessionID, &h.agg)
		for _, ev := range translated {
			emitEvent(ev)

			// Update session ID if CC assigned a new one (fork case).
			if ev.Type == msg.EventSystem && ev.System != nil && ev.System.Subtype == "init" {
				var init struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
					h.sessionID = init.SessionID
				}
			}

			// A result or error event means this turn is done.
			if ev.Type == msg.EventResult || ev.Type == msg.EventError {
				return
			}
		}
	}

	// If we get here, the event channel closed — process died.
	if h.proc != nil && !h.proc.Alive() {
		emitEvent(msg.Event{
			Type:      msg.EventError,
			Harness:   harness,
			SessionID: h.sessionID,
			Timestamp: time.Now(),
			Error: &msg.ErrorEvent{
				Code:    "PROCESS_DIED",
				Message: fmt.Sprintf("Claude Code process exited unexpectedly: %v", h.proc.Err()),
			},
		})
		emitEvent(msg.Event{
			Type:      msg.EventSessionState,
			Harness:   harness,
			SessionID: h.sessionID,
			Timestamp: time.Now(),
			State:     &msg.StateEvent{State: msg.SessionError, Previous: msg.SessionRunning, Reason: "process_died"},
		})
	}
}
