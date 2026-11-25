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
	if len(args) > 0 && args[0] == "watchdog" {
		return runWatchdog(ctx, args[1:], stderr)
	}
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
	switch {
	case ac.Dials():
		// agent_dial: dial out to the main server instead of listening for it.
		// The same worker / monitor logic, on an outbound connection.
		if ac.Role != config.RoleMonitor {
			dataDir, err = ac.ResolveDataDir()
			if err != nil {
				fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
				return 1
			}
		}
		a, err = agent.DialFromConfig(ac, dataDir, log)
	case ac.Role == config.RoleMonitor:
		// A monitor agent needs no data directory and no root: probes are
		// unprivileged (raw ICMP is the one exception and fails per probe).
		a, err = agent.MonitorFromConfig(ac, log)
	default:
		dataDir, err = ac.ResolveDataDir()
		if err != nil {
			fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
			return 1
		}
		// The worker also takes part in the watchdog protocol: it stores the
		// policy the server pushes and forwards the events the watchdog
		// process (`fleet-agent watchdog`) records.
		a, err = agent.WorkerFromConfig(ac, dataDir, log)
	}
	if err != nil {
		fmt.Fprintf(stderr, "fleet-agent: %v\n", err)
		return 1
	}

	log.Info("starting", "component", "fleet-agent", "version", version.Version, "role", ac.Role,
		"machine", ac.MachineID, "config", *cfgPath, "data_dir", dataDir, "listen", a.Addr().String())
	// The heartbeat fleet-agent-liveness.timer checks (T-0027), for every role.
	agent.StartHeartbeat(ctx, a, ac, dataDir, log)
	if err := a.Run(ctx); err != nil {
		log.Error("agent stopped", "component", "fleet-agent", "err", err)
		return 1
	}
	log.Info("shut down", "component", "fleet-agent")
	return 0
}

// runWatchdog is `fleet-agent watchdog`: the load-safety net as its own
// process (fleet-watchdog.service). It reads the pushed policy from the data
// directory (the main agent stores it there) and applies changes live, acts
// through systemctl/cgroups/tc (root), and records its events in the data
// directory for the main agent to forward. It never opens the channel, so it
// keeps running when the main agent is down. It stops on ctx cancellation.
func runWatchdog(ctx context.Context, args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("fleet-agent watchdog", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config file (JSON); defaults are used if absent")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "fleet-agent watchdog: %v\n", err)
		return 1
	}
	ac := cfg.Agent
	log, err := logging.New(stderr, ac.Log.Level, ac.Log.Format)
	if err != nil {
		fmt.Fprintf(stderr, "fleet-agent watchdog: %v\n", err)
		return 1
	}
	dataDir, err := ac.ResolveDataDir()
	if err != nil {
		fmt.Fprintf(stderr, "fleet-agent watchdog: %v\n", err)
		return 1
	}
	log.Info("starting", "component", "fleet-watchdog", "version", version.Version,
		"machine", ac.MachineID, "config", *cfgPath, "data_dir", dataDir)
	if err := agent.RunWatchdog(ctx, ac, dataDir, log, agent.WatchdogRunOptions{}); err != nil {
		log.Error("watchdog stopped", "component", "fleet-watchdog", "err", err)
		return 1
	}
	log.Info("shut down", "component", "fleet-watchdog")
	return 0
}
