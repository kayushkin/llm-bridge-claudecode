# Design Document: llm-bridge-claudecode

## Overview

`llm-bridge-claudecode` is a harness subprocess binary that bridges `llm-bridge` with the Claude Code CLI. It implements the llm-bridge harness subprocess protocol (JSON-RPC over stdin, NDJSON events over stdout) by wrapping Claude Code's bidirectional `--input-format stream-json --output-format stream-json` mode.

## Design Goals

1. **Pure pass-through** - No formatting, truncation, or transformation of content. Tool outputs, text, and thinking blocks are forwarded exactly as Claude Code emits them.
2. **No content persistence** - Conversation *content* lives in Claude Code's own rollout files and in llm-bridge's store. This binary persists only the session-id chain + message-id identity map in a small local `state.db` (see AD-3); it holds no message bodies.
3. **Single responsibility** - Manage one Claude Code process, translate events, forward messages. Nothing else.
4. **Crash-safe** - If this process dies, llm-bridge can re-spawn it and resume via `--resume`.

## Architecture Decisions

### AD-1: Persistent Bidirectional Process via stream-json Input

**Decision**: Use `--input-format stream-json` to keep one Claude Code process alive for the entire session, sending follow-up messages via stdin.

**Rationale**: Claude Code supports bidirectional streaming where user messages are written as JSON to stdin and events stream back on stdout. This eliminates the spawn-per-turn pattern from `claude-code-adapter`:

| | Spawn-per-turn (old) | Persistent process (new) |
|-|---------------------|--------------------------|
| Follow-up message | Kill + respawn with `--resume` | Write JSON to stdin |
| Startup cost per turn | ~2-3s (Node.js + session load) | ~0ms |
| Interrupt | SIGINT to process | `control_request` on stdin |
| Memory | New Node.js per turn | One shared runtime |
| Context reload | From disk each turn | In-memory |

**Verified behavior**: Tested with Claude Code 2.1.92. Multi-turn works correctly - second message gets full conversation context. Interrupt via `control_request` returns `control_response` + abort result.

**User message format** (written to CC stdin):
```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"..."}]},"session_id":"","parent_tool_use_id":null}
```

**Interrupt format** (written to CC stdin):
```json
{"type":"control_request","request_id":"unique-id","request":{"subtype":"interrupt"}}
```

### AD-2: One Claude Code Process Per Binary Instance

**Decision**: Each `llm-bridge-claudecode` instance manages exactly one Claude Code subprocess.

**Rationale**: llm-bridge spawns a separate `llm-bridge-claudecode` process per session. This gives us:
- Process isolation between sessions
- Simple stdin/stdout piping (no multiplexing)
- Clean process tree (llm-bridge -> llm-bridge-claudecode -> claude)
- ~15MB Go overhead per instance is negligible vs. CC's ~100MB Node.js

### AD-3: Local `state.db` for the session-id chain (no content)

**Decision**: Keep a small local SQLite `state.db` that tracks the
`bridge_session_id ↔ harness_session_id` chain (sessions / rollouts / WAL) and
the harness→canonical message-id identity map. Do **not** store conversation
*content* — that still lives in llm-bridge and in Claude Code's own rollout
files.

**Rationale**: Resumes and forks rotate the Claude Code session UUID, and
bridge-server needs a stable `bridge_session_id` to map them back to one
logical conversation. The harness is the only component that sees CC's
`system:init` UUID, so it owns the chain. A write-ahead-log row is opened
before spawn and committed once init delivers the new UUID, making chain
rotation crash-safe (orphaned WAL rows are reclaimed on boot — see
`recoverOrphansOnBoot`). The identity map lets the harness pre-stamp
`Event.MessageID` so bridge-server no longer owns assistant-id assignment
(Phase III.B). `state.db` lives at
`~/.local/share/llm-bridge-claudecode/state.db`; the pool is pinned to a
single connection (modernc sqlite leaks `SQLITE_BUSY` under concurrent writers
even with WAL + busy_timeout). See `state.go` and `identity_store.go`.

> This supersedes the original "no internal session tracking" design: the
> binary still holds the running `exec.Cmd` + pipes for the live turn, but it
> is no longer stateless across restarts.

### AD-4: Forward Raw Events

**Decision**: Include the raw Claude Code stream-json line in every `msg.Event`'s `raw` field.

**Rationale**: CC emits fields not captured in the canonical schema (e.g. `rate_limit_event`, `modelUsage` per-model breakdown, `permission_denials`, `fast_mode_state`, `terminal_reason`). The `raw` field preserves full fidelity.

### AD-5: Usage Aggregation From Result Event

**Decision**: Use the `result` event's aggregate usage and `modelUsage` as the primary source for stats, supplemented by per-assistant-event tracking.

**Rationale**: Claude Code's `result` event includes:
- `usage`: aggregate token counts across all API calls
- `modelUsage`: per-model breakdown with `contextWindow`, `maxOutputTokens`, `costUSD`
- `total_cost_usd`: total cost for the turn
- `duration_ms`, `duration_api_ms`, `num_turns`

This is more reliable than summing ourselves. We still track per-assistant-event usage for the `api_call_usages` array.

## Data Flow

> **Diagram note.** The sequence diagrams below are schematic and predate two
> changes: (1) the harness no longer emits `state(running)`/`state(idle)` —
> bridge-server derives session state centrally from the init/result/error
> stream, so read those `state(...)` arrows as "bridge-server infers this
> transition," not as events on the wire; and (2) the spawn line shows
> `--dangerously-skip-perms`, but the harness actually spawns with
> `--permission-mode bypassPermissions` and relies on the PreToolUse hook.

### Happy Path: Start -> Multi-turn -> Complete

```
llm-bridge                    llm-bridge-claudecode              claude CLI (persistent)
    |                                |                               |
    |-- start{prompt} (stdin) ----->|                               |
    |                                |-- spawn: claude               |
    |                                |   --input-format stream-json  |
    |                                |   --output-format stream-json |
    |                                |   --verbose                   |
    |                                |   --dangerously-skip-perms    |
    |                                |                               |
    |                                |-- write user msg to CC stdin->|
    |                                |                               |
    |<-- state(running) ------------|                               |
    |                                |<-- system{init} -------------|
    |<-- system(init) --------------|                               |
    |                                |<-- assistant{text} ----------|
    |<-- stream(text_delta) --------|                               |
    |                                |<-- result{success} ----------|
    |<-- result ---- ---------------|                               |
    |<-- state(idle) ---------------|                               |
    |                                |   (CC process still alive)    |
    |                                |                               |
    |-- message{text} (stdin) ----->|                               |
    |                                |-- write user msg to CC stdin->|
    |<-- state(running) ------------|                               |
    |                                |<-- system{init} -------------|
    |                                |<-- assistant{...} ------------|
    |<-- stream/tool events --------|                               |
    |                                |<-- result{success} ----------|
    |<-- result --------------------|                               |
    |<-- state(idle) ---------------|                               |
    |                                |   (CC process still alive)    |
```

### Interrupt Path: control_request

```
llm-bridge                    llm-bridge-claudecode              claude CLI
    |                                |                               |
    |   (CC is streaming response)   |         (generating)          |
    |                                |                               |
    |-- SIGINT -------------------->|                               |
    |                                |-- control_request{interrupt}->|
    |                                |                               |
    |                                |<-- control_response ----------|
    |                                |<-- user{interrupted} ---------|
    |                                |<-- result{error/aborted} -----|
    |<-- state(idle) ---------------|                               |
    |                                |   (CC still alive, ready)     |
```

### Resume Path (after process death)

```
llm-bridge                    llm-bridge-claudecode              claude CLI
    |                                |                               |
    |   (CC process died/killed)     |                               |
    |                                |                               |
    |-- resume (stdin) ------------>|                               |
    |                                |-- spawn: claude --resume ID ->|
    |                                |   --input-format stream-json  |
    |                                |                               |
    |<-- state(running) ------------|                               |
    ...  (normal flow)
```

### Fork Path

```
llm-bridge                    llm-bridge-claudecode              claude CLI
    |                                |                               |
    |-- start{fork=parent} -------->|                               |
    |                                |-- spawn: claude               |
    |                                |   --resume <parent_id>        |
    |                                |   --fork-session              |
    |                                |   --input-format stream-json  |
    |                                |                               |
    |<-- state(running) ------------|                               |
    |                                |<-- system{init} (new ID) ----|
    ...
```

## Module Layout

### `main.go` - Entry Point

```go
func main() {
    // 1. Mode/subcommand selection (mutually exclusive, checked first):
    //    -version            → print version, exit
    //    LLMBRIDGE_PTY_MODE=1 → execClaudePTY(): syscall.Exec into `claude`, no flags
    //    -otel-sidecar       → runOTelSidecar() (PTY telemetry + rollout tailer)
    //    -discover [project]  → discoverSessions(), print JSON, exit
    //    -import-history ...  → importHistory(), print NDJSON, exit

    // 2. JSON-RPC harness mode (the default):
    cfg := loadConfig()
    h := NewHarness(cfg)
    if err := h.openStateAndRecover(); err != nil { // open state.db, reclaim orphan WAL rows
        log.Fatalf(...)
    }
    // SIGINT → h.Interrupt(); SIGTERM → h.Shutdown()

    scanner := bufio.NewScanner(os.Stdin) // 10MB max line
    for scanner.Scan() {
        req := parse(scanner.Bytes())
        // set_model / control / config:* run on their own goroutine so a
        // long streaming turn can't starve mid-session control.
        if isAsync(req.Method) { go h.HandleRequest(req); continue }
        h.HandleRequest(req)
    }
    h.Shutdown() // stdin closed = llm-bridge done with us
}
```

- Select mode: PTY exec / OTel sidecar / one-shot subcommand / JSON-RPC loop
- In JSON-RPC mode: load config, build the `Harness`, open `state.db` and
  recover orphaned WAL rows from a prior crash (fatal on failure)
- Enter the blocking stdin read loop; dispatch via `h.HandleRequest`, running
  async methods on a goroutine so they don't block a streaming turn
- Interrupt on SIGINT, clean shutdown (interrupt CC, close `state.db`) on
  SIGTERM or stdin EOF

### `handler.go` - Harness + Request Handlers

All handlers are methods on `*Harness`, which owns the live `*CCProcess`, the
shared event channel, the `*State` (state.db), the `*identity.Tracker`, the
`UsageAggregator`, and the persisted-across-respawn fields (workDir,
permission config, system prompts).

```go
type Harness struct { /* cfg, proc, events, state, tracker, agg, ... */ }

func NewHarness(cfg *Config) *Harness
func (h *Harness) openStateAndRecover() error   // open state.db, reclaim orphan WAL
func (h *Harness) HandleRequest(req Request) error // dispatch on req.Method
func (h *Harness) handleStart(params StartParams) error
func (h *Harness) handleMessage(params MessageParams) error
func (h *Harness) handleCompact(params CompactParams) error
func (h *Harness) handleResume() error
func (h *Harness) handleSetModel(params SetModelParams) error
func (h *Harness) handleControl(params ControlParams) error
func (h *Harness) handleConfig(raw json.RawMessage) error // "config:<json>" route
func (h *Harness) Interrupt()
func (h *Harness) Shutdown()
```

- `handleStart`: Builds the CC flag list from `StartParams`, stages the chain
  rotation in `state.db` (cold-start/fork open a WAL row; resume stages intent
  only), starts the OTel receiver, spawns CC, then writes the initial user
  message (text `Prompt` or multimodal `Blocks` — mutually exclusive)
- `handleMessage`: Writes a user message to the live CC stdin; if the process
  died, transparently respawns via `handleStart{Resume:true}`
- `handleCompact`: Emits a `compact_ack` system event (CC compacts internally)
- `handleResume`: Respawns with `--resume` only if the process isn't alive,
  recovering the harness UUID from `state.db` if it was lost
- `handleSetModel` / `handleControl` / `handleConfig`: Forward a
  `control_request` to CC stdin (no respawn). `handleConfig` inspects the
  payload `subtype` and routes to `set_model` / `interrupt` / generic control
- `recordChainOnInit` (called from `drainUntilResult` on CC's `system:init`):
  commits the pending WAL + writes the rollout row, or bumps the resume
  timestamp; also emits an `EventSessionInfo` carrying the init metadata +
  persisted system prompts

### `process.go` - Claude Code Process Management

```go
type CCProcess struct {
    cmd       *exec.Cmd
    stdin     io.WriteCloser  // write user messages + control_requests
    stdout    io.ReadCloser   // read stream-json events
    mu        sync.Mutex      // guards stdin writes
    sessionID string
    done      chan struct{}   // closed when process exits
    err       error           // exit error, set before done closes
    otelRecv  *OTelReceiver   // per-process OTLP receiver; shut down after exit
}

func spawnClaudeCode(cfg *Config, sessionID string, allowedTools []string, otelRecv *OTelReceiver, extraArgs ...string) (*CCProcess, error)
func (p *CCProcess) WriteMessage(content string) error                       // text-only user message
func (p *CCProcess) WriteMessageBlocks(blocks []msg.ContentBlock) error      // multimodal user message
func (p *CCProcess) WriteInterrupt(requestID string) error                   // control_request: interrupt
func (p *CCProcess) WriteControl(requestID, subtype string, payload map[string]any) error // generic control_request
func (p *CCProcess) ReadEvents(ctx context.Context) <-chan json.RawMessage
func (p *CCProcess) Done() <-chan struct{}
func (p *CCProcess) Err() error
func (p *CCProcess) Alive() bool
func (p *CCProcess) Kill() error
```

- Spawn `exec.Command` for Claude Code with the always-on stream-json flags
  (`-p --input-format stream-json --output-format stream-json --verbose`),
  `--allowed-tools` when non-empty, then the caller's `extraArgs`
- Wire the OTel receiver's endpoint into CC's env (`otelRecv.Env()`); the exit
  goroutine drains and shuts the receiver down ~2s after CC exits
- Pipe stdin for writing, stdout for reading; forward CC stderr to ours
- Mutex-protected writes (message and control can race)
- `ReadEvents` returns a channel of raw JSON lines from CC stdout
- No `CommandContext` — process outlives individual turns; lifecycle is
  tracked via `done`/`err`/`Alive()`

### `translate.go` - Event Translation

```go
func translateEvent(raw json.RawMessage, sessionID string, agg *UsageAggregator, tracker *identity.Tracker) []msg.Event
```

Single entry point that inspects the `type` field and dispatches:

| CC type | Handler |
|---------|---------|
| `system` | `translateSystem` - maps subtype to canonical event (init/api_retry/task_*/etc → `system`; `hook_*` → `hook`) |
| `assistant` | `translateAssistant` - iterates content blocks, emits stream/tool/thinking; pre-stamps `Event.MessageID` via the tracker |
| `result` | `translateResult` - extracts usage/cost, emits `result` (no session_state) |
| `rate_limit_event` | `translateRateLimit` - emits as `system` event |
| `user` | consumed internally (echo of synthetic interrupt message) |
| `control_response` | consumed internally, not forwarded |
| `keep_alive` | consumed internally, not forwarded |
| `tool_progress` | `translateToolProgress` - emits as `system` event |
| *(unknown)* | forwarded as a `system` event for visibility |

Session state is **not** emitted — bridge-server derives it centrally from
the init / result / error stream.

One CC event may produce multiple canonical events (e.g., an `assistant` with text + tool_use blocks produces both a `stream` and a `tool_call` event).

### `usage.go` - Usage Aggregation

```go
type UsageAggregator struct {
    calls     []msg.TokenUsage  // per-API-call breakdown
    toolCalls int
}

func (a *UsageAggregator) AddAPICall(usage ccAssistantUsage)
func (a *UsageAggregator) AddToolCall()
func (a *UsageAggregator) Finalize(raw json.RawMessage) (msg.TokenUsage, *msg.Cost)
func (a *UsageAggregator) APICallUsages() []msg.TokenUsage
func (a *UsageAggregator) ToolCalls() int
func (a *UsageAggregator) Reset()
```

- `AddAPICall`: Accumulates per-API-call token counts from assistant event usage
- `Finalize`: Uses the result event's aggregate `usage`, `modelUsage`, and `total_cost_usd` as source of truth. Attaches the per-call breakdown from `calls`.

### `config.go` - Configuration

```go
type Config struct {
    ClaudePath string // CLAUDE_PATH
    Model      string // CLAUDE_MODEL
    WorkDir    string // CLAUDE_WORKDIR
    APIKey     string // ANTHROPIC_API_KEY
}

func loadConfig() *Config
```

Reads from environment variables. No flags, no config files.

### `state.go` - Session-id chain (state.db)

SQLite store of the `bridge_session_id ↔ harness_session_id` chain. Three
tables: `sessions` (current harness UUID per bridge id), `rollouts` (one row
per CC session UUID in the chain, with `sequence` / `parent_harness_id` /
`kind` ∈ start|resume|fork), and `wal` (pending chain mutations). Opened once
at boot via `OpenState(DefaultStatePath())`; the pool is pinned to a single
connection (modernc sqlite leaks `SQLITE_BUSY` under concurrent writers). Path:
`~/.local/share/llm-bridge-claudecode/state.db`.

### `identity_store.go` - Message-id identity map

Bridges `*State` to `identity.Store` so an `identity.Tracker` can map CC's
per-message ids to canonical `msg_<ulid>` ids. Lets the harness pre-stamp
`Event.MessageID` (Phase III.B) instead of bridge-server assigning it.

### `otel.go` - In-process OTLP receiver

`OTelReceiver` is an OTLP/HTTP-JSON listener on `127.0.0.1:<random>`. CC exports
to it via `OTEL_EXPORTER_OTLP_ENDPOINT` (`Env()`); it translates `/v1/logs` and
`/v1/metrics` records into `EventAPICall` (and related) events handed to the
harness `emit` callback. One receiver per CC process — cross-session
correlation is impossible by construction. Always on, because it surfaces
auxiliary API calls (session-title, prompt-suggestion) that stream-json hides.

### `sidecar.go` - PTY-mode OTel sidecar (`-otel-sidecar`)

Standalone process bridge-server spawns *alongside* a PTY-mode `claude` (which
has no Go host process). Starts an `OTelReceiver` + a rollout tailer, prints the
receiver URL on stdout line 1 (for the PTY child's
`OTEL_EXPORTER_OTLP_ENDPOINT`), and POSTs each translated event to
`<LLMBRIDGE_BRIDGE_SERVER_URL>/sidecar/event/<bridge_id>`. Exits on stdin close
or SIGTERM after a short OTel flush window.

### `rollout.go` - PTY-mode content tailer

Tails CC's per-session JSONL rollout under
`~/.claude/projects/<encoded(cwd)>/<uuid>.jsonl` and emits granular content
events (user/assistant text, thinking, tool args + outputs) tagged
`Extensions["source"]="rollout"`. This is the only content source for PTY
sessions — OTel carries metadata only. See `PTY-COVERAGE.md` for the full
coverage matrix versus stream-json mode.

### `discover.go` / `import_history.go` - Out-of-band subcommands

- `discover.go` (`-discover [project]`): lists one `msg.StoredSession` per
  `bridge_session_id` in `state.db`. On-disk rollout files not yet in the chain
  are cold-imported as synthetic single-rollout sessions (idempotent).
- `import_history.go` (`-import-history <id> [path]`): reads a CC session
  `.jsonl` and replays it as translated `msg.Event` NDJSON on stdout.

## Claude Code CLI Flags

### Always used

```bash
claude \
  -p \                                   # Print mode (non-interactive)
  --input-format stream-json \           # Bidirectional stdin messaging
  --output-format stream-json \          # NDJSON streaming output
  --verbose                              # Include system events
```

`--allowed-tools <t...>` is also appended at spawn whenever the caller passed a
non-empty allowlist. Permission gating is **not** `--dangerously-skip-permissions`;
it defaults to `--permission-mode bypassPermissions` plus the bridge-server
PreToolUse hook (see *Permission-prompt flow* below).

### Conditional flags

Session / resume:
```bash
  [--resume <session_id>]              # Resume existing session
  [--fork-session]                     # Fork from resumed session
  [--continue]                         # Continue most recent session in cwd
  [--from-pr <pr>]                     # Resume session linked to a PR
  [--session-id <uuid>]                # Caller-supplied UUID
  [--name <name>]                      # Session display name
  [--no-session-persistence]           # Ephemeral sessions
```

Model / cost / effort:
```bash
  [--model <model>]                    # Model override
  [--fallback-model <model>]           # Auto-fallback on overload
  [--max-budget-usd <budget>]          # Per-session cost cap
  [--effort <low|medium|high|xhigh|max>]  # Reasoning effort
```

Prompts / context:
```bash
  [--system-prompt <prompt>]           # Replace system prompt
  [--append-system-prompt <prompt>]    # Append to system prompt
  [--add-dir <dirs...>]                # Additional tool access dirs
  [--file <file_id:path> ...]          # File resources to download at start
  [--json-schema <schema>]             # Structured output validation
```

Tools / permissions:
```bash
  [--allowed-tools <tool1> ...]        # Tool allowlist
  [--disallowed-tools <tool1> ...]     # Tool deny-list
  [--tools <tool1> ...]                # Exact built-in tool set
  [--permission-mode <mode>]           # Fine-grained permissions
  [--disable-slash-commands]           # Disable all skills
  [--brief]                            # Enable SendUserMessage tool
```

MCP / plugins / settings:
```bash
  [--mcp-config <configs...>]          # MCP server configuration
  [--strict-mcp-config]                # Only use --mcp-config
  [--plugin-dir <path>] ...            # Load plugins from directories
  [--settings <file-or-json>]          # Load additional settings
  [--setting-sources user,project,...] # Which setting layers to load
  [--agent <agent>]                    # Select configured agent
  [--agents <json>]                    # Inline agent definitions
```

Modes / observability:
```bash
  [--worktree [name]]                  # Git worktree isolation
  [--bare]                             # Minimal mode
  [--include-partial-messages]         # Finer streaming deltas
  [--include-hook-events]              # Hook lifecycle events in stream
  [--replay-user-messages]             # Echo user msgs back on stdout
  [--debug [filter]]                   # Debug mode
  [--debug-file <path>]                # Debug log file
  [--betas <betas...>]                 # Beta API opt-ins
```

### Explicitly not exposed

Interactive-only flags have no role in a headless subprocess:

- `--ide`, `--tmux`, `--chrome` / `--no-chrome`
- `--dangerously-skip-permissions` / `--allow-dangerously-skip-permissions` (permission gating is the PreToolUse hook + `--permission-mode bypassPermissions`)
- `--remote-control-session-name-prefix` (niche remote-control feature)
- `--mcp-debug` (deprecated; use `--debug`)

### Mid-session control_request subtypes

These forward to CC's stream-json `control_request` channel on stdin. Only
`set_model`, `control`, and `config:<json>` are dispatched JSON-RPC *methods*
(see `HandleRequest` in `handler.go`); `set_permission_mode` and `interrupt`
have **no** dedicated method — they ride the generic `control` / `config:`
route by `subtype`.

| Method | control_request subtype | Params | Purpose |
|---|---|---|---|
| `set_model` | `set_model` | `{model}` | Change model mid-session |
| `control` | caller-supplied | `{subtype, payload}` | Generic pass-through (e.g. `set_permission_mode`, or any new CC subtype) |
| `config:<json>` | derived from `subtype` field | JSON in method tail | Used by bridge-server's `handleConfigSession` route; dispatches `set_model` / `interrupt` / generic control |

`interrupt` is routed either via `config:` with `subtype:"interrupt"` or via a
process SIGINT from bridge-server (see `Harness.Interrupt`); the control
subtypes require a live CC process.

### Permission-prompt flow

Permission gating now lives in bridge-server: a PreToolUse HTTP hook is injected via `--settings` that points at `/permission/cc-prehook/<bridge_id>`. The harness hardcodes `--permission-mode bypassPermissions` so CC's own permission system stays off; the hook is the sole gate.

How it works:

1. Bridge-server's `buildClaudeCodeSettings` synthesizes a settings JSON that prepends the permission entry to `PreToolUse`:
   ```json
   { "matcher": "*",
     "hooks": [{ "type": "http",
                 "url": "http://bridge/permission/cc-prehook/<bridge_id>",
                 "timeout": 86400 }] }
   ```
   CC executes `type:"http"` hooks natively; `timeout` is in seconds (1 day) — long enough that no human-driven approval flow will hit it.
2. On every tool call CC POSTs the hook payload to that URL. Bridge-server consults bridge-prefs (bypass shortcut), then permission-store `/evaluate`. allow/deny → respond immediately. ask → mint a `request_id`, park on `parkedAsks`, broadcast `HookEvent{phase:"awaiting_resolution"}` over SSE.
3. The UI's `POST /sessions/:id/hooks/:request_id/resolve` delivers a decision to the parked call. Bridge-server emits the matching `HookEvent{phase:"completed"}` and writes the CC-shaped response: `{ hookSpecificOutput: { hookEventName: "PreToolUse", permissionDecision: "allow"|"deny", permissionDecisionReason } }`.
4. If CC drops the request (interrupt, process exit), the parked entry is reaped via the request's `Context().Done()` and a synthetic deny is broadcast so the UI banner clears.

The harness no longer ships a `bridge_perm` MCP, no longer takes a `resolve_hook` JSON-RPC method, and no longer wires `--permission-prompt-tool` / `--mcp-config` for permissions. See `permission-store/docs/cc-mcp-retrospective.md` for why we left the MCP path behind.

## Error Handling

### Claude Code Process Death

If the Claude Code process exits unexpectedly:
- Emit an `error` event (`PROCESS_DIED`, or `SPAWN_ERROR` if it never started) with exit info
- Orphan any pending WAL chain row so it doesn't shadow the next start
- (bridge-server derives the `error` session state centrally — the harness emits no `session_state`)
- On the next `message` or `resume` request, respawn with `--resume`

### Interrupt Response

When interrupt `control_request` is sent, Claude Code emits:
1. `{"type":"control_response","response":{"subtype":"success","request_id":"..."}}`
2. `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]},...}`
3. `{"type":"result","subtype":"error_during_execution",...,"terminal_reason":"aborted_streaming"}`

We consume the `control_response` internally and translate the result as a normal turn completion with `is_error: true`.

### Malformed stream-json Lines

If a line from Claude Code's stdout is not valid JSON:
- Log the raw line to stderr
- Skip the line, continue processing
- Do not emit an error event (transient noise)

### stdin EOF (from llm-bridge)

When llm-bridge closes our stdin:
- Send interrupt to Claude Code if a turn is active
- Terminate Claude Code process
- Exit cleanly

## Testing Strategy

### Unit Tests

- `translate_test.go` - Each CC event type maps to the correct canonical events
- `handler_chain_test.go` - state.db chain rotation (start / resume / fork, WAL commit + orphan recovery)
- `handler_blocks_test.go` / `process_blocks_test.go` - multimodal `Blocks` validation + wire translation
- `otel_test.go` - OTLP record → `EventAPICall` translation (incl. bare-number `intValue`)
- `rollout_test.go` - rollout JSONL → granular content events; terminal `EventResult` synthesis
- `state_test.go` - state.db storage layer (sessions / rollouts / WAL)
- `discover_test.go` - session discovery + cold import

There is no `usage_test.go` / `config_test.go`.

### Integration Tests

- Spawn the binary, write JSON-RPC to stdin, read events from stdout
- Use a mock script that mimics CC's stream-json protocol
- Test multi-turn: start + message + message
- Test interrupt: start + message + SIGINT -> verify control_request sent
- Test process death + resume

### Mock Claude Code

```bash
#!/bin/bash
# mock-claude: reads stdin for user messages, emits stream-json events
echo '{"type":"system","subtype":"init","session_id":"test-123","model":"mock","cwd":"/tmp","tools":[]}'
while IFS= read -r line; do
  type=$(echo "$line" | jq -r '.type // empty')
  if [ "$type" = "user" ]; then
    echo '{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"Mock response"}],"usage":{"input_tokens":10,"output_tokens":5}}}'
    echo '{"type":"result","subtype":"success","is_error":false,"duration_ms":100,"num_turns":1,"result":"Mock response","total_cost_usd":0.001}'
  fi
done
```

## Dependencies

- `github.com/kayushkin/llm-bridge/msg` - Canonical event types
- `github.com/kayushkin/llm-bridge/identity` - Message-id `Tracker` / `Store`
- `modernc.org/sqlite` - Pure-Go SQLite driver for `state.db` (no cgo)
- `github.com/oklog/ulid/v2` - ULID minting for canonical message ids
- Standard library for everything else (the OTLP receiver is hand-rolled on
  `net/http` + `encoding/json` — no OpenTelemetry SDK dependency)

> `go.mod` carries `replace github.com/kayushkin/llm-bridge => ../llm-bridge`
> for local development; see the README's *OSS publish blockers* section.
