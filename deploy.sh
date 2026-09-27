#!/usr/bin/env bash
set -euo pipefail

# One shared gate decides whether this tree may be deployed (main clone, default
# branch, clean, pushed, not behind, and the same for every tree the build reads).
# It lives in healthcheck/scripts/deploy-gate.sh. Do not inline or copy it.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" check )

# This deploy does not restart llm-bridge.service, so it runs in the caller's
# own shell and the caller sees how it ended. It used to restart the bridge to
# make the next spawn pick up the new binary, which it does anyway: the bridge
# looks the wrapper up on PATH and runs it afresh at every spawn
# (llm-bridge-server internal/harness/manager.go, Available). The restart
# killed every live session on the host for nothing, and an agent that deployed
# this and then llm-bridge-server restarted the bridge twice in one turn
# (br_1790462284393059025, 2026-09-26). That also meant detaching into a
# transient unit, whose pasted copy had stopped carrying the session id into
# the ledger.

REPO_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN_NAME="llm-bridge-claudecode"
USER_BIN="$HOME/bin/$BIN_NAME"

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
# The default branch is main or master as origin has it, and origin/HEAD only
# when origin has neither — see resolve_default_branch for why origin/HEAD
# cannot go first.
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
# Which branch is "the default"? Ask the REMOTE's branches, not origin/HEAD.
# origin/HEAD is whatever branch the remote had checked out when this clone was
# made — and a clone of a LOCAL checkout (`git clone ~/repos/llm-bridge-claudecode`)
# inherits that checkout's CURRENT branch as its "default". That is how the
# 2026-09-09 redeploy refused with "HEAD is missing 18 commits that
# origin/fix/discovery-reads-the-configured-claude-directory has": the guard was
# comparing main against a parked feature branch it had been told was the
# default. So: a branch named main or master on origin wins outright, and
# origin/HEAD is consulted only when the remote has neither.
resolve_default_branch() {
  local name
  git fetch --quiet origin 'refs/heads/main:refs/remotes/origin/main' 2>/dev/null || true
  git fetch --quiet origin 'refs/heads/master:refs/remotes/origin/master' 2>/dev/null || true
  for name in main master; do
    if git rev-parse --verify --quiet "refs/remotes/origin/$name" >/dev/null \
      || git rev-parse --verify --quiet "refs/heads/$name" >/dev/null; then
      echo "$name"; return
    fi
  done
  name="$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD 2>/dev/null)"
  name="${name#origin/}"
  if [ -n "$name" ]; then
    echo "$name"; return
  fi
  echo ""
}

# A clone whose origin is a path on this disk is not looking at the source of
# truth — it is looking at another checkout, which can be parked on any branch
# and behind the real remote by any amount. Refuse rather than compare against
# it; point origin at the real remote first (git remote set-url origin <url>).
ORIGIN_URL="$(git remote get-url origin 2>/dev/null || true)"
case "$ORIGIN_URL" in
  /*|file://*|.*)
    echo "ERROR: origin is a local path ($ORIGIN_URL), not the real remote. The ancestry" >&2
    echo "       guard would compare against another checkout's branches, which is how the" >&2
    echo "       2026-09-09 redeploy refused against a parked feature branch. Run" >&2
    echo "       'git remote set-url origin <the real remote>' in this clone, then retry." >&2
    exit 1
    ;;
esac

echo "==> Checking ancestry against the default branch..."
DEFAULT_BRANCH="$(resolve_default_branch)"
if [ -z "$DEFAULT_BRANCH" ]; then
  echo "ERROR: no default branch resolvable (no origin/HEAD, no main, no master);" >&2
  echo "       cannot establish that this deploy loses nothing. Refusing." >&2
  exit 1
fi
# Compare against the REMOTE default tip, not the local ref. A local `main`
# can itself be behind origin — nobody fetched — and then a check against
# `refs/heads/main` passes while the build is behind the branch it claims to
# contain. That is exactly how this repo came to sit one commit behind
# origin/main on 2026-09-09 with every LOCAL guard green: the drift the guard
# was built to stop had simply moved up one level, into local main. So fetch
# the default branch (best-effort — offline is not fatal, but say so) and
# compare against the tracking ref; fall back to the local ref only when there
# is no origin to ask.
git fetch --quiet origin "$DEFAULT_BRANCH" 2>/dev/null \
  || echo "    ⚠️  could not fetch origin/$DEFAULT_BRANCH — comparing against the LOCAL ref, which may itself be stale" >&2
if git rev-parse --verify --quiet "refs/remotes/origin/$DEFAULT_BRANCH" >/dev/null; then
  COMPARE_REF="refs/remotes/origin/$DEFAULT_BRANCH"
  COMPARE_DESC="origin/$DEFAULT_BRANCH"
else
  COMPARE_REF="refs/heads/$DEFAULT_BRANCH"
  COMPARE_DESC="$DEFAULT_BRANCH (local; origin unreachable)"
fi
echo "    comparing HEAD against $COMPARE_DESC"
MISSING="$(git rev-list --count "HEAD..$COMPARE_REF")"
if [ "$MISSING" -gt 0 ]; then
  if [ "${1:-}" = "--allow-unmerged" ]; then
    echo "    ⚠️  DEPLOYING ANYWAY (--allow-unmerged): HEAD is missing $MISSING commit(s)"
    echo "        that $COMPARE_DESC has:"
    git log --oneline "HEAD..$COMPARE_REF" | sed 's/^/        /'
  else
    echo "REFUSING TO DEPLOY: HEAD is missing $MISSING commit(s) that $COMPARE_DESC has:" >&2
    git log --oneline "HEAD..$COMPARE_REF" | sed 's/^/    /' >&2
    echo "" >&2
    echo "A binary built here would UNDO that work for every session — this exact" >&2
    echo "shape stranded user sessions on 2026-08-31 and 2026-09-01 (the parked" >&2
    echo "llm-bridge-claudecode branch missing the unprompted-turn fix)." >&2
    echo "Merge $COMPARE_DESC into this branch first (git merge $COMPARE_REF)," >&2
    echo "or pass --allow-unmerged if losing it is genuinely intended." >&2
    exit 1
  fi
fi
echo "    HEAD contains all of $COMPARE_DESC"

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

# install replaces the file rather than writing into it, so a wrapper that is
# running keeps the binary it started with, and /proc shows that binary as
# deleted. Those sessions switch to this build when their process next starts:
# after the idle reaper stops it, or on a resume. Name them, so nobody mistakes
# a live session's behaviour for this build's.
echo "==> Wrappers still running the previous binary..."
still_on_previous_binary=0
for exe in /proc/[0-9]*/exe; do
  [ "$(readlink "$exe" 2>/dev/null)" = "$USER_BIN (deleted)" ] || continue
  pid_dir="$(dirname "$exe")"
  session="$(tr '\0' '\n' <"$pid_dir/environ" 2>/dev/null | sed -n 's/^LLM_BRIDGE_SESSION_ID=//p')"
  echo "    pid ${pid_dir#/proc/} session ${session:-unknown (started before the bridge named sessions to their children)}"
  still_on_previous_binary=$((still_on_previous_binary + 1))
done
echo "    $still_on_previous_binary; new spawns get $BUILD_REV"

echo "==> Done."

# Last act: write this deploy to repo-store's ledger, so the next agent sees what is live.
( cd "$(dirname "$0")" && "$HOME/bin/deploy-gate" record )
