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
		Server: ServerConfig{Log: LogConfig{Level: "debug", Format: "json"}},
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
