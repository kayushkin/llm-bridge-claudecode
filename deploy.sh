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

echo "==> Building $BIN_NAME..."
go build -o "$BIN_NAME" .
echo "    built: $(ls -lh "$BIN_NAME" | awk '{print $5}')"

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
