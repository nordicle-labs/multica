#!/usr/bin/env bash
set -euo pipefail

# ==========================================================================
# Full verification pipeline: typecheck → unit tests → Go tests → E2E
# Usage: bash scripts/check.sh
# ==========================================================================

ENV_FILE="${ENV_FILE:-.env}"
if [ ! -f "$ENV_FILE" ]; then
  echo "Missing env file: $ENV_FILE"
  echo "Create .env from .env.example, or run 'make worktree-env' and use .env.worktree."
  exit 1
fi

set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a

# shellcheck disable=SC1091
. scripts/local-env.sh
# shellcheck disable=SC1091
. scripts/check-e2e-process.sh

BACKEND_PID=""
FRONTEND_PID=""
STARTED_BACKEND=false
STARTED_FRONTEND=false
EXIT_CODE=0
NEXT_ENV_FILE="apps/web/next-env.d.ts"
NEXT_ENV_BACKUP=""
NEXT_ENV_EXISTED=false

# --------------------------------------------------------------------------
# Cleanup: kill only services this script started
# --------------------------------------------------------------------------
cleanup() {
  if [ -n "$NEXT_ENV_BACKUP" ]; then
    if [ "$NEXT_ENV_EXISTED" = true ]; then
      cp "$NEXT_ENV_BACKUP" "$NEXT_ENV_FILE"
    else
      rm -f "$NEXT_ENV_FILE"
    fi
    rm -f "$NEXT_ENV_BACKUP"
  fi
  echo ""
  if [ "$STARTED_BACKEND" = true ] && [ -n "$BACKEND_PID" ]; then
    terminate_process_tree "$BACKEND_PID" TERM
    wait "$BACKEND_PID" 2>/dev/null || true
    echo "    Stopped backend tree (root PID $BACKEND_PID)"
  fi
  if [ "$STARTED_FRONTEND" = true ] && [ -n "$FRONTEND_PID" ]; then
    terminate_process_tree "$FRONTEND_PID" TERM
    wait "$FRONTEND_PID" 2>/dev/null || true
    echo "    Stopped frontend tree (root PID $FRONTEND_PID)"
  fi
  echo ""
  if [ "$EXIT_CODE" -eq 0 ]; then
    echo "✓ All checks passed."
  else
    echo "✗ Checks FAILED."
  fi
  exit "$EXIT_CODE"
}
trap cleanup EXIT

# --------------------------------------------------------------------------
# Utility: wait until a port responds
# --------------------------------------------------------------------------
wait_for_port() {
  local port=$1 name=$2 max_wait=${3:-60} path=${4:-/}
  local elapsed=0
  echo "    Waiting for $name on :$port..."
  while ! curl -sf "http://localhost:${port}${path}" > /dev/null 2>&1; do
    sleep 1
    elapsed=$((elapsed + 1))
    if [ "$elapsed" -ge "$max_wait" ]; then
      echo "    ERROR: $name did not start within ${max_wait}s"
      EXIT_CODE=1
      exit 1
    fi
  done
  echo "    $name ready (${elapsed}s)"
}

echo "==> Checking E2E service ports..."
bash scripts/check-e2e-preflight.sh || { EXIT_CODE=1; exit 1; }
bash scripts/check-e2e-preflight.test.sh || { EXIT_CODE=1; exit 1; }
bash scripts/check-e2e-process.test.sh || { EXIT_CODE=1; exit 1; }
bash scripts/check-e2e-frontend-mode.test.sh || { EXIT_CODE=1; exit 1; }

# --------------------------------------------------------------------------
# Step 0: Ensure DB
# --------------------------------------------------------------------------
echo "==> Using env file: $ENV_FILE"
echo "==> Checking PostgreSQL..."
bash scripts/ensure-postgres.sh "$ENV_FILE"

# --------------------------------------------------------------------------
# Step 1: TypeScript typecheck
# --------------------------------------------------------------------------
echo ""
echo "==> [1/5] TypeScript typecheck..."
pnpm typecheck || { EXIT_CODE=1; exit 1; }

# --------------------------------------------------------------------------
# Step 2: TypeScript unit tests (Vitest)
# --------------------------------------------------------------------------
echo ""
echo "==> [2/5] TypeScript unit tests..."
pnpm test || { EXIT_CODE=1; exit 1; }

# --------------------------------------------------------------------------
# Step 3: Go tests
# --------------------------------------------------------------------------
echo ""
echo "==> [3/5] Go tests..."
echo "==> Verifying Go test wrapper..."
bash scripts/test-go.test.sh || { EXIT_CODE=1; exit 1; }
echo "==> Running database migrations..."
(cd server && go run ./cmd/migrate up) || { EXIT_CODE=1; exit 1; }
bash scripts/test-go.sh || { EXIT_CODE=1; exit 1; }

# --------------------------------------------------------------------------
# Step 4: Build frontend and start services for E2E
# --------------------------------------------------------------------------
echo ""
echo "==> [4/5] Building frontend and starting services for E2E..."

echo "    Building frontend production bundle..."
# Next rewrites this tracked generated declaration between dev and production
# modes. Preserve the caller's exact pre-gate state so verification is
# read-only even when the build succeeds or exits early.
NEXT_ENV_BACKUP="$(mktemp "${TMPDIR:-/tmp}/multica-next-env.XXXXXX")"
if [ -f "$NEXT_ENV_FILE" ]; then
  NEXT_ENV_EXISTED=true
  cp "$NEXT_ENV_FILE" "$NEXT_ENV_BACKUP"
fi
STANDALONE=false pnpm --filter @multica/web build || { EXIT_CODE=1; exit 1; }

echo "    Starting backend..."
(cd server && exec go run ./cmd/server) > /tmp/multica-check-backend.log 2>&1 &
BACKEND_PID=$!
STARTED_BACKEND=true
wait_for_port "$PORT" "Backend" 90 "/health"

echo "    Starting frontend..."
(cd apps/web && exec ./node_modules/.bin/next start --port "$FRONTEND_PORT") > /tmp/multica-check-frontend.log 2>&1 &
FRONTEND_PID=$!
STARTED_FRONTEND=true
wait_for_port "$FRONTEND_PORT" "Frontend" 120 "/"

# --------------------------------------------------------------------------
# Step 5: E2E tests (Playwright)
# --------------------------------------------------------------------------
echo ""
echo "==> [5/5] E2E tests (Playwright)..."
pnpm exec playwright test || { EXIT_CODE=1; exit 1; }
