#!/usr/bin/env bash
# Offline certificate issuance. This script never replaces existing files.
set -euo pipefail
umask 077
action="${1:-}"
root="${2:-}"
value="${3:-}"
days="${DEPLOYER_CERT_DAYS:-3650}"
[[ "$days" =~ ^[0-9]+$ && "$days" -ge 1 && "$days" -le 36500 ]] || { echo 'Invalid DEPLOYER_CERT_DAYS' >&2; exit 2; }
command -v openssl >/dev/null
new_dir() {
  [[ ! -e "$1" && ! -L "$1" ]] || { echo 'Output directory must not exist' >&2; exit 1; }
  mkdir -p -- "$(dirname -- "$1")"
  mkdir -m 0700 -- "$1"
}
issue() {
  local destination="$1" common_name="$2" san="$3" usage="$4" serial
  openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$destination/key.pem" -out "$destination/request.csr" -subj "/CN=$common_name"
  cat > "$destination/extensions.cnf" <<EXT
basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=$usage
subjectAltName=$san
EXT
  serial="$(openssl rand -hex 16)"
  openssl x509 -req -in "$destination/request.csr" -CA "$root/ca.pem" \
    -CAkey "$root/ca-key.pem" -set_serial "0x$serial" -days "$days" -sha256 \
    -extfile "$destination/extensions.cnf" -out "$destination/cert.pem"
  rm -- "$destination/request.csr" "$destination/extensions.cnf"
}
if [[ "$action" == init && -n "$root" && -n "$value" ]]; then
  san=''
  for host in "${@:3}"; do
    [[ "$host" =~ ^[a-zA-Z0-9:._-]+$ && ${#host} -le 253 ]] || { echo 'Invalid server hostname/IP' >&2; exit 2; }
    if [[ "$host" =~ ^[0-9.]+$ || "$host" == *:* ]]; then entry="IP:$host"; else entry="DNS:$host"; fi
    san="${san:+$san,}$entry"
  done
  new_dir "$root"
  openssl req -new -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
    -keyout "$root/ca-key.pem" -out "$root/ca.pem" -days "$days" -sha256 \
    -subj '/CN=Deployer CA' -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
    -addext 'keyUsage=critical,keyCertSign,cRLSign'
  new_dir "$root/server"
  issue "$root/server" 'Deployer Server' "$san" serverAuth
  printf 'CA: %s/ca.pem\nServer certificate: %s/server/cert.pem\nKeep ca-key.pem on the issuing machine only.\n' "$root" "$root"
elif [[ "$action" == node && -n "$root" && -n "$value" ]]; then
  [[ "$value" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$ && "$value" != . && "$value" != .. ]] || { echo 'Invalid node ID' >&2; exit 2; }
  [[ -f "$root/ca.pem" && -f "$root/ca-key.pem" ]] || { echo 'Missing issuing CA' >&2; exit 1; }
  destination="${4:-$root/nodes/$value}"
  new_dir "$destination"
  issue "$destination" 'Deployer Agent' "URI:spiffe://deployer/nodes/$value" clientAuth
  printf 'Node ID: %s\nClient certificate: %s/cert.pem\n' "$value" "$destination"
else
  printf 'Usage:\n  bash scripts/pki.sh init NEW_PKI_DIR SERVER_HOSTNAME_OR_IP [...]\n  bash scripts/pki.sh node PKI_DIR NODE_ID [NEW_OUTPUT_DIR]\n' >&2
  exit 2
fi
