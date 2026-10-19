#!/usr/bin/env bash
# Shared uninstall entry point. State, identity and application data are retained.
set -euo pipefail
role="${1:-${role:-all}}"
[[ "$role" == server || "$role" == agent || "$role" == all ]] || { echo 'Usage: remove.sh [server|agent|all]' >&2; exit 2; }
[[ "$(id -u)" == 0 ]] || { echo 'Run as root.' >&2; exit 1; }
root="${DEPLOYER_ROOT:-/data/deployer}"
[[ ! -L "$root" && ! -L "$root/bin" ]] || { echo 'Refusing symlink installation' >&2; exit 1; }
[[ -e "$root" ]] || exit 0
exec 9>"$root/.lifecycle.lock"
flock 9
roles=("$role"); [[ "$role" != all ]] || roles=(server agent)
for selected in "${roles[@]}"; do
  if [[ -x "$root/bin/deployer-launcher" ]]; then
    "$root/bin/deployer-launcher" remove --lock-fd 9 --root "$root" --role "$selected"
  elif [[ -e "$root/bin/deployer-$selected" ]]; then
    echo 'Missing launcher; cannot safely stop deployment.' >&2; exit 1
  fi
done
printf 'Deployment removed. Configuration, identities, logs and business data preserved.\n'
