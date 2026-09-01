#!/usr/bin/env bash
set -euo pipefail

# Re-exec inside a fresh systemd transient unit so the deploy survives
# `systemctl restart llm-bridge.service`. When the deploy is triggered by
# an agent running inside llm-bridge.service (this harness is spawned as a
# subprocess of that service), the agent's bash is in the service's cgroup;
# `setsid nohup` does NOT escape systemd's control-group kill, so the
# restart takes the deploy with it. A transient unit lives in its own
# cgroup under system.slice and is untouched by the service restart.
if [ -z "${DEPLOY_DETACHED:-}" ]; then
  # Log lives under $HOME (not /tmp) because systemd transient units get a
  # PrivateTmp namespace, so the unit can't write to the host's /tmp.
  LOG="$HOME/.cache/llm-bridge-claudecode-deploy.log"
  mkdir -p "$(dirname "$LOG")"
  : >"$LOG"
  UNIT="llm-bridge-claudecode-deploy-$$.service"
  # Resolve $0 to an absolute path — the transient unit doesn't inherit our
  # working directory, so a relative ./deploy.sh would fail to find itself.
  SCRIPT="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
  sudo systemd-run \
    --collect \
    --unit="$UNIT" \
    --description="llm-bridge-claudecode deploy ($USER)" \
    --uid="$(id -u)" \
    --gid="$(id -g)" \
    --setenv=DEPLOY_DETACHED=1 \
    --setenv=HOME="$HOME" \
    --setenv=PATH="$PATH" \
    --property=StandardOutput=append:"$LOG" \
    --property=StandardError=append:"$LOG" \
    bash "$SCRIPT" "$@" >/dev/null
  echo "detached deploy (unit=$UNIT), tail -f $LOG"
  echo "  status: systemctl status $UNIT"
  echo "  logs:   journalctl -u $UNIT -f"
  exit 0
fi

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_NAME="llm-bridge-claudecode"
USER_BIN="$HOME/bin/$BIN_NAME"
SERVICE="llm-bridge.service"

cd "$REPO_DIR"

# Add go to PATH if managed by mise
export PATH="$HOME/.local/share/mise/shims:$PATH"

# ---------------------------------------------------------------------------
# Ancestry guard: refuse a build whose commit is missing default-branch work.
#
# Paid for twice. On 2026-08-31 and again on 2026-09-01, an agent built this
# binary from a checkout parked on a side branch forked BEFORE main's
# 2026-08-11 unprompted-turn fix (f3a589c), and installed it. Both times every
# session that then spawned a background subagent stranded in model_generating
# with its final response undelivered, and both times the tree LOOKED fine —
# the build succeeded, the service answered, the nightly guard was hours away.
#
# So the check is: the commit being deployed must contain everything the
# default branch has. Not "must BE main" — deploying a feature branch that has
# MERGED main in is fine, and is exactly how the 2026-09-01 repair shipped.
# The default branch is resolved the same way the repo-deploy guard resolves
# it (origin/HEAD, else main, else master), so this script and that guard can
# never call different things stale.
#
# --allow-unmerged skips the refusal for a deliberate, named exception —
# printing loudly what is being skipped, because the silent version of this
# hatch is just the bug with extra steps.
#
# ⚠️ This guard only binds deploys that go THROUGH this script. Both incidents
# were hand-typed `go build && install` — which is why the deploy-drift judge
# now also checks the RUNNING binary's ancestry every morning. This guard
# closes the front door; the judge watches the window.
# ---------------------------------------------------------------------------
resolve_default_branch() {
  local name
  name="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null)"
  name="${name#origin/}"
  if [ -n "$name" ] && git rev-parse --verify --quiet "refs/heads/$name" >/dev/null; then
    echo "$name"; return
  fi
  for name in main master; do
    if git rev-parse --verify --quiet "refs/heads/$name" >/dev/null; then
      echo "$name"; return
    fi
  done
  echo ""
}

echo "==> Checking ancestry against the default branch..."
DEFAULT_BRANCH="$(resolve_default_branch)"
if [ -z "$DEFAULT_BRANCH" ]; then
  echo "ERROR: no default branch resolvable (no origin/HEAD, no main, no master);" >&2
  echo "       cannot establish that this deploy loses nothing. Refusing." >&2
  exit 1
fi
MISSING="$(git rev-list --count HEAD.."refs/heads/$DEFAULT_BRANCH")"
if [ "$MISSING" -gt 0 ]; then
  if [ "${1:-}" = "--allow-unmerged" ]; then
    echo "    ⚠️  DEPLOYING ANYWAY (--allow-unmerged): HEAD is missing $MISSING commit(s)"
    echo "        that $DEFAULT_BRANCH has:"
    git log --oneline HEAD.."refs/heads/$DEFAULT_BRANCH" | sed 's/^/        /'
  else
    echo "REFUSING TO DEPLOY: HEAD is missing $MISSING commit(s) that $DEFAULT_BRANCH has:" >&2
    git log --oneline HEAD.."refs/heads/$DEFAULT_BRANCH" | sed 's/^/    /' >&2
    echo "" >&2
    echo "A binary built here would UNDO that work for every session — this exact" >&2
    echo "shape stranded user sessions on 2026-08-31 and 2026-09-01 (the parked" >&2
    echo "llm-bridge-claudecode branch missing the unprompted-turn fix)." >&2
    echo "Merge $DEFAULT_BRANCH into this branch first (git merge $DEFAULT_BRANCH)," >&2
    echo "or pass --allow-unmerged if losing it is genuinely intended." >&2
    exit 1
  fi
fi
echo "    HEAD contains all of $DEFAULT_BRANCH"

echo "==> Building $BIN_NAME..."
go build -o "$BIN_NAME" .
echo "    built: $(ls -lh "$BIN_NAME" | awk '{print $5}')"

# The build must be traceable to a commit, and that commit must be the HEAD
# the ancestry check above just cleared — a dirty tree builds a binary whose
# source is not recoverable from any commit, which is how a fix "ships"
# without existing anywhere reviewable.
BUILD_REV="$(go version -m "$BIN_NAME" | awk -F= '$1 ~ /[[:space:]]vcs\.revision$/ {print $2}')"
BUILD_DIRTY="$(go version -m "$BIN_NAME" | awk -F= '$1 ~ /[[:space:]]vcs\.modified$/ {print $2}')"
if [ -z "$BUILD_REV" ]; then
  echo "ERROR: the built binary carries no vcs.revision (built from a worktree," >&2
  echo "       or from a file list). Nothing ties it to a commit. Refusing." >&2
  exit 1
fi
echo "    vcs.revision=$BUILD_REV"
if [ "$BUILD_DIRTY" = "true" ]; then
  echo "    ⚠️  WARNING: built from a DIRTY tree (vcs.modified=true). $BUILD_REV names"
  echo "        the commit this was built NEAR, not the source it was built FROM."
fi

echo "==> Installing binary to $USER_BIN..."
mkdir -p "$(dirname "$USER_BIN")"
install -m 0755 "$BIN_NAME" "$USER_BIN"

# Restart llm-bridge.service so running harness subprocesses are dropped and
# the next spawn picks up the new binary. The transient unit we re-exec'd
# into above is in a different cgroup, so this restart does not kill us.
echo "==> Restarting $SERVICE..."
sudo systemctl restart "$SERVICE"

echo "==> Verifying..."
sleep 2
if ! systemctl is-active --quiet "$SERVICE"; then
  echo "ERROR: $SERVICE failed to start"
  journalctl -u "$SERVICE" -n 15 --no-pager 2>&1
  exit 1
fi
echo "    $SERVICE is running"

# HTTP up — the listener is bound and serving.
if ! curl -fsS http://localhost:8160/sessions >/dev/null 2>&1; then
  echo "ERROR: $SERVICE not responding on :8160/sessions"
  journalctl -u "$SERVICE" -n 30 --no-pager
  exit 1
fi
echo "    smoke test OK"

echo "==> Done."
