package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
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
	WorkDir      string   `json:"work_dir,omitempty"`      // working directory for resumed sessions

	// Claude Code CLI flags (start-time)
	SystemPrompt       string   `json:"system_prompt,omitempty"`        // --system-prompt: replace default system prompt
	AppendSystemPrompt string   `json:"append_system_prompt,omitempty"` // --append-system-prompt: append to default system prompt
	AddDirs            []string `json:"add_dirs,omitempty"`             // --add-dir: additional directories for tool access
	MCPConfig          string   `json:"mcp_config,omitempty"`           // --mcp-config: MCP server config JSON file path
	JSONSchema         string   `json:"json_schema,omitempty"`          // --json-schema: structured output validation schema
	FallbackModel      string   `json:"fallback_model,omitempty"`       // --fallback-model: auto-fallback model on overload
	PermissionMode     string   `json:"permission_mode,omitempty"`      // --permission-mode: acceptEdits/auto/bypassPermissions/default/dontAsk/plan
	Worktree           string   `json:"worktree,omitempty"`             // --worktree: git worktree isolation (name or "true" for auto)
	Betas              []string `json:"betas,omitempty"`                // --betas: beta API feature opt-in flags

	// Additional Claude Code CLI flags
	Effort                 string          `json:"effort,omitempty"`                   // --effort: reasoning effort (low/medium/high/xhigh/max)
	MaxBudgetUSD           float64         `json:"max_budget_usd,omitempty"`           // --max-budget-usd: per-session cost cap
	DisallowedTools        []string        `json:"disallowed_tools,omitempty"`         // --disallowed-tools: tool deny-list
	Tools                  []string        `json:"tools,omitempty"`                    // --tools: exact built-in tool set ("" disables all, "default" enables all)
	DisableSlashCommands   bool            `json:"disable_slash_commands,omitempty"`   // --disable-slash-commands
	NoSessionPersistence   bool            `json:"no_session_persistence,omitempty"`   // --no-session-persistence: ephemeral session
	IncludePartialMessages bool            `json:"include_partial_messages,omitempty"` // --include-partial-messages: finer streaming
	IncludeHookEvents      bool            `json:"include_hook_events,omitempty"`      // --include-hook-events: emit hook lifecycle events
	ReplayUserMessages     bool            `json:"replay_user_messages,omitempty"`     // --replay-user-messages: echo user msgs on stdout
	Settings               string          `json:"settings,omitempty"`                 // --settings: path or inline JSON
	SettingSources         []string        `json:"setting_sources,omitempty"`          // --setting-sources: comma-joined (user,project,local)
	StrictMCPConfig        bool            `json:"strict_mcp_config,omitempty"`        // --strict-mcp-config: only use --mcp-config
	PluginDirs             []string        `json:"plugin_dirs,omitempty"`              // --plugin-dir: repeatable plugin directories
	Bare                   bool            `json:"bare,omitempty"`                     // --bare: minimal mode (skip hooks/LSP/memory/etc)
	Agent                  string          `json:"agent,omitempty"`                    // --agent: select a configured CC agent
	Agents                 json.RawMessage `json:"agents,omitempty"`                   // --agents: inline JSON agent definitions
	Brief                  bool            `json:"brief,omitempty"`                    // --brief: enable SendUserMessage tool
	Files                  []string        `json:"files,omitempty"`                    // --file: file_id:relative_path entries
	Continue               bool            `json:"continue,omitempty"`                 // --continue: resume most recent conversation in cwd
	FromPR                 string          `json:"from_pr,omitempty"`                  // --from-pr: resume session linked to PR
	SessionIDOverride      string          `json:"session_id_override,omitempty"`      // --session-id: caller-supplied UUID (must be valid UUID)
	Debug                  string          `json:"debug,omitempty"`                    // --debug: optional filter (e.g. "api,hooks")
	DebugFile              string          `json:"debug_file,omitempty"`               // --debug-file: write debug logs to path
}

// MessageParams are the parameters for the "message" method.
type MessageParams struct {
	Content string `json:"content"`
}

// CompactParams are the parameters for the "compact" method.
type CompactParams struct {
	Summary string `json:"summary"`
}

// SetModelParams are the parameters for the "set_model" method.
type SetModelParams struct {
	Model string `json:"model"`
}

// SetPermissionModeParams are the parameters for the "set_permission_mode" method.
type SetPermissionModeParams struct {
	Mode string `json:"mode"`
}

// ControlParams is a generic control_request pass-through.
// Subtype identifies the command; Payload carries any additional fields.
type ControlParams struct {
	Subtype string         `json:"subtype"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Harness holds the runtime state for a single harness session.
type Harness struct {
	cfg          *Config
	proc         *CCProcess
	events       <-chan json.RawMessage // single reader for the process lifetime
	sessionID    string
	workDir      string   // persisted across respawns (for resumed sessions)
	autoApprove  *bool    // persisted across respawns
	allowedTools []string // persisted across respawns

	// Start-time prompt flags persisted across respawns so they can be
	// surfaced in SessionInfo after every init (CC never echoes them back).
	systemPrompt       string
	appendSystemPrompt string

	agg    UsageAggregator
	ctx    context.Context
	cancel context.CancelFunc
}

// NewHarness creates a new harness instance.
func NewHarness(cfg *Config) *Harness {
	ctx, cancel := context.WithCancel(context.Background())
	return &Harness{cfg: cfg, ctx: ctx, cancel: cancel}
}

// HandleRequest dispatches a JSON-RPC request to the appropriate handler.
func (h *Harness) HandleRequest(req Request) error {
	// The bridge-server routes mid-session config updates as a single method
	// string of the form "config:<json>" (see llm-bridge-server
	// internal/server/sessions.go handleConfigSession). Split that here before
	// the normal switch so the JSON payload can drive the actual dispatch.
	if strings.HasPrefix(req.Method, "config:") {
		raw := strings.TrimPrefix(req.Method, "config:")
		if raw == "" {
			return fmt.Errorf("empty config payload")
		}
		return h.handleConfig(json.RawMessage(raw))
	}

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

	case "set_model":
		var params SetModelParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fmt.Errorf("parse set_model params: %w", err)
		}
		return h.handleSetModel(params)

	case "set_permission_mode":
		var params SetPermissionModeParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fmt.Errorf("parse set_permission_mode params: %w", err)
		}
		return h.handleSetPermissionMode(params)

	case "control":
		var params ControlParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return fmt.Errorf("parse control params: %w", err)
		}
		return h.handleControl(params)

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
	if params.WorkDir != "" {
		h.workDir = params.WorkDir
	}
	if params.SystemPrompt != "" {
		h.systemPrompt = params.SystemPrompt
	}
	if params.AppendSystemPrompt != "" {
		h.appendSystemPrompt = params.AppendSystemPrompt
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

	if params.SystemPrompt != "" {
		extraArgs = append(extraArgs, "--system-prompt", params.SystemPrompt)
	}
	if params.AppendSystemPrompt != "" {
		extraArgs = append(extraArgs, "--append-system-prompt", params.AppendSystemPrompt)
	}
	for _, dir := range params.AddDirs {
		extraArgs = append(extraArgs, "--add-dir", dir)
	}
	if params.MCPConfig != "" {
		extraArgs = append(extraArgs, "--mcp-config", params.MCPConfig)
	}
	if params.JSONSchema != "" {
		extraArgs = append(extraArgs, "--json-schema", params.JSONSchema)
	}
	if params.FallbackModel != "" {
		extraArgs = append(extraArgs, "--fallback-model", params.FallbackModel)
	}
	if params.PermissionMode != "" {
		extraArgs = append(extraArgs, "--permission-mode", params.PermissionMode)
	}
	if params.Worktree != "" {
		if params.Worktree == "true" {
			extraArgs = append(extraArgs, "--worktree")
		} else {
			extraArgs = append(extraArgs, "--worktree", params.Worktree)
		}
	}
	if len(params.Betas) > 0 {
		extraArgs = append(extraArgs, "--betas")
		extraArgs = append(extraArgs, params.Betas...)
	}

	// DisplayName is forwarded as --name. Skip path-like values: the
	// bridge-server treats a DisplayName starting with "/" as a WorkDir
	// sentinel (see llm-bridge-server internal/harness/process.go buildStartParams).
	if params.DisplayName != "" && !strings.HasPrefix(params.DisplayName, "/") {
		extraArgs = append(extraArgs, "--name", params.DisplayName)
	}
	if params.Effort != "" {
		extraArgs = append(extraArgs, "--effort", params.Effort)
	}
	if params.MaxBudgetUSD > 0 {
		extraArgs = append(extraArgs, "--max-budget-usd", strconv.FormatFloat(params.MaxBudgetUSD, 'f', -1, 64))
	}
	if len(params.DisallowedTools) > 0 {
		extraArgs = append(extraArgs, "--disallowed-tools")
		extraArgs = append(extraArgs, params.DisallowedTools...)
	}
	if len(params.Tools) > 0 {
		extraArgs = append(extraArgs, "--tools")
		extraArgs = append(extraArgs, params.Tools...)
	}
	if params.DisableSlashCommands {
		extraArgs = append(extraArgs, "--disable-slash-commands")
	}
	if params.NoSessionPersistence {
		extraArgs = append(extraArgs, "--no-session-persistence")
	}
	if params.IncludePartialMessages {
		extraArgs = append(extraArgs, "--include-partial-messages")
	}
	if params.IncludeHookEvents {
		extraArgs = append(extraArgs, "--include-hook-events")
	}
	if params.ReplayUserMessages {
		extraArgs = append(extraArgs, "--replay-user-messages")
	}
	if params.Settings != "" {
		extraArgs = append(extraArgs, "--settings", params.Settings)
	}
	if len(params.SettingSources) > 0 {
		extraArgs = append(extraArgs, "--setting-sources", strings.Join(params.SettingSources, ","))
	}
	if params.StrictMCPConfig {
		extraArgs = append(extraArgs, "--strict-mcp-config")
	}
	for _, dir := range params.PluginDirs {
		extraArgs = append(extraArgs, "--plugin-dir", dir)
	}
	if params.Bare {
		extraArgs = append(extraArgs, "--bare")
	}
	if params.Agent != "" {
		extraArgs = append(extraArgs, "--agent", params.Agent)
	}
	if len(params.Agents) > 0 {
		extraArgs = append(extraArgs, "--agents", string(params.Agents))
	}
	if params.Brief {
		extraArgs = append(extraArgs, "--brief")
	}
	for _, f := range params.Files {
		extraArgs = append(extraArgs, "--file", f)
	}
	if params.Continue {
		extraArgs = append(extraArgs, "--continue")
	}
	if params.FromPR != "" {
		extraArgs = append(extraArgs, "--from-pr", params.FromPR)
	}
	if params.SessionIDOverride != "" {
		extraArgs = append(extraArgs, "--session-id", params.SessionIDOverride)
	}
	if params.Debug != "" {
		extraArgs = append(extraArgs, "--debug", params.Debug)
	}
	if params.DebugFile != "" {
		extraArgs = append(extraArgs, "--debug-file", params.DebugFile)
	}

	// Use params.WorkDir if provided (for resumed sessions), otherwise fall back to config.
	cfg := h.cfg
	if params.WorkDir != "" {
		cfgCopy := *h.cfg
		cfgCopy.WorkDir = params.WorkDir
		cfg = &cfgCopy
	}

	proc, err := spawnClaudeCode(cfg, params.SessionID, h.autoApprove, h.allowedTools, extraArgs...)
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
			WorkDir:   h.workDir,
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
		WorkDir:   h.workDir,
	})
}

// handleSetModel forwards a set_model control_request to Claude Code.
func (h *Harness) handleSetModel(params SetModelParams) error {
	if params.Model == "" {
		return fmt.Errorf("set_model: model is required")
	}
	return h.handleControl(ControlParams{
		Subtype: "set_model",
		Payload: map[string]any{"model": params.Model},
	})
}

// handleSetPermissionMode forwards a set_permission_mode control_request to Claude Code.
func (h *Harness) handleSetPermissionMode(params SetPermissionModeParams) error {
	if params.Mode == "" {
		return fmt.Errorf("set_permission_mode: mode is required")
	}
	return h.handleControl(ControlParams{
		Subtype: "set_permission_mode",
		Payload: map[string]any{"mode": params.Mode},
	})
}

// handleControl sends a generic control_request to Claude Code's stdin. The
// subtype identifies the command; the payload is merged into the request body.
func (h *Harness) handleControl(params ControlParams) error {
	if h.proc == nil || !h.proc.Alive() {
		return fmt.Errorf("no live Claude Code process to control")
	}
	if params.Subtype == "" {
		return fmt.Errorf("control: subtype is required")
	}
	requestID := fmt.Sprintf("ctl-%d", time.Now().UnixMilli())
	return h.proc.WriteControl(requestID, params.Subtype, params.Payload)
}

// handleConfig is the entry point for "config:<json>" method routing from the
// bridge-server. It inspects the JSON payload's "subtype" field and dispatches
// to the typed handler. Any extra fields are passed as the control payload.
func (h *Harness) handleConfig(raw json.RawMessage) error {
	var probe struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("parse config payload: %w", err)
	}
	switch probe.Subtype {
	case "":
		return fmt.Errorf("config: subtype is required")
	case "set_model":
		var p SetModelParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("parse set_model: %w", err)
		}
		return h.handleSetModel(p)
	case "set_permission_mode":
		var p SetPermissionModeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("parse set_permission_mode: %w", err)
		}
		return h.handleSetPermissionMode(p)
	case "interrupt":
		h.Interrupt()
		return nil
	default:
		// Unknown subtype: forward as generic control_request so new CC
		// subtypes work without a code change here.
		var payload map[string]any
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("parse control payload: %w", err)
		}
		delete(payload, "subtype")
		return h.handleControl(ControlParams{Subtype: probe.Subtype, Payload: payload})
	}
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

			// Update session ID if CC assigned a new one (fork case), and emit
			// a SessionInfo event carrying the start-time flags + CC init payload.
			if ev.Type == msg.EventSystem && ev.System != nil && ev.System.Subtype == "init" {
				var init struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
					h.sessionID = init.SessionID
				}
				if info := parseInitInfo(raw); info != nil {
					info.SystemPrompt = h.systemPrompt
					info.AppendSystemPrompt = h.appendSystemPrompt
					if info.WorkingDir == "" {
						info.WorkingDir = h.workDir
					}
					emitEvent(msg.Event{
						Type:      msg.EventSessionInfo,
						Harness:   harness,
						SessionID: h.sessionID,
						Timestamp: time.Now(),
						Info:      info,
					})
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
