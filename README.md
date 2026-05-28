# llm-bridge-claudecode

[![status: implemented](https://img.shields.io/badge/status-implemented-green)](#)

Claude Code harness subprocess for [llm-bridge](https://github.com/kayushkin/llm-bridge). Wraps the [Claude Code](https://docs.claude.com/en/docs/claude-code) CLI as a managed subprocess, translating its `--output-format stream-json` output into the canonical `msg.Event` protocol that llm-bridge expects.

## How It Works

llm-bridge spawns `llm-bridge-claudecode` as a child process. Communication is bidirectional over stdio:

- **stdin** - llm-bridge sends JSON-RPC requests (start, message, compact, resume, set_model, control, config:&lt;json&gt;)
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

### Operating Modes

The binary runs in one of two modes, selected by llm-bridge-server:

- **stream-json mode (default).** The flow described above: the harness spawns
  `claude -p --input-format stream-json --output-format stream-json` and
  translates its NDJSON output into `msg.Event`. This is what the rest of this
  document describes unless noted otherwise.
- **PTY mode.** When llm-bridge-server launches the harness with
  `LLMBRIDGE_PTY_MODE=1` (inside a pseudoterminal), `main` immediately
  `exec`s into the upstream `claude` CLI with **no flags** so the pty fd is
  wired straight to Claude Code's native TUI. There is no JSON-RPC channel and
  no event translation in the harness process itself. Conversation content and
  telemetry are instead captured out-of-band by the **OTel sidecar**
  (`-otel-sidecar`, see `sidecar.go`) and the **rollout tailer** (`rollout.go`).
  See [PTY-COVERAGE.md](PTY-COVERAGE.md) for exactly which events each path
  produces versus stream-json mode.

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
2. Spawn Claude Code with `--input-format stream-json --output-format stream-json --verbose` plus any start-time flags from `StartParams` (see `handler.go`)
3. Write the initial user message to Claude Code's stdin
4. Read stream-json events from Claude Code's stdout in a goroutine
5. Translate each event to canonical `msg.Event` and write to our stdout
6. On `result` event, aggregate usage and emit canonical result
7. Wait for next JSON-RPC request from llm-bridge (message, compact, resume, set_model, control, config:&lt;json&gt;)
8. For `message` requests, write a new user message to Claude Code's stdin (no new process)
9. For mid-session control requests (`set_model`, `control`, `config:<json>`), write a `control_request` JSON to Claude Code's stdin without respawning. `set_model` and `control` (and `config:` requests) run on their own goroutine so they don't block the read loop while a turn is streaming (see the `asyncMethods` set in `main.go`).

### Key Responsibilities

| Responsibility | Description |
|---------------|-------------|
| **Process lifecycle** | Spawn and manage one long-lived Claude Code process per session |
| **Event translation** | Map Claude Code's stream-json events to `msg.Event` types |
| **Message injection** | Forward follow-up messages to Claude Code's stdin as stream-json |
| **Interrupt forwarding** | Send `control_request` interrupts to Claude Code's stdin |
| **Usage aggregation** | Track per-API-call token usage across a multi-turn agentic run |
| **Session continuity** | Support `--resume` and `--fork-session` for session management |
| **Chain persistence** | Maintain `state.db`: the bridge↔harness session-id chain (sessions/rollouts/WAL) and the harness→canonical message-id identity map |
| **Telemetry capture** | Run an in-process OTLP receiver so per-API-call usage/cost (including auxiliary calls hidden from stream-json) flows out as `api_call` events |
| **Config application** | Apply model, effort, budget, and tool restrictions at session start |

### What This Binary Does NOT Do

- No client-facing HTTP API (llm-bridge handles that). It *does* run a private
  in-process OTLP receiver on `127.0.0.1:<random>` for Claude Code's telemetry
  (`otel.go`), and the `-otel-sidecar` mode runs one standalone.
- No session *content* storage (llm-bridge handles message history). It *does*
  keep a small local `state.db` for the bridge↔harness session-id chain and
  message-id identity map (`state.go`, `identity_store.go`).
- No NATS integration (llm-bridge can optionally bridge to NATS)
- No SSE streaming (llm-bridge handles client-facing streams)

## Project Structure

```
llm-bridge-claudecode/
├── main.go              # Entry point: mode select (PTY / otel-sidecar / subcommands), stdin dispatch loop
├── handler.go           # Harness struct + JSON-RPC handlers (start, message, compact, resume, set_model, control, config:<json>)
├── process.go           # Claude Code subprocess: spawn, stdin writes, stdout reader, OTel env wiring
├── translate.go         # CC stream-json event -> msg.Event translation
├── usage.go             # Token usage aggregation and cost calculation
├── config.go            # Environment config (CLAUDE_PATH / CLAUDE_MODEL / CLAUDE_WORKDIR / ANTHROPIC_API_KEY)
├── state.go             # state.db: bridge_session_id <-> harness_session_id chain (sessions/rollouts/WAL)
├── identity_store.go    # state.db-backed identity.Tracker for pre-stamping Event.MessageID
├── otel.go              # In-process OTLP/HTTP receiver -> EventAPICall (telemetry, always on)
├── sidecar.go           # `-otel-sidecar` mode: standalone OTLP receiver + rollout tailer for PTY sessions
├── rollout.go           # Tails CC's per-session JSONL rollout for PTY-mode content events
├── discover.go          # `-discover` mode: list sessions from state.db (cold-imports disk sessions)
├── import_history.go    # `-import-history` mode: replay a CC session .jsonl as NDJSON
├── scripts/             # smoke-telemetry.sh and other dev helpers
├── *_test.go            # Unit tests (translate, handler chain/blocks, process blocks, otel, rollout, state)
├── DESIGN.md            # Design notes
├── PTY-COVERAGE.md      # PTY-mode vs stream-json event coverage matrix
├── LICENSE              # Apache 2.0
└── go.mod
```

## Configuration

All configuration comes from environment variables set by llm-bridge or the system:

| Variable | Default | Description |
|----------|---------|-------------|
| `CLAUDE_PATH` | `claude` | Path to the Claude Code CLI binary |
| `CLAUDE_MODEL` | *(none)* | Default model override (e.g. `claude-sonnet-4-20250514`) |
| `CLAUDE_WORKDIR` | *(cwd)* | Working directory for Claude Code processes |
| `ANTHROPIC_API_KEY` | *(none)* | API key (if not set, Claude Code uses its own OAuth) |

`state.db` lives at `~/.local/share/llm-bridge-claudecode/state.db` (see
`DefaultStatePath` in `state.go`).

### PTY-mode environment

Set by llm-bridge-server when launching the harness in PTY mode or its
`-otel-sidecar` companion — not user-facing:

| Variable | Used by | Description |
|----------|---------|-------------|
| `LLMBRIDGE_PTY_MODE=1` | `main` | Switches the harness into PTY mode: `exec` straight into `claude` with no flags. |
| `LLMBRIDGE_BRIDGE_SESSION_ID` | `-otel-sidecar` | Canonical `bridge_session_id` stamped on emitted events. Required. |
| `LLMBRIDGE_BRIDGE_SERVER_URL` | `-otel-sidecar` | Base URL the sidecar POSTs translated events to (`/sidecar/event/<id>`). Required. |
| `LLMBRIDGE_PTY_CWD` | `-otel-sidecar` | Working dir the rollout tailer maps to `~/.claude/projects/<encoded>`. Defaults to `/`. |
| `LLMBRIDGE_PTY_RESUME_ID` | `-otel-sidecar` | Harness UUID to short-circuit the rollout file lookup on resume. |

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

### `set_model` / `control` / `config:<json>` - Mid-session control

These methods surface as a `control_request` written to Claude Code's stdin without respawning the process (see the *Claude Code stdin Protocol* section below). The dispatched JSON-RPC methods are:

```jsonc
{"method":"set_model","params":{"model":"sonnet"}}
{"method":"control","params":{"subtype":"some_new_subtype","payload":{"k":"v"}}}
// "config:<json>" — the method name itself carries an inline JSON blob whose
// "subtype" drives the dispatch (set_model / interrupt / generic control).
// This is the route bridge-server's handleConfigSession uses.
```

`control` is the generic forward — use it (or `config:<json>` with the matching
`subtype`) to ship new CC `control_request` subtypes without a code change here.

> **Note.** There is **no** dedicated `set_model`-style method for permission
> mode. To change permission mode mid-session, send `control` (or `config:`)
> with `subtype: "set_permission_mode"` — it falls through to the generic
> control forward (see `handleControl` / `handleConfig` in `handler.go`). The
> dispatched method set is exactly: `start`, `message`, `compact`, `resume`,
> `set_model`, `control`, and `config:<json>`. `interrupt` arrives either as
> `config:` with `subtype:"interrupt"` or as a process SIGINT.

## CLI subcommands (out-of-band)

In addition to the JSON-RPC dispatch loop, the binary supports a few one-shot subcommands invoked via flags:

| Flag | Purpose |
|------|---------|
| `-version` | Print the harness version (currently `0.1.0`) and exit. |
| `-discover [project]` | List known sessions as JSON. Source of truth is `state.db`; on-disk `~/.claude/projects/<project>/*.jsonl` files not yet in `state.db` are cold-imported as synthetic single-rollout sessions. `project` filters to one project dir. |
| `-import-history <session_id> [path]` | Replay a CC session `.jsonl` as NDJSON `msg.Event` lines on stdout. If `path` is omitted, resolves `session_id` via `state.db` (latest rollout in the chain), falling back to a direct filesystem walk for a harness UUID. Exits 0 as a no-op when nothing resolves. |
| `-otel-sidecar` | PTY-mode companion. Starts a standalone OTLP receiver + rollout tailer, prints the receiver URL on stdout line 1 (for `OTEL_EXPORTER_OTLP_ENDPOINT`), and POSTs translated events to `<LLMBRIDGE_BRIDGE_SERVER_URL>/sidecar/event/<bridge_id>`. Exits when its stdin closes. Spawned by bridge-server, not invoked by hand. |

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

### Set Model (control_request)

```json
{
  "type": "control_request",
  "request_id": "ctl-...",
  "request": {
    "subtype": "set_model",
    "model": "sonnet"
  }
}
```

Triggered by the `set_model` JSON-RPC method or the `config:<json>` pass-through.

### Set Permission Mode (control_request)

```json
{
  "type": "control_request",
  "request_id": "ctl-...",
  "request": {
    "subtype": "set_permission_mode",
    "mode": "acceptEdits"
  }
}
```

Triggered by a generic `control` (or `config:<json>`) request carrying
`subtype: "set_permission_mode"` — there is no dedicated JSON-RPC method for it.

### Generic Control (control_request)

The `control` JSON-RPC method accepts any subtype plus an optional payload, allowing the bridge to forward new CC control_request subtypes without requiring a code change here:

```jsonc
// JSON-RPC request from llm-bridge
{"method":"control","params":{"subtype":"some_new_subtype","payload":{"k":"v"}}}
```

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

> **Note on session state.** This harness does *not* emit `session_state`
> events. Session lifecycle (idle / running / completed / error) is derived
> centrally by llm-bridge-server from the raw event stream (init, result,
> error). See `translate.go` — `translateSystem`'s `init` case is explicit
> about this.

#### `session_info` - Session metadata (emitted after each init)

```json
{
  "type": "session_info",
  "info": {
    "working_dir": "/home/user/repo",
    "model": "claude-sonnet-4-20250514",
    "permission_mode": "bypassPermissions",
    "tools": [{"name": "Edit"}, {"name": "Bash"}],
    "slash_commands": ["/review"],
    "agents": ["dagda"],
    "skills": ["frontend-design"],
    "mcp_servers": [{"name": "tool-store", "status": "connected"}],
    "system_prompt": "...",
    "append_system_prompt": "..."
  }
}
```

Emitted right after Claude Code's `system`/`init` event. Most fields come
from CC's init payload (it is the canonical source for tools / slash_commands
/ agents / skills / mcp_servers / model); `system_prompt` and
`append_system_prompt` are backfilled from the `start` request's `StartParams`
because CC never echoes them back. See `parseInitInfo` in `translate.go`.

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

Subtypes: `init`, `compact_boundary`, `api_retry`, `status`, `task_notification`, `task_started`, `task_progress`, `compact_ack`, and any other CC system subtype forwarded as-is. (CC `hook_*` subtypes are emitted as `hook` events, not `system` — see above.)

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
    "code": "SPAWN_ERROR",
    "message": "Claude Code process exited with code 1"
  }
}
```

Error codes emitted by the harness: `SPAWN_ERROR` (CC failed to start) and
`PROCESS_DIED` (CC exited mid-turn without delivering a result).

#### `hook` - Hook lifecycle (with `--include-hook-events`)

```json
{
  "type": "hook",
  "hook": {
    "event": "PreToolUse",
    "tool_name": "Bash",
    "phase": "completed",
    "exit_code": 0,
    "decision": "allow"
  }
}
```

Translated from CC's `system`/`hook_started|hook_progress|hook_response`
events. `phase` is `started` / `progress` / `completed`. These are CC-native
hooks observed in the stream — distinct from the bridge-server PreToolUse
permission hook (see *Permission gating* below).

#### `api_call` - Per-API-call telemetry (via OTel)

```json
{
  "type": "api_call",
  "api_call": {
    "model": "claude-sonnet-4-20250514",
    "input_tokens": 1200,
    "output_tokens": 80,
    "cost_usd": 0.004,
    "duration_ms": 900
  }
}
```

Sourced from the in-process OTLP receiver (`otel.go`), not from stream-json.
Surfaces auxiliary API calls (session-title, prompt-suggestion) that
stream-json hides, which the cost/usage views depend on. Always on for
claudecode harnesses.

## Claude Code stream-json -> msg.Event Mapping

| CC Event Type | CC Subtype/Block | Maps To |
|---------------|-----------------|---------|
| `system` | `init` | `system` (init) + `session_info` |
| `system` | `compact_boundary` | `system` (compact_boundary) |
| `system` | `api_retry` | `system` (api_retry) |
| `system` | `hook_*` | `hook` (EventHook) |
| `system` | `task_started` / `task_progress` | `system` (forwarded, with correlator fields) |
| `system` | `task_notification` / `status` / other | `system` (forwarded) |
| `assistant` | text block | `stream` (text_delta) |
| `assistant` | thinking block | `thinking` + `stream` (thinking_delta) |
| `assistant` | tool_use block | `tool_call` |
| `assistant` | tool_result block | `tool_result` |
| `result` | `success` | `result` |
| `result` | `error_*` | `error` |
| `rate_limit_event` | | `system` (rate_limit) |
| `user` | | consumed internally (echo of synthetic interrupt message) |
| `control_response` | | consumed internally (not forwarded) |
| `keep_alive` | | consumed internally (not forwarded) |
| `tool_progress` | | `system` (tool_progress) |
| *(unknown type)* | | `system` (forwarded for visibility) |

Session state (`running` / `completed` / `error`) is **not** emitted here —
llm-bridge-server derives it centrally from the `init` / `result` / `error`
events above. Per-API-call telemetry (`api_call`) arrives out-of-band via the
OTLP receiver, not from this table.

### Usage Aggregation

Claude Code reports usage per-API-call within `assistant` events and aggregate totals in `result` events:

1. Per-API-call: `assistant.message.usage` has `input_tokens`, `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens`
2. Aggregate: `result.usage` has totals, `result.modelUsage` has per-model breakdown with `contextWindow` and `costUSD`
3. This binary captures per-API-call usage from each assistant event and emits aggregated `TokenUsage` in the result

## Claude Code CLI Flags

### Flags used at session start

Always-on flags:

| Flag | Description |
|------|-------------|
| `-p` | Headless / programmatic mode |
| `--input-format stream-json` | Bidirectional streaming input |
| `--output-format stream-json` | NDJSON streaming output |
| `--verbose` | Include system events in output |

Permission gating:

| Flag | When |
|------|------|
| `--permission-mode bypassPermissions` | default — added whenever `permission_mode` is empty, so CC never prompts and the bridge-server PreToolUse hook is the sole gate |
| `--permission-mode <translated>` | `permission_mode` is set (canonical bridge values are mapped to CC vocab; `plan` maps to CC native plan mode, all other bridge-gating modes map to `bypassPermissions`) |
| `--allowed-tools <t...>` | `allowed_tools` is non-empty — narrows the tool surface; the permission hook still fires on every call |

> **Note.** `--dangerously-skip-permissions` is **not** used. Permission
> gating lives in bridge-server as a PreToolUse HTTP hook injected via
> `--settings`; CC runs with `bypassPermissions` so the hook is the only
> decision point. The legacy `auto_approve` field on `StartParams` is still
> accepted and persisted but no longer selects a flag — use `permission_mode`
> and `allowed_tools` instead. See *Permission gating* in `DESIGN.md`.

Resume / fork:

| Flag | When |
|------|------|
| `--resume <id>` | `resume:true` or `fork:<id>` |
| `--fork-session` | `fork:<id>` |

Per-session flags wired through `StartParams` (each is added only when the corresponding field is non-zero):

| Flag | StartParams field |
|------|-------------------|
| `--session-id <uuid>` | `session_id_override` only — bridge session IDs aren't UUIDs, so by default CC picks its own |
| `--name <name>` | `display_name` (path-like values are skipped) |
| `--model <model>` | env `CLAUDE_MODEL` |
| `--system-prompt <p>` | `system_prompt` |
| `--append-system-prompt <p>` | `append_system_prompt` |
| `--add-dir <dir>` (repeatable) | `add_dirs` |
| `--mcp-config <path>` | `mcp_config` |
| `--strict-mcp-config` | `strict_mcp_config` |
| `--json-schema <schema>` | `json_schema` |
| `--fallback-model <model>` | `fallback_model` |
| `--permission-mode <mode>` | `permission_mode` (acceptEdits / auto / bypassPermissions / default / dontAsk / plan) |
| `--worktree [name]` | `worktree` (`"true"` for auto) |
| `--betas <flag...>` | `betas` |
| `--effort <level>` | `effort` (low / medium / high / xhigh / max) |
| `--max-budget-usd <n>` | `max_budget_usd` |
| `--disallowed-tools <t...>` | `disallowed_tools` |
| `--tools <t...>` | `tools` (`""` disables all, `"default"` enables all) |
| `--disable-slash-commands` | `disable_slash_commands` |
| `--no-session-persistence` | `no_session_persistence` |
| `--include-partial-messages` | `include_partial_messages` |
| `--include-hook-events` | `include_hook_events` |
| `--replay-user-messages` | `replay_user_messages` |
| `--settings <path-or-json>` | `settings` |
| `--setting-sources <a,b>` | `setting_sources` |
| `--plugin-dir <dir>` (repeatable) | `plugin_dirs` |
| `--bare` | `bare` |
| `--agent <name>` | `agent` |
| `--agents <inline-json>` | `agents` |
| `--brief` | `brief` |
| `--file <id:path>` (repeatable) | `files` |
| `--continue` | `continue` |
| `--from-pr <ref>` | `from_pr` |
| `--debug [filter]` | `debug` |
| `--debug-file <path>` | `debug_file` |

### Wiring a new Claude Code flag

If `claude --help` exposes a flag that's not in the table above, wiring it is mechanical: add a field on `StartParams` in `handler.go`, then append it to `extraArgs` in `handleStart`. The canonical list of supported flags is `claude --help`; this README is best-effort.

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
| SIGINT | `Harness.Interrupt`: send an interrupt control_request to CC stdin. If the write fails, fall back to killing the process. |
| SIGTERM | `Harness.Shutdown`: cancel the context, interrupt CC and wait up to 3s (then kill), close `state.db`, exit. No `session_state` event is emitted — bridge-server derives the terminal state. |

## Building

```bash
go build -o llm-bridge-claudecode .
```

The binary should be placed on `$PATH` where the parent llm-bridge process can find it. llm-bridge discovers harness binaries by looking for `llm-bridge-*` executables on `$PATH`.

> **Local-development note.** `go.mod` carries `replace github.com/kayushkin/llm-bridge => ../llm-bridge`, which expects a sibling checkout of [llm-bridge](https://github.com/kayushkin/llm-bridge) on disk. This makes the harness easy to develop alongside the protocol library, but means publishing a tagged release of this repo requires either dropping the `replace` line in favour of a published `llm-bridge` version, or moving the override into a top-level `go.work` file. See *OSS publish blockers* below.

## OSS publish blockers

This repo currently builds and runs against a local sibling checkout of [llm-bridge](https://github.com/kayushkin/llm-bridge). Before tagging a v1 release on GitHub, resolve:

- **`go.mod` `replace` directive.** Drop `replace github.com/kayushkin/llm-bridge => ../llm-bridge` (or move it into a `go.work` overlay that doesn't ship in the tagged module). Without this, `go install github.com/kayushkin/llm-bridge-claudecode@latest` will fail for downstream consumers because the relative path doesn't exist in their tree.
- **Tagged `llm-bridge` version.** The `require` line currently pins `v0.0.0`; bump it to a real published `llm-bridge` tag at the same time the `replace` line is removed.

Both changes are coordinated across every `llm-bridge-*` harness, so they're tracked together rather than per-repo.

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
