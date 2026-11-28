package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

func TestAgentManeuverSyncStartReadyAndMissed(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	w1Dir := t.TempDir()
	w2Dir := t.TempDir()

	// w2's clock is shifted ahead so a prepare for T in near future appears past T on w2.
	var mu sync.Mutex
	w2Offset := time.Duration(0)
	ra1 := startAgent(t, mt, "w1", func(c *agent.Config) { c.DataDir = w1Dir })
	ra2 := startAgent(t, mt, "w2", func(c *agent.Config) {
		c.DataDir = w2Dir
		c.Now = func() time.Time {
			mu.Lock()
			off := w2Offset
			mu.Unlock()
			return time.Now().Add(off)
		}
	})
	_ = ra1
	_ = ra2

	reg := registry.New()
	for _, id := range []string{"w1", "w2"} {
		if err := reg.Add(registry.Machine{
			ID: id, Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial,
			PrivateIPs: []string{"127.0.0.1"}, Provisioned: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("sync-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		NewLink:      mt.Factory(),
		PollInterval: 10 * time.Millisecond,
		GateSecret:   []byte("sync-secret"),
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })

	eventually(t, "both workers online", func() bool {
		i1, ok1 := m.Machine("w1")
		i2, ok2 := m.Machine("w2")
		return ok1 && ok2 && i1.State == fleet.StateOnline && i2.State == fleet.StateOnline
	})

	// 1) Happy path: both agents ACK ready, server commits T, both run synced.
	marker1 := filepath.Join(outDir, "w1.ran")
	marker2 := filepath.Join(outDir, "w2.ran")
	plan := &maneuver.MaterializedPlan{
		ManeuverID: "mv-sync",
		Name:       "Sync",
		Target:     maneuver.Target{IP: "198.51.100.10"},
		TotalSec:   4,
		Pattern:    maneuver.PatternConfig{Type: maneuver.PatternAllInOne},
		Items: []maneuver.MaterializedItem{
			{ItemID: "i1", BaselineID: "b1", Command: "touch " + marker1, StartSec: 0, DurationSec: 2, Replicas: 1},
			{ItemID: "i2", BaselineID: "b2", Command: "touch " + marker2, StartSec: 0, DurationSec: 2, Replicas: 1},
		},
		Agents: []string{"w1", "w2"},
	}

	run, err := m.StartManeuver(ctx, fleet.StartManeuverRequest{
		Plan:     plan,
		LeadTime: 350 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartManeuver: %v", err)
	}

	eventually(t, "maneuver completed", func() bool {
		r, ok := m.ManeuverRun(run.RunID)
		return ok && r.State == fleet.ManeuverStateCompleted
	})
	if _, err := os.Stat(marker1); err != nil {
		t.Errorf("marker1 missing: %v", err)
	}
	if _, err := os.Stat(marker2); err != nil {
		t.Errorf("marker2 missing: %v", err)
	}

	// 2) Missed path: w2 receives the prepare order after its T, reports "missed",
	//    and neither w1 nor w2 starts late.
	lateMarker := filepath.Join(outDir, "late.ran")
	mu.Lock()
	w2Offset = 10 * time.Second
	mu.Unlock()

	latePlan := &maneuver.MaterializedPlan{
		ManeuverID: "mv-missed",
		Name:       "Missed",
		Target:     maneuver.Target{IP: "198.51.100.10"},
		TotalSec:   3,
		Pattern:    maneuver.PatternConfig{Type: maneuver.PatternAllInOne},
		Items: []maneuver.MaterializedItem{
			{ItemID: "i1", BaselineID: "b1", Command: "touch " + lateMarker, StartSec: 0, DurationSec: 2, Replicas: 1},
		},
		Agents: []string{"w1", "w2"},
	}

	missedRun, err := m.StartManeuver(ctx, fleet.StartManeuverRequest{
		Plan:     latePlan,
		LeadTime: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartManeuver missed: %v", err)
	}
	eventually(t, "missed run aborted", func() bool {
		r, ok := m.ManeuverRun(missedRun.RunID)
		return ok && r.State == fleet.ManeuverStateAborted && r.Agents["w2"].State == channel.ManeuverAckMissed
	})

	// Wait past T to be sure w1 didn't start uncommitted either.
	time.Sleep(450 * time.Millisecond)
	if _, err := os.Stat(lateMarker); !os.IsNotExist(err) {
		t.Fatalf("late marker was created despite missed start!")
	}
	_ = config.CaptureConfig{}
}
