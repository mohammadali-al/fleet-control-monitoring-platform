package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRejectsNewerAndInvalidSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	newerPath := filepath.Join(dir, "newer.json")
	if err := os.WriteFile(newerPath, []byte(`{"schema_version": 99, "future_only_field": true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(newerPath); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("Load(newer) err = %v, want ErrNewerSchema", err)
	}

	negPath := filepath.Join(dir, "neg.json")
	if err := os.WriteFile(negPath, []byte(`{"schema_version": -1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(negPath); err == nil {
		t.Fatal("Load(negative schema_version) succeeded, want error")
	}
}

func TestLoadAcceptsCurrentAndOlderSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	for _, ver := range []int{1, CurrentSchemaVersion} {
		p := filepath.Join(dir, "c.json")
		body := `{"schema_version": ` + string(rune('0'+ver)) + `, "server": {"listen_addr": ":6790"}}`
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(p)
		if err != nil {
			t.Fatalf("Load(v%d): %v", ver, err)
		}
		if cfg.SchemaVersion != ver || cfg.Server.ListenAddr != ":6790" {
			t.Fatalf("Load(v%d) = %+v", ver, cfg)
		}
	}
}

func TestReadSchemaVersion(t *testing.T) {
	ver, exists, err := ReadSchemaVersion("")
	if err != nil || exists || ver != 0 {
		t.Fatalf("empty path: (%d, %v, %v)", ver, exists, err)
	}
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"schema_version": 2, "unknown": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ver, exists, err = ReadSchemaVersion(p)
	if err != nil || !exists || ver != 2 {
		t.Fatalf("existing: (%d, %v, %v)", ver, exists, err)
	}
}
