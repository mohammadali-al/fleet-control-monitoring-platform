package enroll

// The installer: the bootstrap script a machine runs (as root) to enroll, and
// the pieces it installs. Everything that ends up on the machine is rendered
// here, from one source, so the "show script" view, the served script and the
// tests all see exactly what will run.
//
// The service is permanent: systemd on Linux (enabled at boot, Restart=always),
// launchd on macOS (RunAtLoad + KeepAlive). Role and connect_mode=agent_dial
// are baked into the agent's config. macOS is monitor-only: capture and the
// command role are Linux-only, so a macOS worker is refused (here, at mint
// time, and again by the server when the machine tries to enroll).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

// Where things go on the enrolled machine.
const (
	BinaryPath  = "/usr/local/lib/fleet-agent/fleet-agent" // the real agent binary
	WrapperPath = "/usr/local/bin/fleet-agent"             // control wrapper: start|stop|status, else the binary
	ConfigDir   = "/etc/fleet-agent"
	StateDir    = "/var/lib/fleet-agent"

	SystemdUnit     = "fleet-agent.service"
	SystemdUnitPath = "/etc/systemd/system/fleet-agent.service"

	LaunchdLabel     = "com.fleet.agent"
	LaunchdPlistPath = "/Library/LaunchDaemons/com.fleet.agent.plist"

	machineIDPlaceholder = "@MACHINE_ID@"
	tokenPlaceholder     = "<one-time token: filled in when the script is downloaded with the code>"
)

// Operating systems an agent can enroll from.
const (
	OSLinux  = "linux"
	OSDarwin = "darwin"
)

// ErrMacWorker is returned when a macOS machine is enrolled as a worker.
var ErrMacWorker = errors.New("enroll: macOS machines can only be monitor agents (evidence capture and the command role are Linux-only)")

// CheckRoleOS refuses a role the operating system cannot serve. os may be
// empty (not known yet): only a known macOS worker is refused.
func CheckRoleOS(role, os string) error {
	switch role {
	case config.RoleWorker, config.RoleMonitor:
	default:
		return fmt.Errorf("enroll: invalid role %q (must be %q or %q)", role, config.RoleWorker, config.RoleMonitor)
	}
	switch os {
	case "", OSLinux, OSDarwin:
	default:
		return fmt.Errorf("enroll: unsupported operating system %q (linux and macOS only)", os)
	}
	if role == config.RoleWorker && os == OSDarwin {
		return ErrMacWorker
	}
	return nil
}

// ScriptParams is everything baked into the bootstrap script.
type ScriptParams struct {
	BaseURL    string // https://host:port of the enrollment endpoint
	Pin        string // base64 SHA-256 of the endpoint's public key (curl --pinnedpubkey sha256//...)
	CASHA256   string // hex SHA-256 of the CA certificate (DER), double-checked after download
	Token      string // the one-time token; empty renders the "show script" placeholder
	Role       string
	ServerAddr string // the agent_dial listener the agent dials, host:port
	ServerID   string // identity the agent expects of the main server
}

// OneLiner is the command an operator hands to the person at the machine. The
// short code (not the token) is what travels in it; the endpoint's TLS key is
// pinned so the pipe into bash cannot be intercepted.
func OneLiner(baseURL, pin, code string) string {
	return fmt.Sprintf("curl -fsSk --pinnedpubkey %s %s | sudo bash",
		shq("sha256//"+pin), shq(strings.TrimRight(baseURL, "/")+"/enroll/e/"+code))
}

// shq single-quotes s for a POSIX shell.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// RenderAgentConfig is the agent's config file; the machine id is the
// placeholder the installer substitutes with the id the server assigned.
func RenderAgentConfig(role, serverAddr, serverID string) string {
	doc := struct {
		Agent config.AgentConfig `json:"agent"`
	}{Agent: config.AgentConfig{
		Role:        role,
		MachineID:   machineIDPlaceholder,
		ConnectMode: config.ConnectModeAgentDial,
		ServerAddr:  serverAddr,
		ServerID:    serverID,
		CAFile:      ConfigDir + "/ca.crt",
		CertFile:    ConfigDir + "/agent.crt",
		KeyFile:     ConfigDir + "/agent.key",
	}}
	b, _ := json.MarshalIndent(doc, "", "  ") // a struct of strings cannot fail to marshal
	return string(b) + "\n"
}

// RenderSystemdUnit is the Linux service: enabled at boot and restarted
// whatever the reason it ends (Restart=always). The agent runs as root, as the
// SSH-onboarded unit does (architecture 5).
func RenderSystemdUnit() string {
	return fmt.Sprintf(`[Unit]
Description=Fleet agent
After=network-online.target
Wants=network-online.target

[Service]
User=root
Group=root
Environment=FLEET_DATA_DIR=%[3]s
ExecStart=%[1]s --config %[2]s/config.json
WorkingDirectory=%[3]s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, BinaryPath, ConfigDir, StateDir)
}

// RenderLaunchdPlist is the macOS service (a launch daemon, so it runs at boot
// before anyone logs in): RunAtLoad and KeepAlive.
func RenderLaunchdPlist() string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%[1]s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%[2]s</string>
    <string>--config</string>
    <string>%[3]s/config.json</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>FLEET_DATA_DIR</key><string>%[4]s</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>3</integer>
  <key>StandardErrorPath</key><string>/var/log/fleet-agent.log</string>
  <key>StandardOutPath</key><string>/var/log/fleet-agent.log</string>
</dict>
</plist>
`, LaunchdLabel, BinaryPath, ConfigDir, StateDir)
}

// Service-control commands, one set per operating system. The panel's Stop
// sends StopCommand to the agent; the machine's own `fleet-agent start` (the
// wrapper) runs StartCommand.

// StopCommand disables the service so it stays down across a reboot, and stops
// it. The disable comes first: the agent runs the command and is stopped by it.
func StopCommand(os string) string {
	if os == OSDarwin {
		return "launchctl disable system/" + LaunchdLabel + " && launchctl bootout system/" + LaunchdLabel
	}
	return "systemctl disable --now --no-block " + SystemdUnit
}

// StartCommand re-enables the service and starts it.
func StartCommand(os string) string {
	if os == OSDarwin {
		return "launchctl enable system/" + LaunchdLabel + " && launchctl bootstrap system " + LaunchdPlistPath
	}
	return "systemctl enable --now " + SystemdUnit
}

// RenderWrapper is /usr/local/bin/fleet-agent: start|stop|status control the
// service, anything else runs the real agent binary.
func RenderWrapper() string {
	return fmt.Sprintf(`#!/bin/sh
# fleet-agent: service control for this machine's fleet agent.
#   fleet-agent start   enable the service (at boot) and start it
#   fleet-agent stop    disable the service and stop it (stays down after a reboot)
#   fleet-agent status  show the service state
# Anything else is passed to the agent binary (fleet-agent --version, ...).
BIN=%[1]q
if [ "$(uname -s)" = Darwin ]; then
  case "${1:-}" in
    start)  exec sh -c %[2]q ;;
    stop)   exec sh -c %[3]q ;;
    status) exec launchctl print system/%[4]s ;;
  esac
else
  case "${1:-}" in
    start)  exec sh -c %[5]q ;;
    stop)   exec sh -c %[6]q ;;
    status) exec systemctl status %[7]s ;;
  esac
fi
exec "$BIN" "$@"
`, BinaryPath, StartCommand(OSDarwin), StopCommand(OSDarwin), LaunchdLabel,
		StartCommand(OSLinux), StopCommand(OSLinux), SystemdUnit)
}

// RenderScript renders the bootstrap script. With an empty Token it renders
// the read-only "show script" view (the token is a placeholder there).
func RenderScript(p ScriptParams) string {
	token := p.Token
	if token == "" {
		token = tokenPlaceholder
	}
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	w(`#!/bin/bash
# Fleet agent enrollment (role: %s).
#
# What this does, in order. Read it before you run it.
#   1. Downloads the fleet CA certificate and the agent binary from %s,
#      over TLS whose public key is pinned below, so a man in the middle cannot
#      substitute either; the CA certificate is also checked against a SHA-256
#      fingerprint baked into this script.
#   2. Generates this machine's private key HERE (it never leaves this machine)
#      and a certificate request, and sends only the request plus a few facts
#      about the machine (host name, OS, CPU, memory) with the one-time token.
#   3. Installs the signed certificate, the agent, its config (connect_mode
#      agent_dial: the agent dials OUT to %s) and a permanent service:
#      systemd on Linux, launchd on macOS, enabled at boot and restarted if it
#      ever stops. Needs root.
#   4. Installs a "fleet-agent" command: start | stop | status.
# macOS machines can only be monitor agents (capture is Linux-only).
set -euo pipefail

BASE=%s
PIN=%s
CA_SHA256=%s
ROLE=%s
SERVER_ADDR=%s
TOKEN=%s
`, p.Role, p.BaseURL, p.ServerAddr, shq(p.BaseURL), shq("sha256//"+p.Pin), shq(p.CASHA256),
		shq(p.Role), shq(p.ServerAddr), shq(token))

	w(`
# The whole installer is one function that bash parses before running anything,
# so nothing below can consume the rest of a piped script.
main() {
die() { echo "fleet enrollment: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo bash)"
command -v curl >/dev/null || die "curl is required"
command -v openssl >/dev/null || die "openssl is required"

case "$(uname -s)" in
  Linux)  OS=linux ;;
  Darwin) OS=darwin ;;
  *) die "unsupported operating system: $(uname -s) (Linux and macOS only)" ;;
esac
if [ "$OS" = darwin ] && [ "$ROLE" = worker ]; then
  die "macOS machines can only be monitor agents (evidence capture is Linux-only)"
fi
if [ "$OS" = linux ]; then
  command -v systemctl >/dev/null || die "systemd (systemctl) is required on Linux"
fi
case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

umask 077
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
# The token goes to curl through a private file, not the command line.
printf 'Authorization: Bearer %%s\n' "$TOKEN" > "$WORK/auth"

# fetch <url> <out> [curl args...]: pinned TLS, fails with the server's message.
fetch() {
  local url="$1" out="$2"; shift 2
  local code
  code="$(curl -sS -k --pinnedpubkey "$PIN" -H "@$WORK/auth" -o "$out" -w '%%{http_code}' "$@" "$url")" \
    || die "cannot reach $BASE"
  if [ "$code" != 200 ]; then
    echo "fleet enrollment: the server answered HTTP $code: $(head -c 300 "$out" 2>/dev/null)" >&2
    exit 1
  fi
}

echo "Downloading the CA certificate..."
fetch "$BASE/enroll/ca.crt" "$WORK/ca.crt"
GOT="$(openssl x509 -in "$WORK/ca.crt" -outform DER | openssl dgst -sha256 | awk '{print $NF}')"
[ "$GOT" = "$CA_SHA256" ] || die "the CA certificate does not match the fingerprint in this script; aborting"

echo "Downloading the agent ($OS/$ARCH)..."
fetch "$BASE/enroll/agent?os=$OS&arch=$ARCH" "$WORK/fleet-agent"

echo "Generating this machine's key and certificate request..."
openssl ecparam -name prime256v1 -genkey -noout -out "$WORK/ec.key"
openssl pkcs8 -topk8 -nocrypt -in "$WORK/ec.key" -out "$WORK/agent.key"
openssl req -new -key "$WORK/agent.key" -subj "/CN=enrolling" -out "$WORK/agent.csr"

HOSTN="$(hostname 2>/dev/null || echo unknown)"
KERNEL="$(uname -r)"
if [ "$OS" = linux ]; then
  OSVER="$( (. /etc/os-release 2>/dev/null && echo "${PRETTY_NAME:-linux}") || echo linux)"
  CPUS="$(nproc 2>/dev/null || echo 0)"
  MEM="$(awk '/^MemTotal:/ {print $2 * 1024}' /proc/meminfo 2>/dev/null || echo 0)"
else
  OSVER="macOS $(sw_vers -productVersion 2>/dev/null || echo)"
  CPUS="$(sysctl -n hw.ncpu 2>/dev/null || echo 0)"
  MEM="$(sysctl -n hw.memsize 2>/dev/null || echo 0)"
fi

echo "Enrolling..."
fetch "$BASE/enroll/csr" "$WORK/resp" -X POST \
  --data-urlencode "csr@$WORK/agent.csr" \
  --data-urlencode "hostname=$HOSTN" --data-urlencode "os=$OS" --data-urlencode "arch=$ARCH" \
  --data-urlencode "os_version=$OSVER" --data-urlencode "kernel=$KERNEL" \
  --data-urlencode "cpus=$CPUS" --data-urlencode "mem_bytes=$MEM"
# The reply: the assigned machine id on the first line, the certificate after it.
ID="$(head -n 1 "$WORK/resp")"
case "$ID" in
  ''|*[!A-Za-z0-9._-]*) die "the server sent an invalid machine id" ;;
esac
tail -n +2 "$WORK/resp" > "$WORK/agent.crt"
openssl x509 -in "$WORK/agent.crt" -noout || die "the server sent an invalid certificate"

echo "Installing as $ID..."
install -d -m 0755 "$(dirname %s)" /usr/local/bin
install -d -m 0750 %s %s
install -m 0755 "$WORK/fleet-agent" %s
install -m 0644 "$WORK/ca.crt" %s/ca.crt
install -m 0644 "$WORK/agent.crt" %s/agent.crt
install -m 0600 "$WORK/agent.key" %s/agent.key
sed "s/%s/$ID/" > %s/config.json <<'FLEET_CONFIG'
%sFLEET_CONFIG
chmod 0640 %s/config.json
cat > %s <<'FLEET_WRAPPER'
%sFLEET_WRAPPER
chmod 0755 %s

if [ "$OS" = linux ]; then
  install -d -m 0755 "$(dirname %s)"
  cat > %s <<'FLEET_UNIT'
%sFLEET_UNIT
  systemctl daemon-reload
  systemctl enable %s
  systemctl restart %s
else
  install -d -m 0755 "$(dirname %s)"
  cat > %s <<'FLEET_PLIST'
%sFLEET_PLIST
  chmod 0644 %s
  launchctl bootout system/%s 2>/dev/null || true
  launchctl enable system/%s
  launchctl bootstrap system %s
fi

echo "Done: $ID is enrolled as a permanent %s agent and is connecting to $SERVER_ADDR."
echo "Control it with: fleet-agent start | stop | status"
}
main </dev/null
`, shq(BinaryPath), shq(ConfigDir), shq(StateDir), shq(BinaryPath),
		ConfigDir, ConfigDir, ConfigDir, machineIDPlaceholder, ConfigDir,
		RenderAgentConfig(p.Role, p.ServerAddr, p.ServerID), ConfigDir,
		shq(WrapperPath), RenderWrapper(), shq(WrapperPath),
		shq(SystemdUnitPath), shq(SystemdUnitPath), RenderSystemdUnit(), SystemdUnit, SystemdUnit,
		shq(LaunchdPlistPath), shq(LaunchdPlistPath), RenderLaunchdPlist(), shq(LaunchdPlistPath),
		LaunchdLabel, LaunchdLabel, shq(LaunchdPlistPath), p.Role)
	return b.String()
}
