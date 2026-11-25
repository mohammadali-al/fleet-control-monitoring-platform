package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentDialSettings(t *testing.T) {
	var a AgentConfig
	if a.Dials() {
		t.Error("an agent with no connect_mode dials; the default is server_dial")
	}
	if a.ServerIDOrDefault() != DefaultServerID {
		t.Errorf("ServerIDOrDefault() = %q, want %q", a.ServerIDOrDefault(), DefaultServerID)
	}
	a = AgentConfig{ConnectMode: ConnectModeAgentDial, ServerID: "srv"}
	if !a.Dials() || a.ServerIDOrDefault() != "srv" {
		t.Errorf("agent_dial agent: Dials=%v ServerID=%q", a.Dials(), a.ServerIDOrDefault())
	}
	if a = (AgentConfig{ConnectMode: ConnectModeServerDial}); a.Dials() {
		t.Error("a server_dial agent dials")
	}
}

func TestAgentDialConfigRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	doc := `{"server":{"agent_dial_addr":":7444"},
	 "agent":{"role":"worker","connect_mode":"agent_dial","server_addr":"fleet.example:7444","server_id":"srv"}}`
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.AgentDialAddr != ":7444" || cfg.Agent.ServerAddr != "fleet.example:7444" || cfg.Agent.ServerID != "srv" {
		t.Errorf("loaded %+v / %+v", cfg.Server, cfg.Agent)
	}
	if err := cfg.Agent.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if (ServerConfig{}).AgentDialAddr != "" {
		t.Error("agent_dial listener is on by default; it must be opt-in")
	}
}
