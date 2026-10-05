#!/usr/bin/env bash
set -euo pipefail

# Install img-gen as a per-user launchd LaunchAgent (com.img-gen).
#
# The M6 sidecar IP is deliberately NOT hardcoded here (repo constraint: no
# real usernames/hostnames/IPs in tracked files). Pass it in when you install:
#
#   IMAGE_URL=http://<m6-sidecar>:8899 ./deploy/install-macos.sh
#
# Re-run this script after rebuilding bin/img-gen to pick up a new binary
# (the plist points at the path, so a fresh build is picked up on restart).

LABEL="com.img-gen"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TEMPLATE="$REPO_ROOT/deploy/$LABEL.plist.in"
BIN="$REPO_ROOT/bin/img-gen"
DEST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG_DIR="$HOME/Library/Logs/img-gen"
DOMAIN="gui/$UID"

IMAGE_URL="${IMAGE_URL:?set IMAGE_URL to the mflux sidecar URL, e.g. IMAGE_URL=http://<m6>:8899 ./deploy/install-macos.sh}"

if [[ ! -x "$BIN" ]]; then
  echo "error: $BIN is missing or not executable." >&2
  echo "build it first:" >&2
  echo "  go build -o bin/img-gen ./cmd/img-gen" >&2
  exit 1
fi

# Drop any prior registration before rebinding (ignores "not loaded").
launchctl bootout "$DOMAIN/$LABEL" 2>/dev/null || true

# Refuse to fight an unmanaged listener on :8099 (e.g. a leftover `nohup` run).
busy="$(lsof -nP -iTCP:8099 -sTCP:LISTEN -t 2>/dev/null || true)"
if [[ -n "$busy" ]]; then
  for pid in $busy; do
    if [[ "$(ps -o command= -p "$pid" 2>/dev/null)" == *img-gen* ]]; then
      echo "error: an unmanaged img-gen (pid $pid) is holding :8099." >&2
      echo "stop it, then re-run this script:" >&2
      echo "  kill $pid" >&2
      exit 1
    fi
  done
fi

mkdir -p "$LOG_DIR" "$(dirname "$DEST")"
sed -e "s|__REPO_ROOT__|$REPO_ROOT|g" \
    -e "s|__LOG_DIR__|$LOG_DIR|g" \
    -e "s|__IMAGE_URL__|$IMAGE_URL|g" \
    "$TEMPLATE" > "$DEST"

launchctl bootstrap "$DOMAIN" "$DEST"

if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
  echo "installed: $DEST"
  echo "running  : $DOMAIN/$LABEL"
  echo "logs     : $LOG_DIR/img-gen.{out,err}.log"
  echo "image_url: $IMAGE_URL"
else
  echo "error: bootstrap failed — check $LOG_DIR/img-gen.err.log" >&2
  exit 1
fi
