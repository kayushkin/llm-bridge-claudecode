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
	// BridgeSessionID is the routing/storage id from bridge-server. Stable
	// across resumes and forks. New code reads this; legacy SessionID is the
	// fallback for older bridge-server binaries that haven't been rebuilt.
	BridgeSessionID string `json:"bridge_session_id,omitempty"`

	// HarnessSessionID is the harness-internal id (Claude Code session UUID).
	// New code passes this on Resume/Fork; --resume reads from here.
	HarnessSessionID string `json:"harness_session_id,omitempty"`

	// SessionID historically meant either the bridge_id (fresh start) or the
	// harness_id (resume). Kept for backward compatibility — when Resume is
	// true it carries the Claude Code session UUID to --resume against.
	SessionID    string   `json:"session_id,omitempty"`
	DisplayName  string   `json:"display_name"`
	AgentID      string   `json:"agent_id"`
	Prompt       string   `json:"prompt"`
	// Blocks is the multimodal alternative to Prompt: a canonical content-block
	// array (text, image, document, audio, video) for the initial user message.
	// Mutually exclusive with Prompt — setting both returns an error.
	Blocks       []msg.ContentBlock `json:"blocks,omitempty"`
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
	// Blocks is the multimodal alternative to Content: a canonical content-block
	// array (text, image, document, audio, video). Mutually exclusive with
	// Content — setting both returns an error.
	Blocks []msg.ContentBlock `json:"blocks,omitempty"`
}

// CompactParams are the parameters for the "compact" method.
type CompactParams struct {
	Summary string `json:"summary"`
}

// SetModelParams are the parameters for the "set_model" method.
type SetModelParams struct {
	Model string `json:"model"`
}

// ControlParams is a generic control_request pass-through.
// Subtype identifies the command; Payload carries any additional fields.
type ControlParams struct {
	Subtype string         `json:"subtype"`
	Payload map[string]any `json:"payload,omitempty"`
}

// Harness holds the runtime state for a single harness session.
type Harness struct {
	cfg             *Config
	proc            *CCProcess
	events          <-chan json.RawMessage // single reader for the process lifetime
	bridgeSessionID string                 // bridge-server's stable session id; stamped on every emitted event
	sessionID       string                 // harness session id (Claude Code session UUID after init)
	workDir         string                 // persisted across respawns (for resumed sessions)
	autoApprove     *bool                  // persisted across respawns
	allowedTools    []string               // persisted across respawns

	// Start-time prompt flags persisted across respawns so they can be
	// surfaced in SessionInfo after every init (CC never echoes them back).
	systemPrompt       string
	appendSystemPrompt string

	// state is the per-bridge persistent chain (sessions/rollouts/wal).
	// Opened once at boot via openStateAndRecover.
	state *State

	// pendingWALID/Intent/Parent track an in-flight chain rotation. handleStart
	// (cold-start) opens a WAL row before spawning CC; drainUntilResult commits
	// it once CC's system:init event delivers the new session UUID. Cleared on
	// commit or on orphan (process died before init).
	pendingWALID  int64
	pendingIntent string // 'start' | 'fork' (children 2 + 4)
	pendingParent string // parent harness id when intent='fork'

	agg    UsageAggregator
	ctx    context.Context
	cancel context.CancelFunc

}

// NewHarness creates a new harness instance.
func NewHarness(cfg *Config) *Harness {
	ctx, cancel := context.WithCancel(context.Background())
	return &Harness{cfg: cfg, ctx: ctx, cancel: cancel}
}

// openStateAndRecover opens state.db at the canonical path and orphans any
// WAL rows left pending by a prior crash. Called once at boot before the
// JSON-RPC read loop. Failure here is fatal — without state.db the chain
// contract silently regresses to in-memory-only IDs.
func (h *Harness) openStateAndRecover() error {
	st, err := OpenState(DefaultStatePath())
	if err != nil {
		return fmt.Errorf("open state.db: %w", err)
	}
	h.state = st
	if err := recoverOrphansOnBoot(h.state); err != nil {
		log.Printf("[harness] WAL recovery: %v", err)
	}
	return nil
}

// nextSequence returns the sequence number to use for the next rollout in
// the chain for this bridge_session_id. Returns 0 when state.db is unset
// or the chain is empty (i.e. the cold-start case).
func (h *Harness) nextSequence() int {
	if h.state == nil || h.bridgeSessionID == "" {
		return 0
	}
	rs, err := h.state.ListRollouts(h.bridgeSessionID)
	if err != nil || len(rs) == 0 {
		return 0
	}
	return rs[len(rs)-1].Sequence + 1
}

// recordChainOnInit applies persistent chain state once CC's system:init
// event has delivered the new session UUID. Behavior depends on the staged
// pendingIntent:
//
//   - "start" / "fork": commit the pending WAL row, insert a rollout row,
//     upsert the session's current_harness_id.
//   - "resume": CC's --resume normally keeps the same UUID — touch
//     sessions.updated_at so the resume timestamp is fresh. If newHarnessID
//     differs from pendingParent (defensive: CC unexpectedly rotated the
//     UUID), insert a kind='resume' rollout row + UpsertSession with the
//     new id.
//
// Any pending state is cleared before returning. The caller supplies
// rolloutPath so tests can pass an empty string without touching the
// filesystem.
func (h *Harness) recordChainOnInit(newHarnessID, rolloutPath string) {
	if h.state == nil || h.bridgeSessionID == "" {
		return
	}
	intent := h.pendingIntent
	parent := h.pendingParent
	walID := h.pendingWALID
	h.pendingWALID = 0
	h.pendingIntent = ""
	h.pendingParent = ""

	switch intent {
	case "start", "fork":
		if walID == 0 {
			return
		}
		seq := h.nextSequence()
		if cErr := h.state.CommitWAL(walID, newHarnessID, rolloutPath); cErr != nil {
			log.Printf("[harness] commit WAL: %v", cErr)
			return
		}
		if iErr := h.state.InsertRollout(RolloutRow{
			HarnessSessionID: newHarnessID,
			BridgeSessionID:  h.bridgeSessionID,
			RolloutPath:      rolloutPath,
			Sequence:         seq,
			ParentHarnessID:  parent,
			Kind:             intent,
		}); iErr != nil {
			log.Printf("[harness] insert rollout: %v", iErr)
		}
		if uErr := h.state.UpsertSession(h.bridgeSessionID, newHarnessID); uErr != nil {
			log.Printf("[harness] upsert session: %v", uErr)
		}

	case "resume":
		// Same UUID is the expected case under current CC semantics. Bump
		// sessions.updated_at without inserting a rollout row.
		if newHarnessID == "" || parent == "" || newHarnessID == parent {
			id := newHarnessID
			if id == "" {
				id = parent
			}
			if uErr := h.state.UpsertSession(h.bridgeSessionID, id); uErr != nil {
				log.Printf("[harness] touch session on resume: %v", uErr)
			}
			return
		}
		// CC rotated the UUID on resume — treat as fork-in-disguise.
		seq := h.nextSequence()
		if iErr := h.state.InsertRollout(RolloutRow{
			HarnessSessionID: newHarnessID,
			BridgeSessionID:  h.bridgeSessionID,
			RolloutPath:      rolloutPath,
			Sequence:         seq,
			ParentHarnessID:  parent,
			Kind:             "resume",
		}); iErr != nil {
			log.Printf("[harness] insert rollout on resume rotation: %v", iErr)
		}
		if uErr := h.state.UpsertSession(h.bridgeSessionID, newHarnessID); uErr != nil {
			log.Printf("[harness] upsert session on resume rotation: %v", uErr)
		}
	}
}

// recoverOrphansOnBoot marks any pending WAL rows from a prior crash as
// orphaned so they don't shadow future operations. Idempotent: a second
// call after recovery is a no-op.
func recoverOrphansOnBoot(s *State) error {
	pending, err := s.ListPendingWAL()
	if err != nil {
		return fmt.Errorf("list pending: %w", err)
	}
	for _, w := range pending {
		if err := s.OrphanWAL(w.ID); err != nil {
			log.Printf("[harness] WAL recovery: orphan id=%d: %v", w.ID, err)
			continue
		}
		log.Printf("[harness] WAL recovery: orphaned id=%d intent=%s parent=%s", w.ID, w.Intent, w.ParentHarnessID)
	}
	return nil
}

// emit stamps the canonical bridge_session_id and harness_session_id onto the
// event before sending it to bridge-server. Bridge-server's manager re-stamps
// the legacy BridgeID mirror for downstream NATS subscribers — bridges
// themselves only emit the new fields.
func (h *Harness) emit(e msg.Event) {
	if e.BridgeSessionID == "" {
		e.BridgeSessionID = h.bridgeSessionID
	}
	if e.HarnessSessionID == "" {
		e.HarnessSessionID = h.sessionID
	}
	emitEvent(e)
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
	// Validate up-front, before any state mutation or subprocess spawn.
	if params.Prompt != "" && len(params.Blocks) > 0 {
		return fmt.Errorf("start: Prompt and Blocks are mutually exclusive")
	}

	// Prefer the explicit HarnessSessionID; fall back to legacy SessionID
	// when older callers (or our own internal respawns) only set that field.
	if params.HarnessSessionID != "" {
		h.sessionID = params.HarnessSessionID
	} else {
		h.sessionID = params.SessionID
	}

	// Adopt bridge-server's stable id from the new field if present. For older
	// bridge-server binaries that don't send BridgeSessionID, fall back: on a
	// fresh start params.SessionID is the bridge_id; on resume we have no way
	// to recover bridge_id locally — leave it empty and let bridge-server's
	// readEvents() backfill it onto outgoing events.
	if params.BridgeSessionID != "" {
		h.bridgeSessionID = params.BridgeSessionID
	} else if !params.Resume && h.bridgeSessionID == "" {
		h.bridgeSessionID = params.SessionID
	}

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

	resumeID := params.HarnessSessionID
	if resumeID == "" {
		resumeID = params.SessionID
	}
	if params.Resume {
		extraArgs = append(extraArgs, "--resume", resumeID)
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

	// Permission gating runs as a PreToolUse HTTP hook injected by
	// bridge-server via --settings (see internal/server/hook_settings.go,
	// /permission/cc-prehook/<bridge_id>). CC's own permission system
	// stays off — we hardcode --permission-mode bypassPermissions so it
	// doesn't try to consult a --permission-prompt-tool we no longer
	// wire. Hooks fire regardless of mode, so the gate still runs on
	// every tool call. params.PermissionMode (if set explicitly
	// upstream) wins.
	if params.PermissionMode == "" {
		extraArgs = append(extraArgs, "--permission-mode", "bypassPermissions")
	}

	// Use params.WorkDir if provided (for resumed sessions), otherwise fall back to config.
	cfg := h.cfg
	if params.WorkDir != "" {
		cfgCopy := *h.cfg
		cfgCopy.WorkDir = params.WorkDir
		cfg = &cfgCopy
	}

	// Cold-start chain rotation: open a WAL row before spawning CC. The init
	// event delivered later in drainUntilResult commits this WAL with the new
	// session UUID and writes the kind='start' rollout.
	isColdStart := !params.Resume && params.Fork == ""
	if isColdStart && h.state != nil && h.bridgeSessionID != "" {
		if err := h.state.UpsertSession(h.bridgeSessionID, ""); err != nil {
			return fmt.Errorf("upsert session: %w", err)
		}
		walID, err := h.state.InsertWAL(WALRow{
			BridgeSessionID: h.bridgeSessionID,
			Intent:          "start",
		})
		if err != nil {
			return fmt.Errorf("insert WAL: %w", err)
		}
		h.pendingWALID = walID
		h.pendingIntent = "start"
		h.pendingParent = ""
	}

	// Fork chain rotation: same WAL machinery as cold-start, but intent='fork'
	// and parent_harness_id stamped with the parent UUID. CC's --fork-session
	// always mints a new UUID (verified via the init.SessionID overwrite at
	// drainUntilResult below + CC's per-UUID `~/.claude/projects/<dir>/<uuid>.jsonl`
	// file layout), so this is a real chain rotation: the post-init commit
	// writes a kind='fork' rollout pointing at the parent UUID.
	//
	// params.Fork must be the parent's harness UUID, not the parent's
	// bridge_session_id. Bridge-server's ForkSession handler is responsible
	// for resolving that — see llm-bridge-server sessions.go handleForkSession
	// where ParentID is now set to parent.HarnessSessionID.
	if params.Fork != "" && h.state != nil && h.bridgeSessionID != "" {
		walID, err := h.state.InsertWAL(WALRow{
			BridgeSessionID: h.bridgeSessionID,
			Intent:          "fork",
			ParentHarnessID: params.Fork,
		})
		if err != nil {
			return fmt.Errorf("insert WAL: %w", err)
		}
		h.pendingWALID = walID
		h.pendingIntent = "fork"
		h.pendingParent = params.Fork
	}

	// Resume chain rotation: CC's --resume keeps the same session UUID, so
	// there is no chain rotation in the normal case — just bump
	// sessions.updated_at when init arrives. We still stage pendingIntent +
	// pendingParent so drainUntilResult can detect an unexpected UUID change
	// (treated as fork-in-disguise: insert a kind='resume' rollout row). No
	// WAL row is opened; resume is a no-op for the WAL.
	if params.Resume && h.state != nil && h.bridgeSessionID != "" {
		h.pendingWALID = 0
		h.pendingIntent = "resume"
		h.pendingParent = h.sessionID
	}

	proc, err := spawnClaudeCode(cfg, params.SessionID, h.allowedTools, extraArgs...)
	if err != nil {
		// Spawn failed before CC could mint a session UUID — the chain
		// rotation never happened. Orphan any pending WAL row and clear
		// staged intent so the next call doesn't read stale state.
		if h.state != nil && h.pendingWALID != 0 {
			if oErr := h.state.OrphanWAL(h.pendingWALID); oErr != nil {
				log.Printf("[harness] orphan WAL after spawn failure: %v", oErr)
			}
		}
		h.pendingWALID = 0
		h.pendingIntent = ""
		h.pendingParent = ""
		h.emit(msg.Event{
			Type:      msg.EventError,
			Harness:   harness,
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

	// Announce that the session is running. The CC subprocess also emits a
	// system:init event that translates to SessionRunning, but that only
	// fires after CC starts processing — so without an explicit emission
	// here, a no-prompt start would leave clients waiting indefinitely.
	h.emit(msg.Event{
		Type:      msg.EventSessionState,
		Harness:   harness,
		Timestamp: time.Now(),
		State:     &msg.StateEvent{State: msg.SessionRunning, Previous: msg.SessionIdle},
	})

	// Send the initial user message — either as plain text (Prompt) or as a
	// canonical content-block array (Blocks). Mutual-exclusion was checked
	// at function entry.
	switch {
	case len(params.Blocks) > 0:
		if err := proc.WriteMessageBlocks(params.Blocks); err != nil {
			log.Printf("failed to write initial blocks: %v", err)
			return err
		}
		h.drainUntilResult()
	case params.Prompt != "":
		if err := proc.WriteMessage(params.Prompt); err != nil {
			log.Printf("failed to write initial prompt: %v", err)
			return err
		}
		h.drainUntilResult()
	}
	// If neither, just return — CC is ready and waiting for a message.
	return nil
}

// handleMessage sends a follow-up message to the running Claude Code process.
func (h *Harness) handleMessage(params MessageParams) error {
	if params.Content != "" && len(params.Blocks) > 0 {
		return fmt.Errorf("message: Content and Blocks are mutually exclusive")
	}

	if h.proc == nil || !h.proc.Alive() {
		// Process died or was never started. Respawn with --resume, forwarding
		// whichever of Content/Blocks the caller provided.
		return h.handleStart(StartParams{
			HarnessSessionID: h.sessionID,
			Prompt:           params.Content,
			Blocks:           params.Blocks,
			Resume:           true,
			WorkDir:          h.workDir,
		})
	}

	// Write user message to the existing CC process's stdin.
	var err error
	if len(params.Blocks) > 0 {
		err = h.proc.WriteMessageBlocks(params.Blocks)
	} else {
		err = h.proc.WriteMessage(params.Content)
	}
	if err != nil {
		return fmt.Errorf("write message: %w", err)
	}

	// Stream events for this turn.
	h.drainUntilResult()
	return nil
}

// handleCompact acknowledges a compact request. CC manages compaction internally.
func (h *Harness) handleCompact(params CompactParams) error {
	h.emit(msg.Event{
		Type:      msg.EventSystem,
		Harness:   harness,
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
	// Defensive: if h.sessionID was lost (e.g. bridge restart between
	// start and resume), recover it from state.db. The persisted
	// current_harness_id is the latest known UUID for this bridge_session_id.
	resumeID := h.sessionID
	if resumeID == "" && h.state != nil && h.bridgeSessionID != "" {
		if row, err := h.state.GetSession(h.bridgeSessionID); err == nil && row.CurrentHarnessID != "" {
			resumeID = row.CurrentHarnessID
			h.sessionID = resumeID
		}
	}
	return h.handleStart(StartParams{
		HarnessSessionID: resumeID,
		Resume:           true,
		WorkDir:          h.workDir,
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
	if h.state != nil {
		_ = h.state.Close()
	}
}

// drainUntilResult reads events from the shared event channel until a result
// or error event is seen, indicating the current turn is complete.
// The channel persists across turns — only one goroutine reads from it.
func (h *Harness) drainUntilResult() {
	for raw := range h.events {
		translated := translateEvent(raw, h.sessionID, &h.agg)
		for _, ev := range translated {
			h.emit(ev)

			// Update session ID if CC assigned a new one (fork case), and emit
			// a SessionInfo event carrying the start-time flags + CC init payload.
			if ev.Type == msg.EventSystem && ev.System != nil && ev.System.Subtype == "init" {
				var init struct {
					SessionID string `json:"session_id"`
				}
				if json.Unmarshal(raw, &init) == nil && init.SessionID != "" {
					h.sessionID = init.SessionID
					// Apply staged chain rotation. Cold-start / fork commit
					// the pending WAL and write a rollout row; resume bumps
					// sessions.updated_at (and writes a kind='resume'
					// rollout if CC unexpectedly rotated the UUID).
					h.recordChainOnInit(init.SessionID, findRolloutForUUID(init.SessionID))
				}
				if info := parseInitInfo(raw); info != nil {
					info.SystemPrompt = h.systemPrompt
					info.AppendSystemPrompt = h.appendSystemPrompt
					if info.WorkingDir == "" {
						info.WorkingDir = h.workDir
					}
					h.emit(msg.Event{
						Type:      msg.EventSessionInfo,
						Harness:   harness,
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
		// Process died before delivering an init event for a still-pending
		// chain rotation — orphan the WAL row eagerly so it doesn't shadow
		// the next start (boot recovery would also catch it, but this
		// avoids leaking pending rows for the lifetime of this bridge).
		// Resume has no WAL row but still needs pending state cleared.
		if h.state != nil && h.pendingWALID != 0 {
			if oErr := h.state.OrphanWAL(h.pendingWALID); oErr != nil {
				log.Printf("[harness] orphan WAL after process died: %v", oErr)
			}
		}
		h.pendingWALID = 0
		h.pendingIntent = ""
		h.pendingParent = ""
		h.emit(msg.Event{
			Type:      msg.EventError,
			Harness:   harness,
			Timestamp: time.Now(),
			Error: &msg.ErrorEvent{
				Code:    "PROCESS_DIED",
				Message: fmt.Sprintf("Claude Code process exited unexpectedly: %v", h.proc.Err()),
			},
		})
		h.emit(msg.Event{
			Type:      msg.EventSessionState,
			Harness:   harness,
			Timestamp: time.Now(),
			State:     &msg.StateEvent{State: msg.SessionError, Previous: msg.SessionRunning, Reason: "process_died"},
		})
	}
}
