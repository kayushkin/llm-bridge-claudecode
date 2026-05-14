#!/usr/bin/env bash
# End-to-end smoke for the OTel receiver: builds the harness, spawns it
# with a single "start" request, and verifies that EventAPICall events
# (with Extensions.source = "otel") appear on stdout — proving CC's
# telemetry made it through the per-process OTLP receiver into the
# canonical msg.Event stream.
#
# Requires `claude` on PATH and either subscription auth or an API key.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORKDIR=$(mktemp -d -t cc-otel-smoke.XXXXXX)
trap "rm -rf $WORKDIR" EXIT

cd "$ROOT"
echo "==> building llm-bridge-claudecode"
go build -o "$WORKDIR/llm-bridge-claudecode" .

START_REQ=$(python3 -c "
import json, uuid
print(json.dumps({
    'method': 'start',
    'params': {
        'bridge_session_id': str(uuid.uuid4()),
        'display_name': 'otel-smoke',
        'agent_id': 'smoke',
        'prompt': 'Reply with exactly the word: pong',
        'work_dir': '$WORKDIR',
    },
}))
")

echo "==> spawning harness with start request"
STDOUT="$WORKDIR/stdout.ndjson"
STDERR="$WORKDIR/stderr.log"

# The harness reads JSON-RPC requests from stdin one line at a time and
# emits msg.Event NDJSON on stdout. Send the start request, then close
# stdin so the harness shuts CC down once the turn completes.
(echo "$START_REQ"; sleep 25) | "$WORKDIR/llm-bridge-claudecode" >"$STDOUT" 2>"$STDERR" || true

echo "==> stdout lines: $(wc -l < "$STDOUT")"
echo "==> stderr tail:"
tail -10 "$STDERR" | sed 's/^/    /'

# Count event types observed
echo "==> event types:"
python3 -c "
import json, sys, collections
counts = collections.Counter()
api_calls = []
for line in open('$STDOUT'):
    line = line.strip()
    if not line: continue
    try:
        ev = json.loads(line)
    except json.JSONDecodeError:
        continue
    counts[ev.get('type', '?')] += 1
    if ev.get('type') == 'api_call':
        api_calls.append(ev)
for t, n in counts.most_common():
    print(f'    {t:20} {n}')
print()
print(f'    api_call events: {len(api_calls)}')
for ev in api_calls:
    ac = ev.get('api_call', {})
    src = ev.get('extensions', {}).get('source')
    print(f'      model={ac.get(\"model\"):30} tokens={ac.get(\"input_tokens\"):>4}/{ac.get(\"output_tokens\"):<4} cost=\${ac.get(\"cost_usd\"):.6f} qsrc={ac.get(\"query_source\"):25} source={src}')
"

# Verify at least one OTel-sourced api_call event arrived
if ! grep -q '\"api_call\":' "$STDOUT"; then
    echo "FAIL: no api_call events captured"
    exit 1
fi
if ! grep -q '\"source\":\"otel\"' "$STDOUT"; then
    echo "FAIL: api_call events did not carry source=otel provenance"
    exit 1
fi

echo "==> OK"
