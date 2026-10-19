#!/usr/bin/env bash
# Source checkout: remove both roles by default; optionally server or agent.
set -euo pipefail
source "$(dirname -- "$0")/scripts/remove-runtime.sh" "$@"
