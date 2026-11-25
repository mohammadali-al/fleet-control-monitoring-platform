// Package config loads the fleet-server and fleet-agent configuration.
//
// Format choice: JSON, via the standard library's encoding/json. The brief
// allows TOML or YAML but also says "no new config library unless stdlib can't
// do it"; neither TOML nor YAML is in the standard library, so JSON keeps the
// module dependency-free. Unknown fields are rejected so typos fail loudly.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Agent roles (one binary, two role profiles).
const (
	RoleWorker  = "worker"
	RoleMonitor = "monitor"
)

// Connection initiation modes (architecture 4.2). server_dial is the default.
const (
	ConnectModeServerDial = "server_dial"
	ConnectModeAgentDial  = "agent_dial"
)

// Subdirectories of the base data dir (D-0006).
const (
	RegistryDirName = "registry"
	CADirName       = "ca"
	ConfigDirName   = "config"
	MonitorDirName  = "monitor"
	// WatchdogDirName holds the watchdog's last-known policy and guard counters.
	WatchdogDirName = "watchdog"
	// ProjectsDirName holds Maneuver/project metadata (part of the backup set,
	// T-0013).
	ProjectsDirName = "projects"
)

// Backup state files under <data>/config/ (T-0013). They are deliberately not
// part of the backup set: the credential file never leaves the main server.
const (
	BackupStateFile      = "backup.json"       // backup server record, schedule, last run
	BackupCredentialFile = "backup-credential" // SSH key (or fallback password), mode 0600
)

// Control-channel defaults for the main server (architecture 4.1, 4.2).
const (
	// DefaultChannelPort is the port a worker or monitor agent listens on
	// under server_dial; the main server dials <machine IP>:<port>.
	DefaultChannelPort = 7443
	// DefaultServerID is the main server's identity (its client certificate CN).
	DefaultServerID = "fleet-server"
	// DefaultAgentDialPort is the conventional port of the main server's
	// agent_dial listener (ServerConfig.AgentDialAddr). The listener is off
	// unless an address is configured; this is the value to put there.
	DefaultAgentDialPort = 7444
)

// LogConfig selects logging behavior. Environment variables
// (FLEET_LOG_LEVEL, FLEET_LOG_FORMAT) take precedence; see internal/logging.
type LogConfig struct {
	Level  string `json:"level"`  // debug|info|warn|error
	Format string `json:"format"` // text|json
}

// Defaults and environment overrides for the server (D-0004, D-0006).
const (
	DefaultListenAddr = ":6789"
	EnvListenAddr     = "FLEET_LISTEN_ADDR"
	EnvDataDir        = "FLEET_DATA_DIR"
)

// ServerConfig is the fleet-server configuration.
type ServerConfig struct {
	// ListenAddr is the panel's listen address (default ":6789"). The
	// FLEET_LISTEN_ADDR environment variable overrides the file value.
	ListenAddr string `json:"listen_addr"`
	// DataDir is the base data directory for all persistent state. Empty
	// means "use the default"; see ResolveDataDir. The FLEET_DATA_DIR
	// environment variable overrides the file value.
	DataDir string `json:"data_dir"`
	// AgentBinaryPath is the fleet-agent binary the onboarding flow installs
	// on new machines (packaging is deferred, so it is configured, not
	// embedded). Empty means onboarding is unavailable and reports a clear
	// error.
	AgentBinaryPath string `json:"agent_binary_path,omitempty"`
	// ChannelPort is the port agents listen on for the control channel under
	// server_dial. Zero means DefaultChannelPort.
	ChannelPort int `json:"channel_port,omitempty"`
	// ServerID is the main server's identity on the control channel (the CN of
	// its client certificate). Empty means DefaultServerID.
	ServerID string `json:"server_id,omitempty"`
	// AgentDialAddr is where the main server listens for agent-initiated
	// connections (the agent_dial direction), e.g. ":7444". Empty means the
	// listener is off and agent_dial machines stay offline; server_dial is
	// unaffected either way.
	AgentDialAddr string `json:"agent_dial_addr,omitempty"`
	// AgentCapture is the evidence-capture configuration the main server
	// pushes to every worker agent over the channel (config-push, T-0018).
	// An all-zero section pushes nothing, leaving the agent's own defaults.
	AgentCapture CaptureConfig `json:"agent_capture,omitempty"`
	Log          LogConfig     `json:"log"`
}

// ChannelPortOrDefault returns ChannelPort, or DefaultChannelPort when unset.
func (s ServerConfig) ChannelPortOrDefault() int {
	if s.ChannelPort > 0 {
		return s.ChannelPort
	}
	return DefaultChannelPort
}

// ServerIDOrDefault returns ServerID, or DefaultServerID when unset.
func (s ServerConfig) ServerIDOrDefault() string {
	if s.ServerID != "" {
		return s.ServerID
	}
	return DefaultServerID
}

// AgentConfig is the fleet-agent configuration.
type AgentConfig struct {
	// Role is "worker" or "monitor". There is deliberately no default: an
	// agent with no role must fail validation.
	Role string `json:"role"`
	// The fields below are written by the onboarding flow (architecture 4.5);
	// all are optional so a hand-written agent config stays valid.

	// MachineID is the identity issued at onboarding (the client cert CN).
	MachineID string `json:"machine_id,omitempty"`
	// ConnectMode is the per-machine dial direction: server_dial (default
	// when empty) or agent_dial.
	ConnectMode string `json:"connect_mode,omitempty"`
	// ServerAddr is the main server's agent_dial listener (host:port) that an
	// agent_dial agent dials out to. Required in that mode, unused otherwise.
	ServerAddr string `json:"server_addr,omitempty"`
	// ServerID is the identity (certificate CN) the agent expects of the main
	// server under agent_dial. Empty means DefaultServerID.
	ServerID string `json:"server_id,omitempty"`
	// CAFile is the pinned internal CA certificate; CertFile and KeyFile are
	// this machine's client certificate and key.
	CAFile   string `json:"ca_file,omitempty"`
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`

	// ListenAddr is where a server_dial agent listens for the main server
	// (default ":7443"; see ListenAddrOrDefault).
	ListenAddr string `json:"listen_addr,omitempty"`
	// DataDir is the agent's base data directory (D-0006): evidence segments
	// live under <DataDir>/evidence/<machine>. Empty means FLEET_DATA_DIR,
	// else the agent's working directory (the onboarding unit sets that to the
	// state dir); see ResolveDataDir.
	DataDir string `json:"data_dir,omitempty"`
	// CgroupRoot is the cgroup v2 directory under which each command session
	// gets its own cgroup (needed for evidence capture). Empty means
	// /sys/fs/cgroup/fleet when running as root on a cgroup v2 host, else
	// cgroups (and so capture) are off.
	CgroupRoot string `json:"cgroup_root,omitempty"`

	Log LogConfig `json:"log"`

	// Capture tunes the worker-agent evidence-capture subsystem (chapter 6,
	// 7.1). All fields are optional; zero values select the built-in defaults.
	Capture CaptureConfig `json:"capture,omitempty"`

	// Watchdog holds the host-local watchdog settings (chapter 11). Thresholds
	// and the ladder are policy, pushed by the server; only what is specific
	// to this host lives here.
	Watchdog WatchdogConfig `json:"watchdog,omitempty"`
}

// WatchdogConfig is the host-local part of the watchdog configuration (T-0012).
type WatchdogConfig struct {
	// Groups are the policy-override groups this host belongs to; a profile's
	// per-group overrides apply in this order, then the host's own override.
	Groups []string `json:"groups,omitempty"`
	// EgressInterface is the interface shaped when egress is throttled. Empty
	// means the default route's interface.
	EgressInterface string `json:"egress_interface,omitempty"`
}

// CaptureConfig configures evidence capture on a worker agent (T-0008). It is
// read by internal/capture; zero fields fall back to that package's defaults.
type CaptureConfig struct {
	// TrimThresholdBytes is the per-node payload trim threshold (§6.3): a
	// payload longer than this is stored trimmed (head only). 0 -> default.
	TrimThresholdBytes int `json:"trim_threshold_bytes,omitempty"`
	// SegmentMaxBytes and SegmentMaxAgeSeconds roll segments (§7.1): a new
	// segment starts on the first of 50 MB or 5 min by default. 0 -> default.
	SegmentMaxBytes      int64 `json:"segment_max_bytes,omitempty"`
	SegmentMaxAgeSeconds int   `json:"segment_max_age_seconds,omitempty"`
	// DiskWarnPercent / DiskPausePercent are the local disk guard (§6.5):
	// warn at 80 %, pause capture writes at 90 % by default. 0 -> default.
	DiskWarnPercent  int `json:"disk_warn_percent,omitempty"`
	DiskPausePercent int `json:"disk_pause_percent,omitempty"`
	// Reflectors overrides the public-IP reflector set (§6.4). Empty ->
	// default six services.
	Reflectors []string `json:"reflectors,omitempty"`
	// NFLOGGroup is the base nftables NFLOG group for the live capture path.
	// 0 -> default. Each concurrent session uses a distinct group above it.
	NFLOGGroup int `json:"nflog_group,omitempty"`
}

// IsZero reports whether no capture setting is set.
func (c CaptureConfig) IsZero() bool {
	return c.TrimThresholdBytes == 0 && c.SegmentMaxBytes == 0 && c.SegmentMaxAgeSeconds == 0 &&
		c.DiskWarnPercent == 0 && c.DiskPausePercent == 0 && len(c.Reflectors) == 0 && c.NFLOGGroup == 0
}

// Config is the on-disk file: one section per binary.
type Config struct {
	Server ServerConfig `json:"server"`
	Agent  AgentConfig  `json:"agent"`
}

// Defaults returns the configuration used when no file is present.
func Defaults() Config {
	log := LogConfig{Level: "info", Format: "text"}
	return Config{
		Server: ServerConfig{ListenAddr: DefaultListenAddr, Log: log},
		Agent:  AgentConfig{Log: log},
	}
}

// ServerIDOrDefault returns the main server identity an agent_dial agent
// expects, or DefaultServerID when unset.
func (a AgentConfig) ServerIDOrDefault() string {
	if a.ServerID != "" {
		return a.ServerID
	}
	return DefaultServerID
}

// Dials reports whether this agent dials out to the main server (agent_dial)
// rather than listening for it (server_dial, the default).
func (a AgentConfig) Dials() bool { return a.ConnectMode == ConnectModeAgentDial }

// ListenAddrOrDefault returns ListenAddr, or ":7443" when unset.
func (a AgentConfig) ListenAddrOrDefault() string {
	if a.ListenAddr != "" {
		return a.ListenAddr
	}
	return fmt.Sprintf(":%d", DefaultChannelPort)
}

// ResolveDataDir returns the agent's absolute base data directory and creates
// it (mode 0o750) if needed: DataDir, else $FLEET_DATA_DIR, else the working
// directory.
func (a AgentConfig) ResolveDataDir() (string, error) {
	dir := a.DataDir
	if dir == "" {
		dir = os.Getenv(EnvDataDir)
	}
	if dir == "" {
		dir = "."
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve agent data dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return "", fmt.Errorf("create agent data dir %s: %w", abs, err)
	}
	return abs, nil
}

// Validate reports whether the agent configuration is usable.
func (a AgentConfig) Validate() error {
	switch a.ConnectMode {
	case "", ConnectModeServerDial, ConnectModeAgentDial:
	default:
		return fmt.Errorf("invalid agent connect_mode %q (must be %q or %q)", a.ConnectMode, ConnectModeServerDial, ConnectModeAgentDial)
	}
	switch a.Role {
	case RoleWorker, RoleMonitor:
		return nil
	case "":
		return fmt.Errorf("agent role is not set (use --role or config; must be %q or %q)", RoleWorker, RoleMonitor)
	default:
		return fmt.Errorf("invalid agent role %q (must be %q or %q)", a.Role, RoleWorker, RoleMonitor)
	}
}

// Load reads the config file at path. An empty path or a missing file yields
// Defaults(); any other read or parse error is returned. Fields absent from
// the file keep their default values. Server environment overrides
// (FLEET_LISTEN_ADDR, FLEET_DATA_DIR) are applied last and win over the file.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			dec := json.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&cfg); err != nil {
				return Config{}, fmt.Errorf("parse config %s: %w", path, err)
			}
		case errors.Is(err, fs.ErrNotExist):
			// Missing file: keep the defaults.
		default:
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
	}
	cfg.Server.applyEnv()
	return cfg, nil
}

func (s *ServerConfig) applyEnv() {
	if v := os.Getenv(EnvListenAddr); v != "" {
		s.ListenAddr = v
	}
	if v := os.Getenv(EnvDataDir); v != "" {
		s.DataDir = v
	}
}

// ResolveDataDir returns the absolute base data directory (D-0006) and creates
// it (mode 0o750) if it does not exist. The directory is s.DataDir (config or
// FLEET_DATA_DIR) when set; otherwise it is a "data" directory one level above
// the binary's install dir, i.e. <dir of exe>/../data. exePath is the path of
// the running binary (os.Executable). Nothing is written into the directory.
func (s ServerConfig) ResolveDataDir(exePath string) (string, error) {
	dir := s.DataDir
	if dir == "" {
		if exePath == "" {
			return "", errors.New("resolve data dir: no data_dir configured and binary path unknown")
		}
		if real, err := filepath.EvalSymlinks(exePath); err == nil {
			exePath = real
		}
		dir = filepath.Join(filepath.Dir(exePath), "..", "data")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir %q: %w", dir, err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return "", fmt.Errorf("create data dir %s: %w", abs, err)
	}
	return abs, nil
}
