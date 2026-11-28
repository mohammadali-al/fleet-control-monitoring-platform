#!/usr/bin/env bash
# Install or upgrade the Fleet main server and its auth proxy (T-0024 / T-0036).
# Safe to re-run and runnable via: curl -fsSL ... | sudo bash
#
# Usage:
#   sudo deploy/install-server.sh [OPTIONS]
#
# Modes:
#   --check                  Run preflight checks only and exit
#   --upgrade                Upgrade an existing installation (delegates to upgrade-server.sh)
#   --proxy native|docker    Auth proxy mode: native (standalone binaries, default) or docker (containers)
#                            or native (standalone Caddy+Authelia binaries + systemd units)
#   --yes, -y                Auto-approve installing missing OS packages
#   --no-install             Do not install missing OS packages; fail if any are missing
#   --answers FILE           Load answers from env file (non-interactive capable)
#   --non-interactive        Never prompt; require answers via flags/--answers/defaults
#   --bin-dir DIR            Use local built binaries instead of downloading a GitHub Release
#   --no-start               Install files without enabling/starting systemd services
#
# Release & auth options:
#   --version TAG            Release tag to install (default: latest)
#   --repo OWNER/REPO        GitHub repo slug (default: mohammadali-al/fleet-control-monitoring-platform)
#   --token TOKEN            GitHub token for private repos / API rate limits (or GH_TOKEN / GITHUB_TOKEN)
#
# Configuration overrides (also settable in --answers FILE):
#   --single-domain          Serve Authelia under /authelia on the panel domain (one DNS name; default)
#   --two-domain             Serve Authelia on separate --auth-domain
#   --panel-domain DOMAIN    Panel hostname (default prompt: fleet.internal)
#   --auth-domain DOMAIN     Authelia login hostname (default: auth.<panel-domain>; unused in --single-domain)
#   --tls-mode internal|acme TLS certificate mode (default: internal)
#   --acme-email EMAIL       Contact email for public ACME certificates
#   --admin-user USER        Initial Authelia admin username (default: admin)
#   --admin-display NAME     Initial Authelia admin display name (default: Fleet Admin)
#   --admin-email EMAIL      Initial Authelia admin email
#   --admin-password PASS    Initial Authelia admin password (prefer interactive or --admin-password-file)
#   --admin-password-file F  Read initial Authelia admin password from file F
#   --panel-port PORT        Proxy HTTPS port (default: 443)
#   --server-port PORT       Loopback fleet-server port (default: 6789)
#   --agent-port PORT        Agent control channel port (default: 7443)
#   --auth-port PORT         Loopback Authelia port (default: 9091)
#   --tsdb-port PORT         Loopback VictoriaMetrics port (default: 8428)
#   --grafana-port PORT      Loopback Grafana port (default: 3000)
#   --data-dir DIR           fleet-server data directory (default: /var/lib/fleet-server)
#   --tsdb-dir DIR           VictoriaMetrics data directory (default: /var/lib/fleet-victoriametrics)
#   --tsdb-retention PERIOD  VictoriaMetrics retention period (default: 90d)
#   --grafana-dir DIR        Grafana data directory (default: /var/lib/fleet-grafana)
#   --proxy-dir DIR          Proxy configuration directory (default: /opt/fleet/proxy)
#   --proxy-state-dir DIR    Proxy mutable state directory (default: /var/lib/fleet-proxy)
#
# Staging:
#   DESTDIR=/some/dir        Stage files into DESTDIR without user creation or systemd calls.
set -euo pipefail

DEFAULT_REPO="mohammadali-al/fleet-control-monitoring-platform"
CADDY_VERSION="2.10.0"
AUTHELIA_VERSION="4.39.27"
VICTORIAMETRICS_VERSION="1.157.0"
VICTORIAMETRICS_SHA256_AMD64="7b31192da6cf805b8b9244a904b022948e8022a067a822c30cb9f94600d0c387"
VICTORIAMETRICS_SHA256_ARM64="e5368dec72d212dcb2b994345ab872245299000d37197f723123e63a50e573b5"
GRAFANA_VERSION="11.6.1"
GRAFANA_SHA256_AMD64="35a5efca8c535ce961217765faf8d6895c6d1c1587eaeec50d606a5cb218603b"
GRAFANA_SHA256_ARM64="6b45fc6c2e534aee02220c1e8ed792bf4bb44ceac2da5ba0cd66e9579496f73e"

SCRIPT_DIR=""
if [ -n "${BASH_SOURCE[0]:-}" ] && [ -f "${BASH_SOURCE[0]}" ]; then
  SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
fi

MODE_CHECK=0
MODE_UPGRADE=0
YES_INSTALL=0
NO_INSTALL=0
NON_INTERACTIVE=0
START=1
ANSWERS_FILE=""
BIN_DIR=""
TOKEN="${GH_TOKEN:-${GITHUB_TOKEN:-}}"
ADMIN_PASSWORD=""
ADMIN_PASSWORD_FILE=""
UPGRADE_ARGS=()

# Configurable answers (empty means "not explicitly set yet").
FLEET_REPO="${FLEET_REPO:-$DEFAULT_REPO}"
FLEET_INSTALL_VERSION="${FLEET_INSTALL_VERSION:-latest}"
FLEET_PROXY_MODE="${FLEET_PROXY_MODE:-}"
FLEET_DOMAIN_MODE="${FLEET_DOMAIN_MODE:-}"
FLEET_PANEL_DOMAIN="${FLEET_PANEL_DOMAIN:-}"
FLEET_AUTH_DOMAIN="${FLEET_AUTH_DOMAIN:-}"
FLEET_TLS_MODE="${FLEET_TLS_MODE:-}"
FLEET_ACME_EMAIL="${FLEET_ACME_EMAIL:-}"
FLEET_ADMIN_USER="${FLEET_ADMIN_USER:-}"
FLEET_ADMIN_DISPLAY_NAME="${FLEET_ADMIN_DISPLAY_NAME:-}"
FLEET_ADMIN_EMAIL="${FLEET_ADMIN_EMAIL:-}"
FLEET_PANEL_PORT="${FLEET_PANEL_PORT:-}"
FLEET_SERVER_PORT="${FLEET_SERVER_PORT:-}"
FLEET_AGENT_PORT="${FLEET_AGENT_PORT:-}"
FLEET_AGENT_DIAL_PORT="${FLEET_AGENT_DIAL_PORT:-}"
FLEET_ENROLL_PORT="${FLEET_ENROLL_PORT:-}"
FLEET_AUTH_PORT="${FLEET_AUTH_PORT:-}"
FLEET_TSDB_PORT="${FLEET_TSDB_PORT:-}"
FLEET_GRAFANA_PORT="${FLEET_GRAFANA_PORT:-}"
FLEET_DATA_DIR="${FLEET_DATA_DIR:-}"
FLEET_TSDB_DIR="${FLEET_TSDB_DIR:-}"
FLEET_TSDB_RETENTION="${FLEET_TSDB_RETENTION:-}"
FLEET_GRAFANA_DIR="${FLEET_GRAFANA_DIR:-}"
FLEET_GRAFANA_CONF_DIR="${FLEET_GRAFANA_CONF_DIR:-/etc/fleet-grafana}"
FLEET_GRAFANA_HOME="${FLEET_GRAFANA_HOME:-/usr/local/share/grafana}"
FLEET_PROXY_DIR="${FLEET_PROXY_DIR:-}"
FLEET_PROXY_STATE_DIR="${FLEET_PROXY_STATE_DIR:-}"
FLEET_CONF_DIR="${FLEET_CONF_DIR:-/etc/fleet-server}"
FLEET_SERVER_BIN="${FLEET_SERVER_BIN:-/usr/local/bin/fleet-server}"
FLEET_AGENT_BIN="${FLEET_AGENT_BIN:-/usr/local/share/fleet/fleet-agent}"
FLEET_AGENT_DIR="${FLEET_AGENT_DIR:-/usr/local/share/fleet/agents}"

say() { echo "install-server: $*"; }
warn() { echo "install-server: WARN: $*" >&2; }
die() { echo "install-server: ERROR: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: sudo deploy/install-server.sh [OPTIONS]

Modes:
  --check                  Run preflight checks only and exit
  --upgrade                Upgrade an existing installation (delegates to upgrade-server.sh)
  --proxy native|docker    Auth proxy mode: native (default, no Docker) or docker (containers)
  --yes, -y                Auto-approve installing missing OS packages
  --no-install             Do not install missing OS packages; fail if any are missing
  --answers FILE           Load answers from env file (non-interactive capable)
  --non-interactive        Never prompt; require answers via flags/--answers/defaults
  --bin-dir DIR            Use local built binaries instead of downloading a GitHub Release
  --no-start               Install files without enabling/starting systemd services

Release & auth options:
  --version TAG            Release tag to install (default: latest)
  --repo OWNER/REPO        GitHub repo slug (default: mohammadali-al/fleet-control-monitoring-platform)
  --token TOKEN            GitHub token for private repos / API rate limits (or GH_TOKEN / GITHUB_TOKEN)

Configuration overrides (also settable in --answers FILE):
  --single-domain          Serve Authelia under /authelia on the panel domain (one DNS name; default)
  --two-domain             Serve Authelia on separate --auth-domain
  --panel-domain DOMAIN    Panel hostname (default prompt: fleet.internal)
  --auth-domain DOMAIN     Authelia login hostname (default: auth.<panel-domain>; unused in --single-domain)
  --tls-mode internal|acme TLS certificate mode (default: internal)
  --acme-email EMAIL       Contact email for public ACME certificates
  --admin-user USER        Initial Authelia admin username (default: admin)
  --admin-display NAME     Initial Authelia admin display name (default: Fleet Admin)
  --admin-email EMAIL      Initial Authelia admin email
  --admin-password PASS    Initial Authelia admin password (prefer interactive or --admin-password-file)
  --admin-password-file F  Read initial Authelia admin password from file F
  --panel-port PORT        Proxy HTTPS port (default: 443)
  --server-port PORT       Loopback fleet-server port (default: 6789)
  --agent-port PORT        Agent control channel port (default: 7443)
  --auth-port PORT         Loopback Authelia port (default: 9091)
  --tsdb-port PORT         Loopback VictoriaMetrics port (default: 8428)
  --grafana-port PORT      Loopback Grafana port (default: 3000)
  --data-dir DIR           fleet-server data directory (default: /var/lib/fleet-server)
  --tsdb-dir DIR           VictoriaMetrics data directory (default: /var/lib/fleet-victoriametrics)
  --tsdb-retention PERIOD  VictoriaMetrics retention period (default: 90d)
  --grafana-dir DIR        Grafana data directory (default: /var/lib/fleet-grafana)
  --proxy-dir DIR          Proxy configuration directory (default: /opt/fleet/proxy)
  --proxy-state-dir DIR    Proxy mutable state directory (default: /var/lib/fleet-proxy)

Staging:
  DESTDIR=/some/dir        Stage files into DESTDIR without user creation or systemd calls.
EOF
}

need_val() {
  [ "$#" -ge 2 ] || die "flag $1 requires a value"
}
parse_args() {
while [ "$#" -gt 0 ]; do
  case "$1" in
    --check) MODE_CHECK=1; UPGRADE_ARGS+=("--check"); shift ;;
    --upgrade) MODE_UPGRADE=1; shift ;;
    --proxy) need_val "$@"; FLEET_PROXY_MODE="$2"; shift 2 ;;
    --proxy=*) FLEET_PROXY_MODE="${1#--proxy=}"; shift ;;
    -y|--yes) YES_INSTALL=1; UPGRADE_ARGS+=("--yes"); shift ;;
    --no-install) NO_INSTALL=1; shift ;;
    --answers) need_val "$@"; ANSWERS_FILE="$2"; shift 2 ;;
    --non-interactive) NON_INTERACTIVE=1; UPGRADE_ARGS+=("--non-interactive"); shift ;;
    --bin-dir) need_val "$@"; BIN_DIR="$2"; UPGRADE_ARGS+=("--bin-dir" "$2"); shift 2 ;;
    --no-start) START=0; shift ;;
    --version) need_val "$@"; FLEET_INSTALL_VERSION="$2"; UPGRADE_ARGS+=("--version" "$2"); shift 2 ;;
    --repo) need_val "$@"; FLEET_REPO="$2"; UPGRADE_ARGS+=("--repo" "$2"); shift 2 ;;
    --token) need_val "$@"; TOKEN="$2"; UPGRADE_ARGS+=("--token" "$2"); shift 2 ;;
    --single-domain) FLEET_DOMAIN_MODE="single"; shift ;;
    --two-domain) FLEET_DOMAIN_MODE="two"; shift ;;
    --panel-domain) need_val "$@"; FLEET_PANEL_DOMAIN="$2"; shift 2 ;;
    --auth-domain) need_val "$@"; FLEET_AUTH_DOMAIN="$2"; shift 2 ;;
    --tls-mode) need_val "$@"; FLEET_TLS_MODE="$2"; shift 2 ;;
    --acme-email) need_val "$@"; FLEET_ACME_EMAIL="$2"; shift 2 ;;
    --admin-user) need_val "$@"; FLEET_ADMIN_USER="$2"; shift 2 ;;
    --admin-display) need_val "$@"; FLEET_ADMIN_DISPLAY_NAME="$2"; shift 2 ;;
    --admin-email) need_val "$@"; FLEET_ADMIN_EMAIL="$2"; shift 2 ;;
    --admin-password) need_val "$@"; ADMIN_PASSWORD="$2"; shift 2 ;;
    --admin-password-file) need_val "$@"; ADMIN_PASSWORD_FILE="$2"; shift 2 ;;
    --panel-port) need_val "$@"; FLEET_PANEL_PORT="$2"; shift 2 ;;
    --server-port) need_val "$@"; FLEET_SERVER_PORT="$2"; shift 2 ;;
    --agent-port) need_val "$@"; FLEET_AGENT_PORT="$2"; shift 2 ;;
    --auth-port) need_val "$@"; FLEET_AUTH_PORT="$2"; shift 2 ;;
    --tsdb-port) need_val "$@"; FLEET_TSDB_PORT="$2"; shift 2 ;;
    --grafana-port) need_val "$@"; FLEET_GRAFANA_PORT="$2"; shift 2 ;;
    --data-dir) need_val "$@"; FLEET_DATA_DIR="$2"; shift 2 ;;
    --tsdb-dir) need_val "$@"; FLEET_TSDB_DIR="$2"; shift 2 ;;
    --tsdb-retention) need_val "$@"; FLEET_TSDB_RETENTION="$2"; shift 2 ;;
    --grafana-dir) need_val "$@"; FLEET_GRAFANA_DIR="$2"; shift 2 ;;
    --proxy-dir) need_val "$@"; FLEET_PROXY_DIR="$2"; shift 2 ;;
    --proxy-state-dir) need_val "$@"; FLEET_PROXY_STATE_DIR="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1 (run with --help)" ;;
  esac
done
}

TMP_DIR=""
cleanup() {
  if [ -n "$TMP_DIR" ] && [ -d "$TMP_DIR" ]; then
    rm -rf "$TMP_DIR"
  fi
}

make_tmp_dir() {
  if [ -z "$TMP_DIR" ]; then
    TMP_DIR="$(mktemp -d /tmp/fleet-install.XXXXXX)"
  fi
}

# Safely load KEY=VALUE env files without arbitrary shell execution.
load_answers_file() {
  local file="$1"
  [ -f "$file" ] || die "answers file not found: $file"
  local line key val
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line#"${line%%[![:space:]]*}"}"
    [ -z "$line" ] && continue
    case "$line" in
      \#*) continue ;;
    esac
    key="${line%%=*}"
    val="${line#*=}"
    # Strip matching surrounding quotes.
    case "$val" in
      \"*\") val="${val#\"}"; val="${val%\"}" ;;
      \'*\') val="${val#\'}"; val="${val%\'}" ;;
    esac
    case "$key" in
      FLEET_REPO) [ "$FLEET_REPO" = "$DEFAULT_REPO" ] && FLEET_REPO="$val" ;;
      FLEET_INSTALL_VERSION) [ "$FLEET_INSTALL_VERSION" = "latest" ] && FLEET_INSTALL_VERSION="$val" ;;
      FLEET_PROXY_MODE) [ -z "$FLEET_PROXY_MODE" ] && FLEET_PROXY_MODE="$val" ;;
      FLEET_DOMAIN_MODE) [ -z "$FLEET_DOMAIN_MODE" ] && FLEET_DOMAIN_MODE="$val" ;;
      FLEET_PANEL_DOMAIN) [ -z "$FLEET_PANEL_DOMAIN" ] && FLEET_PANEL_DOMAIN="$val" ;;
      FLEET_AUTH_DOMAIN) [ -z "$FLEET_AUTH_DOMAIN" ] && FLEET_AUTH_DOMAIN="$val" ;;
      FLEET_TLS_MODE) [ -z "$FLEET_TLS_MODE" ] && FLEET_TLS_MODE="$val" ;;
      FLEET_ACME_EMAIL) [ -z "$FLEET_ACME_EMAIL" ] && FLEET_ACME_EMAIL="$val" ;;
      FLEET_ADMIN_USER) [ -z "$FLEET_ADMIN_USER" ] && FLEET_ADMIN_USER="$val" ;;
      FLEET_ADMIN_DISPLAY_NAME) [ -z "$FLEET_ADMIN_DISPLAY_NAME" ] && FLEET_ADMIN_DISPLAY_NAME="$val" ;;
      FLEET_ADMIN_EMAIL) [ -z "$FLEET_ADMIN_EMAIL" ] && FLEET_ADMIN_EMAIL="$val" ;;
      FLEET_PANEL_PORT) [ -z "$FLEET_PANEL_PORT" ] && FLEET_PANEL_PORT="$val" ;;
      FLEET_SERVER_PORT) [ -z "$FLEET_SERVER_PORT" ] && FLEET_SERVER_PORT="$val" ;;
      FLEET_AGENT_PORT) [ -z "$FLEET_AGENT_PORT" ] && FLEET_AGENT_PORT="$val" ;;
      FLEET_AGENT_DIAL_PORT) [ -z "$FLEET_AGENT_DIAL_PORT" ] && FLEET_AGENT_DIAL_PORT="$val" ;;
      FLEET_ENROLL_PORT) [ -z "$FLEET_ENROLL_PORT" ] && FLEET_ENROLL_PORT="$val" ;;
      FLEET_AUTH_PORT) [ -z "$FLEET_AUTH_PORT" ] && FLEET_AUTH_PORT="$val" ;;
      FLEET_TSDB_PORT) [ -z "$FLEET_TSDB_PORT" ] && FLEET_TSDB_PORT="$val" ;;
      FLEET_GRAFANA_PORT) [ -z "$FLEET_GRAFANA_PORT" ] && FLEET_GRAFANA_PORT="$val" ;;
      FLEET_DATA_DIR) [ -z "$FLEET_DATA_DIR" ] && FLEET_DATA_DIR="$val" ;;
      FLEET_TSDB_DIR) [ -z "$FLEET_TSDB_DIR" ] && FLEET_TSDB_DIR="$val" ;;
      FLEET_TSDB_RETENTION) [ -z "$FLEET_TSDB_RETENTION" ] && FLEET_TSDB_RETENTION="$val" ;;
      FLEET_GRAFANA_DIR) [ -z "$FLEET_GRAFANA_DIR" ] && FLEET_GRAFANA_DIR="$val" ;;
      FLEET_PROXY_DIR) [ -z "$FLEET_PROXY_DIR" ] && FLEET_PROXY_DIR="$val" ;;
      FLEET_PROXY_STATE_DIR) [ -z "$FLEET_PROXY_STATE_DIR" ] && FLEET_PROXY_STATE_DIR="$val" ;;
      FLEET_ADMIN_PASSWORD) [ -z "$ADMIN_PASSWORD" ] && ADMIN_PASSWORD="$val" ;;
      *) ;;
    esac
  done < "$file"
}

handle_upgrade_delegation() {
  if [ -n "$ANSWERS_FILE" ]; then
    load_answers_file "$ANSWERS_FILE"
  fi
  if [ -n "$ADMIN_PASSWORD_FILE" ]; then
    [ -r "$ADMIN_PASSWORD_FILE" ] || die "cannot read password file: $ADMIN_PASSWORD_FILE"
    ADMIN_PASSWORD="$(tr -d '\r\n' < "$ADMIN_PASSWORD_FILE")"
  fi

  # Delegate --upgrade cleanly to upgrade-server.sh (local or installed or downloaded).
  if [ "$MODE_UPGRADE" -eq 1 ]; then
    local upgrade_script=""
    if [ -n "$SCRIPT_DIR" ] && [ -x "$SCRIPT_DIR/upgrade-server.sh" ]; then
      upgrade_script="$SCRIPT_DIR/upgrade-server.sh"
    elif [ -x /usr/local/lib/fleet/upgrade-server.sh ]; then
      upgrade_script="/usr/local/lib/fleet/upgrade-server.sh"
    else
      make_tmp_dir
      upgrade_script="$TMP_DIR/upgrade-server.sh"
      local api_url="https://api.github.com/repos/$FLEET_REPO/releases/latest"
      if [ "$FLEET_INSTALL_VERSION" != "latest" ]; then
        api_url="https://api.github.com/repos/$FLEET_REPO/releases/tags/$FLEET_INSTALL_VERSION"
      fi
      local hdrs=(-H "Accept: application/vnd.github+json")
      [ -n "$TOKEN" ] && hdrs+=(-H "Authorization: Bearer $TOKEN")
      local rel_json asset_url=""
      rel_json="$(curl -fsSL "${hdrs[@]}" "$api_url")" || die "failed to query release API for upgrade-server.sh"
      if command -v python3 >/dev/null 2>&1; then
        asset_url="$(printf '%s' "$rel_json" | python3 -c '
import json, sys
d = json.load(sys.stdin)
for a in d.get("assets", []):
    if a.get("name") == "upgrade-server.sh":
        print(a.get("url") if sys.argv[1] else a.get("browser_download_url"))
        break
' "$TOKEN")"
      fi
      [ -n "$asset_url" ] || die "upgrade-server.sh not found locally or in release $FLEET_INSTALL_VERSION"
      if [ -n "$TOKEN" ]; then
        curl -fsSL -H "Authorization: Bearer $TOKEN" -H "Accept: application/octet-stream" "$asset_url" -o "$upgrade_script"
      else
        curl -fsSL "$asset_url" -o "$upgrade_script"
      fi
      chmod 0755 "$upgrade_script"
    fi
    if [ "${#UPGRADE_ARGS[@]}" -gt 0 ]; then
      exec bash "$upgrade_script" "${UPGRADE_ARGS[@]}"
    else
      exec bash "$upgrade_script"
    fi
  fi
}

DESTDIR="${DESTDIR:-}"
FLEET_PROXY_MODE="${FLEET_PROXY_MODE:-native}"

validate_install_modes() {
  case "$FLEET_PROXY_MODE" in
    docker|native) ;;
    *) die "invalid --proxy mode '$FLEET_PROXY_MODE' (want: docker or native)" ;;
  esac

  if [ -n "$FLEET_DOMAIN_MODE" ]; then
    case "$FLEET_DOMAIN_MODE" in
      two|single) ;;
      *) die "invalid domain mode '$FLEET_DOMAIN_MODE' (want: two or single)" ;;
    esac
  fi
}

is_ip_literal() {
  local host="$1"
  if [[ "$host" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || [[ "$host" == *:* ]]; then
    return 0
  fi
  return 1
}

FLEET_PANEL_PORT="${FLEET_PANEL_PORT:-443}"
FLEET_SERVER_PORT="${FLEET_SERVER_PORT:-6789}"
FLEET_AGENT_PORT="${FLEET_AGENT_PORT:-7443}"
FLEET_AGENT_DIAL_PORT="${FLEET_AGENT_DIAL_PORT:-7444}"
FLEET_ENROLL_PORT="${FLEET_ENROLL_PORT:-7445}"
FLEET_AUTH_PORT="${FLEET_AUTH_PORT:-9091}"
FLEET_TSDB_PORT="${FLEET_TSDB_PORT:-8428}"
FLEET_GRAFANA_PORT="${FLEET_GRAFANA_PORT:-3000}"
FLEET_DATA_DIR="${FLEET_DATA_DIR:-/var/lib/fleet-server}"
FLEET_TSDB_DIR="${FLEET_TSDB_DIR:-/var/lib/fleet-victoriametrics}"
FLEET_TSDB_RETENTION="${FLEET_TSDB_RETENTION:-90d}"
FLEET_GRAFANA_DIR="${FLEET_GRAFANA_DIR:-/var/lib/fleet-grafana}"
FLEET_PROXY_DIR="${FLEET_PROXY_DIR:-/opt/fleet/proxy}"
FLEET_PROXY_STATE_DIR="${FLEET_PROXY_STATE_DIR:-/var/lib/fleet-proxy}"

SVC_USER="fleet"
PROXY_USER="fleet-proxy"
TSDB_USER="fleet-tsdb"
GRAFANA_USER="fleet-grafana"

has_tty() {
  [ "$NON_INTERACTIVE" -eq 0 ] && [ -r /dev/tty ] && [ -w /dev/tty ]
}

prompt_val() {
  local prompt_text="$1" default_val="$2" var_name="$3" ans=""
  if [ "$NON_INTERACTIVE" -eq 1 ]; then
    if [ -n "$default_val" ]; then
      printf -v "$var_name" '%s' "$default_val"
      return 0
    fi
    die "missing required value for $var_name in non-interactive mode"
  fi
  has_tty || die "no interactive TTY available for '$prompt_text'; pass --answers or flags"
  if [ -n "$default_val" ]; then
    printf "%s [%s]: " "$prompt_text" "$default_val" >/dev/tty
  else
    printf "%s: " "$prompt_text" >/dev/tty
  fi
  IFS= read -r ans </dev/tty || true
  if [ -z "$ans" ]; then
    ans="$default_val"
  fi
  printf -v "$var_name" '%s' "$ans"
}

prompt_secret() {
  local prompt_text="$1" var_name="$2" p1="" p2=""
  has_tty || die "no interactive TTY available for '$prompt_text'; pass --admin-password or --admin-password-file"
  while true; do
    printf "%s: " "$prompt_text" >/dev/tty
    IFS= read -rs p1 </dev/tty || true
    printf "\n" >/dev/tty
    [ -n "$p1" ] || { printf "Password cannot be empty.\n" >/dev/tty; continue; }
    printf "Confirm %s: " "$prompt_text" >/dev/tty
    IFS= read -rs p2 </dev/tty || true
    printf "\n" >/dev/tty
    if [ "$p1" = "$p2" ]; then
      printf -v "$var_name" '%s' "$p1"
      return 0
    fi
    printf "Passwords did not match. Try again.\n" >/dev/tty
  done
}

# ---- Preflight & OS package installation ----

detect_pkg_mgr() {
  if command -v apt-get >/dev/null 2>&1; then echo "apt"
  elif command -v dnf >/dev/null 2>&1; then echo "dnf"
  elif command -v yum >/dev/null 2>&1; then echo "yum"
  elif command -v zypper >/dev/null 2>&1; then echo "zypper"
  elif command -v pacman >/dev/null 2>&1; then echo "pacman"
  else echo "unknown"
  fi
}

kernel_at_least() {
  local want_maj="$1" want_min="$2" rel maj min
  rel="$(uname -r)"
  maj="${rel%%.*}"
  min="${rel#*.}"; min="${min%%.*}"; min="${min%%-*}"
  if [ "$maj" -gt "$want_maj" ] 2>/dev/null; then return 0; fi
  if [ "$maj" -eq "$want_maj" ] 2>/dev/null && [ "$min" -ge "$want_min" ] 2>/dev/null; then return 0; fi
  return 1
}

check_port_state() {
  local port="$1" kind="$2" lines proc_info=""
  if ! command -v ss >/dev/null 2>&1; then
    return 0
  fi
  lines="$(ss -H -ltnp "sport = :$port" 2>/dev/null || ss -H -ltn "sport = :$port" 2>/dev/null || true)"
  if [ -z "$lines" ]; then
    say "preflight: port $port/tcp is free"
    return 0
  fi
  proc_info="$(printf '%s\n' "$lines" | head -n1)"
  if [ "$kind" = "ssh" ]; then
    if printf '%s' "$proc_info" | grep -qiE 'sshd|ssh'; then
      say "preflight: port $port/tcp is used by sshd (OK)"
      return 0
    fi
    warn "port $port/tcp is occupied: $proc_info"
    return 0
  fi
  # Allow existing Fleet components on re-run.
  if printf '%s' "$proc_info" | grep -qiE 'fleet-server|caddy|authelia|docker-proxy|victoria-metrics|grafana'; then
    say "preflight: port $port/tcp is already held by a Fleet/proxy process (re-run OK)"
    return 0
  fi
  echo "preflight: FAIL - port $port/tcp is already in use: $proc_info" >&2
  return 1
}

install_os_packages() {
  local need_docker="$1"
  shift
  local missing_cmds=("$@")
  local mgr pkgs=()
  mgr="$(detect_pkg_mgr)"
  [ "$mgr" != "unknown" ] || die "cannot auto-install missing dependencies (${missing_cmds[*]}): unsupported package manager"

  for c in "${missing_cmds[@]}"; do
    case "$c" in
      openssl) pkgs+=("openssl") ;;
      nft) pkgs+=("nftables") ;;
      curl) pkgs+=("curl") ;;
      tar) pkgs+=("tar") ;;
      sha256sum) pkgs+=("coreutils") ;;
      ss)
        case "$mgr" in
          apt|dnf|yum|pacman) pkgs+=("iproute2") ;;
          zypper) pkgs+=("iproute2") ;;
        esac
        ;;
      ca-certificates) pkgs+=("ca-certificates") ;;
    esac
  done

  if [ "$need_docker" -eq 1 ]; then
    case "$mgr" in
      apt) pkgs+=("docker.io") ;;
      dnf|yum) pkgs+=("docker" "docker-compose-plugin") ;;
      zypper) pkgs+=("docker" "docker-compose") ;;
      pacman) pkgs+=("docker" "docker-compose") ;;
    esac
  fi

  local msg="Missing OS packages to install ($mgr): ${pkgs[*]}"
  [ "$need_docker" -eq 1 ] && msg="$msg (+ docker compose plugin)"
  if [ "$NO_INSTALL" -eq 1 ]; then
    die "$msg (--no-install specified)"
  fi
  if [ "$YES_INSTALL" -ne 1 ]; then
    if has_tty; then
      local ans=""
      printf "%s. Install now? [y/N]: " "$msg" >/dev/tty
      IFS= read -r ans </dev/tty || true
      case "$ans" in
        y|Y|yes|YES) ;;
        *) die "declined installing required OS packages ($msg)" ;;
      esac
    else
      die "$msg (pass --yes to auto-install or install manually)"
    fi
  fi

  say "installing OS packages via $mgr: ${pkgs[*]}"
  case "$mgr" in
    apt)
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq "${pkgs[@]}"
      if [ "$need_docker" -eq 1 ] && ! docker compose version >/dev/null 2>&1; then
        DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-compose-v2 2>/dev/null || \
          DEBIAN_FRONTEND=noninteractive apt-get install -y -qq docker-compose-plugin 2>/dev/null || \
          die "Docker Compose v2 plugin could not be installed via apt-get; install docker-compose-plugin/docker-compose-v2 or use --proxy=native"
      fi
      ;;
    dnf) dnf install -y -q "${pkgs[@]}" ;;
    yum) yum install -y -q "${pkgs[@]}" ;;
    zypper) zypper --non-interactive install "${pkgs[@]}" ;;
    pacman) pacman -Sy --noconfirm "${pkgs[@]}" ;;
  esac

  if [ "$need_docker" -eq 1 ]; then
    systemctl enable --now docker >/dev/null 2>&1 || systemctl start docker
  fi
}

run_preflight() {
  local hard_fail=0
  local missing_cmds=()
  local need_docker=0

  say "running preflight checks (proxy=$FLEET_PROXY_MODE)..."

  if [ -z "$DESTDIR" ] && [ "$(id -u)" -ne 0 ]; then
    echo "preflight: FAIL - must run as root (try: sudo $0)" >&2
    hard_fail=1
  else
    say "preflight: privilege OK"
  fi

  local os arch
  os="$(uname -s)"
  arch="$(uname -m)"
  [ "$os" = "Linux" ] || { echo "preflight: FAIL - unsupported OS '$os' (Linux required)" >&2; hard_fail=1; }
  case "$arch" in
    x86_64|amd64|aarch64|arm64) say "preflight: platform $os/$arch OK" ;;
    *) echo "preflight: FAIL - unsupported architecture '$arch' (need x86_64 or aarch64)" >&2; hard_fail=1 ;;
  esac

  if kernel_at_least 5 4; then
    if kernel_at_least 5 15; then
      say "preflight: kernel $(uname -r) OK"
    else
      warn "kernel $(uname -r) meets minimum 5.4, though 5.15+ is recommended"
    fi
  else
    echo "preflight: FAIL - kernel $(uname -r) is older than minimum 5.4" >&2
    hard_fail=1
  fi

  if [ -z "$DESTDIR" ]; then
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
      say "preflight: systemd active"
    else
      echo "preflight: FAIL - systemd is required and not active on this host" >&2
      hard_fail=1
    fi
  fi

  if [ -f /sys/fs/cgroup/cgroup.controllers ] || [ "$(stat -fc %T /sys/fs/cgroup 2>/dev/null || true)" = "cgroup2fs" ]; then
    say "preflight: cgroup v2 unified hierarchy OK"
  else
    echo "preflight: FAIL - cgroup v2 is not mounted at /sys/fs/cgroup" >&2
    hard_fail=1
  fi

  local cmd
  for cmd in openssl curl tar sha256sum ss nft; do
    if ! command -v "$cmd" >/dev/null 2>&1; then
      missing_cmds+=("$cmd")
    fi
  done

  if [ "$FLEET_PROXY_MODE" = "docker" ]; then
    if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
      need_docker=1
    fi
  fi

  # Disk space check (MB).
  local check_var_path="/var/lib"
  [ -d "$check_var_path" ] || check_var_path="/"
  local avail_var avail_usr
  avail_var="$(df -Pm "$check_var_path" | awk 'NR==2 {print $4}')"
  avail_usr="$(df -Pm /usr/local | awk 'NR==2 {print $4}')"
  if [ "${avail_var:-0}" -lt 2048 ]; then
    echo "preflight: FAIL - $check_var_path has ${avail_var} MiB free (need >= 2048 MiB)" >&2
    hard_fail=1
  else
    say "preflight: disk space on $check_var_path (${avail_var} MiB free) OK"
  fi
  if [ "${avail_usr:-0}" -lt 1024 ]; then
    echo "preflight: FAIL - /usr/local has ${avail_usr} MiB free (need >= 1024 MiB)" >&2
    hard_fail=1
  fi

  if [ "$hard_fail" -ne 0 ]; then
    die "preflight failed due to unrecoverable host requirements"
  fi

  if [ "${#missing_cmds[@]}" -gt 0 ] || [ "$need_docker" -eq 1 ]; then
    if [ "$MODE_CHECK" -eq 1 ] && [ "$YES_INSTALL" -ne 1 ]; then
      echo "preflight: missing OS tools: ${missing_cmds[*]} (docker_needed=$need_docker)" >&2
      die "preflight --check found missing dependencies (re-run without --check or with --yes to install)"
    fi
    install_os_packages "$need_docker" "${missing_cmds[@]}"
  fi

  # Re-verify nftables kernel support + docker daemon after any package installs.
  if [ -z "$DESTDIR" ]; then
    if ! nft list tables >/dev/null 2>&1; then
      modprobe nf_tables 2>/dev/null || true
      nft list tables >/dev/null 2>&1 || die "nftables kernel support (nf_tables) is unavailable"
    fi
    say "preflight: nftables OK"
  fi

  if [ "$FLEET_PROXY_MODE" = "docker" ] && [ -z "$DESTDIR" ]; then
    if ! docker info >/dev/null 2>&1; then
      systemctl enable --now docker >/dev/null 2>&1 || true
      docker info >/dev/null 2>&1 || die "Docker daemon is not running"
    fi
    docker compose version >/dev/null 2>&1 || die "Docker Compose v2 plugin is unavailable"
    say "preflight: Docker + Compose OK"
  fi

  if [ -z "$DESTDIR" ]; then
    local p_fail=0
    check_port_state "$FLEET_PANEL_PORT" "service" || p_fail=1
    check_port_state "$FLEET_SERVER_PORT" "service" || p_fail=1
    check_port_state "$FLEET_AGENT_PORT" "service" || p_fail=1
    check_port_state "$FLEET_AGENT_DIAL_PORT" "service" || p_fail=1
    check_port_state "$FLEET_ENROLL_PORT" "service" || p_fail=1
    check_port_state "$FLEET_AUTH_PORT" "service" || p_fail=1
    check_port_state "$FLEET_TSDB_PORT" "service" || p_fail=1
    check_port_state "$FLEET_GRAFANA_PORT" "service" || p_fail=1
    check_port_state 22 "ssh" || p_fail=1
    [ "$p_fail" -eq 0 ] || die "one or more required ports are already occupied"
  fi

  say "preflight: all checks passed"
}

host_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo "amd64" ;;
    aarch64|arm64) echo "arm64" ;;
    *) die "unsupported arch $(uname -m)" ;;
  esac
}

# ---- GitHub Release download + SHA256 verification ----

github_api_get() {
  local url="$1"
  local hdrs=(-H "Accept: application/vnd.github+json" -H "X-GitHub-Api-Version: 2022-11-28")
  [ -n "$TOKEN" ] && hdrs+=(-H "Authorization: Bearer $TOKEN")
  local resp_file="$TMP_DIR/api_resp.json"
  local code
  code="$(curl -sS -L -w '%{http_code}' "${hdrs[@]}" -o "$resp_file" "$url" || true)"
  case "$code" in
    200) cat "$resp_file" ;;
    401|403|404)
      die "GitHub Releases API returned HTTP $code for $url. For private repos or rate limits, set GH_TOKEN / --token."
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
    # Pure awk fallback for hosts without python3.
    printf '%s' "$json" | tr ',' '\n' | awk -F'"' '
      /"tag_name"[[:space:]]*:/ { print "TAG=" $4 }
      /"url"[[:space:]]*:/ && $4 ~ /\/releases\/assets\// { cur_url=$4 }
      /"name"[[:space:]]*:/ { cur_name=$4 }
      /"browser_download_url"[[:space:]]*:/ { print "ASSET\t" cur_name "\t" cur_url "\t" $4 }
    '
  fi
}

download_release_asset() {
  local api_asset_url="$1" browser_url="$2" dest="$3"
  if [ -n "$TOKEN" ] && [ -n "$api_asset_url" ]; then
    curl -fsSL -L -H "Authorization: Bearer $TOKEN" -H "Accept: application/octet-stream" "$api_asset_url" -o "$dest"
  else
    curl -fsSL -L "$browser_url" -o "$dest"
  fi
}

verify_sha256() {
  local dir="$1" fname="$2"
  [ -f "$dir/SHA256SUMS" ] || die "SHA256SUMS missing"
  grep -qE "[[:space:]](\./)?${fname}$" "$dir/SHA256SUMS" || die "checksum entry missing for $fname in SHA256SUMS"
  (cd "$dir" && sha256sum -c --ignore-missing SHA256SUMS 2>&1 | grep -E "^(\./)?${fname}: OK$" >/dev/null) || \
    die "SHA256 verification failed for $fname"
}

fetch_release_binaries() {
  make_tmp_dir
  local arch; arch="$(host_arch)"
  local dl_dir="$TMP_DIR/release"
  mkdir -p "$dl_dir/agents"

  local api_url
  if [ "$FLEET_INSTALL_VERSION" = "latest" ]; then
    api_url="https://api.github.com/repos/$FLEET_REPO/releases/latest"
  else
    api_url="https://api.github.com/repos/$FLEET_REPO/releases/tags/$FLEET_INSTALL_VERSION"
  fi

  say "querying release $FLEET_INSTALL_VERSION from $FLEET_REPO..."
  local rel_json parsed tag_name=""
  rel_json="$(github_api_get "$api_url")"
  parsed="$(parse_release_assets "$rel_json")"
  tag_name="$(printf '%s\n' "$parsed" | awk -F= '/^TAG=/{print $2; exit}')"
  [ -n "$tag_name" ] || die "could not determine release tag from GitHub API response"
  FLEET_INSTALL_VERSION="$tag_name"
  say "resolved release tag: $tag_name"

  declare -A asset_api asset_browser
  local kind name aurl burl
  while IFS=$'\t' read -r kind name aurl burl; do
    [ "$kind" = "ASSET" ] || continue
    asset_api["$name"]="$aurl"
    asset_browser["$name"]="$burl"
  done <<< "$parsed"

  [ -n "${asset_browser[SHA256SUMS]:-}" ] || die "release $tag_name does not contain SHA256SUMS"
  download_release_asset "${asset_api[SHA256SUMS]}" "${asset_browser[SHA256SUMS]}" "$dl_dir/SHA256SUMS"

  local srv_asset="fleet-server-linux-$arch"
  if [ -z "${asset_browser[$srv_asset]:-}" ] && [ "$arch" = "amd64" ] && [ -n "${asset_browser[fleet-server]:-}" ]; then
    srv_asset="fleet-server"
  fi
  [ -n "${asset_browser[$srv_asset]:-}" ] || die "release $tag_name does not contain $srv_asset"
  say "downloading $srv_asset..."
  download_release_asset "${asset_api[$srv_asset]}" "${asset_browser[$srv_asset]}" "$dl_dir/$srv_asset"
  verify_sha256 "$dl_dir" "$srv_asset"
  cp "$dl_dir/$srv_asset" "$dl_dir/fleet-server-bin"
  chmod 0755 "$dl_dir/fleet-server-bin"

  local agent_targets=("fleet-agent-linux-amd64" "fleet-agent-linux-arm64" "fleet-agent-darwin-amd64" "fleet-agent-darwin-arm64")
  local ag
  for ag in "${agent_targets[@]}"; do
    if [ -n "${asset_browser[$ag]:-}" ]; then
      say "downloading $ag..."
      download_release_asset "${asset_api[$ag]}" "${asset_browser[$ag]}" "$dl_dir/$ag"
      verify_sha256 "$dl_dir" "$ag"
      cp "$dl_dir/$ag" "$dl_dir/agents/$ag"
      chmod 0755 "$dl_dir/agents/$ag"
    fi
  done

  local host_ag="$dl_dir/agents/fleet-agent-linux-$arch"
  [ -s "$host_ag" ] || die "release $tag_name is missing host agent binary fleet-agent-linux-$arch"
  cp "$host_ag" "$dl_dir/fleet-agent-bin"
  chmod 0755 "$dl_dir/fleet-agent-bin"

  if [ -n "${asset_browser[upgrade-server.sh]:-}" ]; then
    download_release_asset "${asset_api[upgrade-server.sh]}" "${asset_browser[upgrade-server.sh]}" "$dl_dir/upgrade-server.sh"
    verify_sha256 "$dl_dir" "upgrade-server.sh"
    chmod 0755 "$dl_dir/upgrade-server.sh"
  fi
  if [ -n "${asset_browser[install-server.sh]:-}" ]; then
    download_release_asset "${asset_api[install-server.sh]}" "${asset_browser[install-server.sh]}" "$dl_dir/install-server.sh"
    verify_sha256 "$dl_dir" "install-server.sh"
    chmod 0755 "$dl_dir/install-server.sh"
  fi

  BIN_DIR="$dl_dir"
  say "release $tag_name binaries downloaded and SHA256-verified"
}

install_native_proxy_binaries() {
  [ -z "$DESTDIR" ] || return 0
  make_tmp_dir
  local arch; arch="$(host_arch)"
  local p_dir="$TMP_DIR/native-proxy"
  mkdir -p "$p_dir"

  # Caddy v2.10.0
  local caddy_tgz="caddy_${CADDY_VERSION}_linux_${arch}.tar.gz"
  local caddy_sums="caddy_${CADDY_VERSION}_checksums.txt"
  local caddy_base="https://github.com/caddyserver/caddy/releases/download/v${CADDY_VERSION}"
  if [ ! -x /usr/local/bin/caddy ] || ! /usr/local/bin/caddy version 2>/dev/null | grep -q "v${CADDY_VERSION}"; then
    say "downloading Caddy v${CADDY_VERSION} (${arch})..."
    curl -fsSL -L "${caddy_base}/${caddy_tgz}" -o "$p_dir/$caddy_tgz"
    curl -fsSL -L "${caddy_base}/${caddy_sums}" -o "$p_dir/$caddy_sums"
    (cd "$p_dir" && sha512sum -c --ignore-missing "$caddy_sums" | grep -q "^${caddy_tgz}: OK$") || \
      die "checksum verification failed for $caddy_tgz"
    tar -xzf "$p_dir/$caddy_tgz" -C "$p_dir" caddy
    install -D -m 0755 "$p_dir/caddy" /usr/local/bin/caddy
    CHANGED=1
    say "installed /usr/local/bin/caddy v${CADDY_VERSION}"
  fi

  # Authelia v4.39.27
  local auth_tgz="authelia-v${AUTHELIA_VERSION}-linux-${arch}.tar.gz"
  local auth_sum="checksums.sha256"
  local auth_base="https://github.com/authelia/authelia/releases/download/v${AUTHELIA_VERSION}"
  if [ ! -x /usr/local/bin/authelia ] || ! /usr/local/bin/authelia --version 2>/dev/null | grep -q "${AUTHELIA_VERSION}"; then
    say "downloading Authelia v${AUTHELIA_VERSION} (${arch})..."
    curl -fsSL -L "${auth_base}/${auth_tgz}" -o "$p_dir/$auth_tgz"
    curl -fsSL -L "${auth_base}/${auth_sum}" -o "$p_dir/$auth_sum"
    (cd "$p_dir" && sha256sum -c --ignore-missing "$auth_sum" | grep -q "^${auth_tgz}: OK$") || \
      die "checksum verification failed for $auth_tgz"
    mkdir -p "$p_dir/authelia-extract"
    tar -xzf "$p_dir/$auth_tgz" -C "$p_dir/authelia-extract"
    local auth_bin
    auth_bin="$(find "$p_dir/authelia-extract" -type f -name 'authelia' | head -n1)"
    [ -n "$auth_bin" ] || die "authelia binary not found inside $auth_tgz"
    install -D -m 0755 "$auth_bin" /usr/local/bin/authelia
    CHANGED=1
    say "installed /usr/local/bin/authelia v${AUTHELIA_VERSION}"
  fi
}

install_victoriametrics_binary() {
  [ -z "$DESTDIR" ] || return 0
  make_tmp_dir
  local arch; arch="$(host_arch)"
  local vm_dir="$TMP_DIR/victoriametrics"
  mkdir -p "$vm_dir"

  local vm_tgz="victoria-metrics-linux-${arch}-v${VICTORIAMETRICS_VERSION}.tar.gz"
  local vm_sums="victoria-metrics-linux-${arch}-v${VICTORIAMETRICS_VERSION}_checksums.txt"
  local vm_base="https://github.com/VictoriaMetrics/VictoriaMetrics/releases/download/v${VICTORIAMETRICS_VERSION}"
  local expected_sha=""
  case "$arch" in
    amd64) expected_sha="$VICTORIAMETRICS_SHA256_AMD64" ;;
    arm64) expected_sha="$VICTORIAMETRICS_SHA256_ARM64" ;;
    *) die "unsupported arch for VictoriaMetrics: $arch" ;;
  esac

  if [ ! -x /usr/local/bin/victoria-metrics ] || ! /usr/local/bin/victoria-metrics --version 2>&1 | grep -q "v${VICTORIAMETRICS_VERSION}"; then
    say "downloading VictoriaMetrics v${VICTORIAMETRICS_VERSION} (${arch})..."
    curl -fsSL -L "${vm_base}/${vm_tgz}" -o "$vm_dir/$vm_tgz"
    curl -fsSL -L "${vm_base}/${vm_sums}" -o "$vm_dir/$vm_sums"
    local actual_sha
    actual_sha="$(sha256sum "$vm_dir/$vm_tgz" | awk '{print $1}')"
    [ "$actual_sha" = "$expected_sha" ] || \
      die "pinned SHA256 mismatch for $vm_tgz (got $actual_sha, want $expected_sha)"
    (cd "$vm_dir" && sha256sum -c --ignore-missing "$vm_sums" | grep -q "^${vm_tgz}: OK$") || \
      die "checksum verification failed for $vm_tgz"
    tar -xzf "$vm_dir/$vm_tgz" -C "$vm_dir" victoria-metrics-prod
    install -D -m 0755 "$vm_dir/victoria-metrics-prod" /usr/local/bin/victoria-metrics
    CHANGED=1
    say "installed /usr/local/bin/victoria-metrics v${VICTORIAMETRICS_VERSION}"
  fi
}

install_grafana_binary() {
  [ -z "$DESTDIR" ] || return 0
  make_tmp_dir
  local arch; arch="$(host_arch)"
  local gf_dir="$TMP_DIR/grafana"
  mkdir -p "$gf_dir"

  local gf_tgz="grafana-${GRAFANA_VERSION}.linux-${arch}.tar.gz"
  local gf_base="https://dl.grafana.com/oss/release"
  local expected_sha=""
  case "$arch" in
    amd64) expected_sha="$GRAFANA_SHA256_AMD64" ;;
    arm64) expected_sha="$GRAFANA_SHA256_ARM64" ;;
    *) die "unsupported arch for Grafana: $arch" ;;
  esac

  if [ ! -x "${FLEET_GRAFANA_HOME}/bin/grafana" ] || ! "${FLEET_GRAFANA_HOME}/bin/grafana" --version 2>&1 | grep -q "version ${GRAFANA_VERSION}"; then
    say "downloading Grafana v${GRAFANA_VERSION} (${arch})..."
    curl -fsSL -L "${gf_base}/${gf_tgz}" -o "$gf_dir/$gf_tgz"
    curl -fsSL -L "${gf_base}/${gf_tgz}.sha256" -o "$gf_dir/${gf_tgz}.sha256"
    local actual_sha remote_sha
    actual_sha="$(sha256sum "$gf_dir/$gf_tgz" | awk '{print $1}')"
    remote_sha="$(awk '{print $1}' "$gf_dir/${gf_tgz}.sha256")"
    [ "$actual_sha" = "$expected_sha" ] || \
      die "pinned SHA256 mismatch for $gf_tgz (got $actual_sha, want $expected_sha)"
    [ "$actual_sha" = "$remote_sha" ] || \
      die "remote .sha256 mismatch for $gf_tgz (got $actual_sha, remote $remote_sha)"
    mkdir -p "$gf_dir/extract"
    tar -xzf "$gf_dir/$gf_tgz" -C "$gf_dir/extract"
    local extracted="$gf_dir/extract/grafana-v${GRAFANA_VERSION}"
    [ -x "$extracted/bin/grafana" ] || die "grafana binary not found inside $gf_tgz"
    rm -rf "${FLEET_GRAFANA_HOME}.new"
    mkdir -p "$(dirname "$FLEET_GRAFANA_HOME")"
    cp -a "$extracted" "${FLEET_GRAFANA_HOME}.new"
    rm -rf "$FLEET_GRAFANA_HOME"
    mv "${FLEET_GRAFANA_HOME}.new" "$FLEET_GRAFANA_HOME"
    ln -sf "${FLEET_GRAFANA_HOME}/bin/grafana" /usr/local/bin/grafana
    CHANGED=1
    say "installed ${FLEET_GRAFANA_HOME} (v${GRAFANA_VERSION})"
  elif [ ! -L /usr/local/bin/grafana ] || [ "$(readlink -f /usr/local/bin/grafana 2>/dev/null || true)" != "${FLEET_GRAFANA_HOME}/bin/grafana" ]; then
    ln -sf "${FLEET_GRAFANA_HOME}/bin/grafana" /usr/local/bin/grafana
    CHANGED=1
  fi
}

# ---- Interactive / Non-interactive Configuration Collection ----

collect_answers() {
  # Load prior answers if re-running on an existing host.
  local saved_answers="$DESTDIR$FLEET_CONF_DIR/install-answers.env"
  if [ -f "$saved_answers" ]; then
    load_answers_file "$saved_answers"
  fi
  if [ -z "$FLEET_DOMAIN_MODE" ] && [ "$NON_INTERACTIVE" -eq 0 ] && has_tty; then
    local layout_ans=""
    printf "DNS layout: [1] one name (panel + Authelia under /authelia)  [2] two names (separate auth subdomain) [1]: " >/dev/tty
    IFS= read -r layout_ans </dev/tty || true
    layout_ans="${layout_ans#"${layout_ans%%[![:space:]]*}"}"
    layout_ans="${layout_ans%"${layout_ans##*[![:space:]]}"}"
    case "${layout_ans,,}" in
      ""|1|single|s|one) FLEET_DOMAIN_MODE="single" ;;
      2|two|t) FLEET_DOMAIN_MODE="two" ;;
      *)
        warn "unrecognized DNS layout '$layout_ans'; defaulting to single-domain"
        FLEET_DOMAIN_MODE="single"
        ;;
    esac
  fi
  FLEET_DOMAIN_MODE="${FLEET_DOMAIN_MODE:-single}"
  case "$FLEET_DOMAIN_MODE" in
    two|single) ;;
    *) die "invalid domain mode '$FLEET_DOMAIN_MODE' (want: two or single)" ;;
  esac

  if [ -z "$FLEET_PANEL_DOMAIN" ]; then
    prompt_val "Panel domain (DNS name, e.g. fleet.example.com)" "fleet.internal" FLEET_PANEL_DOMAIN
  fi
  if is_ip_literal "$FLEET_PANEL_DOMAIN"; then
    die "panel domain '$FLEET_PANEL_DOMAIN' is an IP address; Authelia requires a DNS name (IP-only is unsupported)"
  fi
  if [ "$FLEET_DOMAIN_MODE" = "two" ]; then
    if [ -z "$FLEET_AUTH_DOMAIN" ]; then
      prompt_val "Auth portal domain" "auth.$FLEET_PANEL_DOMAIN" FLEET_AUTH_DOMAIN
    fi
    if is_ip_literal "$FLEET_AUTH_DOMAIN"; then
      die "auth domain '$FLEET_AUTH_DOMAIN' is an IP address; Authelia requires a DNS name (IP-only is unsupported)"
    fi
  else
    FLEET_AUTH_DOMAIN=""
  fi
  if [ -z "$FLEET_TLS_MODE" ]; then
    prompt_val "TLS mode (internal = self-signed/internal CA, acme = public Let's Encrypt)" "internal" FLEET_TLS_MODE
  fi
  case "$FLEET_TLS_MODE" in
    internal|acme) ;;
    *) die "invalid TLS mode '$FLEET_TLS_MODE' (want: internal or acme)" ;;
  esac

  if [ -z "$FLEET_ADMIN_USER" ]; then
    prompt_val "Initial admin username" "admin" FLEET_ADMIN_USER
  fi
  if [ -z "$FLEET_ADMIN_DISPLAY_NAME" ]; then
    prompt_val "Admin display name" "Fleet Admin" FLEET_ADMIN_DISPLAY_NAME
  fi
  if [ -z "$FLEET_ADMIN_EMAIL" ]; then
    prompt_val "Admin email" "${FLEET_ADMIN_USER}@${FLEET_PANEL_DOMAIN}" FLEET_ADMIN_EMAIL
  fi
  if [ "$FLEET_TLS_MODE" = "acme" ] && [ -z "$FLEET_ACME_EMAIL" ]; then
    prompt_val "ACME contact email" "$FLEET_ADMIN_EMAIL" FLEET_ACME_EMAIL
  fi

  local users_file="$DESTDIR$FLEET_PROXY_STATE_DIR/authelia/users.yml"
  if [ ! -f "$users_file" ] && [ -z "$ADMIN_PASSWORD" ]; then
    if [ "$NON_INTERACTIVE" -eq 1 ]; then
      die "initial admin password required (pass --admin-password or --admin-password-file) when $users_file does not exist"
    fi
    prompt_secret "Initial admin password for '$FLEET_ADMIN_USER'" ADMIN_PASSWORD
  fi

  if [ "$NON_INTERACTIVE" -eq 0 ] && has_tty; then
    local custom_ports=""
    printf "Customize default ports/paths (443, 3000, 6789, 7443, 7444, 7445, 8428, 9091, %s)? [y/N]: " "$FLEET_DATA_DIR" >/dev/tty
    IFS= read -r custom_ports </dev/tty || true
    case "$custom_ports" in
      y|Y|yes|YES)
        prompt_val "Proxy HTTPS port" "$FLEET_PANEL_PORT" FLEET_PANEL_PORT
        prompt_val "Loopback fleet-server port" "$FLEET_SERVER_PORT" FLEET_SERVER_PORT
        prompt_val "Agent channel port" "$FLEET_AGENT_PORT" FLEET_AGENT_PORT
        prompt_val "Agent dial mTLS port" "$FLEET_AGENT_DIAL_PORT" FLEET_AGENT_DIAL_PORT
        prompt_val "Token enrollment HTTPS port" "$FLEET_ENROLL_PORT" FLEET_ENROLL_PORT
        prompt_val "Loopback Authelia port" "$FLEET_AUTH_PORT" FLEET_AUTH_PORT
        prompt_val "Loopback VictoriaMetrics port" "$FLEET_TSDB_PORT" FLEET_TSDB_PORT
        prompt_val "Loopback Grafana port" "$FLEET_GRAFANA_PORT" FLEET_GRAFANA_PORT
        prompt_val "Server data directory" "$FLEET_DATA_DIR" FLEET_DATA_DIR
        prompt_val "VictoriaMetrics data directory" "$FLEET_TSDB_DIR" FLEET_TSDB_DIR
        prompt_val "VictoriaMetrics retention period" "$FLEET_TSDB_RETENTION" FLEET_TSDB_RETENTION
        prompt_val "Grafana data directory" "$FLEET_GRAFANA_DIR" FLEET_GRAFANA_DIR
        prompt_val "Proxy config directory" "$FLEET_PROXY_DIR" FLEET_PROXY_DIR
        prompt_val "Proxy mutable state directory" "$FLEET_PROXY_STATE_DIR" FLEET_PROXY_STATE_DIR
        ;;
    esac
  fi
}

write_install_answers() {
  local out="$DESTDIR$FLEET_CONF_DIR/install-answers.env"
  install -d -m 0755 "$DESTDIR$FLEET_CONF_DIR"
  cat >"$out" <<EOF
# Non-secret answers saved by deploy/install-server.sh (reused by upgrades).
# Secrets, passwords, tokens, and CA keys are deliberately EXCLUDED.
FLEET_REPO=$FLEET_REPO
FLEET_INSTALL_VERSION=$FLEET_INSTALL_VERSION
FLEET_PROXY_MODE=$FLEET_PROXY_MODE
FLEET_DOMAIN_MODE=$FLEET_DOMAIN_MODE
FLEET_PANEL_DOMAIN=$FLEET_PANEL_DOMAIN
FLEET_AUTH_DOMAIN=$FLEET_AUTH_DOMAIN
FLEET_TLS_MODE=$FLEET_TLS_MODE
FLEET_ACME_EMAIL=$FLEET_ACME_EMAIL
FLEET_ADMIN_USER=$FLEET_ADMIN_USER
FLEET_ADMIN_DISPLAY_NAME=$FLEET_ADMIN_DISPLAY_NAME
FLEET_ADMIN_EMAIL=$FLEET_ADMIN_EMAIL
FLEET_PANEL_PORT=$FLEET_PANEL_PORT
FLEET_SERVER_PORT=$FLEET_SERVER_PORT
FLEET_AGENT_PORT=$FLEET_AGENT_PORT
FLEET_AGENT_DIAL_PORT=$FLEET_AGENT_DIAL_PORT
FLEET_ENROLL_PORT=$FLEET_ENROLL_PORT
FLEET_AUTH_PORT=$FLEET_AUTH_PORT
FLEET_TSDB_PORT=$FLEET_TSDB_PORT
FLEET_GRAFANA_PORT=$FLEET_GRAFANA_PORT
FLEET_DATA_DIR=$FLEET_DATA_DIR
FLEET_TSDB_DIR=$FLEET_TSDB_DIR
FLEET_TSDB_RETENTION=$FLEET_TSDB_RETENTION
FLEET_GRAFANA_DIR=$FLEET_GRAFANA_DIR
FLEET_PROXY_DIR=$FLEET_PROXY_DIR
FLEET_PROXY_STATE_DIR=$FLEET_PROXY_STATE_DIR
FLEET_CONF_DIR=$FLEET_CONF_DIR
EOF
  chmod 0640 "$out"
  say "saved non-secret install answers to $FLEET_CONF_DIR/install-answers.env"
}

# ---- Template rendering (never clobber live operator-edited files) ----

render_server_config_example() {
  cat <<EOF
{
  "schema_version": 2,
  "server": {
    "listen_addr": "127.0.0.1:${FLEET_SERVER_PORT}",
    "data_dir": "${FLEET_DATA_DIR}",
    "agent_binary_dir": "${FLEET_AGENT_DIR}",
    "agent_binary_path": "${FLEET_AGENT_BIN}",
    "channel_port": ${FLEET_AGENT_PORT},
    "agent_dial_addr": ":${FLEET_AGENT_DIAL_PORT}",
    "enroll_addr": ":${FLEET_ENROLL_PORT}",
    "enroll_host": "${FLEET_PANEL_DOMAIN}",
    "monitor_store": {
      "addr": "http://127.0.0.1:${FLEET_TSDB_PORT}",
      "retention": "${FLEET_TSDB_RETENTION}"
    },
    "log": { "level": "info", "format": "text" }
  }
}
EOF
}

render_caddyfile() {
  local tls_line=""
  local port_suffix=""
  [ "$FLEET_PANEL_PORT" != "443" ] && port_suffix=":${FLEET_PANEL_PORT}"
  if [ "$FLEET_TLS_MODE" = "acme" ]; then
    tls_line="tls ${FLEET_ACME_EMAIL}"
  elif [ "$FLEET_PROXY_MODE" = "docker" ]; then
    tls_line="tls /certs/tls.crt /certs/tls.key"
  else
    tls_line="tls ${FLEET_PROXY_STATE_DIR}/certs/tls.crt ${FLEET_PROXY_STATE_DIR}/certs/tls.key"
  fi

  if [ "$FLEET_DOMAIN_MODE" = "single" ]; then
    cat <<EOF
# Reverse proxy for the Fleet panel (generated by install-server.sh, single-domain mode).
# Reserved panel paths: /authelia* (Authelia portal + API + forward-auth) and /grafana* (Grafana UI).
# fleet-server must not serve either prefix.
{
	auto_https disable_redirects
	https_port ${FLEET_PANEL_PORT}
}

${FLEET_PANEL_DOMAIN}${port_suffix} {
	${tls_line}

	handle /authelia* {
		reverse_proxy 127.0.0.1:${FLEET_AUTH_PORT}
	}

	handle /grafana* {
		forward_auth 127.0.0.1:${FLEET_AUTH_PORT} {
			uri /authelia/api/authz/forward-auth
			copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
		}
		reverse_proxy 127.0.0.1:${FLEET_GRAFANA_PORT}
	}

	handle {
		forward_auth 127.0.0.1:${FLEET_AUTH_PORT} {
			uri /authelia/api/authz/forward-auth
			copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
		}

		reverse_proxy 127.0.0.1:${FLEET_SERVER_PORT}
	}
}
EOF
    return 0
  fi

  cat <<EOF
# Reverse proxy for the Fleet panel (generated by install-server.sh).
{
	auto_https disable_redirects
	https_port ${FLEET_PANEL_PORT}
}

${FLEET_AUTH_DOMAIN}${port_suffix} {
	${tls_line}
	reverse_proxy 127.0.0.1:${FLEET_AUTH_PORT}
}

${FLEET_PANEL_DOMAIN}${port_suffix} {
	${tls_line}

	handle /grafana* {
		forward_auth 127.0.0.1:${FLEET_AUTH_PORT} {
			uri /api/authz/forward-auth
			copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
		}
		reverse_proxy 127.0.0.1:${FLEET_GRAFANA_PORT}
	}

	handle {
		forward_auth 127.0.0.1:${FLEET_AUTH_PORT} {
			uri /api/authz/forward-auth
			copy_headers Remote-User Remote-Groups Remote-Name Remote-Email
		}

		reverse_proxy 127.0.0.1:${FLEET_SERVER_PORT}
	}
}
EOF
}

render_authelia_config() {
  local data_prefix="/data"
  local bind_host=""
  if [ "$FLEET_PROXY_MODE" = "native" ]; then
    data_prefix="${FLEET_PROXY_STATE_DIR}/authelia"
    bind_host="127.0.0.1"
  fi
  local port_suffix=""
  [ "$FLEET_PANEL_PORT" != "443" ] && port_suffix=":${FLEET_PANEL_PORT}"
  local server_path="/"
  local authelia_url="https://${FLEET_AUTH_DOMAIN}"
  local redirect_url="https://${FLEET_PANEL_DOMAIN}"
  if [ "$FLEET_DOMAIN_MODE" = "single" ]; then
    server_path="/authelia"
    authelia_url="https://${FLEET_PANEL_DOMAIN}${port_suffix}/authelia"
    redirect_url="https://${FLEET_PANEL_DOMAIN}${port_suffix}"
  fi
  cat <<EOF
# Authelia configuration for Fleet panel (Authelia ${AUTHELIA_VERSION}).
server:
  address: 'tcp://${bind_host}:${FLEET_AUTH_PORT}${server_path}'
  endpoints:
    authz:
      forward-auth:
        implementation: 'ForwardAuth'

log:
  level: 'info'

theme: 'auto'

authentication_backend:
  file:
    path: '${data_prefix}/users.yml'
    watch: true
    password:
      algorithm: 'argon2'

access_control:
  default_policy: 'deny'
  rules:
    - domain: '${FLEET_PANEL_DOMAIN}'
      resources:
        - '^/grafana(/.*)?$'
      policy: 'two_factor'
    - domain: '${FLEET_PANEL_DOMAIN}'
      policy: 'two_factor'

session:
  name: 'fleet_session'
  expiration: '8h'
  inactivity: '1h'
  cookies:
    - domain: '${FLEET_PANEL_DOMAIN}'
      authelia_url: '${authelia_url}'
      default_redirection_url: '${redirect_url}'

regulation:
  max_retries: 5
  find_time: '2m'
  ban_time: '10m'

storage:
  local:
    path: '${data_prefix}/db.sqlite3'

notifier:
  filesystem:
    filename: '${data_prefix}/notification.txt'

totp:
  issuer: 'Fleet panel'
  period: 30
  digits: 6
EOF
}

render_compose_yml() {
  cat <<EOF
name: fleet-proxy

services:
  authelia:
    image: authelia/authelia:${AUTHELIA_VERSION}
    restart: unless-stopped
    ports:
      - "127.0.0.1:${FLEET_AUTH_PORT}:${FLEET_AUTH_PORT}"
    volumes:
      - ${FLEET_PROXY_DIR}/authelia/configuration.yml:/config/configuration.yml:ro
      - ${FLEET_PROXY_STATE_DIR}/authelia:/data
      - ${FLEET_PROXY_STATE_DIR}/secrets:/secrets:ro
    environment:
      TZ: "\${TZ:-UTC}"
      AUTHELIA_IDENTITY_VALIDATION_RESET_PASSWORD_JWT_SECRET_FILE: /secrets/jwt
      AUTHELIA_SESSION_SECRET_FILE: /secrets/session
      AUTHELIA_STORAGE_ENCRYPTION_KEY_FILE: /secrets/storage

  caddy:
    image: caddy:${CADDY_VERSION}
    restart: unless-stopped
    network_mode: host
    depends_on:
      - authelia
    volumes:
      - ${FLEET_PROXY_DIR}/Caddyfile:/etc/caddy/Caddyfile:ro
      - ${FLEET_PROXY_STATE_DIR}/certs:/certs:ro
      - ${FLEET_PROXY_STATE_DIR}/caddy:/data
EOF
}

render_victoriametrics_service() {
  cat <<EOF
# VictoriaMetrics time-series store systemd unit (installed by deploy/install-server.sh).
[Unit]
Description=VictoriaMetrics monitor time-series store for Fleet
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${TSDB_USER}
Group=${TSDB_USER}
ExecStart=/usr/local/bin/victoria-metrics -httpListenAddr=127.0.0.1:${FLEET_TSDB_PORT} -storageDataPath=${FLEET_TSDB_DIR} -retentionPeriod=${FLEET_TSDB_RETENTION}
StateDirectory=fleet-victoriametrics
StateDirectoryMode=0750
ReadWritePaths=${FLEET_TSDB_DIR}
UMask=0077
Restart=on-failure
RestartSec=3

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF
}

render_grafana_ini() {
  local port_suffix=""
  [ "$FLEET_PANEL_PORT" != "443" ] && port_suffix=":${FLEET_PANEL_PORT}"
  cat <<EOF
# Grafana configuration for Fleet panel (Grafana ${GRAFANA_VERSION}).
# Managed by deploy/install-server.sh; listens on loopback only behind Caddy + Authelia.

[paths]
data = ${FLEET_GRAFANA_DIR}
logs = ${FLEET_GRAFANA_DIR}/logs
plugins = ${FLEET_GRAFANA_DIR}/plugins
provisioning = ${FLEET_GRAFANA_CONF_DIR}/provisioning

[server]
protocol = http
http_addr = 127.0.0.1
http_port = ${FLEET_GRAFANA_PORT}
domain = ${FLEET_PANEL_DOMAIN}
root_url = https://${FLEET_PANEL_DOMAIN}${port_suffix}/grafana/
serve_from_sub_path = true

[database]
type = sqlite3
path = grafana.db

[analytics]
reporting_enabled = false
check_for_updates = false
check_for_plugin_updates = false

[security]
allow_embedding = true
cookie_secure = true
cookie_samesite = lax
disable_gravatar = true

[snapshots]
external_enabled = false

[dashboards]
default_home_dashboard_path = ${FLEET_GRAFANA_CONF_DIR}/dashboards/fleet-overview.json

[users]
allow_sign_up = false
auto_assign_org = true
auto_assign_org_role = Viewer
viewers_can_edit = false
default_theme = dark

[auth]
disable_login_form = true
disable_signout_menu = true

[auth.proxy]
enabled = true
header_name = Remote-User
header_property = username
auto_sign_up = true
sync_ttl = 60
whitelist = 127.0.0.1, ::1
headers = Email:Remote-Email Name:Remote-Name Groups:Remote-Groups
enable_login_token = false

[log]
mode = console
level = info
EOF
}

render_grafana_datasource() {
  cat <<EOF
apiVersion: 1

datasources:
  - name: VictoriaMetrics
    uid: fleet-victoriametrics
    type: prometheus
    access: proxy
    url: http://127.0.0.1:${FLEET_TSDB_PORT}
    isDefault: true
    editable: false
    jsonData:
      httpMethod: POST
      timeInterval: 5s
EOF
}

render_grafana_dashboard_provider() {
  cat <<EOF
apiVersion: 1

providers:
  - name: Fleet Monitor
    orgId: 1
    folder: Fleet
    folderUid: fleet
    type: file
    disableDeletion: true
    updateIntervalSeconds: 30
    allowUiUpdates: false
    options:
      path: ${FLEET_GRAFANA_CONF_DIR}/dashboards
      foldersFromFilesStructure: false
EOF
}

render_grafana_service() {
  local port_suffix=""
  [ "$FLEET_PANEL_PORT" != "443" ] && port_suffix=":${FLEET_PANEL_PORT}"
  cat <<EOF
# Grafana monitor dashboards systemd unit (installed by deploy/install-server.sh).
[Unit]
Description=Grafana monitor dashboards for Fleet
After=network-online.target fleet-victoriametrics.service
Wants=network-online.target fleet-victoriametrics.service

[Service]
Type=simple
User=${GRAFANA_USER}
Group=${GRAFANA_USER}
Environment=GF_SERVER_ROOT_URL=https://${FLEET_PANEL_DOMAIN}${port_suffix}/grafana/
Environment=GF_SERVER_SERVE_FROM_SUB_PATH=true
Environment=GF_AUTH_PROXY_ENABLED=true
Environment=GF_SECURITY_SECRET_KEY__FILE=${FLEET_GRAFANA_DIR}/secret_key
ExecStart=${FLEET_GRAFANA_HOME}/bin/grafana server --homepath=${FLEET_GRAFANA_HOME} --config=${FLEET_GRAFANA_CONF_DIR}/grafana.ini
StateDirectory=fleet-grafana
StateDirectoryMode=0750
ReadWritePaths=${FLEET_GRAFANA_DIR}
ReadOnlyPaths=${FLEET_GRAFANA_HOME} ${FLEET_GRAFANA_CONF_DIR}
UMask=0077
Restart=on-failure
RestartSec=3

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF
}

install_grafana_dashboards() {
  local dash_dir="$FLEET_GRAFANA_CONF_DIR/dashboards"
  install -d -m 0755 "$DESTDIR$dash_dir"
  make_tmp_dir
  if [ -n "$SCRIPT_DIR" ] && [ -d "$SCRIPT_DIR/grafana/dashboards" ]; then
    local f
    for f in "$SCRIPT_DIR/grafana/dashboards"/*.json; do
      [ -f "$f" ] || continue
      put_file 0644 "$f" "$dash_dir/$(basename "$f")"
    done
    return 0
  fi
  # Remote curl | bash fallback: fetch pinned dashboards from the same repository.
  local base="https://raw.githubusercontent.com/${FLEET_REPO}/main/deploy/grafana/dashboards"
  local name
  for name in fleet-overview.json fleet-agents.json; do
    if curl -fsSL "${base}/${name}" -o "$TMP_DIR/$name"; then
      put_file 0644 "$TMP_DIR/$name" "$dash_dir/$name"
    else
      warn "could not download default Grafana dashboard $name from $base"
    fi
  done
}

render_fleet_server_service() {
  cat <<EOF
# Fleet main server systemd unit (installed by deploy/install-server.sh).
[Unit]
Description=Fleet main server (panel and agent control channel)
After=network-online.target fleet-victoriametrics.service
Wants=network-online.target fleet-victoriametrics.service

[Service]
Type=simple
User=${SVC_USER}
Group=${SVC_USER}
Environment=FLEET_LISTEN_ADDR=127.0.0.1:${FLEET_SERVER_PORT}
Environment=FLEET_DATA_DIR=${FLEET_DATA_DIR}
Environment=FLEET_MONITOR_STORE_ADDR=http://127.0.0.1:${FLEET_TSDB_PORT}
EnvironmentFile=-${FLEET_CONF_DIR}/fleet-server.env
ExecStart=${FLEET_SERVER_BIN} --config ${FLEET_CONF_DIR}/config.json
StateDirectory=fleet-server
StateDirectoryMode=0750
ReadWritePaths=${FLEET_DATA_DIR}
UMask=0077
Restart=on-failure
RestartSec=3

NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
EOF
}

render_proxy_systemd_units() {
  if [ "$FLEET_PROXY_MODE" = "docker" ]; then
    cat >"$DESTDIR/etc/systemd/system/fleet-proxy.service" <<EOF
# Fleet auth proxy (Caddy + Authelia via Docker Compose from ${FLEET_PROXY_DIR}).
[Unit]
Description=Fleet Auth Proxy (Caddy + Authelia)
After=network-online.target docker.service fleet-server.service fleet-grafana.service
Wants=network-online.target fleet-server.service fleet-grafana.service
Requires=docker.service
Requires=docker.service

[Service]
Type=oneshot
RemainAfterExit=yes
WorkingDirectory=${FLEET_PROXY_DIR}
TimeoutStartSec=180
ExecStartPre=/usr/bin/env docker compose -f ${FLEET_PROXY_DIR}/compose.yml config -q
ExecStart=/usr/bin/env docker compose -f ${FLEET_PROXY_DIR}/compose.yml up -d --remove-orphans
ExecReload=/usr/bin/env docker compose -f ${FLEET_PROXY_DIR}/compose.yml up -d --remove-orphans
ExecStop=/usr/bin/env docker compose -f ${FLEET_PROXY_DIR}/compose.yml down

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "$DESTDIR/etc/systemd/system/fleet-proxy.service"
  else
    cat >"$DESTDIR/etc/systemd/system/fleet-authelia.service" <<EOF
[Unit]
Description=Authelia forward-auth portal for Fleet
PartOf=fleet-proxy.service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=${PROXY_USER}
Group=${PROXY_USER}
Environment=AUTHELIA_IDENTITY_VALIDATION_RESET_PASSWORD_JWT_SECRET_FILE=${FLEET_PROXY_STATE_DIR}/secrets/jwt
Environment=AUTHELIA_SESSION_SECRET_FILE=${FLEET_PROXY_STATE_DIR}/secrets/session
Environment=AUTHELIA_STORAGE_ENCRYPTION_KEY_FILE=${FLEET_PROXY_STATE_DIR}/secrets/storage
ExecStart=/usr/local/bin/authelia --config ${FLEET_PROXY_DIR}/authelia/configuration.yml
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${FLEET_PROXY_STATE_DIR}/authelia
ReadOnlyPaths=${FLEET_PROXY_DIR} ${FLEET_PROXY_STATE_DIR}/secrets

[Install]
WantedBy=multi-user.target
EOF

    cat >"$DESTDIR/etc/systemd/system/fleet-caddy.service" <<EOF
[Unit]
Description=Caddy reverse proxy for Fleet
PartOf=fleet-proxy.service
After=network-online.target fleet-authelia.service fleet-server.service fleet-grafana.service
Wants=network-online.target fleet-authelia.service fleet-server.service fleet-grafana.service

[Service]
Type=simple
User=${PROXY_USER}
Group=${PROXY_USER}
Environment=XDG_DATA_HOME=${FLEET_PROXY_STATE_DIR}/caddy
Environment=XDG_CONFIG_HOME=${FLEET_PROXY_STATE_DIR}/caddy/config
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
ExecStart=/usr/local/bin/caddy run --environ --config ${FLEET_PROXY_DIR}/Caddyfile
ExecReload=/usr/local/bin/caddy reload --config ${FLEET_PROXY_DIR}/Caddyfile --force
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=${FLEET_PROXY_STATE_DIR}/caddy
ReadOnlyPaths=${FLEET_PROXY_DIR} ${FLEET_PROXY_STATE_DIR}/certs

[Install]
WantedBy=multi-user.target
EOF

    cat >"$DESTDIR/etc/systemd/system/fleet-proxy.service" <<EOF
[Unit]
Description=Fleet Auth Proxy (native Caddy + Authelia)
Requires=fleet-authelia.service fleet-caddy.service
After=fleet-authelia.service fleet-caddy.service fleet-server.service fleet-grafana.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/true

[Install]
WantedBy=multi-user.target
EOF
    chmod 0644 "$DESTDIR/etc/systemd/system/fleet-authelia.service" \
      "$DESTDIR/etc/systemd/system/fleet-caddy.service" \
      "$DESTDIR/etc/systemd/system/fleet-proxy.service"
  fi
}

generate_proxy_secrets_and_certs() {
  local state="$DESTDIR$FLEET_PROXY_STATE_DIR"
  install -d -m 0750 "$state" "$state/certs" "$state/authelia" "$state/caddy" "$state/caddy/config"
  install -d -m 0700 "$state/secrets"

  local f
  for f in jwt session storage; do
    if [ ! -s "$state/secrets/$f" ]; then
      openssl rand -hex 32 > "$state/secrets/$f"
      chmod 0600 "$state/secrets/$f"
      say "generated proxy secret $FLEET_PROXY_STATE_DIR/secrets/$f"
    fi
  done

  if [ "$FLEET_TLS_MODE" = "internal" ]; then
    if [ ! -s "$state/certs/tls.crt" ] || [ ! -s "$state/certs/tls.key" ]; then
      make_tmp_dir
      local san_list="DNS:${FLEET_PANEL_DOMAIN},DNS:${FLEET_AUTH_DOMAIN}"
      local san_desc="${FLEET_PANEL_DOMAIN}, ${FLEET_AUTH_DOMAIN}"
      if [ "$FLEET_DOMAIN_MODE" = "single" ]; then
        san_list="DNS:${FLEET_PANEL_DOMAIN}"
        san_desc="${FLEET_PANEL_DOMAIN}"
      fi
      cat >"$TMP_DIR/san.cnf" <<EOF
[req]
distinguished_name=dn
x509_extensions=v3_req
prompt=no
[dn]
CN=${FLEET_PANEL_DOMAIN}
[v3_req]
subjectAltName=${san_list}
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
EOF
      openssl req -x509 -newkey rsa:2048 -nodes -days 825 \
        -keyout "$state/certs/tls.key" -out "$state/certs/tls.crt" \
        -config "$TMP_DIR/san.cnf" >/dev/null 2>&1
      chmod 0600 "$state/certs/tls.key"
      chmod 0644 "$state/certs/tls.crt"
      say "generated self-signed TLS cert at $FLEET_PROXY_STATE_DIR/certs/tls.crt (SANs: $san_desc)"
    else
      say "preserving existing TLS cert at $FLEET_PROXY_STATE_DIR/certs/tls.crt"
    fi
  fi
}

generate_authelia_users() {
  local users_file="$DESTDIR$FLEET_PROXY_STATE_DIR/authelia/users.yml"
  if [ -s "$users_file" ]; then
    say "preserving existing $FLEET_PROXY_STATE_DIR/authelia/users.yml"
    return 0
  fi
  [ -z "$DESTDIR" ] || return 0
  [ -n "$ADMIN_PASSWORD" ] || die "admin password required to initialize $users_file"

  local hash_out="" hash=""
  if [ "$FLEET_PROXY_MODE" = "docker" ]; then
    hash_out="$(docker run --rm "authelia/authelia:${AUTHELIA_VERSION}" authelia crypto hash generate argon2 --password "$ADMIN_PASSWORD" 2>&1 || true)"
  else
    hash_out="$(/usr/local/bin/authelia crypto hash generate argon2 --password "$ADMIN_PASSWORD" 2>&1 || true)"
  fi
  hash="$(printf '%s\n' "$hash_out" | grep -oE '[$]argon2id[$][^[:space:]]+' | head -n1 || true)"
  if [ -z "$hash" ]; then
    [ -n "$hash_out" ] && printf '%s\n' "$hash_out" >&2
    die "failed to generate argon2id password hash via Authelia"
  fi

  cat >"$users_file" <<EOF
users:
  ${FLEET_ADMIN_USER}:
    disabled: false
    displayname: '${FLEET_ADMIN_DISPLAY_NAME}'
    password: '${hash}'
    email: '${FLEET_ADMIN_EMAIL}'
    groups:
      - 'operators'
EOF
  chmod 0600 "$users_file"
  say "created initial Authelia admin user '$FLEET_ADMIN_USER' in $FLEET_PROXY_STATE_DIR/authelia/users.yml"
}

put_file() {
  local mode="$1" src="$2" dest="$DESTDIR$3"
  if [ -f "$dest" ] && cmp -s "$src" "$dest"; then
    chmod "$mode" "$dest"
    return 0
  fi
  install -D -m "$mode" "$src" "$dest"
  CHANGED=1
  say "installed $3"
}

# ---- Main ----

main() {
parse_args "$@"
trap cleanup EXIT
handle_upgrade_delegation
validate_install_modes

if [ "$MODE_CHECK" -eq 1 ]; then
  run_preflight
  exit 0
fi

run_preflight

if [ -n "$BIN_DIR" ]; then
  # Local binary directory mode (T-0024 compatible).
  local_srv="$BIN_DIR/fleet-server"
  [ -f "$local_srv" ] || local_srv="$BIN_DIR/fleet-server-bin"
  [ -f "$local_srv" ] || die "$BIN_DIR/fleet-server not found; run scripts/build.sh or omit --bin-dir"
  local_ag="$BIN_DIR/fleet-agent"
  [ -f "$local_ag" ] || local_ag="$BIN_DIR/fleet-agent-bin"
  [ -s "$local_ag" ] || die "$BIN_DIR/fleet-agent not found or empty"
else
  fetch_release_binaries
  local_srv="$BIN_DIR/fleet-server-bin"
  local_ag="$BIN_DIR/fleet-agent-bin"
fi

CHANGED=0
install_victoriametrics_binary
install_grafana_binary
if [ "$FLEET_PROXY_MODE" = "native" ]; then
  install_native_proxy_binaries
fi

collect_answers

# 1. System users (real installs only).
if [ -z "$DESTDIR" ]; then
  if ! id -u "$SVC_USER" >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$FLEET_DATA_DIR" --no-create-home --shell /usr/sbin/nologin "$SVC_USER"
    say "created system user $SVC_USER"
  fi
  if ! id -u "$TSDB_USER" >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$FLEET_TSDB_DIR" --no-create-home --shell /usr/sbin/nologin "$TSDB_USER"
    say "created system user $TSDB_USER"
  fi
  if ! id -u "$GRAFANA_USER" >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$FLEET_GRAFANA_DIR" --no-create-home --shell /usr/sbin/nologin "$GRAFANA_USER"
    say "created system user $GRAFANA_USER"
  fi
  if [ "$FLEET_PROXY_MODE" = "native" ] && ! id -u "$PROXY_USER" >/dev/null 2>&1; then
    useradd --system --user-group --home-dir "$FLEET_PROXY_STATE_DIR" --no-create-home --shell /usr/sbin/nologin "$PROXY_USER"
    say "created system user $PROXY_USER"
  fi
fi

# 2. Install fleet-server, fleet-agent(s), and upgrade helper.
put_file 0755 "$local_srv" "$FLEET_SERVER_BIN"
put_file 0755 "$local_ag" "$FLEET_AGENT_BIN"

install -d -m 0755 "$DESTDIR$FLEET_AGENT_DIR"
if [ -d "$BIN_DIR/agents" ]; then
  for ag_file in "$BIN_DIR/agents"/fleet-agent-*; do
    [ -f "$ag_file" ] || continue
    put_file 0755 "$ag_file" "$FLEET_AGENT_DIR/$(basename "$ag_file")"
  done
fi

if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/upgrade-server.sh" ]; then
  put_file 0755 "$SCRIPT_DIR/upgrade-server.sh" /usr/local/lib/fleet/upgrade-server.sh
elif [ -f "$BIN_DIR/upgrade-server.sh" ]; then
  put_file 0755 "$BIN_DIR/upgrade-server.sh" /usr/local/lib/fleet/upgrade-server.sh
fi
if [ -n "$SCRIPT_DIR" ] && [ -f "$SCRIPT_DIR/install-server.sh" ]; then
  put_file 0755 "$SCRIPT_DIR/install-server.sh" /usr/local/lib/fleet/install-server.sh
elif [ -f "$BIN_DIR/install-server.sh" ]; then
  put_file 0755 "$BIN_DIR/install-server.sh" /usr/local/lib/fleet/install-server.sh
fi

# 3. Server config (ship *.example, never clobber live config.json).
install -d -m 0755 "$DESTDIR$FLEET_CONF_DIR"
render_server_config_example > "$DESTDIR$FLEET_CONF_DIR/config.json.example"
chmod 0644 "$DESTDIR$FLEET_CONF_DIR/config.json.example"
if [ ! -e "$DESTDIR$FLEET_CONF_DIR/config.json" ]; then
  cp "$DESTDIR$FLEET_CONF_DIR/config.json.example" "$DESTDIR$FLEET_CONF_DIR/config.json"
  chmod 0644 "$DESTDIR$FLEET_CONF_DIR/config.json"
  say "wrote $FLEET_CONF_DIR/config.json"
else
  say "preserving existing $FLEET_CONF_DIR/config.json (updated config.json.example)"
fi

if [ ! -e "$DESTDIR$FLEET_CONF_DIR/fleet-server.env" ]; then
  cat >"$DESTDIR$FLEET_CONF_DIR/fleet-server.env" <<EOF
FLEET_LISTEN_ADDR=127.0.0.1:${FLEET_SERVER_PORT}
FLEET_DATA_DIR=${FLEET_DATA_DIR}
FLEET_MONITOR_STORE_ADDR=http://127.0.0.1:${FLEET_TSDB_PORT}
EOF
  chmod 0640 "$DESTDIR$FLEET_CONF_DIR/fleet-server.env"
fi

write_install_answers

# 4. Data dirs & systemd units for fleet-victoriametrics, fleet-grafana, and fleet-server.
install -d -m 0750 "$DESTDIR$FLEET_DATA_DIR"
install -d -m 0750 "$DESTDIR$FLEET_TSDB_DIR"
install -d -m 0750 "$DESTDIR$FLEET_GRAFANA_DIR" "$DESTDIR$FLEET_GRAFANA_DIR/logs" "$DESTDIR$FLEET_GRAFANA_DIR/plugins"
if [ ! -s "$DESTDIR$FLEET_GRAFANA_DIR/secret_key" ]; then
  openssl rand -hex 32 > "$DESTDIR$FLEET_GRAFANA_DIR/secret_key"
  chmod 0600 "$DESTDIR$FLEET_GRAFANA_DIR/secret_key"
  say "generated Grafana secret_key at $FLEET_GRAFANA_DIR/secret_key"
fi
if [ -z "$DESTDIR" ]; then
  chown "$SVC_USER:$SVC_USER" "$FLEET_DATA_DIR"
  chown "$TSDB_USER:$TSDB_USER" "$FLEET_TSDB_DIR"
  chown -R "$GRAFANA_USER:$GRAFANA_USER" "$FLEET_GRAFANA_DIR"
fi

# Grafana config & provisioning (/etc/fleet-grafana).
install -d -m 0755 "$DESTDIR$FLEET_GRAFANA_CONF_DIR" \
  "$DESTDIR$FLEET_GRAFANA_CONF_DIR/provisioning/datasources" \
  "$DESTDIR$FLEET_GRAFANA_CONF_DIR/provisioning/dashboards" \
  "$DESTDIR$FLEET_GRAFANA_CONF_DIR/dashboards"
render_grafana_ini > "$DESTDIR$FLEET_GRAFANA_CONF_DIR/grafana.ini.example"
chmod 0644 "$DESTDIR$FLEET_GRAFANA_CONF_DIR/grafana.ini.example"
if [ ! -e "$DESTDIR$FLEET_GRAFANA_CONF_DIR/grafana.ini" ]; then
  cp "$DESTDIR$FLEET_GRAFANA_CONF_DIR/grafana.ini.example" "$DESTDIR$FLEET_GRAFANA_CONF_DIR/grafana.ini"
  chmod 0644 "$DESTDIR$FLEET_GRAFANA_CONF_DIR/grafana.ini"
  say "wrote $FLEET_GRAFANA_CONF_DIR/grafana.ini"
else
  say "preserving existing $FLEET_GRAFANA_CONF_DIR/grafana.ini (updated grafana.ini.example)"
fi
render_grafana_datasource > "$DESTDIR$FLEET_GRAFANA_CONF_DIR/provisioning/datasources/victoriametrics.yaml"
chmod 0644 "$DESTDIR$FLEET_GRAFANA_CONF_DIR/provisioning/datasources/victoriametrics.yaml"
render_grafana_dashboard_provider > "$DESTDIR$FLEET_GRAFANA_CONF_DIR/provisioning/dashboards/fleet.yaml"
chmod 0644 "$DESTDIR$FLEET_GRAFANA_CONF_DIR/provisioning/dashboards/fleet.yaml"
install_grafana_dashboards

install -d -m 0755 "$DESTDIR/etc/systemd/system"
render_victoriametrics_service > "$DESTDIR/etc/systemd/system/fleet-victoriametrics.service"
chmod 0644 "$DESTDIR/etc/systemd/system/fleet-victoriametrics.service"
render_grafana_service > "$DESTDIR/etc/systemd/system/fleet-grafana.service"
chmod 0644 "$DESTDIR/etc/systemd/system/fleet-grafana.service"
render_fleet_server_service > "$DESTDIR/etc/systemd/system/fleet-server.service"
chmod 0644 "$DESTDIR/etc/systemd/system/fleet-server.service"

# 5. Proxy home (/opt/fleet/proxy) + mutable state (/var/lib/fleet-proxy).
install -d -m 0755 "$DESTDIR$FLEET_PROXY_DIR" "$DESTDIR$FLEET_PROXY_DIR/authelia"
render_caddyfile > "$DESTDIR$FLEET_PROXY_DIR/Caddyfile.example"
render_authelia_config > "$DESTDIR$FLEET_PROXY_DIR/authelia/configuration.yml.example"
render_compose_yml > "$DESTDIR$FLEET_PROXY_DIR/compose.yml.example"

if [ ! -e "$DESTDIR$FLEET_PROXY_DIR/Caddyfile" ]; then
  cp "$DESTDIR$FLEET_PROXY_DIR/Caddyfile.example" "$DESTDIR$FLEET_PROXY_DIR/Caddyfile"
  chmod 0644 "$DESTDIR$FLEET_PROXY_DIR/Caddyfile"
  say "wrote $FLEET_PROXY_DIR/Caddyfile"
else
  say "preserving existing $FLEET_PROXY_DIR/Caddyfile"
fi

if [ ! -e "$DESTDIR$FLEET_PROXY_DIR/authelia/configuration.yml" ]; then
  cp "$DESTDIR$FLEET_PROXY_DIR/authelia/configuration.yml.example" "$DESTDIR$FLEET_PROXY_DIR/authelia/configuration.yml"
  chmod 0644 "$DESTDIR$FLEET_PROXY_DIR/authelia/configuration.yml"
  say "wrote $FLEET_PROXY_DIR/authelia/configuration.yml"
else
  say "preserving existing $FLEET_PROXY_DIR/authelia/configuration.yml"
fi

if [ "$FLEET_PROXY_MODE" = "docker" ]; then
  if [ ! -e "$DESTDIR$FLEET_PROXY_DIR/compose.yml" ]; then
    cp "$DESTDIR$FLEET_PROXY_DIR/compose.yml.example" "$DESTDIR$FLEET_PROXY_DIR/compose.yml"
    chmod 0644 "$DESTDIR$FLEET_PROXY_DIR/compose.yml"
    say "wrote $FLEET_PROXY_DIR/compose.yml"
  else
    say "preserving existing $FLEET_PROXY_DIR/compose.yml"
  fi
fi

generate_proxy_secrets_and_certs
generate_authelia_users
render_proxy_systemd_units

if [ -z "$DESTDIR" ] && [ "$FLEET_PROXY_MODE" = "native" ]; then
  chown -R "$PROXY_USER:$PROXY_USER" "$FLEET_PROXY_STATE_DIR"
fi

if [ -n "$DESTDIR" ]; then
  say "staged under $DESTDIR (skipping systemd and service start)"
  exit 0
fi

if [ "$START" -eq 0 ]; then
  say "installed (--no-start). Start with: systemctl enable --now fleet-victoriametrics fleet-grafana fleet-server fleet-proxy"
  exit 0
fi

# 6. Enable & start VictoriaMetrics, Grafana, then fleet-server, wait for internal CA, then bring up proxy.
systemctl daemon-reload
systemctl enable fleet-victoriametrics.service >/dev/null 2>&1
if systemctl is-active --quiet fleet-victoriametrics.service; then
  if [ "$CHANGED" -eq 1 ]; then
    systemctl restart fleet-victoriametrics.service
    say "restarted fleet-victoriametrics"
  fi
else
  systemctl start fleet-victoriametrics.service
  say "started fleet-victoriametrics"
fi
vm_ok=0
for _ in $(seq 1 15); do
  if curl -fsS "http://127.0.0.1:${FLEET_TSDB_PORT}/health" >/dev/null 2>&1; then
    vm_ok=1
    break
  fi
  sleep 1
done
[ "$vm_ok" -eq 1 ] || die "fleet-victoriametrics health check failed on http://127.0.0.1:${FLEET_TSDB_PORT}/health"
say "VictoriaMetrics ready on 127.0.0.1:${FLEET_TSDB_PORT} (state: $FLEET_TSDB_DIR)"

systemctl enable fleet-grafana.service >/dev/null 2>&1
if systemctl is-active --quiet fleet-grafana.service; then
  if [ "$CHANGED" -eq 1 ]; then
    systemctl restart fleet-grafana.service
    say "restarted fleet-grafana"
  fi
else
  systemctl start fleet-grafana.service
  say "started fleet-grafana"
fi
gf_ok=0
for _ in $(seq 1 30); do
  if curl -fsS "http://127.0.0.1:${FLEET_GRAFANA_PORT}/grafana/api/health" >/dev/null 2>&1; then
    gf_ok=1
    break
  fi
  sleep 1
done
[ "$gf_ok" -eq 1 ] || die "fleet-grafana health check failed on http://127.0.0.1:${FLEET_GRAFANA_PORT}/grafana/api/health"
say "Grafana ready on 127.0.0.1:${FLEET_GRAFANA_PORT}/grafana (state: $FLEET_GRAFANA_DIR)"

systemctl enable fleet-server.service >/dev/null 2>&1
if systemctl is-active --quiet fleet-server.service; then
  if [ "$CHANGED" -eq 1 ]; then
    systemctl restart fleet-server.service
    say "restarted fleet-server"
  fi
else
  systemctl start fleet-server.service
  say "started fleet-server"
fi

ca_ok=0
for _ in $(seq 1 30); do
  if [ -s "$FLEET_DATA_DIR/ca/ca.crt" ] && [ -s "$FLEET_DATA_DIR/ca/ca.key" ]; then
    ca_ok=1
    break
  fi
  sleep 1
done
[ "$ca_ok" -eq 1 ] || die "fleet-server started but no internal CA appeared at $FLEET_DATA_DIR/ca after 30s; check: journalctl -u fleet-server"
say "internal CA ready at $FLEET_DATA_DIR/ca (never overwritten on upgrade)"

srv_ok=0
for _ in $(seq 1 15); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:${FLEET_SERVER_PORT}/" || true)"
  if [ "$code" = "200" ]; then
    srv_ok=1
    break
  fi
  sleep 1
done
[ "$srv_ok" -eq 1 ] || die "fleet-server health check failed on http://127.0.0.1:${FLEET_SERVER_PORT}/"

systemctl enable fleet-proxy.service >/dev/null 2>&1
if [ "$FLEET_PROXY_MODE" = "native" ]; then
  systemctl enable fleet-authelia.service fleet-caddy.service >/dev/null 2>&1
fi
systemctl restart fleet-proxy.service

port_hint=""
if [ "$FLEET_PANEL_PORT" != "443" ]; then
  port_hint=":${FLEET_PANEL_PORT}"
fi
say "fleet-server and fleet-proxy ($FLEET_PROXY_MODE) are up!"
say "panel URL:   https://${FLEET_PANEL_DOMAIN}${port_hint}/"
say "Grafana URL: https://${FLEET_PANEL_DOMAIN}${port_hint}/grafana/"
if [ "$FLEET_DOMAIN_MODE" = "single" ]; then
  say "auth URL:    https://${FLEET_PANEL_DOMAIN}${port_hint}/authelia"
else
  say "auth URL:    https://${FLEET_AUTH_DOMAIN}${port_hint}/"
fi
if [ "$FLEET_TLS_MODE" = "internal" ]; then
  say "TLS cert:  $FLEET_PROXY_STATE_DIR/certs/tls.crt (trust on operator browsers or replace with CA-signed cert)"
fi
say "TOTP code on first login: cat $FLEET_PROXY_STATE_DIR/authelia/notification.txt"
}

if [ "${FLEET_INSTALL_LIB_ONLY:-0}" != "1" ]; then
  main "$@"
fi
