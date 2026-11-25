#!/usr/bin/env bash
# Install the Fleet main server on this host (T-0024). Safe to re-run.
#
# What it does
#   - installs bin/fleet-server        -> /usr/local/bin/fleet-server
#   - installs bin/fleet-agent         -> /usr/local/share/fleet/fleet-agent
#     (the binary onboarding pushes to new machines; server.agent_binary_path points here)
#   - installs the systemd unit        -> /etc/systemd/system/fleet-server.service
#   - creates the `fleet` system user, /etc/fleet-server/config.json (only if absent)
#     and the data dir /var/lib/fleet-server (FLEET_DATA_DIR, set by the unit)
#   - enables and starts the service, then waits for the first run to create the
#     internal CA (<data>/ca/ca.crt, ca.key)
#
# Usage:  sudo deploy/install-server.sh [--bin-dir DIR] [--no-start]
#   --bin-dir DIR  where fleet-server and fleet-agent were built (default: <repo>/bin,
#                  from scripts/build.sh)
#   --no-start     install only; do not enable/start or wait for the CA
#
# Staging: with DESTDIR=/some/dir the files are installed under that root and no user is
# created, nothing is chown'ed and systemctl is not called (for packaging and tests).
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$REPO/bin"
START=1

while [ "$#" -gt 0 ]; do
  case "$1" in
    --bin-dir) [ "$#" -ge 2 ] || { echo "install-server: --bin-dir needs a value" >&2; exit 2; }
               BIN_DIR="$2"; shift 2 ;;
    --no-start) START=0; shift ;;
    -h|--help) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d' | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "install-server: unknown argument: $1" >&2; exit 2 ;;
  esac
done

DESTDIR="${DESTDIR:-}"
SERVER_BIN=/usr/local/bin/fleet-server
AGENT_BIN=/usr/local/share/fleet/fleet-agent
UNIT=/etc/systemd/system/fleet-server.service
CONF_DIR=/etc/fleet-server
CONF=$CONF_DIR/config.json
DATA_DIR=/var/lib/fleet-server
SVC_USER=fleet

say() { echo "install-server: $*"; }
die() { echo "install-server: $*" >&2; exit 1; }

if [ -z "$DESTDIR" ] && [ "$(id -u)" -ne 0 ]; then
  die "must run as root (try: sudo $0), or set DESTDIR to stage into a directory"
fi
[ -f "$BIN_DIR/fleet-server" ] || die "$BIN_DIR/fleet-server not found; run scripts/build.sh first (or pass --bin-dir)"
[ -f "$BIN_DIR/fleet-agent" ]  || die "$BIN_DIR/fleet-agent not found; run scripts/build.sh first (or pass --bin-dir)"
[ -s "$BIN_DIR/fleet-agent" ]  || die "$BIN_DIR/fleet-agent is empty"
[ -f "$REPO/deploy/systemd/fleet-server.service" ] || die "unit file missing from the repo checkout"

CHANGED=0
# put MODE SRC DEST: install SRC at DESTDIR/DEST only if it differs; sets CHANGED=1 on change.
put() {
  local mode="$1" src="$2" dest="$DESTDIR$3"
  if [ -f "$dest" ] && cmp -s "$src" "$dest"; then
    chmod "$mode" "$dest"
    return 0
  fi
  install -D -m "$mode" "$src" "$dest"
  CHANGED=1
  say "installed $3"
}

# 1. Service user (real installs only).
if [ -z "$DESTDIR" ] && ! id -u "$SVC_USER" >/dev/null 2>&1; then
  useradd --system --user-group --home-dir "$DATA_DIR" --no-create-home \
          --shell /usr/sbin/nologin "$SVC_USER"
  say "created system user $SVC_USER"
fi

# 2. Binaries and unit.
put 0755 "$BIN_DIR/fleet-server" "$SERVER_BIN"
put 0755 "$BIN_DIR/fleet-agent"  "$AGENT_BIN"   # a payload onboarding pushes; the server only reads it
put 0644 "$REPO/deploy/systemd/fleet-server.service" "$UNIT"

# 3. Config: written once, never overwritten (the operator may edit it).
install -d -m 0755 "$DESTDIR$CONF_DIR"
if [ ! -e "$DESTDIR$CONF" ]; then
  cat >"$DESTDIR$CONF" <<EOF
{
  "server": {
    "agent_binary_path": "$AGENT_BIN",
    "log": { "level": "info", "format": "text" }
  }
}
EOF
  chmod 0644 "$DESTDIR$CONF"
  say "wrote $CONF"
elif ! grep -q "\"agent_binary_path\"[[:space:]]*:[[:space:]]*\"$AGENT_BIN\"" "$DESTDIR$CONF"; then
  say "NOTE: $CONF exists and does not set server.agent_binary_path to $AGENT_BIN;"
  say "      left unchanged. Onboarding needs that path to point at a fleet-agent binary."
fi
# listen_addr and data_dir are NOT in the file: the unit sets FLEET_LISTEN_ADDR (loopback
# :6789, reached only through the auth proxy) and FLEET_DATA_DIR. Override both in
# /etc/fleet-server/fleet-server.env (optional; see the unit).

# 4. Data dir. systemd's StateDirectory would create it too; doing it here lets the
# directory exist before first start with the right owner. 0750: it will hold the CA key.
install -d -m 0750 "$DESTDIR$DATA_DIR"
if [ -z "$DESTDIR" ]; then
  chown "$SVC_USER:$SVC_USER" "$DATA_DIR"
fi

# 5. Start, and confirm the first run created the internal CA.
if [ -n "$DESTDIR" ]; then
  say "staged under $DESTDIR (no service user, systemctl or CA check)"
  exit 0
fi
if [ "$START" -eq 0 ]; then
  say "installed; not started (--no-start). Start with: systemctl enable --now fleet-server"
  exit 0
fi

systemctl daemon-reload
systemctl enable fleet-server.service >/dev/null 2>&1
if systemctl is-active --quiet fleet-server.service; then
  if [ "$CHANGED" -eq 1 ]; then
    systemctl restart fleet-server.service
    say "restarted fleet-server (files changed)"
  else
    say "fleet-server already running; nothing changed"
  fi
else
  systemctl start fleet-server.service
  say "started fleet-server"
fi

for _ in $(seq 1 30); do
  if [ -s "$DATA_DIR/ca/ca.crt" ] && [ -s "$DATA_DIR/ca/ca.key" ]; then
    say "internal CA present at $DATA_DIR/ca (the first run creates it; it is never replaced)"
    say "panel: http://127.0.0.1:6789 on this host; operators reach it via the auth proxy (deploy/README.md)"
    exit 0
  fi
  sleep 1
done
die "service started but no CA appeared in $DATA_DIR/ca after 30s; see: journalctl -u fleet-server"
