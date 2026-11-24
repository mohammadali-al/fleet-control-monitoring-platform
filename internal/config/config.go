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

// RegistryDirName is the registry's subdirectory of the base data dir (D-0006).
const RegistryDirName = "registry"

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
	AgentBinaryPath string    `json:"agent_binary_path,omitempty"`
	Log             LogConfig `json:"log"`
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
	// CAFile is the pinned internal CA certificate; CertFile and KeyFile are
	// this machine's client certificate and key.
	CAFile   string    `json:"ca_file,omitempty"`
	CertFile string    `json:"cert_file,omitempty"`
	KeyFile  string    `json:"key_file,omitempty"`
	Log      LogConfig `json:"log"`

	// Capture tunes the worker-agent evidence-capture subsystem (chapter 6,
	// 7.1). All fields are optional; zero values select the built-in defaults.
	Capture CaptureConfig `json:"capture,omitempty"`
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
