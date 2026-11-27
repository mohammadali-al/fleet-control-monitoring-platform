package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadMissingFileGivesDefaults(t *testing.T) {
	for _, path := range []string{"", filepath.Join(t.TempDir(), "nope.json")} {
		got, err := Load(path)
		if err != nil {
			t.Fatalf("Load(%q): %v", path, err)
		}
		if !reflect.DeepEqual(got, Defaults()) {
			t.Fatalf("Load(%q) = %+v, want defaults", path, got)
		}
	}
}

func TestLoadSample(t *testing.T) {
	got, err := Load("testdata/sample.json")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Server: ServerConfig{ListenAddr: DefaultListenAddr, Log: LogConfig{Level: "debug", Format: "json"}},
		Agent:  AgentConfig{Role: "monitor", Log: LogConfig{Level: "warn", Format: "text"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestLoadRejectsBadFiles(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"syntax.json":  `{`,
		"unknown.json": `{"agent":{"rolee":"worker"}}`,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("Load(%s) succeeded, want error", name)
		}
	}
}

func TestServerDefaultsAndEnvOverrides(t *testing.T) {
	t.Setenv(EnvListenAddr, "")
	t.Setenv(EnvDataDir, "")
	got, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.ListenAddr != ":6789" {
		t.Errorf("default ListenAddr = %q, want :6789", got.Server.ListenAddr)
	}
	if got.Server.DataDir != "" {
		t.Errorf("default DataDir = %q, want empty (resolved at runtime)", got.Server.DataDir)
	}

	// Config file values are honored.
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"server":{"listen_addr":"127.0.0.1:7000","data_dir":"/from/file"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.ListenAddr != "127.0.0.1:7000" || got.Server.DataDir != "/from/file" {
		t.Errorf("file values not honored: %+v", got.Server)
	}

	// Environment beats the file.
	t.Setenv(EnvListenAddr, ":7001")
	t.Setenv(EnvDataDir, "/from/env")
	got, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.ListenAddr != ":7001" || got.Server.DataDir != "/from/env" {
		t.Errorf("env did not override file: %+v", got.Server)
	}
}

func TestResolveDataDirConfigured(t *testing.T) {
	want := filepath.Join(t.TempDir(), "a", "b")
	got, err := ServerConfig{DataDir: want}.ResolveDataDir("")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	fi, err := os.Stat(got)
	if err != nil || !fi.IsDir() {
		t.Fatalf("data dir not created: %v", err)
	}
	if perm := fi.Mode().Perm(); perm&^0o750 != 0 {
		t.Errorf("data dir perm %o is more open than 0750", perm)
	}
	// A second call on an existing dir is fine.
	if _, err := (ServerConfig{DataDir: want}).ResolveDataDir(""); err != nil {
		t.Errorf("existing dir: %v", err)
	}
}

func TestResolveDataDirDefaultIsAboveBinary(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "install", "bin", "fleet-server")
	got, err := ServerConfig{}.ResolveDataDir(exe)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "install", "data")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if fi, err := os.Stat(got); err != nil || !fi.IsDir() {
		t.Fatalf("default data dir not created: %v", err)
	}
	if _, err := (ServerConfig{}).ResolveDataDir(""); err == nil {
		t.Error("no data_dir and no exe path should fail")
	}
}

func TestAgentValidate(t *testing.T) {
	for role, wantOK := range map[string]bool{
		"worker": true, "monitor": true, "bogus": false, "": false, "Worker": false,
	} {
		err := AgentConfig{Role: role}.Validate()
		if (err == nil) != wantOK {
			t.Errorf("Validate(%q) err=%v, wantOK=%v", role, err, wantOK)
		}
	}
}

func TestMonitorStoreConfigAndEnvOverride(t *testing.T) {
	t.Setenv(EnvMonitorStoreAddr, "")
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"server":{"monitor_store":{"addr":"http://127.0.0.1:8428","retention":"30d"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.MonitorStore.Addr != "http://127.0.0.1:8428" || got.Server.MonitorStore.RetentionOrDefault() != "30d" {
		t.Fatalf("monitor_store = %+v", got.Server.MonitorStore)
	}
	if def := (MonitorStoreConfig{}).RetentionOrDefault(); def != DefaultMonitorStoreRetention {
		t.Errorf("default retention = %q, want %q", def, DefaultMonitorStoreRetention)
	}

	t.Setenv(EnvMonitorStoreAddr, "http://127.0.0.1:18428")
	got, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Server.MonitorStore.Addr != "http://127.0.0.1:18428" {
		t.Errorf("env did not override monitor_store.addr: %+v", got.Server.MonitorStore)
	}
}
