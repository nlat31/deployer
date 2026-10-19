#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
version="${1:-1.0.0}"
arch="${GOARCH:-amd64}"
[[ "$version" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$ ]] || { printf 'Invalid version\n' >&2; exit 1; }
export GOCACHE="${GOCACHE:-$PWD/.cache/go-build}"
mkdir -p dist
stage="$(mktemp -d "$PWD/dist/.sample-XXXXXX")"
trap 'rm -rf -- "$stage"' EXIT
mkdir -p "$stage/bin"
CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -buildvcs=false -trimpath -o "$stage/bin/sample-app" ./examples/sample-app
cp examples/sample-app/{setup,start,stop,clean}.sh "$stage/"
chmod +x "$stage/"*.sh
printf 'app_id: sample-app\nversion: "%s"\nos: linux\narch: %s\n' "$version" "$arch" > "$stage/app.yaml"
tar -czf "dist/sample-app-$version-linux-$arch.tar.gz" -C "$stage" .
printf 'Package: dist/sample-app-%s-linux-%s.tar.gz\n' "$version" "$arch"
