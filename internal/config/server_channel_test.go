package config

import "testing"

func TestServerChannelDefaults(t *testing.T) {
	var s ServerConfig
	if s.ChannelPortOrDefault() != DefaultChannelPort || s.ServerIDOrDefault() != DefaultServerID {
		t.Errorf("defaults = %d %q", s.ChannelPortOrDefault(), s.ServerIDOrDefault())
	}
	s = ServerConfig{ChannelPort: 9000, ServerID: "main-1"}
	if s.ChannelPortOrDefault() != 9000 || s.ServerIDOrDefault() != "main-1" {
		t.Errorf("explicit = %d %q", s.ChannelPortOrDefault(), s.ServerIDOrDefault())
	}
}
