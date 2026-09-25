#!/usr/bin/env bash
# Runs the canaries that need the real claude binary, weekly from scheduler job
# "claude-code notice-hold canary". Today that is one: whether Claude Code still
# holds a background task's notice behind a foreground watcher of its output
# file, and whether stop_task still frees the watcher (finished_task_waiters.go).
#
# A failure is carried as ONE standing noteboard todo tagged
# claude-code-canary, rewritten in place each failing week; a pass does not
# close it, because "Claude Code changed" needs a person to act on it either way.
set -uo pipefail

export PATH="$HOME/.local/share/mise/shims:$PATH"
cd "$(dirname "$0")/.."

output="$(LLM_BRIDGE_CLAUDECODE_LIVE_CLAUDE_CANARY=1 go test -count=1 -timeout 8m -v \
  -run 'TestLiveClaudeCodeStillHoldsNoticeBehindFileWatcher' . 2>&1)"
status=$?
echo "$output"
[ "$status" -eq 0 ] && exit 0

NOTEBOARD_URL="${NOTEBOARD_URL:-http://localhost:8191}"
payload="$(OUTPUT="$output" python3 -c 'import json,os
out=os.environ["OUTPUT"]
body=("The weekly live canary in llm-bridge-claudecode failed.\n\n"
"- **CLAUDE CODE CHANGED**: Claude Code no longer holds a background task notice behind a file watcher. "
"Check it, then remove finishedTaskWaiterStopper (finished_task_waiters.go) and its canary.\n"
"- **STOPPER BROKEN**: stop_task no longer frees the watcher. Fix the stopper.\n"
"- **INCONCLUSIVE**: the model did not build the test scenario. Rerun scripts/live-claude-canary.sh.\n\n"
"```\n"+out[-6000:]+"\n```\n")
print(json.dumps({"type":"todo","title":"Claude Code notice-hold canary failed (llm-bridge-claudecode)",
"tags":["claude-code-canary","llm-bridge-claudecode"],"priority":2,"body":body}))')"
existing_id="$(curl -sf "$NOTEBOARD_URL/api/items?type=todo&status=open&tag=claude-code-canary&limit=5" | python3 -c 'import sys,json
items=json.load(sys.stdin); items=items if isinstance(items,list) else items.get("items",[])
print(items[0]["id"] if items else "")')"
if [ -n "$existing_id" ]; then
  curl -sfS -X PATCH "$NOTEBOARD_URL/api/items/$existing_id" -H 'Content-Type: application/json' -d "$payload" >/dev/null \
    && echo "noteboard todo $existing_id rewritten" || echo "ERROR: could not rewrite noteboard todo $existing_id" >&2
else
  curl -sfS -X POST "$NOTEBOARD_URL/api/items" -H 'Content-Type: application/json' -d "$payload" \
    | python3 -c 'import sys,json; print("noteboard todo", json.load(sys.stdin)["id"], "filed")' \
    || echo "ERROR: could not file the noteboard todo" >&2
fi
exit "$status"
