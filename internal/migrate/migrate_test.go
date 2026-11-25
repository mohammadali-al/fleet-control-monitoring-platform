package migrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

func seedV1Fixture(t *testing.T, withMarkers bool) (dataDir, cfgPath string, caCertBefore, caKeyBefore []byte) {
	t.Helper()
	root := t.TempDir()
	dataDir = filepath.Join(root, "fleet-data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}

	ca, err := pki.NewCA(pki.Options{CommonName: "test-fleet-ca"})
	if err != nil {
		t.Fatal(err)
	}
	caDir := filepath.Join(dataDir, config.CADirName)
	if err := ca.Save(caDir); err != nil {
		t.Fatal(err)
	}
	caCertBefore, err = os.ReadFile(filepath.Join(caDir, pki.CACertFile))
	if err != nil {
		t.Fatal(err)
	}
	caKeyBefore, err = os.ReadFile(filepath.Join(caDir, pki.CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}

	reg := registry.New()
	if err := reg.Add(registry.Machine{
		ID:          "worker-1",
		Role:        registry.RoleWorker,
		PrivateIPs:  []string{"10.0.0.10"},
		Provisioned: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := registry.NewStore(dataDir).Save(reg); err != nil {
		t.Fatal(err)
	}

	cfgPath = filepath.Join(root, "config.json")
	cfgMap := map[string]any{
		"server": map[string]any{
			"listen_addr":       "127.0.0.1:6789",
			"agent_binary_path": "/usr/local/share/fleet/fleet-agent",
			"log":               map[string]any{"level": "info", "format": "text"},
		},
		"agent": map[string]any{
			"role": "worker",
			"log":  map[string]any{"level": "warn", "format": "json"},
		},
	}
	if withMarkers {
		cfgMap["schema_version"] = 1
		if err := os.WriteFile(filepath.Join(dataDir, config.SchemaVersionFile), []byte("1\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	cfgBytes, err := json.MarshalIndent(cfgMap, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(cfgBytes, '\n'), 0o640); err != nil {
		t.Fatal(err)
	}
	return dataDir, cfgPath, caCertBefore, caKeyBefore
}

func assertCAUntouched(t *testing.T, dataDir string, wantCert, wantKey []byte) {
	t.Helper()
	caDir := filepath.Join(dataDir, config.CADirName)
	entries, err := os.ReadDir(caDir)
	if err != nil {
		t.Fatalf("read ca dir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ca dir entry count = %d, want 2 (%v)", len(entries), entries)
	}
	gotCert, err := os.ReadFile(filepath.Join(caDir, pki.CACertFile))
	if err != nil {
		t.Fatal(err)
	}
	gotKey, err := os.ReadFile(filepath.Join(caDir, pki.CAKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotCert, wantCert) {
		t.Fatalf("ca.crt changed during migration")
	}
	if !bytes.Equal(gotKey, wantKey) {
		t.Fatalf("ca.key changed during migration")
	}
}

func countBackups(t *testing.T, dataDir string) []string {
	t.Helper()
	parent := filepath.Dir(dataDir)
	prefix := filepath.Base(dataDir) + ".bak-"
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) {
			out = append(out, filepath.Join(parent, e.Name()))
		}
	}
	return out
}

func TestRunMigratesStampedV1FixtureAndIsIdempotent(t *testing.T) {
	dataDir, cfgPath, caCert, caKey := seedV1Fixture(t, true)
	fixedNow := func() time.Time { return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC) }

	res, err := Run(Options{DataDir: dataDir, ConfigPath: cfgPath, Now: fixedNow})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Migrated || res.FromVersion != 1 || res.ToVersion != CurrentVersion || len(res.Applied) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	wantBak := dataDir + ".bak-1-20261125T120000Z"
	if res.BackupDir != wantBak {
		t.Fatalf("BackupDir = %q, want %q", res.BackupDir, wantBak)
	}
	if _, err := os.Stat(filepath.Join(wantBak, config.SchemaVersionFile)); err != nil {
		t.Fatalf("backup missing SCHEMA_VERSION: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wantBak, config.CADirName, pki.CAKeyFile)); err != nil {
		t.Fatalf("backup missing ca.key: %v", err)
	}

	assertCAUntouched(t, dataDir, caCert, caKey)

	// Marker updated to current.
	rawVer, err := os.ReadFile(filepath.Join(dataDir, config.SchemaVersionFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(rawVer)) != "2" {
		t.Fatalf("SCHEMA_VERSION = %q, want 2", rawVer)
	}

	// Config updated to current while keeping original settings.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load after migrate: %v", err)
	}
	if cfg.SchemaVersion != CurrentVersion {
		t.Fatalf("cfg.SchemaVersion = %d, want %d", cfg.SchemaVersion, CurrentVersion)
	}
	if cfg.Server.ListenAddr != "127.0.0.1:6789" || cfg.Server.AgentBinaryPath != "/usr/local/share/fleet/fleet-agent" || cfg.Agent.Role != "worker" {
		t.Fatalf("config fields lost during migration: %+v", cfg)
	}

	// Registry data intact.
	reg, err := registry.NewStore(dataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reg.Get("worker-1"); !ok {
		t.Fatal("worker-1 missing from registry after migration")
	}

	// Re-running must be a strict no-op (no backup, no steps).
	again, err := Run(Options{DataDir: dataDir, ConfigPath: cfgPath, Now: fixedNow})
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if again.Migrated || again.BackupDir != "" || len(again.Applied) != 0 {
		t.Fatalf("second Run was not a no-op: %+v", again)
	}
	if baks := countBackups(t, dataDir); len(baks) != 1 {
		t.Fatalf("backups = %v, want only 1", baks)
	}
	assertCAUntouched(t, dataDir, caCert, caKey)
}

func TestRunMigratesUnversionedExistingLayout(t *testing.T) {
	dataDir, cfgPath, caCert, caKey := seedV1Fixture(t, false)

	res, err := Run(Options{DataDir: dataDir, ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Migrated || res.FromVersion != 1 || res.ToVersion != CurrentVersion {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(countBackups(t, dataDir)) != 1 {
		t.Fatalf("expected 1 backup, got %v", countBackups(t, dataDir))
	}
	assertCAUntouched(t, dataDir, caCert, caKey)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != CurrentVersion {
		t.Fatalf("cfg.SchemaVersion = %d, want %d", cfg.SchemaVersion, CurrentVersion)
	}
}

func TestRefusesNewerSchemaVersion(t *testing.T) {
	t.Run("newer SCHEMA_VERSION", func(t *testing.T) {
		dataDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dataDir, config.SchemaVersionFile), []byte("99\n"), 0o640); err != nil {
			t.Fatal(err)
		}
		for _, fn := range []func(Options) (Result, error){Run, EnsureCurrent} {
			_, err := fn(Options{DataDir: dataDir})
			if !errors.Is(err, ErrNewerSchema) {
				t.Fatalf("err = %v, want ErrNewerSchema", err)
			}
		}
		if baks := countBackups(t, dataDir); len(baks) != 0 {
			t.Fatalf("newer schema should not create backup, got %v", baks)
		}
	})

	t.Run("newer config schema_version", func(t *testing.T) {
		dataDir := t.TempDir()
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, []byte(`{"schema_version": 99, "future_field": true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, fn := range []func(Options) (Result, error){Run, EnsureCurrent} {
			_, err := fn(Options{DataDir: dataDir, ConfigPath: cfgPath})
			if !errors.Is(err, ErrNewerSchema) {
				t.Fatalf("err = %v, want ErrNewerSchema", err)
			}
		}
	})

	t.Run("newer registry version", func(t *testing.T) {
		dataDir := t.TempDir()
		regDir := filepath.Join(dataDir, config.RegistryDirName)
		if err := os.MkdirAll(regDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(regDir, registry.FileName), []byte(`{"version": 99, "machines": []}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := EnsureCurrent(Options{DataDir: dataDir})
		if !errors.Is(err, ErrNewerSchema) {
			t.Fatalf("err = %v, want ErrNewerSchema", err)
		}
	})

	t.Run("corrupt SCHEMA_VERSION", func(t *testing.T) {
		for _, bad := range []string{"abc\n", "0\n", "-2\n", "\n"} {
			dataDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dataDir, config.SchemaVersionFile), []byte(bad), 0o640); err != nil {
				t.Fatal(err)
			}
			_, err := EnsureCurrent(Options{DataDir: dataDir})
			if !errors.Is(err, ErrInvalidSchemaVersion) {
				t.Fatalf("bad=%q: err = %v, want ErrInvalidSchemaVersion", bad, err)
			}
		}
	})
}

func TestEnsureCurrentStartupBehavior(t *testing.T) {
	// Fresh empty data dir initializes without creating a backup.
	freshDir := filepath.Join(t.TempDir(), "fresh")
	if err := os.MkdirAll(freshDir, 0o750); err != nil {
		t.Fatal(err)
	}
	res, err := EnsureCurrent(Options{DataDir: freshDir})
	if err != nil {
		t.Fatalf("EnsureCurrent fresh: %v", err)
	}
	if !res.Initialized || res.Migrated || res.BackupDir != "" {
		t.Fatalf("unexpected fresh result: %+v", res)
	}
	if len(countBackups(t, freshDir)) != 0 {
		t.Fatal("fresh init should not create a backup")
	}

	// Older populated data dir auto-migrates in place after backing up, keeping CA intact.
	oldDir, cfgPath, caCert, caKey := seedV1Fixture(t, true)
	res, err = EnsureCurrent(Options{DataDir: oldDir, ConfigPath: cfgPath})
	if err != nil {
		t.Fatalf("EnsureCurrent older: %v", err)
	}
	if !res.Migrated || res.BackupDir == "" {
		t.Fatalf("expected auto-migration with backup, got %+v", res)
	}
	assertCAUntouched(t, oldDir, caCert, caKey)
}

func TestOrderedMultiStepAndFailureRecovery(t *testing.T) {
	dataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dataDir, config.SchemaVersionFile), []byte("1\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	var order []string
	failStep3 := true
	steps := []Step{
		{
			FromVersion: 2,
			ToVersion:   3,
			Description: "step 2->3",
			Apply: func(ctx Context) error {
				order = append(order, "2->3")
				if failStep3 {
					return errors.New("boom")
				}
				return os.WriteFile(filepath.Join(ctx.DataDir, "v3-marker"), []byte("ok"), 0o640)
			},
		},
		{
			FromVersion: 1,
			ToVersion:   2,
			Description: "step 1->2",
			Apply: func(ctx Context) error {
				order = append(order, "1->2")
				return os.WriteFile(filepath.Join(ctx.DataDir, "v2-marker"), []byte("ok"), 0o640)
			},
		},
	}

	res, err := Run(Options{DataDir: dataDir, TargetVersion: 3, Steps: steps})
	if err == nil || !strings.Contains(err.Error(), "backup at") {
		t.Fatalf("expected step failure mentioning backup, got res=%+v err=%v", res, err)
	}
	if strings.Join(order, ",") != "1->2,2->3" {
		t.Fatalf("step order = %v", order)
	}
	// Progress up to step 2 was recorded; re-run resumes from 2->3.
	failStep3 = false
	order = nil
	res2, err := Run(Options{DataDir: dataDir, TargetVersion: 3, Steps: steps})
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if res2.FromVersion != 2 || res2.ToVersion != 3 || strings.Join(order, ",") != "2->3" {
		t.Fatalf("resume result=%+v order=%v", res2, order)
	}
}
