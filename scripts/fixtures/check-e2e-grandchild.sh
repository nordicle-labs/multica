#!/usr/bin/env bash
set -euo pipefail
echo "$$" > "$1/grandchild.pid"
sleep 300
