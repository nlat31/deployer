# Shared uninstall body, embedded into release remove.sh.
set -euo pipefail
role="${1:-${role:-all}}"
[[ "$role" == server || "$role" == agent || "$role" == all ]] || { echo 'Usage: sudo ./remove.sh [server|agent|all]' >&2; exit 2; }
[[ $# -le 1 ]] || exit 2
[[ "$(id -u)" == 0 ]] || { echo 'Run this script as root (sudo).' >&2; exit 1; }
# Never follow an installation root redirected outside the setup-owned tree.
for path in /data/deployer /data/deployer/config /data/deployer/bin; do
  [[ ! -L "$path" ]] || { echo "Refusing symlink: $path" >&2; exit 1; }
done
if [[ -d /data/deployer ]]; then
  exec 9>/data/deployer/.setup.lock
  flock 9
fi
roles=("$role")
if [[ "$role" == all ]]; then roles=(server agent); fi
for selected in "${roles[@]}"; do
  unit="deployer-$selected.service"
  if [[ "$(systemctl show "$unit" --property=LoadState --value)" != not-found ]]; then
    systemctl stop "$unit"
    systemctl disable "$unit"
    systemctl reset-failed "$unit" || true
  fi
  if [[ "$selected" == agent ]]; then
    if [[ -d /data/deployer/agent/apps ]]; then
      [[ -x /data/deployer/bin/deployer-agent ]] || { echo 'Agent executable missing; refusing to delete process records.' >&2; exit 1; }
      /data/deployer/bin/deployer-agent --stop-apps --data /data/deployer/agent
    fi
  fi
  rm -f -- "/etc/systemd/system/$unit"
  rm -rf -- "/etc/systemd/system/$unit.d"
  rm -f -- "/data/deployer/bin/deployer-$selected" "/data/deployer/config/$selected.env"
  if [[ "$selected" == server ]]; then
    rm -f -- /data/deployer/bin/deployerctl /data/deployer/config/server-cert.pem /data/deployer/config/server-key.pem
    rm -rf -- /data/deployer/controller
    account=deployer-control
  else
    rm -f -- /data/deployer/config/client-cert.pem /data/deployer/config/client-key.pem
    rm -rf -- /data/deployer/agent
    account=''
  fi
  if [[ -n "$account" ]] && getent passwd "$account" >/dev/null; then userdel "$account"; fi
  if [[ -n "$account" ]] && getent group "$account" >/dev/null; then groupdel "$account"; fi
  printf 'Removed deployer-%s (including its default data).\n' "$selected"
done
systemctl daemon-reload
# Shared certificates and logs belong to both roles; retain them until the last
# role is removed. Also tolerate a partially completed setup without a unit file.
if [[ ! -e /etc/systemd/system/deployer-server.service && ! -e /etc/systemd/system/deployer-agent.service && ! -e /data/deployer/bin/deployer-server && ! -e /data/deployer/bin/deployer-agent ]]; then
  for journal_unit in systemd-journald@deployer.socket systemd-journald-varlink@deployer.socket systemd-journald@deployer.service; do
    if [[ "$(systemctl show "$journal_unit" --property=LoadState --value)" != not-found ]]; then systemctl stop "$journal_unit"; fi
  done
  rm -f -- /etc/systemd/journald@deployer.conf
  if [[ -f /etc/machine-id ]]; then
    machine_id="$(cat /etc/machine-id)"
    [[ "$machine_id" =~ ^[a-f0-9]{32}$ ]] || { echo 'Invalid machine-id; refusing journal cleanup.' >&2; exit 1; }
    rm -rf -- "/var/log/journal/$machine_id.deployer" "/run/log/journal/$machine_id.deployer"
  fi
  rm -rf -- /data/deployer
fi
printf 'Uninstall complete. Custom data paths outside /data/deployer are preserved.\n'
