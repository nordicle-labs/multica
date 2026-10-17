#!/usr/bin/env bash
set -euo pipefail
echo "$$" > "$1/root.pid"
"$(dirname "$0")/check-e2e-child.sh" "$1" &
wait
