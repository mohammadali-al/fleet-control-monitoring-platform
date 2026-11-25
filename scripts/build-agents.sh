#!/usr/bin/env bash
# Cross-compile fleet-agent (CGO off) for all supported platforms into an
# agent_binary_dir (default: bin/agents) as fleet-agent-<os>-<arch>.
# Uses the same reproducible -trimpath and version ldflags as scripts/build.sh.
set -euo pipefail
cd "$(dirname "$0")/.."

if [ "$#" -gt 1 ]; then
  echo "usage: $0 [output-dir]" >&2
  exit 2
fi

OUT_DIR="${1:-${AGENT_BINARY_DIR:-bin/agents}}"
LDFLAGS="${FLEET_LDFLAGS:-$(./scripts/build.sh --ldflags)}"
TARGETS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
)

mkdir -p "$OUT_DIR"
for target in "${TARGETS[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  out="$OUT_DIR/fleet-agent-$os-$arch"
  GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 go build -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/fleet-agent
  echo "built $out"
done
