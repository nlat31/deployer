#!/usr/bin/env bash
set -euo pipefail
role="${1:-agent}"
root="${DEPLOYER_ROOT:-/data/deployer}"
[[ "$(id -u)" == 0 ]] || { echo 'Run as root.' >&2; exit 1; }
[[ "$role" == agent || "$role" == server ]] || exit 2
exec "$root/bin/deployer-launcher" start --role "$role" --root "$root"
