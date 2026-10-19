# Shared setup body; release embeds this into the standalone setup.sh.
set -euo pipefail
[[ "$(id -u)" == 0 ]] || { printf 'Run as root to install system services.\n' >&2; exit 1; }
[[ -x "$source_dir/deployer-$role" ]] || { printf 'Run ./build.sh first.\n' >&2; exit 1; }
[[ "$(uname -s)" == Linux ]] || { echo 'Linux with systemd is required' >&2; exit 1; }
systemd_version="$(systemctl --version | head -1 | awk '{print $2}')"
[[ "$systemd_version" =~ ^[0-9]+$ && "$systemd_version" -ge 249 ]] || { echo 'systemd >= 249 is required' >&2; exit 1; }
if [[ -n "${package_arch:-}" ]]; then
  case "$(uname -m):$package_arch" in x86_64:amd64|aarch64:arm64) ;; *) echo 'Package architecture does not match this host' >&2; exit 1;; esac
fi
"$source_dir/deployer-$role" --help >/dev/null 2>&1
install -d -m 0755 /data/deployer/bin
exec 9>/data/deployer/.setup.lock
flock 9
install -d -m 0755 /data/deployer/config
if [[ ! -e /etc/systemd/journald@deployer.conf ]]; then
  cat > /etc/systemd/journald@deployer.conf <<'JOURNAL'
[Journal]
Storage=persistent
SystemMaxUse=256M
RuntimeMaxUse=64M
SystemMaxFileSize=32M
MaxRetentionSec=7day
JOURNAL
fi
binaries=("deployer-$role")
for binary in "${binaries[@]}"; do
  [[ -x "$source_dir/$binary" ]] || { echo "Missing executable: $binary" >&2; exit 1; }
  if [[ -f "/data/deployer/bin/$binary" ]] && cmp -s "$source_dir/$binary" "/data/deployer/bin/$binary"; then continue; fi
  temporary="$(mktemp "/data/deployer/bin/.$binary.XXXXXX")"
  install -m 0755 "$source_dir/$binary" "$temporary"
  mv -f -- "$temporary" "/data/deployer/bin/$binary"
done
if [[ "$role" == server ]]; then
  id deployer-control >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin deployer-control
  install -d -m 0700 -o deployer-control -g deployer-control /data/deployer/controller
  service_kill_mode=control-group
  service_user=deployer-control
else
  install -d -m 0755 /data/deployer/agent
  service_kill_mode=process
  service_user=root
fi
if [[ ! -e "/data/deployer/config/$role.env" ]]; then
  (umask 077; set -o noclobber; default_environment > "/data/deployer/config/$role.env")
fi
cat > "/etc/systemd/system/deployer-$role.service" <<UNIT
[Unit]
Description=Deployer $role
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=exec
User=$service_user
WorkingDirectory=/data/deployer
EnvironmentFile=/data/deployer/config/$role.env
ExecStart=/data/deployer/bin/deployer-$role
Restart=always
RestartSec=5
TimeoutStopSec=180
UMask=0077
LogNamespace=deployer
KillMode=$service_kill_mode

[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable "deployer-$role"
printf 'Installed deployer-%s.\nEdit /data/deployer/config/%s.env, then run:\n  systemctl enable --now deployer-%s\n' "$role" "$role" "$role"
