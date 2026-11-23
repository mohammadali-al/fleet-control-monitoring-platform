#!/usr/bin/env bash
# Run tests. No arg: everything. Group arg: server | agent | e2e.
set -euo pipefail
cd "$(dirname "$0")/.."

# Group -> package patterns. e2e is intentionally empty for now (exits 0).
group_packages() {
  case "$1" in
    server) echo "./cmd/fleet-server/... ./internal/..." ;;
    agent)  echo "./cmd/fleet-agent/... ./internal/..." ;;
    e2e)    echo "" ;;
    *) echo "unknown group: $1 (want server|agent|e2e)" >&2; return 2 ;;
  esac
}

if [ "$#" -eq 0 ]; then
  exec go test ./...
fi
if [ "$#" -gt 1 ]; then echo "usage: $0 [server|agent|e2e]" >&2; exit 2; fi

pkgs=$(group_packages "$1")
if [ -z "$pkgs" ]; then
  echo "no tests in group '$1' yet"
  exit 0
fi
# shellcheck disable=SC2086
exec go test $pkgs
