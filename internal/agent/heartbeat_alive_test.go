package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// StartHeartbeat on a real worker agent (the wiring main uses): the agent is its
// own probe (Alive) and the heartbeat file appears under its data dir.
func TestStartHeartbeatOnARealAgent(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ra := startAgent(t, mt, "wrk-hb", func(c *agent.Config) { c.DataDir = dir })
	if !ra.Alive() {
		t.Fatal("a running agent must report alive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	agent.StartHeartbeat(ctx, ra.Agent, config.AgentConfig{MachineID: "wrk-hb"}, dir, quiet())
	path := watchdog.HeartbeatPath(dir, watchdog.HeartbeatAgent)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, err := watchdog.ReadHeartbeat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no agent heartbeat was written")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
