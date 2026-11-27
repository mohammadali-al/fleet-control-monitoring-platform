#!/usr/bin/env bash
# Uninstall the Fleet main server and its auth proxy (T-0041).
# Reverses deploy/install-server.sh and deploy/upgrade-server.sh safely and idempotently.
# Runnable via: curl -fsSL ... | sudo bash -s -- --dry-run
#
# Usage:
#   sudo deploy/uninstall-server.sh [OPTIONS]
#
# Options:
#   --dry-run                Print every planned action without modifying or deleting anything
#   -y, --yes                Confirm removing units, binaries, config, and proxy dir (still keeps data unless --purge-data)
#   --non-interactive        Never prompt; requires --yes (unless --dry-run)
#   --purge-data             ALSO delete persistent data dirs (CA key, registry, backups, proxy state/certs/users)
#   --keep-data              Explicitly preserve persistent data dirs and backups (default)
#   --proxy native|docker    Override proxy mode (default: read from install-answers.env, else native)
#   --conf-dir DIR           Override server config directory (default: /etc/fleet-server)
#   --data-dir DIR           Override server data directory (default: /var/lib/fleet-server)
#   --proxy-dir DIR          Override proxy config directory (default: /opt/fleet/proxy)
#   --proxy-state-dir DIR    Override proxy mutable state directory (default: /var/lib/fleet-proxy)
#   -h, --help               Show this help message
#
# Staging:
#   DESTDIR=/some/dir        Operate inside a staged root directory without systemd/docker/userdel calls.
set -euo pipefail

DRY_RUN=0
YES=0
NON_INTERACTIVE=0
PURGE_DATA=0
KEEP_DATA_EXPLICIT=0
PURGE_DATA_EXPLICIT=0

CLI_PROXY_MODE=""
CLI_CONF_DIR=""
CLI_DATA_DIR=""
CLI_PROXY_DIR=""
CLI_PROXY_STATE_DIR=""

FLEET_PROXY_MODE=""
FLEET_PANEL_DOMAIN=""
FLEET_CONF_DIR="/etc/fleet-server"
FLEET_DATA_DIR="/var/lib/fleet-server"
FLEET_PROXY_DIR="/opt/fleet/proxy"
FLEET_PROXY_STATE_DIR="/var/lib/fleet-proxy"

SVC_USER="fleet"
PROXY_USER="fleet-proxy"

# Fixed explicit lists mirrored from deploy/install-server.sh, deploy/upgrade-server.sh, and deploy/systemd/.
FLEET_TIMER_UNITS=(
  "fleet-agent-liveness.timer"
  "fleet-watchdog-liveness.timer"
)
FLEET_SERVICE_UNITS=(
  "fleet-agent-liveness.service"
  "fleet-watchdog-liveness.service"
  "fleet-watchdog.service"
  "fleet-proxy.service"
  "fleet-caddy.service"
  "fleet-authelia.service"
  "fleet-server.service"
)

say() { echo "uninstall-server: $*"; }
warn() { echo "uninstall-server: WARN: $*" >&2; }
die() { echo "uninstall-server: ERROR: $*" >&2; exit 1; }

usage() {
  cat <<'EOF'
Usage: sudo deploy/uninstall-server.sh [OPTIONS]

Safety defaults:
  - Always previews a removal summary and requires confirmation (--yes or interactive prompt).
  - Persistent data (/var/lib/fleet-server including ca/ CA private key, /var/lib/fleet-proxy,
    and /var/backups/fleet-server) is KEPT by default unless --purge-data is passed.
  - Shared OS packages (docker, openssl, nftables, curl, ...) are NEVER uninstalled.

Options:
  --dry-run                Print every action that would run; delete nothing (recommended first run)
  -y, --yes                Non-interactive confirmation for binaries/units/config (still keeps data unless --purge-data)
  --non-interactive        Never prompt; requires --yes to make changes
  --purge-data             Delete persistent data dirs, CA key, proxy state/users/certs, and backups
  --keep-data              Explicitly preserve persistent data dirs and backups (default)
  --proxy native|docker    Override proxy mode (default: from install-answers.env, else native)
  --conf-dir DIR           Server config directory (default: /etc/fleet-server)
  --data-dir DIR           Server data directory (default: /var/lib/fleet-server)
  --proxy-dir DIR          Proxy configuration directory (default: /opt/fleet/proxy)
  --proxy-state-dir DIR    Proxy mutable state directory (default: /var/lib/fleet-proxy)
  -h, --help               Show this help message

Staging:
  DESTDIR=/some/dir        Operate inside a staged root directory without systemd/docker/userdel calls.
EOF
}

need_val() {
  [ "$#" -ge 2 ] || die "flag $1 requires a value"
}

parse_args() {
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --dry-run) DRY_RUN=1; shift ;;
      -y|--yes) YES=1; shift ;;
      --non-interactive) NON_INTERACTIVE=1; shift ;;
      --purge-data) PURGE_DATA=1; PURGE_DATA_EXPLICIT=1; shift ;;
      --keep-data) PURGE_DATA=0; KEEP_DATA_EXPLICIT=1; shift ;;
      --proxy) need_val "$@"; CLI_PROXY_MODE="$2"; shift 2 ;;
      --proxy=*) CLI_PROXY_MODE="${1#--proxy=}"; shift ;;
      --conf-dir) need_val "$@"; CLI_CONF_DIR="$2"; shift 2 ;;
      --data-dir) need_val "$@"; CLI_DATA_DIR="$2"; shift 2 ;;
      --proxy-dir) need_val "$@"; CLI_PROXY_DIR="$2"; shift 2 ;;
      --proxy-state-dir) need_val "$@"; CLI_PROXY_STATE_DIR="$2"; shift 2 ;;
      -h|--help) usage; exit 0 ;;
      *) die "unknown argument: $1 (run with --help)" ;;
    esac
  done

  if [ "$PURGE_DATA_EXPLICIT" -eq 1 ] && [ "$KEEP_DATA_EXPLICIT" -eq 1 ]; then
    die "cannot pass both --purge-data and --keep-data"
  fi
}

has_tty() {
  [ "$NON_INTERACTIVE" -eq 0 ] && [ -r /dev/tty ] && [ -w /dev/tty ]
}

# Normalize an absolute path by collapsing // and stripping trailing / (keeping /).
normalize_abs_path() {
  local p="$1"
  while [[ "$p" == *"//"* ]]; do
    p="${p//\/\///}"
  done
  while [ "${#p}" -gt 1 ] && [[ "$p" == */ ]]; do
    p="${p%/}"
  done
  printf '%s' "$p"
}

resolve_destdir() {
  DESTDIR="${DESTDIR:-}"
  if [ -z "$DESTDIR" ]; then
    return 0
  fi
  if [ -d "$DESTDIR" ]; then
    DESTDIR="$(cd "$DESTDIR" && pwd)"
  elif [[ "$DESTDIR" != /* ]]; then
    DESTDIR="$PWD/$DESTDIR"
  fi
  DESTDIR="$(normalize_abs_path "$DESTDIR")"
  [ "$DESTDIR" != "/" ] || die "refusing DESTDIR=/"
}

VALIDATED_PATH=""

# validate_path_under <expected_prefix> <logical_target>
# Rejects empty paths, non-absolute paths, traversal, globs, protected system dirs, $HOME,
# and anything not strictly under <expected_prefix>. Sets VALIDATED_PATH.
validate_path_under() {
  local expected_prefix="${1:-}"
  local raw_target="${2:-}"
  VALIDATED_PATH=""

  [ -n "$expected_prefix" ] || die "safe_rm: expected_prefix must not be empty"
  [ -n "$raw_target" ] || die "safe_rm: refusing empty path"

  case "$raw_target" in
    /*) ;;
    *) die "safe_rm: refusing non-absolute path: $raw_target" ;;
  esac
  case "$expected_prefix" in
    /*) ;;
    *) die "safe_rm: expected_prefix must be absolute: $expected_prefix" ;;
  esac

  case "$raw_target" in
    *$'\n'*|*$'\r'*) die "safe_rm: refusing path containing newline: $raw_target" ;;
    *"*"*|*"?"*|*"["*) die "safe_rm: refusing glob-like path: $raw_target" ;;
  esac

  if [[ "$raw_target" =~ (^|/)\.\.(/|$) ]]; then
    die "safe_rm: refusing path with '..' traversal: $raw_target"
  fi

  local clean_prefix clean_target
  clean_prefix="$(normalize_abs_path "$expected_prefix")"
  clean_target="$(normalize_abs_path "$raw_target")"

  case "$clean_target" in
    /|/bin|/boot|/dev|/etc|/etc/systemd|/etc/systemd/system|/home|/lib|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/usr/local/bin|/usr/local/lib|/usr/local/share|/var|/var/lib|/var/backups)
      die "safe_rm: refusing protected system path: $clean_target"
      ;;
  esac

  if [ -n "${HOME:-}" ]; then
    local clean_home
    clean_home="$(normalize_abs_path "$HOME")"
    if [ "$clean_target" = "$clean_home" ]; then
      die "safe_rm: refusing \$HOME path: $clean_target"
    fi
  fi

  if [ -n "${DESTDIR:-}" ]; then
    local clean_dest
    clean_dest="$(normalize_abs_path "$DESTDIR")"
    if [ "$clean_target" = "$clean_dest" ]; then
      die "safe_rm: refusing DESTDIR root: $clean_target"
    fi
  fi

  case "$clean_target" in
    "$clean_prefix"/*) ;;
    *) die "safe_rm: refusing '$clean_target' (not under expected prefix '$clean_prefix')" ;;
  esac

  VALIDATED_PATH="$clean_target"
}

# safe_rm <expected_prefix> <logical_target>
# Validates target unconditionally before checking existence or deleting.
safe_rm() {
  local expected_prefix="${1:-}"
  local raw_target="${2:-}"
  validate_path_under "$expected_prefix" "$raw_target"
  local clean_target="$VALIDATED_PATH"
  local full_target="${DESTDIR:-}${clean_target}"

  if [ -n "${DESTDIR:-}" ]; then
    local clean_dest
    clean_dest="$(normalize_abs_path "$DESTDIR")"
    case "$full_target" in
      "$clean_dest"/*) ;;
      *) die "safe_rm: staged path '$full_target' does not stay under DESTDIR '$clean_dest'" ;;
    esac
  fi

  if [ ! -e "$full_target" ] && [ ! -L "$full_target" ]; then
    say "already absent: $full_target"
    return 0
  fi

  if command -v realpath >/dev/null 2>&1; then
    local parent_dir resolved_parent
    parent_dir="$(dirname "$full_target")"
    if [ -d "$parent_dir" ]; then
      resolved_parent="$(realpath "$parent_dir" 2>/dev/null || true)"
      if [ -n "$resolved_parent" ]; then
        case "$resolved_parent" in
          /|/bin|/boot|/dev|/lib|/lib64|/proc|/sbin|/sys)
            die "safe_rm: refusing resolved parent '$resolved_parent' for '$full_target'"
            ;;
        esac
        if [ -n "${DESTDIR:-}" ]; then
          local resolved_dest
          resolved_dest="$(realpath "$DESTDIR" 2>/dev/null || printf '%s' "$DESTDIR")"
          case "$resolved_parent" in
            "$resolved_dest"|"$resolved_dest"/*) ;;
            *) die "safe_rm: resolved parent '$resolved_parent' escapes DESTDIR '$resolved_dest'" ;;
          esac
        fi
      fi
    fi
  fi

  if [ "$DRY_RUN" -eq 1 ]; then
    say "[dry-run] would remove: $full_target"
    return 0
  fi

  rm -rf -- "$full_target"
  say "removed: $full_target"
}

# Validate configurable Fleet directory and derive a safe expected prefix for safe_rm.
# Default paths stay under /etc, /opt/fleet (or /opt), /var/lib; custom overrides are rejected
# if they point at protected OS trees or lack 'fleet' in their path.
expected_prefix_for_dir() {
  local kind="$1"
  local default_prefix="$2"
  local val="$3"

  [ -n "$val" ] || die "refusing empty $kind path"
  case "$val" in
    /*) ;;
    *) die "refusing non-absolute $kind path: $val" ;;
  esac
  case "$val" in
    *$'\n'*|*$'\r'*) die "refusing $kind path with newline: $val" ;;
    *"*"*|*"?"*|*"["*) die "refusing glob-like $kind path: $val" ;;
  esac
  if [[ "$val" =~ (^|/)\.\.(/|$) ]]; then
    die "refusing $kind path with '..' traversal: $val"
  fi

  local clean_val
  clean_val="$(normalize_abs_path "$val")"

  case "$clean_val" in
    /|/bin|/boot|/dev|/etc|/home|/lib|/lib64|/media|/mnt|/opt|/proc|/root|/run|/sbin|/srv|/sys|/tmp|/usr|/usr/local|/var|/var/lib|/var/backups)
      die "refusing protected system path for $kind: $clean_val"
      ;;
  esac
  if [ -n "${HOME:-}" ]; then
    local clean_home
    clean_home="$(normalize_abs_path "$HOME")"
    if [ "$clean_val" = "$clean_home" ]; then
      die "refusing \$HOME for $kind: $clean_val"
    fi
  fi

  # For non-config directories, never allow placing them under /etc or core OS directories.
  if [ "$kind" != "conf-dir" ]; then
    case "$clean_val" in
      /etc|/etc/*) die "refusing $kind inside /etc: $clean_val" ;;
    esac
  fi
  case "$clean_val" in
    /bin/*|/boot/*|/dev/*|/lib/*|/lib64/*|/proc/*|/sbin/*|/sys/*|/run/*|/usr/*|/tmp|/tmp/*)
      die "refusing $kind under OS/temp directory: $clean_val"
      ;;
  esac

  case "$clean_val" in
    "$default_prefix"/*)
      printf '%s' "$default_prefix"
      return 0
      ;;
  esac

  # Custom override outside default prefix: require depth >= 2 and 'fleet' in path.
  local trimmed="${clean_val#/}"
  if [[ "$trimmed" != */* ]]; then
    die "refusing top-level $kind path '$clean_val' (must be at least 2 levels deep)"
  fi
  case "$clean_val" in
    *fleet*) ;;
    *) die "refusing custom $kind path '$clean_val' (path must contain 'fleet')" ;;
  esac

  dirname "$clean_val"
}

# Safely load KEY=VALUE env files without executing shell commands.
load_answers_file() {
  local file="$1"
  [ -f "$file" ] || return 0
  local line key val
  while IFS= read -r line || [ -n "$line" ]; do
    line="${line#"${line%%[![:space:]]*}"}"
    [ -z "$line" ] && continue
    case "$line" in
      \#*) continue ;;
    esac
    key="${line%%=*}"
    val="${line#*=}"
    case "$val" in
      \"*\") val="${val#\"}"; val="${val%\"}" ;;
      \'*\') val="${val#\'}"; val="${val%\'}" ;;
    esac
    case "$key" in
      FLEET_PROXY_MODE) [ -z "$FLEET_PROXY_MODE" ] && FLEET_PROXY_MODE="$val" ;;
      FLEET_PANEL_DOMAIN) [ -z "$FLEET_PANEL_DOMAIN" ] && FLEET_PANEL_DOMAIN="$val" ;;
      FLEET_DATA_DIR) [ -n "$val" ] && FLEET_DATA_DIR="$val" ;;
      FLEET_PROXY_DIR) [ -n "$val" ] && FLEET_PROXY_DIR="$val" ;;
      FLEET_PROXY_STATE_DIR) [ -n "$val" ] && FLEET_PROXY_STATE_DIR="$val" ;;
      FLEET_CONF_DIR) [ -n "$val" ] && FLEET_CONF_DIR="$val" ;;
      *) ;;
    esac
  done < "$file"
}

discover_config() {
  if [ -n "$CLI_CONF_DIR" ]; then
    FLEET_CONF_DIR="$CLI_CONF_DIR"
  fi
  local answers_file="${DESTDIR:-}${FLEET_CONF_DIR}/install-answers.env"
  if [ -f "$answers_file" ]; then
    if [ -r "$answers_file" ]; then
      say "reading installation metadata from $answers_file"
      load_answers_file "$answers_file"
    else
      warn "$answers_file exists but is not readable; using defaults/CLI overrides"
    fi
  else
    say "$answers_file not found; using defaults/CLI overrides"
  fi

  # CLI overrides always take precedence over install-answers.env.
  [ -n "$CLI_CONF_DIR" ] && FLEET_CONF_DIR="$CLI_CONF_DIR"
  [ -n "$CLI_DATA_DIR" ] && FLEET_DATA_DIR="$CLI_DATA_DIR"
  [ -n "$CLI_PROXY_DIR" ] && FLEET_PROXY_DIR="$CLI_PROXY_DIR"
  [ -n "$CLI_PROXY_STATE_DIR" ] && FLEET_PROXY_STATE_DIR="$CLI_PROXY_STATE_DIR"
  [ -n "$CLI_PROXY_MODE" ] && FLEET_PROXY_MODE="$CLI_PROXY_MODE"

  # Normalize paths.
  FLEET_CONF_DIR="$(normalize_abs_path "$FLEET_CONF_DIR")"
  FLEET_DATA_DIR="$(normalize_abs_path "$FLEET_DATA_DIR")"
  FLEET_PROXY_DIR="$(normalize_abs_path "$FLEET_PROXY_DIR")"
  FLEET_PROXY_STATE_DIR="$(normalize_abs_path "$FLEET_PROXY_STATE_DIR")"

  if [ -z "$FLEET_PROXY_MODE" ]; then
    if [ -f "${DESTDIR:-}${FLEET_PROXY_DIR}/compose.yml" ] && [ ! -f "${DESTDIR:-}/etc/systemd/system/fleet-caddy.service" ]; then
      FLEET_PROXY_MODE="docker"
    else
      FLEET_PROXY_MODE="native"
    fi
  fi

  case "$FLEET_PROXY_MODE" in
    native|docker) ;;
    *) die "invalid proxy mode '$FLEET_PROXY_MODE' (want: native or docker)" ;;
  esac
}

collect_backup_siblings() {
  local data_dir="$1"
  local full_data_dir="${DESTDIR:-}${data_dir}"
  local parent base
  parent="$(dirname "$full_data_dir")"
  base="$(basename "$data_dir")"
  [ -d "$parent" ] || return 0

  local item
  while IFS= read -r -d '' item; do
    local rel="$item"
    if [ -n "${DESTDIR:-}" ]; then
      rel="${item#"${DESTDIR}"}"
    fi
    printf '%s\n' "$rel"
  done < <(find "$parent" -maxdepth 1 -type d \( -name "${base}.bak-*" -o -name "${base}.failed-*" \) -print0 2>/dev/null | sort -z)
}

print_plan_summary() {
  local backup_siblings=("$@")
  echo ""
  echo "==================== Fleet Server Uninstall Summary ===================="
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "Mode: DRY RUN (no files, units, containers, or users will be modified)"
  else
    echo "Mode: LIVE UNINSTALL${DESTDIR:+ (staged under $DESTDIR)}"
  fi
  echo "Detected proxy mode: $FLEET_PROXY_MODE"
  echo ""
  echo "1. Systemd units to stop, disable, and remove:"
  local u
  for u in "${FLEET_TIMER_UNITS[@]}" "${FLEET_SERVICE_UNITS[@]}"; do
    echo "   - /etc/systemd/system/$u"
  done
  if [ "$FLEET_PROXY_MODE" = "docker" ]; then
    echo "   - Docker Compose stack: ${FLEET_PROXY_DIR}/compose.yml down$( [ "$PURGE_DATA" -eq 1 ] && echo " --volumes" || echo " (volumes preserved)" )"
  fi
  echo ""
  echo "2. Binaries, helpers, and configuration to REMOVE:"
  echo "   - /usr/local/bin/fleet-server"
  echo "   - /usr/local/bin/fleet-agent (wrapper, if present)"
  if [ "$FLEET_PROXY_MODE" = "native" ]; then
    echo "   - /usr/local/bin/caddy"
    echo "   - /usr/local/bin/authelia"
  fi
  echo "   - /usr/local/share/fleet/ (host + multi-arch fleet-agent binaries)"
  echo "   - /usr/local/lib/fleet/ (upgrade-server.sh helper)"
  echo "   - ${FLEET_CONF_DIR}/ (config.json, fleet-server.env, install-answers.env)"
  echo "   - ${FLEET_PROXY_DIR}/ (Caddyfile, authelia config, compose.yml)"
  echo ""
  if [ "$PURGE_DATA" -eq 1 ]; then
    echo "3. PERSISTENT DATA & BACKUPS TO DELETE (--purge-data):"
    echo "   *** WARNING: THIS PERMANENTLY DESTROYS THE INTERNAL CA PRIVATE KEY ***"
    echo "   *** AND REGISTRY. EVERY ENROLLED AGENT WILL NEED TO BE RE-ONBOARDED. ***"
    echo "   - ${FLEET_DATA_DIR}/ (registry, ca/ CA private key, evidence, archive)"
    if [ "${#backup_siblings[@]}" -gt 0 ]; then
      local sib
      for sib in "${backup_siblings[@]}"; do
        echo "   - ${sib}/ (migration/rollback backup)"
      done
    fi
    echo "   - ${FLEET_PROXY_STATE_DIR}/ (proxy secrets, TLS certs, Authelia users.yml & db)"
    echo "   - /var/backups/fleet-server/ (upgrade backups)"
    echo "   - System users: ${SVC_USER}, ${PROXY_USER}"
  else
    echo "3. Persistent data & backups KEPT (default; pass --purge-data to delete):"
    echo "   - KEPT: ${FLEET_DATA_DIR}/ (registry, ca/ CA private key, evidence, archive)"
    echo "   - KEPT: ${FLEET_PROXY_STATE_DIR}/ (proxy secrets, TLS certs, Authelia users.yml & db)"
    echo "   - KEPT: /var/backups/fleet-server/ (upgrade backups)"
    echo "   - KEPT: system users ${SVC_USER}, ${PROXY_USER}"
  fi
  echo ""
  echo "4. Explicitly untouched:"
  echo "   - Shared OS packages (docker, openssl, nftables, curl, tar, coreutils, iproute2)"
  echo "   - Remote machines running fleet-agent (detach from panel or run fleet-agent on each)"
  echo "========================================================================"
  echo ""
}

confirm_uninstall() {
  if [ "$DRY_RUN" -eq 1 ]; then
    return 0
  fi

  if [ "$YES" -ne 1 ]; then
    if ! has_tty; then
      die "refusing to uninstall without confirmation (no interactive TTY; run with --dry-run first, then pass --yes)"
    fi
    local ans=""
    printf "Proceed with uninstalling Fleet server & proxy? [y/N]: " >/dev/tty
    IFS= read -r ans </dev/tty || true
    case "$ans" in
      y|Y|yes|YES) ;;
      *) die "uninstall aborted by operator" ;;
    esac
  fi

  if [ "$PURGE_DATA" -eq 1 ] && [ "$YES" -ne 1 ]; then
    has_tty || die "no interactive TTY available to confirm --purge-data"
    local expected_token="${FLEET_PANEL_DOMAIN:-DELETE FLEET DATA}"
    echo "" >/dev/tty
    echo "DANGER: --purge-data will permanently delete ${FLEET_DATA_DIR} (including the CA private key!)," >/dev/tty
    echo "${FLEET_PROXY_STATE_DIR}, and /var/backups/fleet-server." >/dev/tty
    if [ -n "$FLEET_PANEL_DOMAIN" ]; then
      printf "Type the panel hostname '%s' to confirm permanent data deletion: " "$expected_token" >/dev/tty
    else
      printf "Type '%s' to confirm permanent data deletion: " "$expected_token" >/dev/tty
    fi
    local typed=""
    IFS= read -r typed </dev/tty || true
    if [ "$typed" != "$expected_token" ]; then
      die "confirmation did not match '$expected_token'; aborting without deleting anything"
    fi
  fi
}

stop_and_remove_unit() {
  local unit="$1"
  local unit_path="/etc/systemd/system/${unit}"
  local wants_multi="/etc/systemd/system/multi-user.target.wants/${unit}"
  local wants_timers="/etc/systemd/system/timers.target.wants/${unit}"

  if [ -z "${DESTDIR:-}" ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    local has_unit=0
    if [ -e "$unit_path" ] || [ -L "$unit_path" ]; then
      has_unit=1
    elif systemctl is-active --quiet "$unit" 2>/dev/null || systemctl is-enabled --quiet "$unit" 2>/dev/null; then
      has_unit=1
    fi

    if [ "$has_unit" -eq 1 ]; then
      if [ "$DRY_RUN" -eq 1 ]; then
        say "[dry-run] would disable and stop systemd unit: $unit"
      else
        systemctl disable --now "$unit" >/dev/null 2>&1 || systemctl stop "$unit" >/dev/null 2>&1 || true
        say "stopped and disabled unit: $unit"
      fi
    else
      say "unit not active/present: $unit"
    fi
  fi

  safe_rm "/etc/systemd/system" "$unit_path"
  if [ -L "${DESTDIR:-}${wants_multi}" ]; then
    safe_rm "/etc/systemd/system/multi-user.target.wants" "$wants_multi"
  fi
  if [ -L "${DESTDIR:-}${wants_timers}" ]; then
    safe_rm "/etc/systemd/system/timers.target.wants" "$wants_timers"
  fi
}

teardown_docker_proxy() {
  local compose_file="${DESTDIR:-}${FLEET_PROXY_DIR}/compose.yml"
  [ "$FLEET_PROXY_MODE" = "docker" ] || return 0
  [ -f "$compose_file" ] || return 0

  if [ -n "${DESTDIR:-}" ]; then
    say "staged DESTDIR mode: skipping docker compose down for ${FLEET_PROXY_DIR}/compose.yml"
    return 0
  fi
  if ! command -v docker >/dev/null 2>&1 || ! docker compose version >/dev/null 2>&1; then
    warn "docker compose not available; skipping container teardown for ${FLEET_PROXY_DIR}/compose.yml"
    return 0
  fi

  local args=(-f "${FLEET_PROXY_DIR}/compose.yml" down --remove-orphans)
  if [ "$PURGE_DATA" -eq 1 ]; then
    args+=(--volumes)
  fi

  if [ "$DRY_RUN" -eq 1 ]; then
    say "[dry-run] would run: docker compose ${args[*]}"
    return 0
  fi

  say "stopping Docker Compose proxy stack (${FLEET_PROXY_DIR}/compose.yml)..."
  docker compose "${args[@]}" || warn "docker compose down returned non-zero; continuing cleanup"
}

remove_system_users() {
  [ "$PURGE_DATA" -eq 1 ] || return 0
  if [ -n "${DESTDIR:-}" ]; then
    say "staged DESTDIR mode: skipping system user removal ($SVC_USER, $PROXY_USER)"
    return 0
  fi
  command -v userdel >/dev/null 2>&1 || return 0

  local u
  for u in "$SVC_USER" "$PROXY_USER"; do
    if id -u "$u" >/dev/null 2>&1; then
      if [ "$DRY_RUN" -eq 1 ]; then
        say "[dry-run] would remove system user: $u"
      else
        if userdel "$u" 2>/dev/null; then
          say "removed system user: $u"
        else
          warn "could not remove system user '$u' (check for remaining processes)"
        fi
      fi
    else
      say "system user already absent: $u"
    fi
  done
}

main() {
  parse_args "$@"
  resolve_destdir

  if [ -z "${DESTDIR:-}" ] && [ "$DRY_RUN" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
    die "must run as root (try: sudo $0)"
  fi

  discover_config

  # Pre-validate all configurable directories before showing summary or touching anything.
  local conf_prefix proxy_prefix data_prefix proxy_state_prefix
  conf_prefix="$(expected_prefix_for_dir "conf-dir" "/etc" "$FLEET_CONF_DIR")"
  proxy_prefix="$(expected_prefix_for_dir "proxy-dir" "/opt" "$FLEET_PROXY_DIR")"
  data_prefix="$(expected_prefix_for_dir "data-dir" "/var/lib" "$FLEET_DATA_DIR")"
  proxy_state_prefix="$(expected_prefix_for_dir "proxy-state-dir" "/var/lib" "$FLEET_PROXY_STATE_DIR")"

  local backup_siblings=()
  if [ "$PURGE_DATA" -eq 1 ]; then
    local sib
    while IFS= read -r sib; do
      [ -n "$sib" ] && backup_siblings+=("$sib")
    done < <(collect_backup_siblings "$FLEET_DATA_DIR")
  fi

  print_plan_summary "${backup_siblings[@]}"
  confirm_uninstall

  # 1. Stop Docker Compose stack before removing /opt/fleet/proxy/compose.yml.
  teardown_docker_proxy

  # 2. Stop, disable, and remove systemd units (timers first, then services), then daemon-reload.
  local u
  for u in "${FLEET_TIMER_UNITS[@]}" "${FLEET_SERVICE_UNITS[@]}"; do
    stop_and_remove_unit "$u"
  done

  if [ -z "${DESTDIR:-}" ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    if [ "$DRY_RUN" -eq 1 ]; then
      say "[dry-run] would run: systemctl daemon-reload"
    else
      systemctl daemon-reload
      systemctl reset-failed >/dev/null 2>&1 || true
      say "reloaded systemd daemon"
    fi
  fi

  # 3. Remove binaries, helpers, config, and proxy config dir.
  safe_rm "/usr/local/bin" "/usr/local/bin/fleet-server"
  safe_rm "/usr/local/bin" "/usr/local/bin/fleet-agent"
  if [ "$FLEET_PROXY_MODE" = "native" ]; then
    safe_rm "/usr/local/bin" "/usr/local/bin/caddy"
    safe_rm "/usr/local/bin" "/usr/local/bin/authelia"
  fi
  safe_rm "/usr/local/share" "/usr/local/share/fleet"
  safe_rm "/usr/local/lib" "/usr/local/lib/fleet"
  safe_rm "$conf_prefix" "$FLEET_CONF_DIR"
  safe_rm "$proxy_prefix" "$FLEET_PROXY_DIR"

  # Remove /opt/fleet only if it is empty and was the parent of the default proxy dir.
  if [ "$FLEET_PROXY_DIR" = "/opt/fleet/proxy" ] && [ -d "${DESTDIR:-}/opt/fleet" ]; then
    if [ "$DRY_RUN" -eq 1 ]; then
      say "[dry-run] would remove empty parent dir if empty: ${DESTDIR:-}/opt/fleet"
    elif rmdir "${DESTDIR:-}/opt/fleet" 2>/dev/null; then
      say "removed empty parent dir: ${DESTDIR:-}/opt/fleet"
    fi
  fi

  # 4. Data dirs + backups (GATED: only when PURGE_DATA=1).
  if [ "$PURGE_DATA" -eq 1 ]; then
    safe_rm "$data_prefix" "$FLEET_DATA_DIR"
    if [ "${#backup_siblings[@]}" -gt 0 ]; then
      local sib_path sib_parent
      for sib_path in "${backup_siblings[@]}"; do
        sib_parent="$(dirname "$sib_path")"
        safe_rm "$sib_parent" "$sib_path"
      done
    fi
    safe_rm "$proxy_state_prefix" "$FLEET_PROXY_STATE_DIR"
    safe_rm "/var/backups" "/var/backups/fleet-server"
    remove_system_users
  else
    echo ""
    say "DATA PRESERVED (default): the following data and backup directories were NOT deleted:"
    say "  - ${DESTDIR:-}${FLEET_DATA_DIR} (registry, ca/ CA private key, evidence)"
    say "  - ${DESTDIR:-}${FLEET_PROXY_STATE_DIR} (proxy secrets, TLS certs, Authelia users & db)"
    say "  - ${DESTDIR:-}/var/backups/fleet-server (upgrade backups)"
    say "Re-run with --purge-data if you intentionally want to destroy the CA and all state."
  fi

  echo ""
  if [ "$DRY_RUN" -eq 1 ]; then
    say "dry-run complete; nothing was modified."
  else
    say "uninstall complete. Shared OS packages (docker, openssl, nftables, ...) were left untouched."
  fi
}

if [ "${FLEET_UNINSTALL_LIB_ONLY:-0}" != "1" ]; then
  main "$@"
fi
