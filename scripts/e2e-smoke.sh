#!/usr/bin/env bash
# Boot-and-answer smoke test for llm-bridge-claudecode.
#
# Builds the harness from THIS checkout and proves the resulting binary can
# actually BOOT and ANSWER. `go build` passing proves nothing of the sort: a
# harness that panics on startup is exactly as dead as a server that does — it
# just fails at session-spawn time instead of at boot, where nobody is looking.
#
# What this harness's -discover does:
#   -discover walks CC's rollout tree at $HOME/.claude/projects/<proj>/<uuid>.jsonl,
#   cold-imports every .jsonl it has not seen into state.db, and emits one
#   msg.StoredSession per session. So it BOTH reads the filesystem and WRITES
#   SQLite — which is why the HOME sandbox below is load-bearing rather than
#   hygiene: run unsandboxed, this binary cold-imports into the live 9.8 MB
#   session state.db that every running Claude Code session writes to.
#
# Three things are asserted, because a harness CLI has three entrypoints and a
# green build covers none of them:
#
#   1. -discover      emits a well-formed JSON ARRAY on stdout and exits 0.
#                     This is the contract bridge-server parses at boot; `null`
#                     (a nil slice encoded straight out) or a stray log line on
#                     stdout would both break it while compiling perfectly.
#   2. the main loop  boots the production JSON-RPC path and shuts down cleanly when stdin closes.
#   3. the sandbox    is honoured — see below.
#
# HERMETICITY IS AN ASSERTION HERE, NOT A PRECAUTION.
# The harness resolves its own state and session paths from $HOME. This smoke
# points HOME at a throwaway directory and then CHECKS that the harness wrote
# there and not to the live tree — an audit that can damage what it audits is
# worse than no audit. PATH is curated down to the system dirs for the same
# reason: several harnesses exec their upstream CLI (claude), and a smoke
# whose result depends on which CLIs happen to be installed on the box is not a
# guard, it is a coin flip.
#
# Exits 0 on success, non-zero on the first failing assertion.
#
# Tunables:
#   E2E_KEEP  — set to "1" to leave $TMP_DIR around after the run

set -euo pipefail

REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
BIN_NAME="llm-bridge-claudecode"

for tool in go jq timeout; do
  if ! command -v "$tool" >/dev/null 2>&1; then
    echo "ERROR: required tool '$tool' not found on PATH" >&2
    exit 2
  fi
done

TMP_DIR="$(mktemp -d -t claudecode-e2e.XXXXXX)"
BIN_DIR="$TMP_DIR/bin"
# The harness's $HOME for the whole run. Everything it persists must land here.
SANDBOX_HOME="$TMP_DIR/home"
BIN="$BIN_DIR/$BIN_NAME"
mkdir -p "$BIN_DIR" "$SANDBOX_HOME"

# The live state.db this harness would open if the sandbox leaked. Captured
# BEFORE anything runs so we can prove at the end that we never touched it.
# In the guard's clean-clone environment HOME is already scratch and this file
# does not exist; when a human runs this smoke by hand it is the real one, and
# that is exactly the case worth protecting.
LIVE_STATE="$HOME/.local/share/$BIN_NAME/state.db"
LIVE_STATE_BEFORE=""
[ -f "$LIVE_STATE" ] && LIVE_STATE_BEFORE="$(sha256sum "$LIVE_STATE" | cut -d' ' -f1)"

check_live_state_untouched() {
  # Runs on EVERY exit path, including the failing ones. An assertion that only
  # runs on success cannot tell you that the run which just failed also
  # corrupted your live database on its way out.
  [ -n "$LIVE_STATE_BEFORE" ] || return 0
  local after
  after="$(sha256sum "$LIVE_STATE" | cut -d' ' -f1)"
  if [ "$LIVE_STATE_BEFORE" != "$after" ]; then
    echo "" >&2
    echo "!!! THIS SMOKE MODIFIED THE LIVE DATABASE $LIVE_STATE" >&2
    echo "!!! The HOME sandbox leaked: $BIN_NAME resolved its state path from" >&2
    echo "!!! somewhere other than \$HOME. Do not ignore this — every running" >&2
    echo "!!! session writes to that file." >&2
    return 1
  fi
  return 0
}

cleanup() {
  local status=$?
  check_live_state_untouched || status=1
  if [ "${E2E_KEEP:-}" = "1" ]; then
    echo "[e2e] keeping $TMP_DIR"
  else
    rm -rf "$TMP_DIR"
  fi
  return "$status"
}
trap cleanup EXIT INT TERM

step() { printf '\n==> %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }

# Run the harness under test: sandboxed HOME, curated PATH, nothing inherited.
# `env -i` is the point — an ambient PATH would let the binary reach the real
# installed upstream CLIs, and an ambient HOME would let it reach the live DB.
#
# The timeout is not belt-and-braces either: a harness that HANGS instead of
# answering is a live failure mode (it would wedge a session spawn exactly the
# same way), and without this the nightly guard would just burn its job budget
# and die opaquely instead of naming the harness that stopped answering.
# `timeout` reports 124 on expiry, which the assertions below check for by name.
HARNESS_TIMEOUT="${HARNESS_TIMEOUT:-60}"
run_harness() {
  timeout "$HARNESS_TIMEOUT" env -i \
    HOME="$SANDBOX_HOME" \
    PATH="/usr/bin:/bin" \
    "$BIN" "$@"
}

step "build $BIN_NAME from $REPO_DIR"
cd "$REPO_DIR"
# Default flags, cgo off. Every harness in the fleet is pure Go (modernc SQLite
# where it needs SQLite at all), so if a cgo-dependent driver is ever pulled in,
# this build fails HERE rather than shipping a binary that compiles green and
# then dies opening its own database — which is precisely how noteboard and
# marginalia both shipped unbootable binaries for months.
CGO_ENABLED=0 go build -o "$BIN" .
echo "    binary: $BIN ($(ls -lh "$BIN" | awk '{print $5}'))"

step "-discover emits a well-formed JSON array"
DISCOVER_OUT="$(run_harness -discover 2>"$TMP_DIR/discover.err")" \
  || fail "-discover exited non-zero: $(cat "$TMP_DIR/discover.err")"
# jq parses the WHOLE of stdout, so this also catches a harness that logs to
# stdout: one stray line and the discover payload bridge-server reads is no
# longer parseable JSON, however green the build was.
DISCOVER_TYPE="$(jq -r 'type' <<<"$DISCOVER_OUT" 2>/dev/null)" \
  || fail "-discover did not emit parseable JSON on stdout: $DISCOVER_OUT"
[ "$DISCOVER_TYPE" = "array" ] \
  || fail "-discover emitted a JSON $DISCOVER_TYPE, want an array. A nil slice encodes as 'null', which is NOT an empty array — bridge-server's discover contract is an array. Got: $DISCOVER_OUT"
echo "    array of $(jq -r 'length' <<<"$DISCOVER_OUT")"

step "state.db landed inside the sandbox — HOME is honoured"
# ORDER MATTERS: this runs against the EMPTY sandbox, before any fixture is
# seeded below. That is deliberate and was paid for. If DefaultStatePath() ever
# stops resolving from $HOME, the -discover above opens the LIVE state.db — and
# had we seeded a rollout first, it would have cold-imported that fixture
# straight into the database every running session writes to. Detecting the leak
# after the write is not good enough; on an empty HOME there is nothing to
# import, so the leak is caught while it is still harmless.
SANDBOX_DB="$SANDBOX_HOME/.local/share/$BIN_NAME/state.db"
[ -f "$SANDBOX_DB" ] \
  || fail "expected $BIN_NAME to create its state.db under the sandboxed HOME at $SANDBOX_DB — it did not, so either DefaultStatePath() ignores HOME (in which case -discover just opened the LIVE database) or the SQLite open/migrate failed silently"
echo "    $SANDBOX_DB ($(ls -lh "$SANDBOX_DB" | awk '{print $5}'))"

step "seed a Claude Code rollout and prove -discover cold-imports it"
# The filename stem IS the harness session id; the head of the file supplies the
# prompt. Both come back out through state.db, so a round-trip here exercises
# the filesystem walk, the SQLite write, and the rollout parse in one shot —
# an empty array alone would also be returned by a discover that had silently
# stopped working.
SEED_UUID="11111111-2222-3333-4444-555555555555"
SEED_PROMPT="e2e smoke seeded prompt"
mkdir -p "$SANDBOX_HOME/.claude/projects/-tmp-e2e-smoke"
cat >"$SANDBOX_HOME/.claude/projects/-tmp-e2e-smoke/$SEED_UUID.jsonl" <<EOF
{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"$SEED_PROMPT"}}
{"type":"assistant","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","content":"ok"}}
EOF

SEEDED="$(run_harness -discover)" || fail "-discover failed after seeding a rollout"
[ "$(jq -r 'length' <<<"$SEEDED")" = "1" ] \
  || fail "expected exactly the 1 seeded session, got: $SEEDED"
[ "$(jq -r '.[0].harness_session_id' <<<"$SEEDED")" = "$SEED_UUID" ] \
  || fail "harness_session_id did not round-trip through the cold import: $SEEDED"
# A cold-imported session has no bridge-minted chain head yet, so the harness
# synthesises one equal to the harness id. If these ever diverge here, the
# synthetic-chain contract in coldImportRollouts has broken.
[ "$(jq -r '.[0].bridge_session_id' <<<"$SEEDED")" = "$SEED_UUID" ] \
  || fail "cold-imported bridge_session_id should equal the harness id: $SEEDED"
[ "$(jq -r '.[0].prompt' <<<"$SEEDED")" = "$SEED_PROMPT" ] \
  || fail "prompt was not parsed out of the rollout head: $SEEDED"
echo "    cold-imported $SEED_UUID with its prompt"

step "the production JSON-RPC loop boots and shuts down cleanly on EOF"
# -discover is a side path. THIS is the entrypoint bridge-server actually
# spawns: no args, canonical messages over stdin, msg.Events back on stdout.
# Booting it with stdin already closed exercises config load, state open and the
# read loop, then expects a clean shutdown — a panic in any of them shows up
# here as a non-zero exit, and nowhere else.
set +e
run_harness </dev/null >"$TMP_DIR/mainloop.out" 2>"$TMP_DIR/mainloop.err"
MAIN_RC=$?
set -e
if [ "$MAIN_RC" = "124" ]; then
  echo "----- stderr -----" >&2; cat "$TMP_DIR/mainloop.err" >&2
  fail "the main JSON-RPC loop HUNG (>${HARNESS_TIMEOUT}s) on stdin EOF instead of shutting down. bridge-server would hang the same way on session spawn."
fi
if [ "$MAIN_RC" != "0" ]; then
  echo "----- stderr -----" >&2; cat "$TMP_DIR/mainloop.err" >&2
  fail "the main JSON-RPC loop exited $MAIN_RC on stdin EOF, want 0 — the harness cannot boot"
fi
echo "    booted and shut down cleanly"

step "-version answers"
VERSION_OUT="$(run_harness -version)" || fail "-version exited non-zero"
[ -n "$VERSION_OUT" ] || fail "-version printed nothing"
echo "    version: $VERSION_OUT"

step "confirm the run never touched the live state"
# check_live_state_untouched also runs from the cleanup trap on every failing
# path; calling it here too means a clean run SAYS so rather than staying silent
# about the one property this smoke most needs to hold.
check_live_state_untouched || exit 1
if [ -n "$LIVE_STATE_BEFORE" ]; then
  echo "    live state.db untouched (verified by checksum)"
else
  echo "    no live state.db on this box to protect (already a scratch HOME)"
fi

step "SUCCESS — $BIN_NAME boots and answers"
