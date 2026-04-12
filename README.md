# llm-bridge-claudecode

Claude Code harness subprocess for [llm-bridge](../llm-bridge). Wraps the Claude Code CLI as a managed subprocess, translating its `--output-format stream-json` output into the canonical `msg.Event` protocol that llm-bridge expects.

## How It Works

llm-bridge spawns `llm-bridge-claudecode` as a child process. Communication is bidirectional over stdio:

- **stdin** - llm-bridge sends JSON-RPC requests (start, message, compact, resume)
- **stdout** - this binary emits NDJSON `msg.Event` objects (stream, tool_call, tool_result, result, etc.)
- **signals** - llm-bridge sends SIGINT for pause/interrupt

```
llm-bridge (parent)
  |
  |  stdin: JSON-RPC requests
  |  stdout: NDJSON events
  |  signals: SIGINT
  |
  v
llm-bridge-claudecode (this binary)
  |
  |  stdin: stream-json user messages + control_requests
  |  stdout: stream-json events (system, assistant, result, etc.)
  |
  v
claude --input-format stream-json --output-format stream-json
  (single long-lived process per session)
```

### Persistent Bidirectional Process

Claude Code supports `--input-format stream-json` which keeps a single process alive for the entire session. Instead of spawning a new `claude -p --resume` process per turn, we:

1. Spawn one `claude` process at session start with `--input-format stream-json --output-format stream-json`
2. Send follow-up messages by writing user message JSON to Claude Code's stdin
3. Send interrupts via `control_request` JSON on stdin (no SIGINT needed for mid-turn interrupt)
4. The process maintains full conversation context in-memory across turns

This eliminates per-turn process startup overhead and keeps the Node.js runtime warm.

## Architecture

### Core Loop

1. Receive `start` JSON-RPC request from llm-bridge on stdin
2. Spawn Claude Code with `--input-format stream-json --output-format stream-json --verbose`
3. Write the initial user message to Claude Code's stdin
4. Read stream-json events from Claude Code's stdout in a goroutine
5. Translate each event to canonical `msg.Event` and write to our stdout
6. On `result` event, aggregate usage and emit canonical result
7. Wait for next JSON-RPC request from llm-bridge (message, compact, etc.)
8. For `message` requests, write a new user message to Claude Code's stdin (no new process)

### Key Responsibilities

| Responsibility | Description |
|---------------|-------------|
| **Process lifecycle** | Spawn and manage one long-lived Claude Code process per session |
| **Event translation** | Map Claude Code's stream-json events to `msg.Event` types |
| **Message injection** | Forward follow-up messages to Claude Code's stdin as stream-json |
| **Interrupt forwarding** | Send `control_request` interrupts to Claude Code's stdin |
| **Usage aggregation** | Track per-API-call token usage across a multi-turn agentic run |
| **Session continuity** | Support `--resume` and `--fork-session` for session management |
| **Config application** | Apply model, effort, budget, and tool restrictions at session start |

### What This Binary Does NOT Do

- No HTTP server (llm-bridge handles that)
- No SQLite persistence (llm-bridge handles session storage)
- No NATS integration (llm-bridge can optionally bridge to NATS)
- No SSE streaming (llm-bridge handles client-facing streams)

## Project Structure

```
llm-bridge-claudecode/
├── main.go              # Entry point: stdin reader, dispatch loop
├── handler.go           # JSON-RPC method handlers (start, message, compact, resume)
├── process.go           # Claude Code subprocess: spawn, stdin writes, stdout reader
├── translate.go         # CC stream-json event -> msg.Event translation
├── usage.go             # Token usage aggregation and cost calculation
├── config.go            # Environment config and per-session overrides
├── go.mod
└── go.sum
```

## Configuration

All configuration comes from environment variables set by llm-bridge or the system:

| Variable | Default | Description |
|----------|---------|-------------|
| `CLAUDE_PATH` | `claude` | Path to the Claude Code CLI binary |
| `CLAUDE_MODEL` | *(none)* | Default model override (e.g. `claude-sonnet-4-20250514`) |
| `CLAUDE_WORKDIR` | *(cwd)* | Working directory for Claude Code processes |
| `ANTHROPIC_API_KEY` | *(none)* | API key (if not set, Claude Code uses its own OAuth) |

## JSON-RPC Protocol (stdin from llm-bridge)

### `start` - Begin a new session

```json
{
  "method": "start",
  "params": {
    "session_id": "uuid-here",
    "display_name": "My Session",
    "agent_id": "dagda",
    "prompt": "Fix the failing tests",
    "resume": false,
    "fork": ""
  }
}
```

Spawns a new Claude Code process with `--input-format stream-json --output-format stream-json --verbose`. Sends the initial prompt as a user message on Claude Code's stdin. If `resume` is true, adds `--resume <session_id>`. If `fork` is set, adds `--resume <fork> --fork-session`.

### `message` - Send a follow-up message

```json
{
  "method": "message",
  "params": {
    "content": "Now run the tests"
  }
}
```

Writes a user message directly to the running Claude Code process's stdin. No new process is spawned. The message format sent to Claude Code is:

```json
{"type":"user","message":{"role":"user","content":[{"type":"text","text":"Now run the tests"}]},"session_id":"","parent_tool_use_id":null}
```

### `compact` - Request context compaction

```json
{
  "method": "compact",
  "params": {
    "summary": "optional summary text"
  }
}
```

Context compaction happens automatically within Claude Code when needed. This emits a system event acknowledging the request.

### `resume` - Resume a paused session

```json
{
  "method": "resume",
  "params": {}
}
```

If the Claude Code process was killed (e.g. after SIGTERM), spawns a new process with `--resume <session_id>`.

## Claude Code stdin Protocol (stream-json input)

These are the messages this binary writes to Claude Code's stdin:

### User Message

```json
{
  "type": "user",
  "message": {
    "role": "user",
    "content": [{"type": "text", "text": "the user's message"}]
  },
  "session_id": "",
  "parent_tool_use_id": null
}
```

Content can be a string or an array of content blocks (text, images, etc).

### Interrupt (control_request)

```json
{
  "type": "control_request",
  "request_id": "unique-id",
  "request": {
    "subtype": "interrupt"
  }
}
```

Claude Code responds with a `control_response` on stdout and aborts the current turn. The session remains alive for further messages.

## Claude Code stdout Protocol (stream-json output)

### Event Types from Claude Code

| CC Event Type | Description |
|---------------|-------------|
| `system` (subtype: `init`) | Session initialized, contains model/tools/config |
| `system` (subtype: `compact_boundary`) | Context was compacted |
| `system` (subtype: `api_retry`) | API request being retried |
| `system` (subtype: `hook_started/progress/response`) | Hook lifecycle events |
| `system` (subtype: `status`) | Status changes |
| `system` (subtype: `session_state_changed`) | Internal session state |
| `system` (subtype: `task_notification`) | Background task updates |
| `system` (subtype: `task_started/task_progress`) | Task lifecycle |
| `system` (subtype: `post_turn_summary`) | Post-turn summary |
| `assistant` | Message with content blocks (text, thinking, tool_use) |
| `result` (subtype: `success`) | Turn completed successfully |
| `result` (subtype: `error_*`) | Turn failed |
| `rate_limit_event` | Rate limit status |
| `control_response` | Response to a control_request |
| `tool_progress` | Tool execution progress |
| `keep_alive` | Keepalive ping |

### Result Subtypes

| Subtype | Meaning |
|---------|---------|
| `success` | Turn completed normally |
| `error_during_execution` | Runtime error or user abort |
| `error_max_turns` | Exceeded max agentic turns |
| `error_max_budget_usd` | Exceeded cost budget |
| `error_max_structured_output_retries` | Structured output validation failures |

## Event Protocol (stdout to llm-bridge)

All events are NDJSON (one JSON object per line) following the `msg.Event` envelope:

```json
{
  "type": "<event_type>",
  "harness": "claude_code",
  "session_id": "uuid-here",
  "timestamp": "2026-04-12T10:30:00Z",
  "<event_type>": { ... },
  "raw": { ... }
}
```

### Event Types Emitted

#### `session_state` - State transitions

```json
{
  "type": "session_state",
  "state": {
    "state": "running",
    "previous": "idle"
  }
}
```

States: `idle` -> `running` -> `completed` | `error` | `aborted`

#### `stream` - Text and thinking deltas

```json
{
  "type": "stream",
  "stream": {
    "delta": {
      "index": 0,
      "type": "text_delta",
      "text": "Here's the fix..."
    },
    "message_id": "msg_abc123"
  }
}
```

Delta types: `text_delta`, `thinking_delta`

#### `tool_call` - Tool invocation

```json
{
  "type": "tool_call",
  "tool_call": {
    "tool_id": "toolu_abc123",
    "name": "Edit",
    "input": {"file_path": "/tmp/foo.go", "old_string": "...", "new_string": "..."}
  }
}
```

#### `tool_result` - Tool output

```json
{
  "type": "tool_result",
  "tool_result": {
    "tool_id": "toolu_abc123",
    "name": "Edit",
    "output": "File edited successfully",
    "is_error": false
  }
}
```

#### `thinking` - Extended thinking blocks

```json
{
  "type": "thinking",
  "thinking": {
    "text": "I need to analyze the test output..."
  }
}
```

#### `system` - System notifications

```json
{
  "type": "system",
  "system": {
    "subtype": "init",
    "message": "Session initialized"
  }
}
```

Subtypes: `init`, `compact_boundary`, `api_retry`, `hook_started`, `hook_progress`, `hook_response`, `status`, `task_notification`, `task_started`

#### `result` - Turn completion

```json
{
  "type": "result",
  "result": {
    "text": "I've fixed the failing tests by...",
    "is_error": false,
    "usage": {
      "input_tokens": 15000,
      "output_tokens": 3200,
      "total_tokens": 18200,
      "cache_read_tokens": 12000,
      "cache_write_tokens": 5000,
      "context_tokens": 45000,
      "context_limit": 200000
    },
    "cost": {
      "total_usd": 0.0842
    },
    "duration_ms": 45000,
    "duration_api_ms": 38000,
    "num_turns": 3,
    "api_calls": 3,
    "model": "claude-sonnet-4-20250514",
    "api_call_usages": [...]
  }
}
```

#### `error` - Error conditions

```json
{
  "type": "error",
  "error": {
    "code": "EXECUTION_ERROR",
    "message": "Claude Code process exited with code 1",
    "retryable": false
  }
}
```

## Claude Code stream-json -> msg.Event Mapping

| CC Event Type | CC Subtype/Block | Maps To |
|---------------|-----------------|---------|
| `system` | `init` | `session_state` (running) + `system` (init) |
| `system` | `compact_boundary` | `system` (compact_boundary) |
| `system` | `api_retry` | `system` (api_retry) |
| `system` | `hook_*` | `system` (forwarded) |
| `system` | `task_notification` | `system` (forwarded) |
| `system` | `status` | `system` (forwarded) |
| `assistant` | text block | `stream` (text_delta) |
| `assistant` | thinking block | `thinking` + `stream` (thinking_delta) |
| `assistant` | tool_use block | `tool_call` |
| `assistant` | tool_result block | `tool_result` |
| `result` | `success` | `result` + `session_state` (completed) |
| `result` | `error_*` | `error` + `session_state` (error) |
| `rate_limit_event` | | `system` (rate_limit) |
| `control_response` | | consumed internally (not forwarded) |
| `keep_alive` | | consumed internally (not forwarded) |
| `tool_progress` | | `system` (tool_progress) |

### Usage Aggregation

Claude Code reports usage per-API-call within `assistant` events and aggregate totals in `result` events:

1. Per-API-call: `assistant.message.usage` has `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`
2. Aggregate: `result.usage` has totals, `result.modelUsage` has per-model breakdown with `contextWindow` and `costUSD`
3. This binary captures per-API-call usage from each assistant event and emits aggregated `TokenUsage` in the result

## Claude Code CLI Flags

### Flags used at session start

| Flag | Source | Description |
|------|--------|-------------|
| `--input-format stream-json` | Always | Bidirectional streaming input |
| `--output-format stream-json` | Always | NDJSON streaming output |
| `--verbose` | Always | Include system events in output |
| `--dangerously-skip-permissions` | Default | No interactive approval prompts |
| `--resume <id>` | resume/fork | Resume existing session |
| `--fork-session` | fork | Fork from resumed session |
| `--session-id <uuid>` | start | Use specific session ID |
| `--model <model>` | config | Model override |
| `--max-budget-usd <n>` | config | Cost cap |
| `--effort <level>` | config | Reasoning effort (low/medium/high/max) |
| `--disallowed-tools <t...>` | config | Tool restrictions |
| `--allowed-tools <t...>` | config | Tool allowlist |
| `--no-session-persistence` | ephemeral | Don't save session to CC's storage |

### Flags available but not yet wired

These Claude Code flags are available for future integration. They need corresponding fields added to the llm-bridge harness protocol to be configurable per-session:

| Flag | Description |
|------|-------------|
| `--system-prompt <prompt>` | Replace default system prompt |
| `--append-system-prompt <prompt>` | Append to default system prompt |
| `--add-dir <dirs...>` | Additional directories for tool access |
| `--mcp-config <configs...>` | Load MCP servers from JSON |
| `--json-schema <schema>` | Structured output validation |
| `--fallback-model <model>` | Auto-fallback on overload |
| `--permission-mode <mode>` | acceptEdits/auto/bypassPermissions/default/dontAsk/plan |
| `--tools <tools...>` | Specify exact built-in tool set |
| `--worktree [name]` | Git worktree isolation |
| `--bare` | Minimal mode (skip hooks, LSP, plugins, etc.) |
| `--betas <betas...>` | Beta API feature opt-in |
| `--include-partial-messages` | Finer-grained streaming deltas |
| `--include-hook-events` | Hook lifecycle in output stream |
| `--name <name>` | Display name for session |
| `--agent <agent>` | Use specific CC agent config |
| `-c` / `--continue` | Continue most recent conversation |

## Interrupt Handling

### Mid-turn Interrupt (via stdin)

Instead of SIGINT, interrupts are sent as `control_request` messages on Claude Code's stdin:

```json
{"type":"control_request","request_id":"int-001","request":{"subtype":"interrupt"}}
```

Claude Code responds with:
1. A `control_response` confirming the interrupt
2. A synthetic user message `[Request interrupted by user]`
3. A `result` with `subtype: "error_during_execution"` and `terminal_reason: "aborted_streaming"`

### Process-level Signals

| Signal | Behavior |
|--------|----------|
| SIGINT | Send interrupt control_request to CC stdin. If CC doesn't respond, fall back to process SIGINT. |
| SIGTERM | Terminate Claude Code process. Emit `session_state` (aborted). Exit. |

## Building

```bash
go build -o llm-bridge-claudecode .
```

The binary should be placed where llm-bridge can find it (typically `~/bin/` or on `$PATH`). llm-bridge discovers harness binaries by looking for `llm-bridge-*` executables.

## Relationship to claude-code-adapter

This project is derived from `claude-code-adapter` but differs in key ways:

| Aspect | claude-code-adapter | llm-bridge-claudecode |
|--------|--------------------|-----------------------|
| **Communication** | NATS pub/sub | stdin/stdout NDJSON |
| **Session storage** | Own JSON file | Delegated to llm-bridge |
| **Event format** | Custom ChatDelta | Canonical msg.Event |
| **Process model** | Standalone service, many sessions | Managed subprocess, one session |
| **CC process per turn** | New process per turn (spawn + resume) | Single persistent process (stream-json input) |
| **Message injection** | Stop + respawn with new prompt | Write to CC stdin (no respawn) |
| **Interrupt mechanism** | SIGINT to CC process | control_request on CC stdin |
| **Control plane** | NATS request/reply | JSON-RPC on stdin + signals |
| **Deployment** | systemd service | Binary on PATH |
