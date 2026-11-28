#!/usr/bin/env bash
# Upgrade an existing Fleet main server, bundled monitoring stack, and auth proxy (T-0036 / T-0067).
#
# Flow:
#   1. Load /etc/fleet-server/install-answers.env (never re-prompts existing answers)
#   2. Query GitHub Releases API, compare versions, download + verify SHA256SUMS
#   3. Stop fleet-server -> back up data dir, binaries, config, and proxy configs/units ->
#      swap binaries -> run `fleet-server migrate` -> restart -> health-check
#   4. Automatic rollback (restore binaries + configs + units + data backup) on any failure
#   5. Converge native bundled services (VictoriaMetrics + Grafana) and fleet-server unit/env
#      idempotently using pinned versions/checksums and renderers shared from install-server.sh
#   6. Back up & re-render proxy configs (Caddyfile + Authelia configuration.yml + units) from
#      install-answers.env, upgrade proxy (docker compose or native), restart & health-check
#      with rollback while preserving /var/lib/fleet-proxy/ and live config.json/CA/secrets.
#
# Usage:
#   sudo deploy/upgrade-server.sh [OPTIONS]
#   --version TAG        Target release tag (default: latest)
#   --repo OWNER/REPO    GitHub repository slug
#   --token TOKEN        GitHub token for private repos / rate limits (or GH_TOKEN / GITHUB_TOKEN)
#   --bin-dir DIR        Upgrade from local built binaries instead of GitHub Release
#   --force              Re-apply even if already on target version
#   --check              Compare versions and check prerequisites without changing anything
#   --skip-proxy         Upgrade fleet-server and bundled monitoring services only
#   --yes, -y            Accept defaults for any genuinely new answer keys without prompting
#   --non-interactive    Do not prompt; fail if a new required answer key has no default
set -euo pipefail

DEFAULT_REPO="mohammadali-al/fleet-control-monitoring-platform"

UPGRADE_SCRIPT_DIR=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  UPGRADE_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi

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
FLEET_DOMAIN_MODE=""
FLEET_PANEL_DOMAIN=""
FLEET_AUTH_DOMAIN=""
FLEET_TLS_MODE="internal"
FLEET_ACME_EMAIL=""
FLEET_PANEL_PORT=""
FLEET_SERVER_PORT="6789"
FLEET_AGENT_PORT="7443"
FLEET_AGENT_DIAL_PORT=""
FLEET_ENROLL_PORT=""
FLEET_AUTH_PORT=""
FLEET_TSDB_PORT=""
FLEET_GRAFANA_PORT=""
FLEET_DATA_DIR="/var/lib/fleet-server"
FLEET_TSDB_DIR=""
FLEET_TSDB_RETENTION=""
FLEET_GRAFANA_DIR=""
FLEET_GRAFANA_CONF_DIR=""
FLEET_GRAFANA_HOME=""
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
  --skip-proxy         Upgrade fleet-server and bundled monitoring services only
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
      FLEET_DOMAIN_MODE) [ -n "$val" ] && FLEET_DOMAIN_MODE="$val" ;;
      FLEET_PANEL_DOMAIN) [ -n "$val" ] && FLEET_PANEL_DOMAIN="$val" ;;
      FLEET_AUTH_DOMAIN) FLEET_AUTH_DOMAIN="$val" ;;
      FLEET_TLS_MODE) [ -n "$val" ] && FLEET_TLS_MODE="$val" ;;
      FLEET_ACME_EMAIL) FLEET_ACME_EMAIL="$val" ;;
      FLEET_PANEL_PORT) [ -n "$val" ] && FLEET_PANEL_PORT="$val" ;;
      FLEET_SERVER_PORT) [ -n "$val" ] && FLEET_SERVER_PORT="$val" ;;
      FLEET_AGENT_PORT) [ -n "$val" ] && FLEET_AGENT_PORT="$val" ;;
      FLEET_AGENT_DIAL_PORT) [ -n "$val" ] && FLEET_AGENT_DIAL_PORT="$val" ;;
      FLEET_ENROLL_PORT) [ -n "$val" ] && FLEET_ENROLL_PORT="$val" ;;
      FLEET_AUTH_PORT) [ -n "$val" ] && FLEET_AUTH_PORT="$val" ;;
      FLEET_TSDB_PORT) [ -n "$val" ] && FLEET_TSDB_PORT="$val" ;;
      FLEET_GRAFANA_PORT) [ -n "$val" ] && FLEET_GRAFANA_PORT="$val" ;;
      FLEET_DATA_DIR) [ -n "$val" ] && FLEET_DATA_DIR="$val" ;;
      FLEET_TSDB_DIR) [ -n "$val" ] && FLEET_TSDB_DIR="$val" ;;
      FLEET_TSDB_RETENTION) [ -n "$val" ] && FLEET_TSDB_RETENTION="$val" ;;
      FLEET_GRAFANA_DIR) [ -n "$val" ] && FLEET_GRAFANA_DIR="$val" ;;
      FLEET_GRAFANA_CONF_DIR) [ -n "$val" ] && FLEET_GRAFANA_CONF_DIR="$val" ;;
      FLEET_GRAFANA_HOME) [ -n "$val" ] && FLEET_GRAFANA_HOME="$val" ;;
      FLEET_PROXY_DIR) [ -n "$val" ] && FLEET_PROXY_DIR="$val" ;;
      FLEET_PROXY_STATE_DIR) [ -n "$val" ] && FLEET_PROXY_STATE_DIR="$val" ;;
      FLEET_CONF_DIR) [ -n "$val" ] && FLEET_CONF_DIR="$val" ;;
      *) ;;
    esac
  done < "$ANSWERS_FILE"
}

resolve_upgrade_answers() {
  FLEET_REPO="${FLEET_REPO:-$DEFAULT_REPO}"
  FLEET_PANEL_PORT="${FLEET_PANEL_PORT:-443}"
  FLEET_AGENT_DIAL_PORT="${FLEET_AGENT_DIAL_PORT:-7444}"
  FLEET_ENROLL_PORT="${FLEET_ENROLL_PORT:-7445}"
  FLEET_AUTH_PORT="${FLEET_AUTH_PORT:-9091}"
  FLEET_TSDB_PORT="${FLEET_TSDB_PORT:-8428}"
  FLEET_GRAFANA_PORT="${FLEET_GRAFANA_PORT:-3000}"
  FLEET_TSDB_DIR="${FLEET_TSDB_DIR:-/var/lib/fleet-victoriametrics}"
  FLEET_TSDB_RETENTION="${FLEET_TSDB_RETENTION:-90d}"
  FLEET_GRAFANA_DIR="${FLEET_GRAFANA_DIR:-/var/lib/fleet-grafana}"
  FLEET_GRAFANA_CONF_DIR="${FLEET_GRAFANA_CONF_DIR:-/etc/fleet-grafana}"
  FLEET_GRAFANA_HOME="${FLEET_GRAFANA_HOME:-/usr/local/share/grafana}"

  # Infer domain mode for older pre-T-0039 installs that predate FLEET_DOMAIN_MODE.
  if [ -z "$FLEET_DOMAIN_MODE" ]; then
    if [ -n "$FLEET_AUTH_DOMAIN" ]; then
      FLEET_DOMAIN_MODE="two"
    elif [ -f "$FLEET_PROXY_DIR/Caddyfile" ] && grep -q 'handle /authelia\*' "$FLEET_PROXY_DIR/Caddyfile"; then
      FLEET_DOMAIN_MODE="single"
    elif [ -f "$FLEET_PROXY_DIR/authelia/configuration.yml" ] && grep -qE "address: .*/authelia" "$FLEET_PROXY_DIR/authelia/configuration.yml"; then
      FLEET_DOMAIN_MODE="single"
    elif [ -f "$FLEET_PROXY_DIR/Caddyfile" ]; then
      FLEET_DOMAIN_MODE="two"
    else
      FLEET_DOMAIN_MODE="single"
    fi
  fi

  # Best-effort fallback if install-answers.env was missing FLEET_PANEL_DOMAIN.
  if [ -z "$FLEET_PANEL_DOMAIN" ] && [ -f "$FLEET_PROXY_DIR/authelia/configuration.yml" ]; then
    FLEET_PANEL_DOMAIN="$(awk -F"'" '/^[[:space:]]*-?[[:space:]]*domain:/{print $2; exit}' "$FLEET_PROXY_DIR/authelia/configuration.yml" || true)"
  fi
  if [ -z "$FLEET_PANEL_DOMAIN" ] && [ -f "$FLEET_CONF_DIR/config.json" ]; then
    FLEET_PANEL_DOMAIN="$(awk -F'"' '/"enroll_host"[[:space:]]*:/{print $4; exit}' "$FLEET_CONF_DIR/config.json" || true)"
  fi

  if [ "$FLEET_DOMAIN_MODE" = "two" ] && [ -n "$FLEET_PANEL_DOMAIN" ] && [ -z "$FLEET_AUTH_DOMAIN" ]; then
    if [ -f "$FLEET_PROXY_DIR/authelia/configuration.yml" ]; then
      FLEET_AUTH_DOMAIN="$(sed -n "s|.*authelia_url:[[:space:]]*'https://\([^/:']*\).*|\1|p" "$FLEET_PROXY_DIR/authelia/configuration.yml" | head -n1 || true)"
    fi
    FLEET_AUTH_DOMAIN="${FLEET_AUTH_DOMAIN:-auth.${FLEET_PANEL_DOMAIN}}"
  fi
}

load_answers
resolve_upgrade_answers

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
  if [ -f "$ANSWERS_FILE" ]; then
    local k v
    for k in FLEET_PROXY_MODE FLEET_DOMAIN_MODE FLEET_ACME_EMAIL \
             FLEET_PANEL_PORT FLEET_AUTH_PORT FLEET_TSDB_PORT FLEET_GRAFANA_PORT \
             FLEET_TSDB_DIR FLEET_TSDB_RETENTION FLEET_GRAFANA_DIR; do
      v="${!k:-}"
      if [ -n "$v" ] && ! grep -q "^${k}=" "$ANSWERS_FILE"; then
        printf '%s=%s\n' "$k" "$v" >> "$ANSWERS_FILE"
        updated=1
      fi
    done
  fi
  [ "$updated" -eq 0 ] || say "updated $ANSWERS_FILE with newly introduced setting defaults"
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
  [ -f "$BIN_DIR/install-server.sh" ] && cp "$BIN_DIR/install-server.sh" "$STAGE_DIR/install-server.sh"
  [ -f "$BIN_DIR/upgrade-server.sh" ] && cp "$BIN_DIR/upgrade-server.sh" "$STAGE_DIR/upgrade-server.sh"
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

  if [ -n "${asset_browser[install-server.sh]:-}" ]; then
    say "downloading install-server.sh (shared provisioning helpers)..."
    download_release_asset "${asset_api[install-server.sh]}" "${asset_browser[install-server.sh]}" "$STAGE_DIR/install-server.sh"
    verify_sha256 "$STAGE_DIR" "install-server.sh"
    chmod 0755 "$STAGE_DIR/install-server.sh"
  fi
  if [ -n "${asset_browser[upgrade-server.sh]:-}" ]; then
    download_release_asset "${asset_api[upgrade-server.sh]}" "${asset_browser[upgrade-server.sh]}" "$STAGE_DIR/upgrade-server.sh"
    verify_sha256 "$STAGE_DIR" "upgrade-server.sh"
    chmod 0755 "$STAGE_DIR/upgrade-server.sh"
  fi
fi

if [ "$CHECK_ONLY" -eq 1 ]; then
  say "check complete: $CUR_VER -> $RESOLVED_TAG"
  exit 0
fi

chmod 0755 "$STAGE_DIR/fleet-server" "$STAGE_DIR/fleet-agent"
chmod 0755 "$STAGE_DIR/agents"/fleet-agent-* 2>/dev/null || true

# Source install-server.sh in library mode so pinned versions/checksums, binary installers,
# and config/unit renderers stay in a single source of truth (never duplicated).
INSTALLER_LIB=""
source_installer_lib() {
  if [ -f "$STAGE_DIR/install-server.sh" ]; then
    INSTALLER_LIB="$STAGE_DIR/install-server.sh"
  elif [ -n "$UPGRADE_SCRIPT_DIR" ] && [ -f "$UPGRADE_SCRIPT_DIR/install-server.sh" ]; then
    INSTALLER_LIB="$UPGRADE_SCRIPT_DIR/install-server.sh"
  elif [ -f /usr/local/lib/fleet/install-server.sh ]; then
    INSTALLER_LIB="/usr/local/lib/fleet/install-server.sh"
  else
    local raw_ref="main"
    if [ -n "$RESOLVED_TAG" ] && [ "$RESOLVED_TAG" != "latest" ] && [ "$RESOLVED_TAG" != "local" ]; then
      raw_ref="$RESOLVED_TAG"
    fi
    local raw_url="https://raw.githubusercontent.com/${FLEET_REPO}/${raw_ref}/deploy/install-server.sh"
    if curl -fsSL -L "$raw_url" -o "$STAGE_DIR/install-server.sh"; then
      INSTALLER_LIB="$STAGE_DIR/install-server.sh"
    elif [ "$raw_ref" != "main" ] && curl -fsSL -L "https://raw.githubusercontent.com/${FLEET_REPO}/main/deploy/install-server.sh" -o "$STAGE_DIR/install-server.sh"; then
      INSTALLER_LIB="$STAGE_DIR/install-server.sh"
    fi
  fi
  if [ -z "$INSTALLER_LIB" ] || [ ! -f "$INSTALLER_LIB" ]; then
    die "could not locate install-server.sh for shared provisioning logic"
  fi

  export FLEET_INSTALL_LIB_ONLY=1
  export FLEET_AGENT_PORT
  export DESTDIR=""
  # shellcheck disable=SC1090
  . "$INSTALLER_LIB"
  unset FLEET_INSTALL_LIB_ONLY
  say() { echo "upgrade-server: $*"; }
  warn() { echo "upgrade-server: WARN: $*" >&2; }
  die() { echo "upgrade-server: ERROR: $*" >&2; exit 1; }
  if [ -n "$UPGRADE_SCRIPT_DIR" ] && [ -d "$UPGRADE_SCRIPT_DIR/grafana/dashboards" ]; then
    if [ -z "${SCRIPT_DIR:-}" ] || [ ! -d "$SCRIPT_DIR/grafana/dashboards" ]; then
      SCRIPT_DIR="$UPGRADE_SCRIPT_DIR"
    fi
  fi
}

source_installer_lib
ensure_new_keys

# Verify free disk space for data dir backup before stopping anything.
if [ -d "$FLEET_DATA_DIR" ]; then
  data_kb="$(du -sk "$FLEET_DATA_DIR" | awk '{print $1}')"
  avail_kb="$(df -Pk "$(dirname "$FLEET_DATA_DIR")" | awk 'NR==2 {print $4}')"
  [ "$avail_kb" -gt $((data_kb + 102400)) ] || die "insufficient disk space in $(dirname "$FLEET_DATA_DIR") to back up $FLEET_DATA_DIR"
fi

TS="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP_DIR="/var/backups/fleet-server/upgrade-${TS}"
mkdir -p "$BACKUP_DIR/bin" "$BACKUP_DIR/agents" "$BACKUP_DIR/systemd" "$BACKUP_DIR/proxy/authelia" "$BACKUP_DIR/grafana"
chmod 0700 /var/backups/fleet-server "$BACKUP_DIR"

WAS_ACTIVE=0
systemctl is-active --quiet fleet-server.service && WAS_ACTIVE=1
VM_WAS_ACTIVE=0
systemctl is-active --quiet fleet-victoriametrics.service && VM_WAS_ACTIVE=1
VM_UNIT_EXISTED=0
[ -f /etc/systemd/system/fleet-victoriametrics.service ] && VM_UNIT_EXISTED=1
GF_WAS_ACTIVE=0
systemctl is-active --quiet fleet-grafana.service && GF_WAS_ACTIVE=1
GF_UNIT_EXISTED=0
[ -f /etc/systemd/system/fleet-grafana.service ] && GF_UNIT_EXISTED=1
PROXY_WAS_ACTIVE=0
systemctl is-active --quiet fleet-proxy.service && PROXY_WAS_ACTIVE=1

# Back up current binaries, config, and unit files.
cp -a "$SERVER_BIN" "$BACKUP_DIR/bin/fleet-server"
[ -f "$AGENT_BIN" ] && cp -a "$AGENT_BIN" "$BACKUP_DIR/bin/fleet-agent"
if [ -d "$AGENT_DIR" ]; then
  cp -a "$AGENT_DIR/." "$BACKUP_DIR/agents/" 2>/dev/null || true
fi
[ -f "$FLEET_CONF_DIR/config.json" ] && cp -a "$FLEET_CONF_DIR/config.json" "$BACKUP_DIR/config.json"
[ -f "$FLEET_CONF_DIR/fleet-server.env" ] && cp -a "$FLEET_CONF_DIR/fleet-server.env" "$BACKUP_DIR/fleet-server.env"
[ -f /etc/systemd/system/fleet-server.service ] && cp -a /etc/systemd/system/fleet-server.service "$BACKUP_DIR/systemd/fleet-server.service"

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
  [ -f "$BACKUP_DIR/fleet-server.env" ] && cp -a "$BACKUP_DIR/fleet-server.env" "$FLEET_CONF_DIR/fleet-server.env"

  [ -f "$BACKUP_DIR/bin/victoria-metrics" ] && install -D -m 0755 "$BACKUP_DIR/bin/victoria-metrics" /usr/local/bin/victoria-metrics
  [ -f "$BACKUP_DIR/bin/caddy" ] && install -D -m 0755 "$BACKUP_DIR/bin/caddy" /usr/local/bin/caddy
  [ -f "$BACKUP_DIR/bin/authelia" ] && install -D -m 0755 "$BACKUP_DIR/bin/authelia" /usr/local/bin/authelia

  if [ -f "$BACKUP_DIR/proxy/Caddyfile" ]; then
    cp -a "$BACKUP_DIR/proxy/Caddyfile" "$FLEET_PROXY_DIR/Caddyfile"
  fi
  if [ -f "$BACKUP_DIR/proxy/authelia/configuration.yml" ]; then
    install -d -m 0755 "$FLEET_PROXY_DIR/authelia"
    cp -a "$BACKUP_DIR/proxy/authelia/configuration.yml" "$FLEET_PROXY_DIR/authelia/configuration.yml"
  fi
  if [ -f "$BACKUP_DIR/proxy/compose.yml" ]; then
    cp -a "$BACKUP_DIR/proxy/compose.yml" "$FLEET_PROXY_DIR/compose.yml"
  fi
  if [ -d "$BACKUP_DIR/grafana" ] && [ -n "$(ls -A "$BACKUP_DIR/grafana" 2>/dev/null)" ]; then
    cp -a "$BACKUP_DIR/grafana/." "$FLEET_GRAFANA_CONF_DIR/" 2>/dev/null || true
  fi
  for unit_bak in "$BACKUP_DIR/systemd"/*.service; do
    [ -f "$unit_bak" ] || continue
    cp -a "$unit_bak" "/etc/systemd/system/$(basename "$unit_bak")"
  done

  if [ -d "$BACKUP_DIR/data" ]; then
    if [ -d "$FLEET_DATA_DIR" ]; then
      mv "$FLEET_DATA_DIR" "${FLEET_DATA_DIR}.failed-${TS}"
    fi
    cp -a "$BACKUP_DIR/data" "$FLEET_DATA_DIR"
    chown -R "$SVC_USER:$SVC_USER" "$FLEET_DATA_DIR"
  fi

  systemctl daemon-reload
  if [ "$VM_UNIT_EXISTED" -eq 0 ]; then
    systemctl stop fleet-victoriametrics.service >/dev/null 2>&1 || true
  elif [ "$VM_WAS_ACTIVE" -eq 1 ]; then
    systemctl restart fleet-victoriametrics.service >/dev/null 2>&1 || warn "rollback restart of fleet-victoriametrics failed"
  fi
  if [ "$GF_UNIT_EXISTED" -eq 0 ]; then
    systemctl stop fleet-grafana.service >/dev/null 2>&1 || true
  elif [ "$GF_WAS_ACTIVE" -eq 1 ]; then
    systemctl restart fleet-grafana.service >/dev/null 2>&1 || warn "rollback restart of fleet-grafana failed"
  fi
  if [ "$PROXY_WAS_ACTIVE" -eq 1 ]; then
    if [ "$FLEET_PROXY_MODE" = "docker" ] && [ -f "$FLEET_PROXY_DIR/compose.yml" ]; then
      docker compose -f "$FLEET_PROXY_DIR/compose.yml" up -d --remove-orphans >/dev/null 2>&1 || true
      docker compose -f "$FLEET_PROXY_DIR/compose.yml" restart >/dev/null 2>&1 || true
    fi
    systemctl restart fleet-proxy.service >/dev/null 2>&1 || warn "rollback restart of fleet-proxy failed"
  fi

  if [ "$WAS_ACTIVE" -eq 1 ]; then
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

# Refresh installed helper scripts in /usr/local/lib/fleet/ when staged.
CHANGED=0
if [ -f "$INSTALLER_LIB" ]; then
  put_file 0755 "$INSTALLER_LIB" /usr/local/lib/fleet/install-server.sh
fi
if [ -f "$STAGE_DIR/upgrade-server.sh" ]; then
  put_file 0755 "$STAGE_DIR/upgrade-server.sh" /usr/local/lib/fleet/upgrade-server.sh
elif [ -n "$UPGRADE_SCRIPT_DIR" ] && [ -f "$UPGRADE_SCRIPT_DIR/upgrade-server.sh" ]; then
  put_file 0755 "$UPGRADE_SCRIPT_DIR/upgrade-server.sh" /usr/local/lib/fleet/upgrade-server.sh
fi

# Ship refreshed *.example config without touching live config.json.
render_server_config_example > "$FLEET_CONF_DIR/config.json.example"
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

# Converge bundled monitoring services (VictoriaMetrics + Grafana) idempotently.
converge_victoriametrics() {
  say "converging VictoriaMetrics (v${VICTORIAMETRICS_VERSION} on 127.0.0.1:${FLEET_TSDB_PORT})..."
  if ! id -u "$TSDB_USER" >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$FLEET_TSDB_DIR" --no-create-home --shell /usr/sbin/nologin "$TSDB_USER"
    say "created system user $TSDB_USER"
  fi
  install -d -m 0750 "$FLEET_TSDB_DIR"
  chown "$TSDB_USER:$TSDB_USER" "$FLEET_TSDB_DIR"

  CHANGED=0
  if [ -x /usr/local/bin/victoria-metrics ] && ! /usr/local/bin/victoria-metrics --version 2>&1 | grep -q "v${VICTORIAMETRICS_VERSION}"; then
    cp -a /usr/local/bin/victoria-metrics "$BACKUP_DIR/bin/victoria-metrics"
  fi
  install_victoriametrics_binary || rollback "VictoriaMetrics binary installation failed"

  render_victoriametrics_service > "$TMP_DIR/fleet-victoriametrics.service"
  if [ -f /etc/systemd/system/fleet-victoriametrics.service ] && ! cmp -s "$TMP_DIR/fleet-victoriametrics.service" /etc/systemd/system/fleet-victoriametrics.service; then
    cp -a /etc/systemd/system/fleet-victoriametrics.service "$BACKUP_DIR/systemd/fleet-victoriametrics.service"
  fi
  put_file 0644 "$TMP_DIR/fleet-victoriametrics.service" /etc/systemd/system/fleet-victoriametrics.service
  local vm_changed="$CHANGED"

  if [ "$vm_changed" -eq 1 ] || [ "$VM_UNIT_EXISTED" -eq 0 ]; then
    systemctl daemon-reload
  fi
  systemctl is-enabled --quiet fleet-victoriametrics.service >/dev/null 2>&1 || systemctl enable fleet-victoriametrics.service >/dev/null 2>&1

  local vm_started=0
  if systemctl is-active --quiet fleet-victoriametrics.service; then
    if [ "$vm_changed" -eq 1 ]; then
      systemctl restart fleet-victoriametrics.service || rollback "fleet-victoriametrics failed to restart"
      vm_started=1
    fi
  else
    systemctl start fleet-victoriametrics.service || rollback "fleet-victoriametrics failed to start"
    vm_started=1
  fi

  local vm_ok=0 tries=15
  [ "$vm_started" -eq 0 ] && tries=5
  for _ in $(seq 1 "$tries"); do
    if curl -fsS "http://127.0.0.1:${FLEET_TSDB_PORT}/health" >/dev/null 2>&1; then
      vm_ok=1
      break
    fi
    sleep 1
  done
  [ "$vm_ok" -eq 1 ] || rollback "fleet-victoriametrics health check failed on http://127.0.0.1:${FLEET_TSDB_PORT}/health"
}

converge_grafana() {
  say "converging Grafana (v${GRAFANA_VERSION} on 127.0.0.1:${FLEET_GRAFANA_PORT})..."
  if ! id -u "$GRAFANA_USER" >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$FLEET_GRAFANA_DIR" --no-create-home --shell /usr/sbin/nologin "$GRAFANA_USER"
    say "created system user $GRAFANA_USER"
  fi
  install -d -m 0750 "$FLEET_GRAFANA_DIR" "$FLEET_GRAFANA_DIR/logs" "$FLEET_GRAFANA_DIR/plugins"
  if [ ! -s "$FLEET_GRAFANA_DIR/secret_key" ]; then
    openssl rand -hex 32 > "$FLEET_GRAFANA_DIR/secret_key"
    chmod 0600 "$FLEET_GRAFANA_DIR/secret_key"
    say "generated Grafana secret_key at $FLEET_GRAFANA_DIR/secret_key"
  fi
  chown -R "$GRAFANA_USER:$GRAFANA_USER" "$FLEET_GRAFANA_DIR"

  CHANGED=0
  install_grafana_binary || rollback "Grafana binary installation failed"

  install -d -m 0755 "$FLEET_GRAFANA_CONF_DIR" \
    "$FLEET_GRAFANA_CONF_DIR/provisioning/datasources" \
    "$FLEET_GRAFANA_CONF_DIR/provisioning/dashboards" \
    "$FLEET_GRAFANA_CONF_DIR/dashboards"

  if [ -n "$FLEET_PANEL_DOMAIN" ]; then
    render_grafana_ini > "$TMP_DIR/grafana.ini"
    put_file 0644 "$TMP_DIR/grafana.ini" "$FLEET_GRAFANA_CONF_DIR/grafana.ini.example"
    if [ ! -e "$FLEET_GRAFANA_CONF_DIR/grafana.ini" ]; then
      cp "$FLEET_GRAFANA_CONF_DIR/grafana.ini.example" "$FLEET_GRAFANA_CONF_DIR/grafana.ini"
      chmod 0644 "$FLEET_GRAFANA_CONF_DIR/grafana.ini"
      CHANGED=1
      say "wrote $FLEET_GRAFANA_CONF_DIR/grafana.ini"
    fi
  else
    warn "FLEET_PANEL_DOMAIN unset; preserving existing $FLEET_GRAFANA_CONF_DIR/grafana.ini"
  fi

  local ds_file="$FLEET_GRAFANA_CONF_DIR/provisioning/datasources/victoriametrics.yaml"
  render_grafana_datasource > "$TMP_DIR/victoriametrics.yaml"
  if [ -f "$ds_file" ] && ! cmp -s "$TMP_DIR/victoriametrics.yaml" "$ds_file"; then
    install -d -m 0755 "$BACKUP_DIR/grafana/provisioning/datasources"
    cp -a "$ds_file" "$BACKUP_DIR/grafana/provisioning/datasources/victoriametrics.yaml"
  fi
  put_file 0644 "$TMP_DIR/victoriametrics.yaml" "$ds_file"

  local prov_file="$FLEET_GRAFANA_CONF_DIR/provisioning/dashboards/fleet.yaml"
  render_grafana_dashboard_provider > "$TMP_DIR/fleet.yaml"
  if [ -f "$prov_file" ] && ! cmp -s "$TMP_DIR/fleet.yaml" "$prov_file"; then
    install -d -m 0755 "$BACKUP_DIR/grafana/provisioning/dashboards"
    cp -a "$prov_file" "$BACKUP_DIR/grafana/provisioning/dashboards/fleet.yaml"
  fi
  put_file 0644 "$TMP_DIR/fleet.yaml" "$prov_file"

  install_grafana_dashboards

  if [ -n "$FLEET_PANEL_DOMAIN" ] || [ ! -f /etc/systemd/system/fleet-grafana.service ]; then
    local svc_domain="$FLEET_PANEL_DOMAIN"
    [ -z "$svc_domain" ] && svc_domain="fleet.internal"
    FLEET_PANEL_DOMAIN="$svc_domain" render_grafana_service > "$TMP_DIR/fleet-grafana.service"
    if [ -f /etc/systemd/system/fleet-grafana.service ] && ! cmp -s "$TMP_DIR/fleet-grafana.service" /etc/systemd/system/fleet-grafana.service; then
      cp -a /etc/systemd/system/fleet-grafana.service "$BACKUP_DIR/systemd/fleet-grafana.service"
    fi
    put_file 0644 "$TMP_DIR/fleet-grafana.service" /etc/systemd/system/fleet-grafana.service
  fi

  local gf_changed="$CHANGED"
  if [ "$gf_changed" -eq 1 ] || [ "$GF_UNIT_EXISTED" -eq 0 ]; then
    systemctl daemon-reload
  fi
  systemctl is-enabled --quiet fleet-grafana.service >/dev/null 2>&1 || systemctl enable fleet-grafana.service >/dev/null 2>&1

  local gf_started=0
  if systemctl is-active --quiet fleet-grafana.service; then
    if [ "$gf_changed" -eq 1 ]; then
      systemctl restart fleet-grafana.service || rollback "fleet-grafana failed to restart"
      gf_started=1
    fi
  else
    systemctl start fleet-grafana.service || rollback "fleet-grafana failed to start"
    gf_started=1
  fi

  local gf_ok=0 tries=30
  [ "$gf_started" -eq 0 ] && tries=5
  for _ in $(seq 1 "$tries"); do
    if curl -fsS "http://127.0.0.1:${FLEET_GRAFANA_PORT}/grafana/api/health" >/dev/null 2>&1; then
      gf_ok=1
      break
    fi
    sleep 1
  done
  [ "$gf_ok" -eq 1 ] || rollback "fleet-grafana health check failed on http://127.0.0.1:${FLEET_GRAFANA_PORT}/grafana/api/health"
}

converge_fleet_server_unit() {
  local env_changed=0
  if [ ! -f "$FLEET_CONF_DIR/fleet-server.env" ]; then
    cat >"$FLEET_CONF_DIR/fleet-server.env" <<EOF
FLEET_LISTEN_ADDR=127.0.0.1:${FLEET_SERVER_PORT}
FLEET_DATA_DIR=${FLEET_DATA_DIR}
FLEET_MONITOR_STORE_ADDR=http://127.0.0.1:${FLEET_TSDB_PORT}
EOF
    chmod 0640 "$FLEET_CONF_DIR/fleet-server.env"
    env_changed=1
  elif ! grep -q '^FLEET_MONITOR_STORE_ADDR=' "$FLEET_CONF_DIR/fleet-server.env"; then
    printf 'FLEET_MONITOR_STORE_ADDR=http://127.0.0.1:%s\n' "$FLEET_TSDB_PORT" >> "$FLEET_CONF_DIR/fleet-server.env"
    env_changed=1
  fi

  CHANGED=0
  render_fleet_server_service > "$TMP_DIR/fleet-server.service"
  put_file 0644 "$TMP_DIR/fleet-server.service" /etc/systemd/system/fleet-server.service
  if [ "$CHANGED" -eq 1 ] || [ "$env_changed" -eq 1 ]; then
    say "updating fleet-server.service / environment for VictoriaMetrics integration..."
    systemctl daemon-reload
    systemctl restart fleet-server.service || rollback "fleet-server failed to restart after unit convergence"
    local srv_ok=0 srv_code=""
    for _ in $(seq 1 15); do
      if systemctl is-active --quiet fleet-server.service; then
        srv_code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${FLEET_SERVER_PORT}/" || true)"
        if [ "$srv_code" = "200" ]; then
          srv_ok=1
          break
        fi
      fi
      sleep 1
    done
    [ "$srv_ok" -eq 1 ] || rollback "fleet-server health check failed after unit convergence"
  fi
}

converge_victoriametrics
converge_grafana
converge_fleet_server_unit

# Converge proxy configs, systemd units, and binaries/images (preserving /var/lib/fleet-proxy/).
if [ "$SKIP_PROXY" -eq 0 ]; then
  install -d -m 0755 "$FLEET_PROXY_DIR" "$FLEET_PROXY_DIR/authelia"
  proxy_cfg_changed=0
  proxy_unit_changed=0
  proxy_bin_changed=0

  # Back up existing proxy configs before re-rendering.
  [ -f "$FLEET_PROXY_DIR/Caddyfile" ] && cp -a "$FLEET_PROXY_DIR/Caddyfile" "$BACKUP_DIR/proxy/Caddyfile"
  [ -f "$FLEET_PROXY_DIR/authelia/configuration.yml" ] && cp -a "$FLEET_PROXY_DIR/authelia/configuration.yml" "$BACKUP_DIR/proxy/authelia/configuration.yml"
  [ -f "$FLEET_PROXY_DIR/compose.yml" ] && cp -a "$FLEET_PROXY_DIR/compose.yml" "$BACKUP_DIR/proxy/compose.yml"

  if [ -n "$FLEET_PANEL_DOMAIN" ]; then
    say "re-rendering proxy configs from $ANSWERS_FILE (domain=$FLEET_PANEL_DOMAIN, mode=$FLEET_DOMAIN_MODE, proxy=$FLEET_PROXY_MODE)..."
    render_caddyfile > "$TMP_DIR/Caddyfile"
    render_authelia_config > "$TMP_DIR/configuration.yml"
    render_compose_yml > "$TMP_DIR/compose.yml"

    CHANGED=0
    put_file 0644 "$TMP_DIR/Caddyfile" "$FLEET_PROXY_DIR/Caddyfile.example"
    put_file 0644 "$TMP_DIR/configuration.yml" "$FLEET_PROXY_DIR/authelia/configuration.yml.example"
    put_file 0644 "$TMP_DIR/compose.yml" "$FLEET_PROXY_DIR/compose.yml.example"

    CHANGED=0
    put_file 0644 "$TMP_DIR/Caddyfile" "$FLEET_PROXY_DIR/Caddyfile"
    put_file 0644 "$TMP_DIR/configuration.yml" "$FLEET_PROXY_DIR/authelia/configuration.yml"
    proxy_cfg_changed="$CHANGED"
  else
    warn "FLEET_PANEL_DOMAIN could not be determined; preserving existing Caddyfile and Authelia configuration.yml"
  fi

  mkdir -p "$TMP_DIR/proxy-units/etc/systemd/system"
  (DESTDIR="$TMP_DIR/proxy-units" render_proxy_systemd_units)
  CHANGED=0
  for unit_file in "$TMP_DIR/proxy-units/etc/systemd/system"/*.service; do
    [ -f "$unit_file" ] || continue
    unit_name="$(basename "$unit_file")"
    [ -f "/etc/systemd/system/$unit_name" ] && cp -a "/etc/systemd/system/$unit_name" "$BACKUP_DIR/systemd/$unit_name"
    put_file 0644 "$unit_file" "/etc/systemd/system/$unit_name"
  done
  proxy_unit_changed="$CHANGED"

  proxy_active_check=1
  if [ "$FLEET_PROXY_MODE" = "docker" ]; then
    if [ -f "$FLEET_PROXY_DIR/compose.yml" ]; then
      say "upgrading Docker proxy in $FLEET_PROXY_DIR (state in $FLEET_PROXY_STATE_DIR preserved)..."
      docker compose -f "$FLEET_PROXY_DIR/compose.yml" pull || rollback "docker compose pull failed"
      docker compose -f "$FLEET_PROXY_DIR/compose.yml" up -d --remove-orphans || rollback "docker compose up failed"
      if [ "$proxy_cfg_changed" -eq 1 ]; then
        docker compose -f "$FLEET_PROXY_DIR/compose.yml" restart || rollback "docker compose restart failed after config refresh"
      fi
      [ "$proxy_unit_changed" -eq 1 ] && systemctl daemon-reload
      systemctl is-enabled --quiet fleet-proxy.service >/dev/null 2>&1 || systemctl enable fleet-proxy.service >/dev/null 2>&1 || true
      if [ "$proxy_unit_changed" -eq 1 ] || ! systemctl is-active --quiet fleet-proxy.service; then
        systemctl restart fleet-proxy.service || rollback "fleet-proxy.service failed to start"
      fi
    else
      warn "$FLEET_PROXY_DIR/compose.yml not found; skipping Docker proxy upgrade"
      proxy_active_check=0
    fi
  else
    say "checking native proxy binaries (Caddy v${CADDY_VERSION}, Authelia v${AUTHELIA_VERSION})..."
    if [ -x /usr/local/bin/caddy ] && ! /usr/local/bin/caddy version 2>/dev/null | grep -q "v${CADDY_VERSION}"; then
      cp -a /usr/local/bin/caddy "$BACKUP_DIR/bin/caddy"
    fi
    if [ -x /usr/local/bin/authelia ] && ! /usr/local/bin/authelia --version 2>/dev/null | grep -q "${AUTHELIA_VERSION}"; then
      cp -a /usr/local/bin/authelia "$BACKUP_DIR/bin/authelia"
    fi

    CHANGED=0
    install_native_proxy_binaries || rollback "native proxy binary upgrade failed"
    proxy_bin_changed="$CHANGED"

    systemctl is-enabled --quiet fleet-proxy.service >/dev/null 2>&1 || systemctl enable fleet-proxy.service >/dev/null 2>&1 || true
    systemctl is-enabled --quiet fleet-authelia.service >/dev/null 2>&1 || systemctl enable fleet-authelia.service >/dev/null 2>&1 || true
    systemctl is-enabled --quiet fleet-caddy.service >/dev/null 2>&1 || systemctl enable fleet-caddy.service >/dev/null 2>&1 || true

    if [ "$proxy_bin_changed" -eq 1 ] || [ "$proxy_cfg_changed" -eq 1 ] || [ "$proxy_unit_changed" -eq 1 ] || ! systemctl is-active --quiet fleet-proxy.service; then
      chown -R "$PROXY_USER:$PROXY_USER" "$FLEET_PROXY_STATE_DIR" 2>/dev/null || true
      systemctl daemon-reload
      systemctl restart fleet-proxy.service || rollback "fleet-proxy.service failed to restart"
    fi
  fi

  if [ "$proxy_active_check" -eq 1 ]; then
    auth_health_path="/api/health"
    [ "$FLEET_DOMAIN_MODE" = "single" ] && auth_health_path="/authelia/api/health"
    proxy_ok=0
    for _ in $(seq 1 15); do
      units_ok=1
      systemctl is-active --quiet fleet-proxy.service || units_ok=0
      if [ "$FLEET_PROXY_MODE" = "native" ]; then
        systemctl is-active --quiet fleet-authelia.service || units_ok=0
        systemctl is-active --quiet fleet-caddy.service || units_ok=0
      fi
      if [ "$units_ok" -eq 1 ] && curl -fsS "http://127.0.0.1:${FLEET_AUTH_PORT}${auth_health_path}" >/dev/null 2>&1; then
        if [ -n "$FLEET_PANEL_DOMAIN" ]; then
          caddy_code="$(curl -sk --max-time 5 -o /dev/null -w '%{http_code}' \
            --resolve "${FLEET_PANEL_DOMAIN}:${FLEET_PANEL_PORT}:127.0.0.1" \
            "https://${FLEET_PANEL_DOMAIN}:${FLEET_PANEL_PORT}/grafana/" || true)"
          if [ -n "$caddy_code" ] && [ "$caddy_code" != "000" ]; then
            proxy_ok=1
            break
          fi
        else
          proxy_ok=1
          break
        fi
      fi
      sleep 1
    done
    [ "$proxy_ok" -eq 1 ] || rollback "proxy health check failed after upgrade (check journalctl -u fleet-proxy -u fleet-caddy -u fleet-authelia)"
    say "proxy converged and healthy ($FLEET_PROXY_MODE)"
  fi
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
