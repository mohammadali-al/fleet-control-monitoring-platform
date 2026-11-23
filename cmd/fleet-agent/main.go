// Command fleet-agent is the role-profiled agent daemon. Scaffolding only.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/logging"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
)

func main() { os.Exit(run()) }

func run() int {
	cfgPath := flag.String("config", "", "path to config file (JSON); defaults are used if absent")
	role := flag.String("role", "", "agent role: worker|monitor (overrides config)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return 0
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet-agent: %v\n", err)
		return 1
	}
	agent := cfg.Agent
	if *role != "" {
		agent.Role = *role
	}
	if err := agent.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "fleet-agent: %v\n", err)
		return 2
	}
	log, err := logging.New(os.Stderr, agent.Log.Level, agent.Log.Format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet-agent: %v\n", err)
		return 1
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	log.Info("starting", "component", "fleet-agent", "version", version.Version, "role", agent.Role, "config", *cfgPath)
	sig := <-sigs
	log.Info("shutting down", "component", "fleet-agent", "signal", sig.String())
	return 0
}
