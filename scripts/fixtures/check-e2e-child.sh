#!/usr/bin/env bash
set -euo pipefail
echo "$$" > "$1/child.pid"
"$(dirname "$0")/check-e2e-grandchild.sh" "$1" &
wait
