#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
CHECK_SCRIPT="$SCRIPT_DIR/check.sh"

if ! grep -Fq 'STANDALONE=false pnpm --filter @multica/web build' "$CHECK_SCRIPT"; then
  echo "check.sh must build the web app in production mode before E2E" >&2
  exit 1
fi
if ! grep -Fq 'exec ./node_modules/.bin/next start --port "$FRONTEND_PORT"' "$CHECK_SCRIPT"; then
  echo "check.sh must serve the production build for E2E" >&2
  exit 1
fi
if grep -Fq 'next dev' "$CHECK_SCRIPT"; then
  echo "check.sh must not use Next dev/JIT for the certification gate" >&2
  exit 1
fi

build_line=$(grep -nF 'STANDALONE=false pnpm --filter @multica/web build' "$CHECK_SCRIPT" | cut -d: -f1)
start_line=$(grep -nF 'exec ./node_modules/.bin/next start --port "$FRONTEND_PORT"' "$CHECK_SCRIPT" | cut -d: -f1)
if [ "$build_line" -ge "$start_line" ]; then
  echo "production build must finish before the frontend starts" >&2
  exit 1
fi

echo "check-e2e-frontend-mode.test.sh: PASS"
