#!/usr/bin/env bash
# Rebuild the committed React SPA bundle (web/assets/dist/app.{js,css} and web/dist/index.html).
# Go builds (`go build ./...`, `./scripts/build.sh`) embed the committed artifacts and do NOT need Node.
set -euo pipefail
WEB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ ! -d "$WEB_DIR/node_modules" ] || [ "${1:-}" = "--ci" ]; then
  npm --prefix "$WEB_DIR" ci
fi

npm --prefix "$WEB_DIR" run build
