// Command fleet-server is the main server binary. Scaffolding only.
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
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return 0
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet-server: %v\n", err)
		return 1
	}
	log, err := logging.New(os.Stderr, cfg.Server.Log.Level, cfg.Server.Log.Format)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fleet-server: %v\n", err)
		return 1
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	log.Info("starting", "component", "fleet-server", "version", version.Version, "config", *cfgPath)
	sig := <-sigs
	log.Info("shutting down", "component", "fleet-server", "signal", sig.String())
	return 0
}
