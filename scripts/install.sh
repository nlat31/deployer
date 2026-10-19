#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
role="${1:-}"
[[ "$role" == server || "$role" == agent ]] || { echo 'Usage: sudo bash scripts/install.sh server|agent' >&2; exit 2; }
source_dir="$PWD/dist"
default_environment() { cat "configs/$role.env.example"; }
source scripts/setup-runtime.sh
