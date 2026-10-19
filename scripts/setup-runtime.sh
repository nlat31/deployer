# Shared root-only setup body, embedded into release setup.sh.
set -euo pipefail
[[ "$(id -u)" == 0 ]] || { echo 'Run as root.' >&2; exit 1; }
[[ "$(uname -s)" == Linux ]] || { echo 'Linux is required.' >&2; exit 1; }
root="${DEPLOYER_ROOT:-/data/deployer}"
[[ "$root" == /* && "$root" != / ]] || { echo 'Invalid DEPLOYER_ROOT' >&2; exit 1; }
for path in "$root" "$root/bin" "$root/config" "$root/lifecycle" "$root/lifecycle/$role"; do
  [[ ! -L "$path" ]] || { echo "Refusing symlink: $path" >&2; exit 1; }
done
if [[ -n "${package_arch:-}" ]]; then
  case "$(uname -m):$package_arch" in x86_64:amd64|aarch64:arm64) ;; *) echo 'Package architecture mismatch' >&2; exit 1;; esac
fi
[[ -x "$source_dir/deployer-$role" && -x "$source_dir/deployer-launcher" ]] || { echo 'Missing release binaries; build first.' >&2; exit 1; }
"$source_dir/deployer-$role" --version >/dev/null
install -d -m 0700 "$root"
exec 9>"$root/.lifecycle.lock"
flock 9
install -d -m 0700 "$root/bin" "$root/config" "$root/lifecycle/$role"
if [[ ! -e "$root/config/$role.env" ]]; then
  (umask 077; set -o noclobber; default_environment > "$root/config/$role.env")
fi
"$source_dir/deployer-launcher" bootstrap --lock-fd 9 --role "$role" --root "$root" --source "$source_dir/deployer-launcher"
for action in start stop remove; do
  temporary="$(mktemp "$root/lifecycle/$role/.$action.XXXXXX")"
  cat > "$temporary" <<SCRIPT
#!/usr/bin/env bash
set -euo pipefail
exec "$root/bin/deployer-launcher" $action --root "$root" --role "$role"
SCRIPT
  chmod 0755 "$temporary"
  mv -f -- "$temporary" "$root/lifecycle/$role/$action.sh"
done
"$root/bin/deployer-launcher" install --lock-fd 9 --root "$root" --role "$role" --source "$source_dir/deployer-$role"
if [[ "$role" == server && -x "$source_dir/deployerctl" ]]; then
  temporary="$(mktemp "$root/bin/.deployerctl.XXXXXX")"
  install -m 0700 "$source_dir/deployerctl" "$temporary"
  mv -f -- "$temporary" "$root/bin/deployerctl"
fi
printf 'Installed %s. Configure %s/config/%s.env, then run %s/lifecycle/%s/start.sh\n' "$role" "$root" "$role" "$root" "$role"
