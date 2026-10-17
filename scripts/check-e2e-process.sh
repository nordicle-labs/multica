#!/usr/bin/env bash

terminate_process_tree() {
  local root_pid=${1:-}
  local signal=${2:-TERM}
  local child

  if [ -z "$root_pid" ]; then
    return 0
  fi

  while IFS= read -r child; do
    [ -n "$child" ] || continue
    terminate_process_tree "$child" "$signal"
  done < <(pgrep -P "$root_pid" 2>/dev/null || true)

  kill -"$signal" "$root_pid" 2>/dev/null || true
}
