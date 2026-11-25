// Command fleet-agent is the role-profiled agent daemon. The worker role runs
// the control-channel listener, the command executor and evidence capture
// (internal/agent); the monitor role runs the probe engine for the targets the
// main server pushes and flushes the results back over the channel.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/logging"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// runner is a built agent of either role: it is bound to its channel address
// and serves until its context is cancelled.
type runner interface {
	Addr() net.Addr
	Run(ctx context.Context) error
}

// run is main with its inputs explicit: it serves until ctx is cancelled (a
// signal, in production) and returns the process exit code.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fleet-agent", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config file (JSON); defaults are used if absent")
	role := fs.String("role", "", "agent role: worker|monitor (overrides config)")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *showVersion {
		fmt.Fprintln(stdout, version.String())
		return 0
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
		return 1
	}
	ac := cfg.Agent
	if *role != "" {
		ac.Role = *role
	}
	if err := ac.Validate(); err != nil {
		fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
		return 2
	}
	log, err := logging.New(stderr, ac.Log.Level, ac.Log.Format)
	if err != nil {
		fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
		return 1
	}

	var (
		a       runner
		dataDir string
	)
	switch ac.Role {
	case config.RoleMonitor:
		// A monitor agent needs no data directory and no root: probes are
		// unprivileged (raw ICMP is the one exception and fails per probe).
		a, err = agent.MonitorFromConfig(ac, log)
	default:
		dataDir, err = ac.ResolveDataDir()
		if err != nil {
			fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
			return 1
		}
		a, err = agent.FromConfig(ac, dataDir, log)
	}
	if err != nil {
		fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
		return 1
	}

	log.Info("starting", "component", "fleet-agent", "version", version.Version, "role", ac.Role,
		"machine", ac.MachineID, "config", *cfgPath, "data_dir", dataDir, "listen", a.Addr().String())
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "component", "fleet-agent", "err", err)
		return 1
	}
	log.Info("shut down", "component", "fleet-agent")
	return 0
}
