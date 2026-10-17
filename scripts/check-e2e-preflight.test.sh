#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT_DIR=$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)
TEST_DIR=$(mktemp -d "${TMPDIR:-/tmp}/multica-check-e2e-preflight.XXXXXX")
PORTS_FILE="$TEST_DIR/ports"
OUTPUT_FILE="$TEST_DIR/output"
ENV_FILE="$TEST_DIR/env"
LISTENER_PID=""

cleanup() {
  if [ -n "$LISTENER_PID" ]; then
    kill "$LISTENER_PID" 2>/dev/null || true
    wait "$LISTENER_PID" 2>/dev/null || true
  fi
  rm -rf "$TEST_DIR"
}
trap cleanup EXIT

node -e '
  const fs = require("node:fs");
  const net = require("node:net");
  const ports = [];
  const ready = () => {
    if (ports.length === 2) {
      fs.writeFileSync(process.argv[1], `BACKEND_PORT=${ports[0]}\nFRONTEND_PORT=${ports[1]}\n`);
    }
  };
  for (let i = 0; i < 2; i += 1) {
    net.createServer((socket) => socket.end()).listen(0, "127.0.0.1", function () {
      ports[i] = this.address().port;
      ready();
    });
  }
' "$PORTS_FILE" &
LISTENER_PID=$!

for _ in $(seq 1 50); do
  [ -s "$PORTS_FILE" ] && break
  sleep 0.1
done
[ -s "$PORTS_FILE" ] || { echo "fake listeners did not start" >&2; exit 1; }
. "$PORTS_FILE"
cp "$PORTS_FILE" "$ENV_FILE"

set +e
(cd "$ROOT_DIR" && ENV_FILE="$ENV_FILE" bash scripts/check.sh) >"$OUTPUT_FILE" 2>&1
status=$?
set -e

[ "$status" -ne 0 ] || { echo "check unexpectedly accepted occupied ports" >&2; exit 1; }
grep -Fq "Backend port :$BACKEND_PORT is already in use" "$OUTPUT_FILE"
grep -Fq "Frontend port :$FRONTEND_PORT is already in use" "$OUTPUT_FILE"
grep -Fq "Stop the existing process or configure fresh ports before rerunning make check." "$OUTPUT_FILE"
if grep -Fq "E2E tests (Playwright)" "$OUTPUT_FILE"; then
  echo "check reached Playwright despite occupied ports" >&2
  exit 1
fi
kill -0 "$LISTENER_PID"

echo "check-e2e-preflight.test.sh: PASS"
