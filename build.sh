#!/usr/bin/env bash
set -euo pipefail
cd -- "$(dirname -- "$0")"
export GOCACHE="${GOCACHE:-$PWD/.cache/go-build}"
npm --prefix web ci --no-audit --no-fund
npm --prefix web run build
mkdir -p dist
for name in deployer-server deployer-agent deployerctl; do
  CGO_ENABLED=0 go build -buildvcs=false -trimpath -ldflags='-s -w' -o "dist/$name" "./cmd/$name"
done
printf 'Built standalone binaries in %s/dist\n' "$PWD"
