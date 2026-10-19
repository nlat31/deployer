#!/usr/bin/env bash
set -euo pipefail
cd -- "${APP_RELEASE_DIR:?}"
exec ./bin/sample-app
