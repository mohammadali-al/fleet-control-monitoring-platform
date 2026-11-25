package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/migrate"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
)

func TestCLIMigrateSubcommandAndStartupGuard(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.NewCA(pki.Options{CommonName: "cli-ca"})
	if err != nil {
		t.Fatal(err)
	}
	caDir := filepath.Join(dataDir, config.CADirName)
	if err := ca.Save(caDir); err != nil {
		t.Fatal(err)
	}
	wantCert, _ := os.ReadFile(filepath.Join(caDir, pki.CACertFile))
	wantKey, _ := os.ReadFile(filepath.Join(caDir, pki.CAKeyFile))
	if err := os.WriteFile(filepath.Join(dataDir, config.SchemaVersionFile), []byte("1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(root, "config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"schema_version": 1, "server": {"listen_addr": "127.0.0.1:0"}}`), 0o640); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"migrate", "--config", cfgPath, "--data-dir", dataDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("migrate exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "backed up") || !strings.Contains(stdout.String(), "upgraded") {
		t.Fatalf("unexpected migrate output: %q", stdout.String())
	}
	gotCert, _ := os.ReadFile(filepath.Join(caDir, pki.CACertFile))
	gotKey, _ := os.ReadFile(filepath.Join(caDir, pki.CAKeyFile))
	if !bytes.Equal(gotCert, wantCert) || !bytes.Equal(gotKey, wantKey) {
		t.Fatal("CA modified by fleet-server migrate")
	}

	// Re-run is a no-op.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"migrate", "--config", cfgPath, "--data-dir", dataDir}, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "already at schema version") {
		t.Fatalf("second migrate exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	// Newer data dir makes both migrate and startup refuse clearly.
	if err := os.WriteFile(filepath.Join(dataDir, config.SchemaVersionFile), []byte("99\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"migrate", "--config", cfgPath, "--data-dir", dataDir}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), migrate.ErrNewerSchema.Error()) {
		t.Fatalf("newer migrate exit=%d stderr=%q", code, stderr.String())
	}

	t.Setenv(config.EnvDataDir, dataDir)
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--config", cfgPath}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), migrate.ErrNewerSchema.Error()) {
		t.Fatalf("startup on newer data dir exit=%d stderr=%q", code, stderr.String())
	}

	// Newer config schema_version also refuses at startup.
	newerCfg := filepath.Join(root, "newer.json")
	if err := os.WriteFile(newerCfg, []byte(`{"schema_version": 99}`), 0o640); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--config", newerCfg}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), config.ErrNewerSchema.Error()) {
		t.Fatalf("startup on newer config exit=%d stderr=%q", code, stderr.String())
	}
}
