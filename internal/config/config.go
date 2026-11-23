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
)

// Agent roles (one binary, two role profiles).
const (
	RoleWorker  = "worker"
	RoleMonitor = "monitor"
)

// LogConfig selects logging behavior. Environment variables
// (FLEET_LOG_LEVEL, FLEET_LOG_FORMAT) take precedence; see internal/logging.
type LogConfig struct {
	Level  string `json:"level"`  // debug|info|warn|error
	Format string `json:"format"` // text|json
}

// ServerConfig is the fleet-server configuration.
type ServerConfig struct {
	Log LogConfig `json:"log"`
}

// AgentConfig is the fleet-agent configuration.
type AgentConfig struct {
	// Role is "worker" or "monitor". There is deliberately no default: an
	// agent with no role must fail validation.
	Role string    `json:"role"`
	Log  LogConfig `json:"log"`
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
		Server: ServerConfig{Log: log},
		Agent:  AgentConfig{Log: log},
	}
}

// Validate reports whether the agent configuration is usable.
func (a AgentConfig) Validate() error {
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
// the file keep their default values.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	return cfg, nil
}
