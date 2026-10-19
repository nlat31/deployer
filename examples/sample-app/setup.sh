#!/usr/bin/env bash
set -euo pipefail
: "${APP_DATA_DIR:?}" "${APP_CONFIG_DIR:?}"
mkdir -p -- "$APP_DATA_DIR" "$APP_CONFIG_DIR" "$APP_LOG_DIR" "$APP_RUN_DIR"
printf 'sample App %s initialized\n' "$APP_VERSION"
