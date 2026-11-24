// Package agent is the worker-agent runtime: it listens for the main server on
// the mTLS control channel (the default server_dial direction: the agent
// listens, the main server dials), executes the commands the server sends
// (internal/exec), captures evidence per session (internal/capture), and takes
// its configuration -- the dangerous-command gate secret and the capture
// settings -- from the main server over the channel (config-push).
//
// The agent holds no key material in code: its certificate, key and the pinned
// CA are read from the files the onboarding flow wrote (D-0006); the gate secret
// arrives over the channel and lives in memory only.
package agent

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

// defaultConfigWait is how long a run request waits for the first config push
// before it is answered with an error (the push normally arrives within
// milliseconds of the connection).
const defaultConfigWait = 5 * time.Second

// Config configures a worker Agent.
type Config struct {
	// MachineID is this agent's id (the CN of its certificate).
	MachineID string
	// DataDir is the base data directory (D-0006); evidence goes under
	// DataDir/evidence/<machine>.
	DataDir string
	// ListenAddr is the channel listen address (default ":7443").
	ListenAddr string
	// TLS is the listener's TLS config (pki.ServerTLSConfig: client certs
	// required, the pinned CA the only trust anchor). Required.
	TLS *tls.Config
	// CgroupRoot is the cgroup v2 directory for session cgroups. Empty runs
	// commands without cgroups, and then nothing can be captured.
	CgroupRoot string
	// Capture is the local capture configuration; a capture section pushed by
	// the main server replaces it.
	Capture config.CaptureConfig

	// CaptureFactory builds the capture subsystem once a config is applied
	// (default: live kernel capture, which needs root; see liveFactory).
	CaptureFactory CaptureFactory
	// ConfigWait bounds how long a run waits for the first config (default 5 s).
	ConfigWait time.Duration
	// TermGrace is the SIGTERM -> SIGKILL delay of the executor (default 3 s).
	TermGrace time.Duration
	// Shell runs one-shot commands (`Shell -c command`); default /bin/sh.
	Shell string
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Agent is a running worker agent. New binds the listener; Run serves it.
type Agent struct {
	log *slog.Logger
	ln  *channel.Listener
	w   *worker
}

// New validates cfg and binds the channel listener.
func New(cfg Config) (*Agent, error) {
	if cfg.MachineID == "" {
		return nil, errors.New("agent: machine id is required")
	}
	if cfg.TLS == nil {
		return nil, errors.New("agent: a TLS config is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("agent: a data dir is required")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = fmt.Sprintf(":%d", config.DefaultChannelPort)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	w := newWorker(cfg)
	ln, err := channel.Listen(channel.ListenerConfig{
		Addr:      cfg.ListenAddr,
		TLS:       cfg.TLS,
		MachineID: cfg.MachineID,
		Handler:   w.handle,
		OnConnect: w.onConnect,
	})
	if err != nil {
		return nil, err
	}
	w.sender.set(ln)
	return &Agent{log: cfg.Logger, ln: ln, w: w}, nil
}

// FromConfig builds an Agent from the agent section of the config file: the
// pinned CA and this machine's certificate and key are read from the files the
// onboarding flow wrote.
func FromConfig(ac config.AgentConfig, dataDir string, log *slog.Logger) (*Agent, error) {
	if ac.MachineID == "" {
		return nil, errors.New("agent: machine_id is not set in the agent config")
	}
	if ac.ConnectMode == config.ConnectModeAgentDial {
		return nil, errors.New("agent: connect_mode agent_dial is not supported yet (only server_dial)")
	}
	tlsCfg, err := LoadTLS(ac.CAFile, ac.CertFile, ac.KeyFile)
	if err != nil {
		return nil, err
	}
	cg := ac.CgroupRoot
	if cg == "" {
		cg = DefaultCgroupRoot()
	}
	return New(Config{
		MachineID:  ac.MachineID,
		DataDir:    dataDir,
		ListenAddr: ac.ListenAddrOrDefault(),
		TLS:        tlsCfg,
		CgroupRoot: cg,
		Capture:    ac.Capture,
		Logger:     log,
	})
}

// LoadTLS reads the pinned CA, certificate and key files and builds the
// listener's TLS config.
func LoadTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	for name, path := range map[string]string{"ca_file": caFile, "cert_file": certFile, "key_file": keyFile} {
		if path == "" {
			return nil, fmt.Errorf("agent: %s is not set in the agent config (was the machine onboarded?)", name)
		}
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("agent: read CA: %w", err)
	}
	cert, err := os.ReadFile(certFile)
	if err != nil {
		return nil, fmt.Errorf("agent: read certificate: %w", err)
	}
	key, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, fmt.Errorf("agent: read key: %w", err)
	}
	return pki.ServerTLSConfig(cert, key, ca)
}

// DefaultCgroupRoot is where session cgroups go when none is configured:
// /sys/fs/cgroup/fleet for root on a cgroup v2 host, "" (no cgroups) otherwise.
func DefaultCgroupRoot() string {
	if os.Geteuid() != 0 {
		return ""
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err != nil {
		return ""
	}
	return filepath.Join("/sys/fs/cgroup", "fleet")
}

// Addr is the bound channel address (useful with port 0).
func (a *Agent) Addr() net.Addr { return a.ln.Addr() }

// Connected reports whether the main server is connected and verified.
func (a *Agent) Connected() bool { return a.ln.Peer() != "" }

// Run serves the channel until ctx is cancelled, then terminates every running
// session, stops captures and returns.
func (a *Agent) Run(ctx context.Context) error {
	a.w.start(ctx)
	err := a.ln.Serve(ctx)
	a.w.shutdown()
	return err
}
