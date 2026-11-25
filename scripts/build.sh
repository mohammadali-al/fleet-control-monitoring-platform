#!/usr/bin/env bash
# Build fleet-server and fleet-agent into bin/, and cross-compile all
# fleet-agent targets into bin/agents/ via scripts/build-agents.sh.
# Optional single arg: fleet-server | fleet-agent | agents.
# Reproducible: -trimpath, CGO off, no build id, and a commit-derived (not wall-clock) build time.
set -euo pipefail
cd "$(dirname "$0")/.."

MODULE=github.com/mohammadali-al/fleet-control-monitoring-platform
BINARIES=(fleet-server fleet-agent)

VERSION=$(tr -d '[:space:]' < VERSION)
COMMIT=$(git rev-parse --short=12 HEAD 2>/dev/null || echo unknown)
if [ -n "${SOURCE_DATE_EPOCH:-}" ]; then ts=$SOURCE_DATE_EPOCH; else ts=$(git log -1 --format=%ct 2>/dev/null || echo ""); fi
if [ -n "$ts" ]; then BUILD_TIME=$(date -u -d "@$ts" +%Y-%m-%dT%H:%M:%SZ); else BUILD_TIME=unknown; fi

LDFLAGS="-s -w -buildid= -X $MODULE/internal/version.Version=$VERSION -X $MODULE/internal/version.Commit=$COMMIT -X $MODULE/internal/version.BuildTime=$BUILD_TIME"

if [ "${1:-}" = "--ldflags" ]; then
  echo "$LDFLAGS"
  exit 0
fi

BUILD_ALL_AGENTS=0
if [ "$#" -gt 1 ]; then echo "usage: $0 [${BINARIES[*]}|agents]" >&2; exit 2; fi
if [ "$#" -eq 0 ]; then
  BUILD_ALL_AGENTS=1
elif [ "$1" = "agents" ]; then
  BINARIES=()
  BUILD_ALL_AGENTS=1
else
  ok=0; for b in "${BINARIES[@]}"; do [ "$b" = "$1" ] && ok=1; done
  [ "$ok" = 1 ] || { echo "unknown binary: $1 (want ${BINARIES[*]} agents)" >&2; exit 2; }
  BINARIES=("$1")
fi

mkdir -p bin
for b in "${BINARIES[@]}"; do
  CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "bin/$b" "./cmd/$b"
  echo "built bin/$b"
done

if [ "$BUILD_ALL_AGENTS" = 1 ]; then
  FLEET_LDFLAGS="$LDFLAGS" ./scripts/build-agents.sh
fi
