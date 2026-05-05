# Design Document: llm-bridge-claudecode

## Overview

`llm-bridge-claudecode` is a harness subprocess binary that bridges `llm-bridge` with the Claude Code CLI. It implements the llm-bridge harness subprocess protocol (JSON-RPC over stdin, NDJSON events over stdout) by wrapping Claude Code's bidirectional `--input-format stream-json --output-format stream-json` mode.

## Design Goals

1. **Pure pass-through** - No formatting, truncation, or transformation of content. Tool outputs, text, and thinking blocks are forwarded exactly as Claude Code emits them.
2. **Stateless between turns** - All session state lives in Claude Code's own session history and in llm-bridge's SQLite store. This binary holds no persistent state.
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

### AD-3: No Internal Session Tracking

**Decision**: Do not maintain a sessions map or persist sessions to disk.

**Rationale**: llm-bridge handles session persistence in SQLite. Claude Code maintains its own session history. This binary just holds a reference to the running `exec.Cmd` and its stdin/stdout pipes.

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
    cfg := loadConfig()
    
    // Read JSON-RPC requests from llm-bridge on our stdin
    scanner := bufio.Scanner(os.Stdin)
    for scanner.Scan() {
        req := parseRequest(scanner.Bytes())
        handleRequest(cfg, req)
    }
    // stdin closed = llm-bridge done with us
    cleanup()
}
```

- Parse environment config
- Enter blocking stdin read loop (reads from llm-bridge)
- Dispatch to handler based on `method`
- Clean up Claude Code process on exit

### `handler.go` - Request Handlers

```go
func handleStart(cfg *Config, params StartParams) error
func handleMessage(cfg *Config, params MessageParams) error
func handleCompact(cfg *Config, params CompactParams) error
func handleResume(cfg *Config, params ResumeParams) error
```

- `handleStart`: Spawns Claude Code process, writes initial user message to CC stdin
- `handleMessage`: Writes user message JSON to existing CC stdin pipe
- `handleCompact`: Emits system event (CC handles compaction internally)
- `handleResume`: Spawns new CC process with `--resume` (only if process died)

### `process.go` - Claude Code Process Management

```go
type CCProcess struct {
    cmd    *exec.Cmd
    stdin  io.WriteCloser  // write user messages + control_requests
    stdout io.ReadCloser   // read stream-json events
    mu     sync.Mutex      // guards stdin writes
}

func spawnClaudeCode(cfg *Config, sessionID string, args ...string) (*CCProcess, error)
func (p *CCProcess) WriteMessage(content string) error                       // text-only user message
func (p *CCProcess) WriteMessageBlocks(blocks []msg.ContentBlock) error      // multimodal user message
func (p *CCProcess) WriteInterrupt(requestID string) error                   // control_request
func (p *CCProcess) ReadEvents(ctx context.Context) <-chan json.RawMessage
func (p *CCProcess) Kill() error
```

- Spawn `exec.Command` for Claude Code with stream-json flags
- Pipe stdin for writing, stdout for reading
- Mutex-protected writes (message and interrupt can race)
- ReadEvents returns a channel of raw JSON lines from CC stdout
- No `CommandContext` - process outlives individual turns

### `translate.go` - Event Translation

```go
func translateEvent(raw json.RawMessage, sessionID string, agg *UsageAggregator) []msg.Event
```

Single entry point that inspects the `type` field and dispatches:

| CC type | Handler |
|---------|---------|
| `system` | `translateSystemEvent` - maps subtype to canonical system event |
| `assistant` | `translateAssistantEvent` - iterates content blocks, emits stream/tool/thinking |
| `result` | `translateResultEvent` - extracts usage/cost, emits result + state |
| `rate_limit_event` | `translateRateLimitEvent` - emits as system event |
| `control_response` | consumed internally, not forwarded |
| `keep_alive` | consumed internally, not forwarded |
| `tool_progress` | `translateToolProgress` - emits as system event |

One CC event may produce multiple canonical events (e.g., an `assistant` with text + tool_use blocks produces both a `stream` and a `tool_call` event).

### `usage.go` - Usage Aggregation

```go
type UsageAggregator struct {
    calls     []msg.TokenUsage  // per-API-call breakdown
    toolCalls int
}

func (a *UsageAggregator) AddAPICall(usage json.RawMessage)
func (a *UsageAggregator) AddToolCall()
func (a *UsageAggregator) Finalize(resultEvent json.RawMessage) (msg.TokenUsage, *msg.Cost)
```

- `AddAPICall`: Extracts token counts from assistant event usage
- `Finalize`: Uses result event's aggregate `usage`, `modelUsage`, and `total_cost_usd` as source of truth. Attaches per-call breakdown from `calls`.

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

## Claude Code CLI Flags

### Always used

```bash
claude \
  -p \                                   # Print mode (non-interactive)
  --input-format stream-json \           # Bidirectional stdin messaging
  --output-format stream-json \          # NDJSON streaming output
  --verbose \                            # Include system events
  --dangerously-skip-permissions         # No interactive approval prompts
```

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
- `--allow-dangerously-skip-permissions` (we pass `--dangerously-skip-permissions` directly when needed)
- `--remote-control-session-name-prefix` (niche remote-control feature)
- `--mcp-debug` (deprecated; use `--debug`)

### Mid-session control_request subtypes

The harness exposes additional JSON-RPC methods that forward to CC's stream-json `control_request` channel on stdin:

| Method | control_request subtype | Params | Purpose |
|---|---|---|---|
| `set_model` | `set_model` | `{model}` | Change model mid-session |
| `set_permission_mode` | `set_permission_mode` | `{mode}` | Change permission mode mid-session |
| `control` | caller-supplied | `{subtype, payload}` | Generic pass-through for new CC subtypes |
| `config:<json>` | derived from `subtype` field | JSON in method tail | Used by bridge-server's `handleConfigSession` route |
`interrupt` is routed via SIGINT from the bridge-server (see `Harness.Interrupt`); the others require a live CC process.

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
- Emit `error` event with exit code info
- Emit `session_state` (error)
- On next `message` or `resume` request, respawn with `--resume`

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

- `translate_test.go` - Test each CC event type maps to correct canonical events
- `usage_test.go` - Test aggregation with real CC output samples
- `config_test.go` - Test env var parsing with defaults

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
- Standard library only for everything else
