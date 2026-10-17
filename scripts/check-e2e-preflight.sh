#!/usr/bin/env bash
set -u

status=0

check_port() {
  local port=$1 service=$2

  if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
    exec 3>&-
    exec 3<&-
    echo "    ERROR: $service port :$port is already in use." >&2
    status=1
  fi
}

check_port "${PORT:?PORT is required}" "Backend"
check_port "${FRONTEND_PORT:?FRONTEND_PORT is required}" "Frontend"

if [ "$status" -ne 0 ]; then
  echo "    Stop the existing process or configure fresh ports before rerunning make check." >&2
  exit "$status"
fi
