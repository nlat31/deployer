#!/usr/bin/env bash
# Build from source; package only an explicit role-specific allowlist.
set -euo pipefail
umask 077
cd -- "$(dirname -- "$0")/.."
role="${1:-}"
version="${2:-}"
arch="${GOARCH:-amd64}"
[[ "$role" == server || "$role" == agent ]] || exit 2
[[ "$version" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$ && "$version" != . && "$version" != .. ]] || {
  echo "Usage: ./release-$role.sh VERSION (GOARCH=amd64|arm64)" >&2; exit 2;
}
[[ "$arch" == amd64 || "$arch" == arm64 ]] || { echo 'Unsupported GOARCH' >&2; exit 2; }
export GOCACHE="${GOCACHE:-$PWD/.cache/go-build}"
if [[ "$role" == server ]]; then
  npm --prefix web ci --no-audit --no-fund
  npm --prefix web run build
fi
mkdir -p dist
stage="$(mktemp -d "$PWD/dist/.release-$role-XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
name="deployer-$role-$version-linux-$arch"
package="$stage/$name"
mkdir -p "$package"
binaries=("deployer-$role" deployer-launcher)
if [[ "$role" == server ]]; then binaries+=(deployerctl); fi
for binary in "${binaries[@]}"; do
  flags="-s -w"
  if [[ "$binary" != deployer-launcher ]]; then flags="$flags -X deployer/internal/selfupdate.Version=$version -X deployer/internal/selfupdate.Commit=$(git rev-parse --short HEAD)"; fi
  CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -ldflags="$flags" -o "$package/$binary" "./cmd/$binary"
done
if [[ "$role" == agent && -n "${DEPLOYER_UPDATE_SIGNING_KEY:-}" ]]; then
  GOOS="$(go env GOHOSTOS)" GOARCH="$(go env GOHOSTARCH)" go run ./cmd/deployerctl sign-agent --key "$DEPLOYER_UPDATE_SIGNING_KEY" --version "$version" --arch "$arch" --binary "$package/deployer-agent" --out "$stage/manifest.json"
  install -m 0600 "$stage/manifest.json" "dist/$name.manifest.json"
  install -m 0700 "$package/deployer-agent" "dist/$name.bin"
fi
python3 scripts/package-lifecycle.py "$role" "$arch" "$version" "$package"
tar --sort=name --mtime="@${SOURCE_DATE_EPOCH:-0}" --owner=0 --group=0 --numeric-owner -cf - -C "$stage" "$name" | gzip -n > "$stage/$name.tar.gz"
mv -- "$stage/$name.tar.gz" "dist/$name.tar.gz"
(cd dist; sha256sum "$name.tar.gz" > "$name.tar.gz.sha256")
printf 'Release: %s/dist/%s.tar.gz\n' "$PWD" "$name"
