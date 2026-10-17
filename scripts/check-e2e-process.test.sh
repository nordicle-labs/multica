#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
. "$SCRIPT_DIR/check-e2e-process.sh"

TMP_ROOT=$(mktemp -d "${TMPDIR:-/tmp}/multica-check-process.XXXXXX")
ROOT_PID=""
cleanup() {
  if [ -n "$ROOT_PID" ]; then
    terminate_process_tree "$ROOT_PID" KILL
    wait "$ROOT_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP_ROOT"
}
trap cleanup EXIT

"$SCRIPT_DIR/fixtures/check-e2e-root.sh" "$TMP_ROOT" &
ROOT_PID=$!

for file in root.pid child.pid grandchild.pid; do
  for _ in $(seq 1 100); do
    [ -s "$TMP_ROOT/$file" ] && break
    sleep 0.01
  done
  [ -s "$TMP_ROOT/$file" ] || { echo "missing PID fixture: $file" >&2; exit 1; }
done

PIDS=$(tr "\n" " " < "$TMP_ROOT/root.pid"; tr "\n" " " < "$TMP_ROOT/child.pid"; tr "\n" " " < "$TMP_ROOT/grandchild.pid")
terminate_process_tree "$ROOT_PID" TERM
wait "$ROOT_PID" 2>/dev/null || true
ROOT_PID=""

for pid in $PIDS; do
  for _ in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.01
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "process survived cleanup: $pid" >&2
    exit 1
  fi
done

echo "check-e2e-process.test.sh: PASS"
