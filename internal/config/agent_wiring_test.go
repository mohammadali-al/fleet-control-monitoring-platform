package config

import (
	"path/filepath"
	"testing"
)

func TestAgentListenAddrDefault(t *testing.T) {
	if got := (AgentConfig{}).ListenAddrOrDefault(); got != ":7443" {
		t.Errorf("default = %q", got)
	}
	if got := (AgentConfig{ListenAddr: "127.0.0.1:9"}).ListenAddrOrDefault(); got != "127.0.0.1:9" {
		t.Errorf("explicit = %q", got)
	}
}

func TestAgentResolveDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	got, err := (AgentConfig{DataDir: dir}).ResolveDataDir()
	if err != nil || got != dir {
		t.Fatalf("explicit: %q, %v", got, err)
	}
	env := filepath.Join(t.TempDir(), "env")
	t.Setenv(EnvDataDir, env)
	if got, err := (AgentConfig{}).ResolveDataDir(); err != nil || got != env {
		t.Errorf("env: %q, %v", got, err)
	}
	if got, err := (AgentConfig{DataDir: dir}).ResolveDataDir(); err != nil || got != dir {
		t.Errorf("file value must win over env: %q, %v", got, err)
	}
}

func TestCaptureConfigIsZero(t *testing.T) {
	if !(CaptureConfig{}).IsZero() {
		t.Error("zero value is not IsZero")
	}
	for _, c := range []CaptureConfig{{TrimThresholdBytes: 1}, {Reflectors: []string{"x"}}, {NFLOGGroup: 3}, {DiskPausePercent: 1}} {
		if c.IsZero() {
			t.Errorf("%+v reported zero", c)
		}
	}
}
