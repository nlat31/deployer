#!/usr/bin/env bash
# One universal Agent archive; identities are created on each host at runtime.
set -euo pipefail
umask 077
cd -- "$(dirname -- "$0")"
version="${1:-}"
[[ $# -eq 1 && "$version" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$ ]] || { echo 'Usage: ./release-public.sh VERSION' >&2; exit 2; }
export DEPLOYER_RELEASE_PKI="$(realpath "${DEPLOYER_RELEASE_PKI:-data/pki/loom}")"
openssl verify -CAfile "$DEPLOYER_RELEASE_PKI/ca.pem" -purpose sslserver -verify_ip 18.138.124.233 "$DEPLOYER_RELEASE_PKI/server/cert.pem"
bash release-server.sh "$version"
bash release-agent.sh "$version"
printf 'Public server: https://18.138.124.233:7710\nUniversal Agent: deploy the SAME Agent archive to each host. Local keys and certificates are created on first start.\n'
