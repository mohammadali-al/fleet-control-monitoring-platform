// Command fleet-server is the main server binary. Scaffolding only.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/logging"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/panel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
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

	exe, err := os.Executable()
	if err != nil {
		log.Warn("cannot determine binary path", "component", "fleet-server", "err", err)
	}
	dataDir, err := cfg.Server.ResolveDataDir(exe)
	if err != nil {
		log.Error("data dir", "component", "fleet-server", "err", err)
		return 1
	}

	ln, err := panel.Listen(cfg.Server.ListenAddr)
	if err != nil {
		log.Error("listen", "component", "fleet-server", "addr", cfg.Server.ListenAddr, "err", err)
		return 1
	}

	// SIGINT/SIGTERM cancel ctx, which triggers graceful shutdown of the panel.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting", "component", "fleet-server", "version", version.Version, "config", *cfgPath)
	log.Info("data directory", "component", "fleet-server", "path", dataDir)
	log.Info("panel listening", "component", "fleet-server", "url", panel.URL(cfg.Server.ListenAddr), "addr", ln.Addr().String())

	if err := panel.Serve(ctx, ln, panel.Handler(web.FS), log); err != nil {
		log.Error("panel server", "component", "fleet-server", "err", err)
		return 1
	}
	log.Info("shutting down", "component", "fleet-server", "reason", "signal")
	return 0
}
