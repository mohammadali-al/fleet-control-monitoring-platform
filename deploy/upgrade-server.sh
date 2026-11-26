#!/usr/bin/env bash
# Upgrade an existing Fleet main server + auth proxy installation (T-0036).
#
# Flow:
#   1. Load /etc/fleet-server/install-answers.env (never re-prompts existing answers)
#   2. Query GitHub Releases API, compare versions, download + verify SHA256SUMS
#   3. Stop fleet-server -> back up data dir & binaries -> swap binaries ->
#      run `fleet-server migrate` -> restart -> health-check
#   4. Automatic rollback (restore binary + config + data backup) on any failure
#   5. Upgrade proxy (docker compose pull && up -d on pinned tags, or native binary swap)
#      while preserving /var/lib/fleet-proxy/ and live operator configs/secrets/CA.
#
# Usage:
#   sudo deploy/upgrade-server.sh [OPTIONS]
#   --version TAG        Target release tag (default: latest)
#   --repo OWNER/REPO    GitHub repository slug
#   --token TOKEN        GitHub token for private repos / rate limits (or GH_TOKEN / GITHUB_TOKEN)
#   --bin-dir DIR        Upgrade from local built binaries instead of GitHub Release
#   --force              Re-apply even if already on target version
#   --check              Compare versions and check prerequisites without changing anything
#   --skip-proxy         Upgrade fleet-server only
#   --yes, -y            Accept defaults for any genuinely new answer keys without prompting
#   --non-interactive    Do not prompt; fail if a new required answer key has no default
set -euo pipefail

DEFAULT_REPO="mohammadali-al/fleet-control-monitoring-platform"
CADDY_VERSION="2.10.0"
AUTHELIA_VERSION="4.39.27"

FLEET_CONF_DIR="/etc/fleet-server"
ANSWERS_FILE="$FLEET_CONF_DIR/install-answers.env"
SERVER_BIN="/usr/local/bin/fleet-server"
AGENT_BIN="/usr/local/share/fleet/fleet-agent"
AGENT_DIR="/usr/local/share/fleet/agents"
SVC_USER="fleet"
PROXY_USER="fleet-proxy"

TARGET_VERSION="latest"
FLEET_REPO=""
TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
BIN_DIR=""
FORCE=0
CHECK_ONLY=0
SKIP_PROXY=0
YES=0
NON_INTERACTIVE=0

# Saved answer defaults (populated from install-answers.env).
FLEET_PROXY_MODE="native"
FLEET_TLS_MODE="internal"
FLEET_ACME_EMAIL=""
FLEET_SERVER_PORT="6789"
FLEET_AGENT_PORT="7443"
FLEET_DATA_DIR="/var/lib/fleet-server"
FLEET_PROXY_DIR="/opt/fleet/proxy"
FLEET_PROXY_STATE_DIR="/var/lib/fleet-proxy"

say() { echo "upgrade-server: $*"; }
warn() { echo "upgrade-server: WARN: $*" >&2; }
die() { echo "upgrade-server: ERROR: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: sudo deploy/upgrade-server.sh [OPTIONS]

  --version TAG        Target release tag (default: latest)
  --repo OWNER/REPO    GitHub repository slug
  --token TOKEN        GitHub token for private repos / rate limits (or GH_TOKEN / GITHUB_TOKEN)
  --bin-dir DIR        Upgrade from local built binaries instead of GitHub Release
  --force              Re-apply even if already on target version
  --check              Compare versions and check prerequisites without changing anything
  --skip-proxy         Upgrade fleet-server only
  --yes, -y            Accept defaults for any genuinely new answer keys without prompting
  --non-interactive    Do not prompt; fail if a new required answer key has no default
EOF
}

need_val() {
  [ "$#" -ge 2 ] || die "flag $1 requires a value"
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) need_val "$@"; TARGET_VERSION="$2"; shift 2 ;;
    --repo) need_val "$@"; FLEET_REPO="$2"; shift 2 ;;
    --token) need_val "$@"; TOKEN="$2"; shift 2 ;;
    --bin-dir) need_val "$@"; BIN_DIR="$2"; shift 2 ;;
    --force) FORCE=1; shift ;;
    --check) CHECK_ONLY=1; shift ;;
    --skip-proxy) SKIP_PROXY=1; shift ;;
    -y|--yes) YES=1; shift ;;
    --non-interactive) NON_INTERACTIVE=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "must run as root (try: sudo $0)"
[ -x "$SERVER_BIN" ] || die "$SERVER_BIN not found; run install-server.sh for initial installation"
if ! command -v systemctl >/dev/null 2>&1 || [ ! -d /run/systemd/system ]; then
  die "systemd is required"
fi

TMP_DIR=""
cleanup() {
  if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
    rm -rf "$TMP_DIR"
  fi
}
trap cleanup EXIT
TMP_DIR="$(mktemp -d /tmp/fleet-upgrade.XXXXXX)"

load_answers() {
  [ -f "$ANSWERS_FILE" ] || { warn "$ANSWERS_FILE not found; using defaults"; return 0; }
  local line key val
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line#"${line%%[![:space:]]*}"}"
    [ -z "$line" ] && continue
    case "$line" in \#*) continue ;; esac
    key="${line%%=*}"
    val="${line#*=}"
    case "$val" in
      \"*\") val="${val#\"}"; val="${val%\"}" ;;
      \'*\') val="${val#\'}"; val="${val%\'}" ;;
    esac
    case "$key" in
      FLEET_REPO) [ -z "$FLEET_REPO" ] && FLEET_REPO="$val" ;;
      FLEET_PROXY_MODE) [ -n "$val" ] && FLEET_PROXY_MODE="$val" ;;
      FLEET_TLS_MODE) [ -n "$val" ] && FLEET_TLS_MODE="$val" ;;
      FLEET_ACME_EMAIL) FLEET_ACME_EMAIL="$val" ;;
      FLEET_SERVER_PORT) [ -n "$val" ] && FLEET_SERVER_PORT="$val" ;;
      FLEET_AGENT_PORT) [ -n "$val" ] && FLEET_AGENT_PORT="$val" ;;
      FLEET_DATA_DIR) [ -n "$val" ] && FLEET_DATA_DIR="$val" ;;
      FLEET_PROXY_DIR) [ -n "$val" ] && FLEET_PROXY_DIR="$val" ;;
      FLEET_PROXY_STATE_DIR) [ -n "$val" ] && FLEET_PROXY_STATE_DIR="$val" ;;
      FLEET_CONF_DIR) [ -n "$val" ] && FLEET_CONF_DIR="$val" ;;
      *) ;;
    esac
  done < "$ANSWERS_FILE"
}

load_answers
FLEET_REPO="${FLEET_REPO:-$DEFAULT_REPO}"

# Prompt ONLY for genuinely new keys that did not exist in older installs.
ensure_new_keys() {
  local updated=0
  if [ -z "$FLEET_PROXY_MODE" ]; then
    if [ "$YES" -eq 1 ] || [ "$NON_INTERACTIVE" -eq 1 ]; then
      FLEET_PROXY_MODE="native"
    elif [ -r /dev/tty ] && [ -w /dev/tty ]; then
      printf "New setting FLEET_PROXY_MODE (native|docker) [native]: " >/dev/tty
      IFS= read -r FLEET_PROXY_MODE </dev/tty || true
      FLEET_PROXY_MODE="${FLEET_PROXY_MODE:-native}"
    else
      FLEET_PROXY_MODE="native"
    fi
    updated=1
  fi
  if [ "$FLEET_TLS_MODE" = "acme" ] && [ -z "$FLEET_ACME_EMAIL" ]; then
    if [ "$NON_INTERACTIVE" -eq 1 ]; then
      die "FLEET_ACME_EMAIL missing in $ANSWERS_FILE for acme TLS mode"
    elif [ -r /dev/tty ] && [ -w /dev/tty ]; then
      printf "New setting FLEET_ACME_EMAIL: " >/dev/tty
      IFS= read -r FLEET_ACME_EMAIL </dev/tty || true
    fi
    updated=1
  fi
  if [ "$updated" -eq 1 ] && [ -f "$ANSWERS_FILE" ]; then
    for k in FLEET_PROXY_MODE FLEET_ACME_EMAIL; do
      v="${!k:-}"
      if [ -n "$v" ] && ! grep -q "^${k}=" "$ANSWERS_FILE"; then
        printf '%s=%s\n' "$k" "$v" >> "$ANSWERS_FILE"
      fi
    done
  fi
  return 0
}

host_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
}

installed_version() {
  local out
  out="$("$SERVER_BIN" --version 2>/dev/null || echo "unknown")"
  printf '%s' "$out" | awk '{print $1}'
}

normalize_ver() {
  printf '%s' "${1#v}"
}

github_api_get() {
  local url="$1"
  local hdrs=(-H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28")
  [ -n "$TOKEN" ] && hdrs+=(-H "Authorization: Bearer $TOKEN")
  local resp_file="$TMP_DIR/api_resp.json" code
  code="$(curl -sS -L -w '%{http_code}' "${hdrs[@]}" -o "$resp_file" "$url" || true)"
  case "$code" in
    200) cat "$resp_file" ;;
    401|403|404)
      die "GitHub Releases API returned HTTP $code for $url (set GH_TOKEN / --token for private repos or rate limits)"
      ;;
    *)
      die "GitHub Releases API failed (HTTP $code) for $url"
      ;;
  esac
}

parse_release_assets() {
  local json="$1"
  if command -v python3 >/dev/null 2>&1; then
    printf '%s' "$json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
print("TAG=" + d.get("tag_name", ""))
for a in d.get("assets", []):
    print("ASSET\t" + a.get("name", "") + "\t" + a.get("url", "") + "\t" + a.get("browser_download_url", ""))
'
  else
    printf '%s' "$json" | tr ',' '\n' | awk -F'"' '
      /"tag_name"[[:space:]]*:/ { print "TAG=" $4 }
      /"url"[[:space:]]*:/ && $4 ~ /\/releases\/assets\// { cur_url=$4 }
      /"name"[[:space:]]*:/ { cur_name=$4 }
      /"browser_download_url"[[:space:]]*:/ { print "ASSET\t" cur_name "\t" cur_url "\t" $4 }
    '
  fi
}

download_release_asset() {
  local api_url="$1" browser_url="$2" dest="$3"
  if [ -n "$TOKEN" ] && [ -n "$api_url" ]; then
    curl -fsSL -L -H "Authorization: Bearer $TOKEN" -H "Accept: application/octet-stream" "$api_url" -o "$dest"
  else
    curl -fsSL -L "$browser_url" -o "$dest"
  fi
}

verify_sha256() {
  local dir="$1" fname="$2"
  grep -qE "[[:space:]](\./)?${fname}$" "$dir/SHA256SUMS" || die "checksum entry missing for $fname"
  (cd "$dir" && sha256sum -c --ignore-missing SHA256SUMS 2>&1 | grep -E "^(\./)?${fname}: OK$" >/dev/null) || \
    die "SHA256 verification failed for $fname"
}

CUR_VER="$(installed_version)"
say "currently installed fleet-server version: $CUR_VER"

STAGE_DIR="$TMP_DIR/stage"
mkdir -p "$STAGE_DIR/agents"
RESOLVED_TAG=""

if [ -n "$BIN_DIR" ]; then
  local_srv="$BIN_DIR/fleet-server"
  [ -f "$local_srv" ] || die "$BIN_DIR/fleet-server not found"
  local_ag="$BIN_DIR/fleet-agent"
  [ -s "$local_ag" ] || die "$BIN_DIR/fleet-agent not found"
  cp "$local_srv" "$STAGE_DIR/fleet-server"
  cp "$local_ag" "$STAGE_DIR/fleet-agent"
  if [ -d "$BIN_DIR/agents" ]; then
    cp "$BIN_DIR/agents"/fleet-agent-* "$STAGE_DIR/agents/" 2>/dev/null || true
  fi
  RESOLVED_TAG="$("$STAGE_DIR/fleet-server" --version 2>/dev/null | awk '{print $1}' || echo "local")"
else
  arch="$(host_arch)"
  api_url="https://api.github.com/repos/$FLEET_REPO/releases/latest"
  if [ "$TARGET_VERSION" != "latest" ]; then
    api_url="https://api.github.com/repos/$FLEET_REPO/releases/tags/$TARGET_VERSION"
  fi
  rel_json="$(github_api_get "$api_url")"
  parsed="$(parse_release_assets "$rel_json")"
  RESOLVED_TAG="$(printf '%s\n' "$parsed" | awk -F= '/^TAG=/{print $2; exit}')"
  [ -n "$RESOLVED_TAG" ] || die "could not resolve target tag from GitHub Releases API"

  say "latest/target release: $RESOLVED_TAG (installed: $CUR_VER)"
  if [ "$FORCE" -eq 0 ] && [ "$(normalize_ver "$CUR_VER")" = "$(normalize_ver "$RESOLVED_TAG")" ]; then
    say "already running version $RESOLVED_TAG; nothing to do (pass --force to re-apply)"
    exit 0
  fi

  if [ "$CHECK_ONLY" -eq 1 ]; then
    say "upgrade available: $CUR_VER -> $RESOLVED_TAG (--check only)"
    exit 0
  fi

  declare -A asset_api asset_browser
  while IFS=$'\t' read -r kind name aurl burl; do
    [ "$kind" = "ASSET" ] || continue
    asset_api["$name"]="$aurl"
    asset_browser["$name"]="$burl"
  done <<< "$parsed"

  [ -n "${asset_browser[SHA256SUMS]:-}" ] || die "release $RESOLVED_TAG missing SHA256SUMS"
  download_release_asset "${asset_api[SHA256SUMS]}" "${asset_browser[SHA256SUMS]}" "$STAGE_DIR/SHA256SUMS"

  srv_asset="fleet-server-linux-$arch"
  if [ -z "${asset_browser[$srv_asset]:-}" ] && [ "$arch" = "amd64" ] && [ -n "${asset_browser[fleet-server]:-}" ]; then
    srv_asset="fleet-server"
  fi
  [ -n "${asset_browser[$srv_asset]:-}" ] || die "release $RESOLVED_TAG missing $srv_asset"
  say "downloading $srv_asset..."
  download_release_asset "${asset_api[$srv_asset]}" "${asset_browser[$srv_asset]}" "$STAGE_DIR/$srv_asset"
  verify_sha256 "$STAGE_DIR" "$srv_asset"
  cp "$STAGE_DIR/$srv_asset" "$STAGE_DIR/fleet-server"

  for ag in fleet-agent-linux-amd64 fleet-agent-linux-arm64 fleet-agent-darwin-amd64 fleet-agent-darwin-arm64; do
    if [ -n "${asset_browser[$ag]:-}" ]; then
      say "downloading $ag..."
      download_release_asset "${asset_api[$ag]}" "${asset_browser[$ag]}" "$STAGE_DIR/$ag"
      verify_sha256 "$STAGE_DIR" "$ag"
      cp "$STAGE_DIR/$ag" "$STAGE_DIR/agents/$ag"
    fi
  done
  [ -s "$STAGE_DIR/agents/fleet-agent-linux-$arch" ] || die "missing host agent binary fleet-agent-linux-$arch"
  cp "$STAGE_DIR/agents/fleet-agent-linux-$arch" "$STAGE_DIR/fleet-agent"
fi

if [ "$CHECK_ONLY" -eq 1 ]; then
  say "check complete: $CUR_VER -> $RESOLVED_TAG"
  exit 0
fi

chmod 0755 "$STAGE_DIR/fleet-server" "$STAGE_DIR/fleet-agent"
chmod 0755 "$STAGE_DIR/agents"/fleet-agent-* 2>/dev/null || true

ensure_new_keys

# Verify free disk space for data dir backup before stopping anything.
if [ -d "$FLEET_DATA_DIR" ]; then
  data_kb="$(du -sk "$FLEET_DATA_DIR" | awk '{print $1}')"
  avail_kb="$(df -Pk "$(dirname "$FLEET_DATA_DIR")" | awk 'NR==2 {print $4}')"
  [ "$avail_kb" -gt $((data_kb + 102400)) ] || die "insufficient disk space in $(dirname "$FLEET_DATA_DIR") to back up $FLEET_DATA_DIR"
fi

TS="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_DIR="/var/backups/fleet-server/upgrade-${TS}"
mkdir -p "$BACKUP_DIR/bin" "$BACKUP_DIR/agents"
chmod 0700 /var/backups/fleet-server "$BACKUP_DIR"

WAS_ACTIVE=0
if systemctl is-active --quiet fleet-server.service; then
  WAS_ACTIVE=1
fi

# Back up current binaries and config.
cp -a "$SERVER_BIN" "$BACKUP_DIR/bin/fleet-server"
[ -f "$AGENT_BIN" ] && cp -a "$AGENT_BIN" "$BACKUP_DIR/bin/fleet-agent"
if [ -d "$AGENT_DIR" ]; then
  cp -a "$AGENT_DIR/." "$BACKUP_DIR/agents/" 2>/dev/null || true
fi
[ -f "$FLEET_CONF_DIR/config.json" ] && cp -a "$FLEET_CONF_DIR/config.json" "$BACKUP_DIR/config.json"

say "stopping fleet-server.service..."
systemctl stop fleet-server.service || true

# Back up data directory (preserving CA, permissions, ownership).
if [ -d "$FLEET_DATA_DIR" ]; then
  say "backing up $FLEET_DATA_DIR -> $BACKUP_DIR/data ..."
  cp -a "$FLEET_DATA_DIR" "$BACKUP_DIR/data"
  if [ -f "$FLEET_DATA_DIR/ca/ca.key" ] && [ ! -s "$BACKUP_DIR/data/ca/ca.key" ]; then
    die "CA key backup verification failed; aborting upgrade before modifying anything"
  fi
fi

rollback() {
  local reason="$1"
  warn "upgrade failed ($reason); rolling back..."
  systemctl stop fleet-server.service || true
  install -D -m 0755 "$BACKUP_DIR/bin/fleet-server" "$SERVER_BIN"
  [ -f "$BACKUP_DIR/bin/fleet-agent" ] && install -D -m 0755 "$BACKUP_DIR/bin/fleet-agent" "$AGENT_BIN"
  if [ -d "$BACKUP_DIR/agents" ]; then
    install -d -m 0755 "$AGENT_DIR"
    cp -a "$BACKUP_DIR/agents/." "$AGENT_DIR/" 2>/dev/null || true
  fi
  [ -f "$BACKUP_DIR/config.json" ] && cp -a "$BACKUP_DIR/config.json" "$FLEET_CONF_DIR/config.json"

  if [ -d "$BACKUP_DIR/data" ]; then
    if [ -d "$FLEET_DATA_DIR" ]; then
      mv "$FLEET_DATA_DIR" "${FLEET_DATA_DIR}.failed-${TS}"
    fi
    cp -a "$BACKUP_DIR/data" "$FLEET_DATA_DIR"
    chown -R "$SVC_USER:$SVC_USER" "$FLEET_DATA_DIR"
  fi

  if [ "$WAS_ACTIVE" -eq 1 ]; then
    systemctl daemon-reload
    if systemctl start fleet-server.service; then
      local rb_ok=0 rb_code=""
      for _ in $(seq 1 10); do
        rb_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${FLEET_SERVER_PORT}/" || true)"
        if [ "$rb_code" = "200" ]; then
          rb_ok=1
          break
        fi
        sleep 1
      done
      [ "$rb_ok" -eq 1 ] || warn "rollback started fleet-server, but health check did not return 200 within 10s"
    else
      warn "rollback start of fleet-server failed; inspect journalctl -u fleet-server"
    fi
  fi
  die "upgrade failed and was rolled back ($reason). Backup retained at $BACKUP_DIR"
}

# Swap binaries (never clobbering live config.json or CA).
say "swapping fleet-server and fleet-agent binaries..."
install -D -m 0755 "$STAGE_DIR/fleet-server" "$SERVER_BIN" || rollback "binary install failed"
install -D -m 0755 "$STAGE_DIR/fleet-agent" "$AGENT_BIN" || rollback "agent binary install failed"
install -d -m 0755 "$AGENT_DIR"
for ag_file in "$STAGE_DIR/agents"/fleet-agent-*; do
  [ -f "$ag_file" ] || continue
  install -D -m 0755 "$ag_file" "$AGENT_DIR/$(basename "$ag_file")" || rollback "multi-arch agent install failed"
done

# Ship refreshed *.example config without touching live config.json.
cat >"$FLEET_CONF_DIR/config.json.example" <<EOF
{
  "schema_version": 2,
  "server": {
    "listen_addr": "127.0.0.1:${FLEET_SERVER_PORT}",
    "data_dir": "${FLEET_DATA_DIR}",
    "agent_binary_dir": "${AGENT_DIR}",
    "agent_binary_path": "${AGENT_BIN}",
    "channel_port": ${FLEET_AGENT_PORT},
    "log": { "level": "info", "format": "text" }
  }
}
EOF
chmod 0644 "$FLEET_CONF_DIR/config.json.example"

# Run schema migration (T-0035).
say "running fleet-server migrate..."
if ! "$SERVER_BIN" migrate --config "$FLEET_CONF_DIR/config.json" --data-dir "$FLEET_DATA_DIR"; then
  rollback "fleet-server migrate failed"
fi
if [ -d "$FLEET_DATA_DIR" ]; then
  chown -R "$SVC_USER:$SVC_USER" "$FLEET_DATA_DIR"
fi

# Verify CA was preserved byte-for-byte if present prior to upgrade.
if [ -f "$BACKUP_DIR/data/ca/ca.key" ]; then
  cmp -s "$BACKUP_DIR/data/ca/ca.key" "$FLEET_DATA_DIR/ca/ca.key" || rollback "CA private key changed unexpectedly"
  cmp -s "$BACKUP_DIR/data/ca/ca.crt" "$FLEET_DATA_DIR/ca/ca.crt" || rollback "CA certificate changed unexpectedly"
fi

say "restarting fleet-server.service..."
systemctl daemon-reload
systemctl start fleet-server.service || rollback "fleet-server failed to start"

health_ok=0
for _ in $(seq 1 20); do
  if systemctl is-active --quiet fleet-server.service; then
    code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${FLEET_SERVER_PORT}/" || true)"
    if [ "$code" = "200" ]; then
      health_ok=1
      break
    fi
  fi
  sleep 1
done
[ "$health_ok" -eq 1 ] || rollback "fleet-server health check failed on http://127.0.0.1:${FLEET_SERVER_PORT}/"
say "fleet-server upgraded cleanly ($CUR_VER -> $RESOLVED_TAG)"

# Upgrade proxy (state in /var/lib/fleet-proxy/ and live Caddyfile/configuration.yml preserved).
if [ "$SKIP_PROXY" -eq 0 ]; then
  if [ "$FLEET_PROXY_MODE" = "docker" ]; then
    if [ -f "$FLEET_PROXY_DIR/compose.yml" ]; then
      say "upgrading Docker proxy in $FLEET_PROXY_DIR (state in $FLEET_PROXY_STATE_DIR preserved)..."
      docker compose -f "$FLEET_PROXY_DIR/compose.yml" pull
      docker compose -f "$FLEET_PROXY_DIR/compose.yml" up -d --remove-orphans
      systemctl restart fleet-proxy.service || true
    else
      warn "$FLEET_PROXY_DIR/compose.yml not found; skipping Docker proxy upgrade"
    fi
  else
    say "checking native proxy binaries (Caddy v${CADDY_VERSION}, Authelia v${AUTHELIA_VERSION})..."
    arch="$(host_arch)"
    p_dir="$TMP_DIR/native-proxy"
    mkdir -p "$p_dir"
    proxy_changed=0

    if [ ! -x /usr/local/bin/caddy ] || ! /usr/local/bin/caddy version 2>/dev/null | grep -q "v${CADDY_VERSION}"; then
      caddy_tgz="caddy_${CADDY_VERSION}_linux_${arch}.tar.gz"
      caddy_sums="caddy_${CADDY_VERSION}_checksums.txt"
      caddy_base="https://github.com/caddyserver/caddy/releases/download/v${CADDY_VERSION}"
      curl -fsSL -L "${caddy_base}/${caddy_tgz}" -o "$p_dir/$caddy_tgz"
      curl -fsSL -L "${caddy_base}/${caddy_sums}" -o "$p_dir/$caddy_sums"
      (cd "$p_dir" && sha512sum -c --ignore-missing "$caddy_sums" | grep -q "^${caddy_tgz}: OK$") || \
        die "Caddy checksum verification failed"
      tar -xzf "$p_dir/$caddy_tgz" -C "$p_dir" caddy
      [ -x /usr/local/bin/caddy ] && cp -a /usr/local/bin/caddy "$BACKUP_DIR/bin/caddy"
      install -D -m 0755 "$p_dir/caddy" /usr/local/bin/caddy
      proxy_changed=1
    fi

    if [ ! -x /usr/local/bin/authelia ] || ! /usr/local/bin/authelia --version 2>/dev/null | grep -q "${AUTHELIA_VERSION}"; then
      auth_tgz="authelia-v${AUTHELIA_VERSION}-linux-${arch}.tar.gz"
      auth_sum="checksums.sha256"
      auth_base="https://github.com/authelia/authelia/releases/download/v${AUTHELIA_VERSION}"
      curl -fsSL -L "${auth_base}/${auth_tgz}" -o "$p_dir/$auth_tgz"
      curl -fsSL -L "${auth_base}/${auth_sum}" -o "$p_dir/$auth_sum"
      (cd "$p_dir" && sha256sum -c --ignore-missing "$auth_sum" | grep -q "^${auth_tgz}: OK$") || \
        die "Authelia checksum verification failed"
      mkdir -p "$p_dir/authelia-extract"
      tar -xzf "$p_dir/$auth_tgz" -C "$p_dir/authelia-extract"
      auth_bin="$(find "$p_dir/authelia-extract" -type f -name 'authelia' | head -n1)"
      [ -n "$auth_bin" ] || die "authelia binary missing in archive"
      [ -x /usr/local/bin/authelia ] && cp -a /usr/local/bin/authelia "$BACKUP_DIR/bin/authelia"
      install -D -m 0755 "$auth_bin" /usr/local/bin/authelia
      proxy_changed=1
    fi

    if [ "$proxy_changed" -eq 1 ]; then
      chown -R "$PROXY_USER:$PROXY_USER" "$FLEET_PROXY_STATE_DIR" 2>/dev/null || true
      systemctl restart fleet-proxy.service
    fi
  fi

  systemctl is-active --quiet fleet-proxy.service || warn "fleet-proxy.service is not active; check systemctl status fleet-proxy"
fi

# Record updated version in install-answers.env (still excluding secrets).
if [ -f "$ANSWERS_FILE" ]; then
  if grep -q '^FLEET_INSTALL_VERSION=' "$ANSWERS_FILE"; then
    sed -i "s|^FLEET_INSTALL_VERSION=.*|FLEET_INSTALL_VERSION=${RESOLVED_TAG}|" "$ANSWERS_FILE"
  else
    printf 'FLEET_INSTALL_VERSION=%s\n' "$RESOLVED_TAG" >> "$ANSWERS_FILE"
  fi
fi

say "upgrade complete! Backup preserved at $BACKUP_DIR"
