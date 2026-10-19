#!/usr/bin/env bash
set -euo pipefail
: "${APP_DATA_DIR:?}"
[[ -d "$APP_DATA_DIR" && ! -L "$APP_DATA_DIR" ]]
find "$APP_DATA_DIR" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
printf 'data cleaned for %s/%s\n' "$APP_ID" "$APP_VERSION"
