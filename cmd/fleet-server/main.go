// Command fleet-server is the main server binary. Scaffolding only.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/logging"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/migrate"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/panel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/version"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "migrate" {
		return runMigrate(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("fleet-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config file (JSON); defaults are used if absent")
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
		fmt.Fprintf(stderr, "fleet-server: %v\n", err)
		return 1
	}
	log, err := logging.New(stderr, cfg.Server.Log.Level, cfg.Server.Log.Format)
	if err != nil {
		fmt.Fprintf(stderr, "fleet-server: %v\n", err)
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

	// Startup schema guard: initializes fresh empty data dirs at current,
	// auto-migrates older state in place after backing up, and refuses to start
	// if on-disk state is newer than this binary.
	migRes, err := migrate.EnsureCurrent(migrate.Options{DataDir: dataDir, ConfigPath: *cfgPath})
	if err != nil {
		log.Error("schema guard", "component", "fleet-server", "err", err)
		return 1
	}
	if migRes.Migrated {
		log.Info("migrated server state", "component", "fleet-server",
			"from", migRes.FromVersion, "to", migRes.ToVersion, "backup", migRes.BackupDir)
	}

	// The live fleet: registry, internal CA, onboarding, and the channel links
	// and command queues the Operate pages are wired to.
	mgr, err := fleet.Open(fleet.Options{Config: cfg.Server, DataDir: dataDir, Logger: log})
	if err != nil {
		log.Error("fleet", "component", "fleet-server", "err", err)
		return 1
	}
	// The Observe/Admin backends behind the Evidence, Backup, Monitor data and
	// Watchdog pages: the evidence archive, the backup service (its schedule
	// runs with the manager), the monitor jobs and probe-result sink, and the
	// watchdog policy store. Bound before Start so agents get their monitor
	// config and watchdog policy as soon as they connect.
	if err := mgr.EnableObserve(dataDir); err != nil {
		log.Error("observe backends", "component", "fleet-server", "err", err)
		return 1
	}
	// agent_dial: agents that cannot be dialed connect out to this listener
	// (off unless server.agent_dial_addr is set; server_dial is unaffected).
	if cfg.Server.AgentDialAddr != "" {
		if err := mgr.EnableAgentDial(cfg.Server.AgentDialAddr, cfg.Server.ServerIDOrDefault()); err != nil {
			log.Error("agent_dial listener", "component", "fleet-server", "addr", cfg.Server.AgentDialAddr, "err", err)
			return 1
		}
		log.Info("agent_dial listening", "component", "fleet-server", "addr", mgr.AgentDialAddr().String())
	}
	handler, err := panel.New(panel.Options{Assets: web.FS, Templates: web.Templates, Backend: mgr, Logger: log})
	if err != nil {
		log.Error("panel", "component", "fleet-server", "err", err)
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
	mgr.Start(ctx)
	defer mgr.Close()
	// Token enrollment (off unless server.enroll_addr is set): its own TLS
	// listener outside the auth proxy, and the operator API beside the panel.
	if handler, err = enableEnroll(ctx, cfg.Server, dataDir, mgr, handler, log); err != nil {
		log.Error("token enrollment", "component", "fleet-server", "err", err)
		return 1
	}

	log.Info("starting", "component", "fleet-server", "version", version.Version, "config", *cfgPath)
	log.Info("data directory", "component", "fleet-server", "path", dataDir)
	log.Info("panel listening", "component", "fleet-server", "url", panel.URL(cfg.Server.ListenAddr), "addr", ln.Addr().String())

	if err := panel.Serve(ctx, ln, handler, log); err != nil {
		log.Error("panel server", "component", "fleet-server", "err", err)
		return 1
	}
	log.Info("shutting down", "component", "fleet-server", "reason", "signal")
	return 0
}

// runMigrate implements `fleet-server migrate`: upgrades persistent state
// (data dir + optional config file) to migrate.CurrentVersion after backing up
// the data directory. Re-running when already current is a no-op.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fleet-server migrate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "", "path to config file (JSON); defaults are used if absent")
	dataDirFlag := fs.String("data-dir", "", "override data directory (defaults to config / FLEET_DATA_DIR)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "fleet-server migrate: %v\n", err)
		return 1
	}

	var dataDir string
	if *dataDirFlag != "" {
		abs, err := filepath.Abs(*dataDirFlag)
		if err != nil {
			fmt.Fprintf(stderr, "fleet-server migrate: resolve data dir %q: %v\n", *dataDirFlag, err)
			return 1
		}
		dataDir = abs
	} else {
		exe, _ := os.Executable()
		dataDir, err = cfg.Server.ResolveDataDir(exe)
		if err != nil {
			fmt.Fprintf(stderr, "fleet-server migrate: %v\n", err)
			return 1
		}
	}

	res, err := migrate.Run(migrate.Options{DataDir: dataDir, ConfigPath: *cfgPath})
	if err != nil {
		fmt.Fprintf(stderr, "fleet-server migrate: %v\n", err)
		return 1
	}

	switch {
	case res.Initialized:
		fmt.Fprintf(stdout, "fleet-server migrate: initialized %s at schema version %d\n", dataDir, res.ToVersion)
	case res.Migrated:
		if res.BackupDir != "" {
			fmt.Fprintf(stdout, "fleet-server migrate: backed up %s -> %s\n", dataDir, res.BackupDir)
		}
		for _, s := range res.Applied {
			fmt.Fprintf(stdout, "fleet-server migrate: v%d -> v%d: %s\n", s.FromVersion, s.ToVersion, s.Description)
		}
		fmt.Fprintf(stdout, "fleet-server migrate: upgraded %s from v%d to v%d\n", dataDir, res.FromVersion, res.ToVersion)
	default:
		fmt.Fprintf(stdout, "fleet-server migrate: %s already at schema version %d (nothing to do)\n", dataDir, res.ToVersion)
	}
	return 0
}
