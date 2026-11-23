#!/usr/bin/env bash
# Build fleet-server and fleet-agent into bin/. Optional single arg: a binary name.
# Reproducible: -trimpath, CGO off, no build id, and a commit-derived (not wall-clock) build time.
set -euo pipefail
cd "$(dirname "$0")/.."

MODULE=github.com/mohammadali-al/fleet-control-monitoring-platform
BINARIES=(fleet-server fleet-agent)

if [ "$#" -gt 1 ]; then echo "usage: $0 [${BINARIES[*]}]" >&2; exit 2; fi
if [ "$#" -eq 1 ]; then
  ok=0; for b in "${BINARIES[@]}"; do [ "$b" = "$1" ] && ok=1; done
  [ "$ok" = 1 ] || { echo "unknown binary: $1 (want ${BINARIES[*]})" >&2; exit 2; }
  BINARIES=("$1")
fi

VERSION=$(tr -d '[:space:]' < VERSION)
COMMIT=$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then ts=$SOURCE_DATE_EPOCH; else ts=$(git log -1 --format=%ct 2>/dev/null || echo ""); fi
if [ -n "$ts" ]; then BUILD_TIME=$(date -u -d "@$ts" +%Y-%m-%dT%H:%M:%SZ); else BUILD_TIME=unknown; fi

LDFLAGS="-s -w -buildid= -X $MODULE/internal/version.Version=$VERSION -X $MODULE/internal/version.Commit=$COMMIT -X $MODULE/internal/version.BuildTime=$BUILD_TIME"

mkdir -p bin
for b in "${BINARIES[@]}"; do
  CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "bin/$b" "./cmd/$b"
  echo "built bin/$b"
done
