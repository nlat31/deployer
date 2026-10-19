#!/usr/bin/env bash
set -euo pipefail
if [[ -n "${APP_MAIN_PID:-}" ]] && kill -0 "$APP_MAIN_PID" 2>/dev/null; then
  kill -TERM "$APP_MAIN_PID" 2>/dev/null || true
  for ((i=0; i<100; i++)); do
    kill -0 "$APP_MAIN_PID" 2>/dev/null || exit 0
    sleep 0.1
  done
  exit 1
fi
