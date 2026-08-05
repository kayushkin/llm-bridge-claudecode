# Dropped OTel events, lost final messages, and stuck turns

Status: **merged to `main` and deployed** (2026-08-05)
Repo: `llm-bridge-claudecode`
Branch: `otel-surface-dropped-errors`, later folded into `feat/subagent-session-demux` and merged
Commits: `54001e0`, `954c50c` (on top of `2a99c97`)
Origin: session "Dash session/todo linker" (`br_1784567616625538966`, harness `0764e87b-6abc-468d-8d3b-774cb377fb46`)
Related noteboard todo: `a367a8d1-0c75-45f9-adf3-476a1566831e` (marked done, holds the same summary)

---

## The issue

A chat session finished its turn but the UI never showed the final assistant
message — it sat looking like it was "waiting for some action to finish," and
only when the user sent another message did the buffered answer flush.

Two independent defects, both in this adapter:

1. **CC emits assistant text over OpenTelemetry, and the adapter dropped it.**
   In `-p` mode Claude Code sends each model text segment on *two* channels:
   stream-json stdout (the authoritative copy the adapter translates into
   content blocks) and an OTel log event `claude_code.assistant_response`. The
   OTel receiver (`otel.go` `dispatchLogRecord`) had no case for that event, so
   it hit the `default:` branch, was logged as `[otel] unhandled ...` at info
   level, and discarded. When a turn's stream-json copy went missing (see #2),
   the OTel copy was the only surviving copy — and it was thrown away. Nothing
   reached log-store or the UI; the text lived only in CC's private rollout file
   and one throwaway journal line.

   The same `default:` branch was also swallowing genuine error telemetry:
   `claude_code.api_error` and `claude_code.api_retries_exhausted` (an
   overloaded API, 529, retries exhausted) — so an API failure surfaced *nowhere*.
   Scope over ~3 weeks of journal: `assistant_response` dropped 10,617 times
   across 785 sessions; `api_error` 623; `api_retries_exhausted` 623;
   `subagent_completed` 107.

2. **The turn loop could hang forever.** `handler.go drainUntilResult()` returned
   only on a stream-json `result`/`error` event. If the CC process stayed alive
   but stopped emitting on stdout (which is what happened here), the loop blocked
   on `for raw := range h.events` indefinitely and the session stayed
   `tool_running` until it was reaped or aborted. The final message was never
   delivered and no error was raised.

---

## What changed

All changes are in `llm-bridge-claudecode`. **No frontend or bridge-server code
was changed** — but the set of events the frontend receives has grown (see the
consumer contract below).

### Commit `54001e0` — surface the dropped error/status events
`otel.go`: added cases so these OTel events become real `msg.Event`s instead of
being swallowed:

| OTel event | now becomes | notes |
|---|---|---|
| `claude_code.api_error` | `EventError` code `api_error` | `Retryable` true for 429/5xx; `StatusCode` passed through |
| `claude_code.api_retries_exhausted` | `EventError` code `api_retries_exhausted` | terminal, `Retryable` false |
| `claude_code.subagent_completed` | `EventSystem` subtype `subagent_completed` | `Message` = agent type |

Benign/lifecycle events (`mcp_server_connection`, `at_mention`,
`skill_activated`, `auth`) are now named in explicit skip cases so the remaining
`default: log unhandled` line is a real "never seen this" alarm rather than
~10k lines of noise.

### Commit `954c50c` — recover the message, watchdog the hang
- **assistant_response recovery.** `otel.go` maps `claude_code.assistant_response`
  to a `source=otel` text block. `handler.go Harness.emit` forwards the
  stream-json copy and **buffers** the OTel copy; `flushRecoveredAssistant`
  surfaces the buffer at turn end **only if stream-json produced no assistant
  text that turn**. Healthy turns drop the buffer (no double-render); a dropped
  turn gets its message back, tagged `recovered=true`. The duplicate is
  suppressed at the source, so **no render-edge dedup is required**. (Verified
  the OTel copy is faithful, not truncated: `response_length` in the wild maxes
  ~700 with no cap cluster. PTY mode is unaffected — its sidecar forwards the
  OTel copy directly, as it's the only copy there.)
- **Turn idle watchdog.** `drainUntilResult` now `select`s on an idle ticker.
  If the process is alive but no event (stream-json *or* OTel) has flowed for
  `Config.TurnIdleTimeout`, it flushes any recovered text, emits an `EventError`
  code `TURN_IDLE_TIMEOUT`, and kills the wedged process (the next message
  respawns via `--resume`, reloading CC's rollout — no work lost).
  Default 5 minutes; env `CLAUDECODE_TURN_IDLE_TIMEOUT_SEC` (0 disables).
- Supporting: `emitEvent` is a swappable package var (test seam);
  `CCProcess.Kill` is nil-safe; watchdog interval adapts to short timeouts.

Files touched: `otel.go`, `handler.go`, `config.go`, `main.go`, `process.go`,
`otel_test.go`, `handler_recovery_test.go`. Tests are race-clean and cover
recover-on-silence, no-double-render-on-healthy-turn, watchdog-unblocks, and
watchdog-recovers-final-message.

---

## Consumer contract — what dash / dashv2 must handle

These events already fit the existing `msg.Event` wire shape; the point is that
some are **new or newly-frequent** and the frontend should render them sensibly.
Every OTel-derived event carries `extensions.source = "otel"`.

### 1. Recovered assistant text — do NOT dedupe or hide it
A normal assistant text block with two extra extension flags:
```json
{
  "type": "block",
  "block": { "index": 0, "block": { "type": "text", "text": "…final message…" } },
  "extensions": { "source": "otel", "recovered": true }
}
```
- Render it as normal assistant text. The backend only emits it when the live
  stream produced nothing, so **there is no duplicate to suppress** — do not add
  frontend dedup that would drop it.
- Optional nicety: when `extensions.recovered == true`, show a subtle marker
  (e.g. "recovered after a stream interruption"). Do not gate visibility on it.

### 2. New error events — surface them and clear the "running" state
```json
{ "type": "error", "error": { "code": "TURN_IDLE_TIMEOUT", "message": "…" } }
{ "type": "error", "error": { "code": "api_error", "message": "Overloaded", "retryable": true, "status_code": 529 }, "extensions": { "source": "otel" } }
{ "type": "error", "error": { "code": "api_retries_exhausted", "message": "Overloaded", "status_code": 529 }, "extensions": { "source": "otel" } }
```
- `TURN_IDLE_TIMEOUT` is a **turn terminator** — treat it like `result`/error:
  transition the session out of `tool_running`/"waiting". This is the fix for the
  spinner-that-never-ends. `PROCESS_DIED` (pre-existing) behaves the same way.
- `api_error` / `api_retries_exhausted` are informational failures (the model
  call was overloaded); show them as a warning/error chip. `retryable` and
  `status_code` are available for styling.

### 3. New system event — subagent progress
```json
{ "type": "system", "system": { "subtype": "subagent_completed", "message": "custom" }, "extensions": { "source": "otel" } }
```
- Optional: show "subagent finished" progress on a turn that spawned Task-tool
  subagents. Safe to ignore if unstyled — just don't treat an unknown
  `system.subtype` as an error.

### General rule for dashv2
Handle unknown `error.code` and `system.subtype` values gracefully (generic
render), and never let an OTel-sourced (`extensions.source == "otel"`) event be
silently dropped by a stricter allowlist — that is precisely the class of bug
this work fixed on the backend.

---

## Deploy

Backend: review the branch, then build + restart `llm-bridge.service`. The
recovery and watchdog take effect for new sessions after restart. `dash` /
`dashv2` need no change to *function* (the recovered block and new errors render
under existing generic handling), but the three items above make them render
*well* and, for `TURN_IDLE_TIMEOUT`, correctly clear the stuck state.

### What actually happened, 2026-08-05

Only the adapter half shipped, and that is worth recording because the gap was
invisible from either side alone.

`~/bin/llm-bridge-claudecode` was rebuilt from this branch (`b3ab3d4`) while
`/usr/local/bin/llm-bridge` stayed on `main`, which had no subagent handling at
all — no `subagent.go`, no consumer of `task_started` / `task_notification`.
The adapter was emitting `harness_parent_id` and task frames to a server that
read neither. Three noteboard todos recorded the work as "deployed to
production, verified end-to-end"; it was not.

Building from a branch also silently reverted the running harness: the deployed
binary predated `0f702c9`, `009042f` and `4713a61`, so it was missing the
resume-working-directory fix, the WAL-conversion wait and the session-config
application. Merging both directions before deploying is what closes that.

Section 3 above ("subagent progress") said the frontend could safely ignore an
unstyled `system.subtype`. That was true of `subagent_completed`, and false of
`task_notification` — it is the only event that ever says a subagent finished,
because a subagent emits no result of its own. Ignoring it is precisely what
left tasks looking unfinished forever. See `msg.SystemEvent.TaskStatus`.
