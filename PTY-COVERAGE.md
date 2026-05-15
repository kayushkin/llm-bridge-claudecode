# PTY-mode coverage vs `-p` / stream-json mode

What every PTY session emits as `msg.Event` after the OTel sidecar + rollout
tailer wiring lands, compared to the baseline `-p`/stream-json path that
`translate.go` produces.

Sources for PTY:
- **OTel**: the per-session OTLP sidecar (otel.go). Carries metadata only —
  no content / args / output bodies.
- **Rollout**: the per-session JSONL tailer (rollout.go). Carries full
  conversation content.
- **Prehook**: bridge-server's PreToolUse HTTP hook (independent of either).

| `-p` emits                                | PTY coverage                                    | Notes                                                                                |
| ----------------------------------------- | ----------------------------------------------- | ------------------------------------------------------------------------------------ |
| `EventSystem` subtype=`init`              | **gap**                                         | Rollout writes session/init metadata as its first entries; translator drops them.    |
| `EventSystem` `rate_limit_event`          | **gap**                                         | Neither OTel nor rollout surfaces this today.                                        |
| `EventSystem` `tool_progress`             | gap                                             | Same — claude renders to pty, no out-of-band signal.                                 |
| `EventHook` PreToolUse / PostToolUse      | ✅ prehook                                       | Bridge-server's hook flow is mode-agnostic; PTY hits the same endpoint.              |
| `EventBlock` text                         | ✅ rollout                                       | Granular per-block emission.                                                         |
| `EventBlock` thinking → `EventThinking`   | ✅ rollout                                       | Mapped to `EventThinking`.                                                           |
| `EventToolCall`                           | ✅ rollout (full args) + OTel (name+decision)    | Rollout gives args; OTel gives accept/deny decision.                                 |
| `EventToolResult`                         | ✅ rollout (full output) + OTel (success flag)   | Rollout pairs ToolID; OTel doesn't, so OTel rows render standalone.                  |
| `EventResult` terminal (per turn)         | ✅ rollout (synthesized)                         | Emitted when assistant `stop_reason` ∈ end_turn/stop_sequence/max_tokens.            |
| `EventError`                              | ✅ OTel `internal_error`                         | Plus EventError from any `claude_code.api_error` (would need translator extension).  |
| `user_message`                            | ✅ rollout + OTel `user_prompt`                  | Both fire; overlap is the dedup target.                                              |
| Per-API-call usage (auxiliary calls!)     | ✅ OTel `api_call` + derived `api_spend_total`   | TUI **surfaces more** than `-p` here — auxiliary calls are visible.                  |
| Real-time text streaming (token deltas)   | **gap** (irreducible)                           | Rollout writes complete blocks; no partial-token signal exists outside `-p`'s stream.|

## Known gaps after this round

1. ~~No `EventResult` per turn.~~ **Closed.** translateRolloutEntry
   synthesizes an `EventResult` after the block events when the
   assistant message's `stop_reason` is terminal (end_turn /
   stop_sequence / max_tokens). Usage is the final API call's only
   (under-counts multi-roundtrip turns) — acceptable since OTel
   api_spend_total is the canonical PTY cost signal; this exists to
   drive the derivation state machine.
2. **No session `init` info.** Rollout's first entry is metadata
   (working dir, model, tools, MCP servers). Translator should
   recognize it and emit `EventSessionInfo`.
3. **Compaction / fork rotation.** Tailer follows the original `.jsonl`
   forever even if claude rotates to a new file mid-session.
4. **Token-by-token streaming.** Genuinely irreducible — only `-p
   --include-partial-messages` exposes deltas. PTY only has whole blocks
   from the rollout.
5. **Rollout vs OTel user-prompt overlap.** Both will emit a user_message
   for the same input. UI dedup decision pending.
