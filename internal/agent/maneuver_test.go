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

func TestAgentManeuverOutageContinuationAndRejoin(t *testing.T) {
	t.Run("continues autonomously during server outage and reconciles on reconnect", func(t *testing.T) {
		mt, err := fleettest.NewMTLS()
		if err != nil {
			t.Fatal(err)
		}
		agentDataDir := t.TempDir()
		outDir := t.TempDir()
		ra := startAgent(t, mt, "w1", func(c *agent.Config) { c.DataDir = agentDataDir })
		_ = ra

		reg := registry.New()
		if err := reg.Add(registry.Machine{
			ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial,
			PrivateIPs: []string{"127.0.0.1"}, Provisioned: true,
		}); err != nil {
			t.Fatal(err)
		}
		gate, err := exec.NewGate([]byte("outage-secret"), nil)
		if err != nil {
			t.Fatal(err)
		}

		ctx1, cancel1 := context.WithCancel(context.Background())
		m1, err := fleet.New(fleet.Config{
			Registry: reg, Gate: gate, NewLink: mt.Factory(),
			PollInterval: 10 * time.Millisecond, GateSecret: []byte("outage-secret"), Logger: quiet(),
		})
		if err != nil {
			t.Fatal(err)
		}
		m1.Start(ctx1)
		eventually(t, "w1 online on server 1", func() bool {
			info, ok := m1.Machine("w1")
			return ok && info.State == fleet.StateOnline
		})

		step1Marker := filepath.Join(outDir, "step1.ran")
		step2Marker := filepath.Join(outDir, "step2.ran")
		run, err := m1.StartManeuver(ctx1, fleet.StartManeuverRequest{
			Plan: &maneuver.MaterializedPlan{
				ManeuverID: "mv-outage",
				Name:       "Outage",
				Target:     maneuver.Target{IP: "198.51.100.10"},
				TotalSec:   4,
				Pattern:    maneuver.PatternConfig{Type: maneuver.PatternAllInOne},
				Items: []maneuver.MaterializedItem{
					{ItemID: "i1", BaselineID: "b1", Command: "touch " + step1Marker, StartSec: 0, DurationSec: 2, Replicas: 1},
					{ItemID: "i2", BaselineID: "b2", Command: "touch " + step2Marker, StartSec: 1, DurationSec: 2, Replicas: 1},
				},
				Agents: []string{"w1"},
			},
			LeadTime: 300 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("StartManeuver: %v", err)
		}
		eventually(t, "run committed/running before outage", func() bool {
			r, ok := m1.ManeuverRun(run.RunID)
			return ok && (r.State == fleet.ManeuverStateCommitted || r.State == fleet.ManeuverStateRunning)
		})

		// Simulate total main-server outage while Maneuver executes on the agent.
		cancel1()
		m1.Close()

		eventually(t, "both timeline items ran autonomously during outage", func() bool {
			_, err1 := os.Stat(step1Marker)
			_, err2 := os.Stat(step2Marker)
			return err1 == nil && err2 == nil
		})

		// Bring up a fresh server (simulating server recovery) and verify agent reports status on reconnect.
		ctx2, cancel2 := context.WithCancel(context.Background())
		defer cancel2()
		m2, err := fleet.New(fleet.Config{
			Registry: reg, Gate: gate, NewLink: mt.Factory(),
			PollInterval: 10 * time.Millisecond, GateSecret: []byte("outage-secret"), Logger: quiet(),
		})
		if err != nil {
			t.Fatal(err)
		}
		m2.Start(ctx2)
		defer m2.Close()

		eventually(t, "restarted server receives maneuver status on reconnect", func() bool {
			r, ok := m2.ManeuverRun(run.RunID)
			return ok && (r.Agents["w1"].State == fleet.ManeuverStateCompleted || r.Agents["w1"].State == fleet.ManeuverStateRunning)
		})
	})

	t.Run("rejoins at wall-clock position after agent reboot mid-maneuver", func(t *testing.T) {
		mt, err := fleettest.NewMTLS()
		if err != nil {
			t.Fatal(err)
		}
		agentDataDir := t.TempDir()
		outDir := t.TempDir()

		// Simulate initial committed run on agent1, then stop/kill agent1 mid-plan
		// and start a fresh agent2 on the same DataDir (reboot).
		reg := registry.New()
		if err := reg.Add(registry.Machine{
			ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial,
			PrivateIPs: []string{"127.0.0.1"}, Provisioned: true,
		}); err != nil {
			t.Fatal(err)
		}
		gate, err := exec.NewGate([]byte("reboot-secret"), nil)
		if err != nil {
			t.Fatal(err)
		}

		// Start agent1 briefly to receive & commit a plan starting at T = now + 200ms with 6s total duration.
		certPEM, keyPEM, err := mt.CA.IssuePeer("w1", nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		_ = certPEM
		_ = keyPEM
		ra1 := startAgent(t, mt, "w1", func(c *agent.Config) { c.DataDir = agentDataDir })

		mCtx, mCancel := context.WithCancel(context.Background())
		m, err := fleet.New(fleet.Config{
			Registry: reg, Gate: gate, NewLink: mt.Factory(),
			PollInterval: 10 * time.Millisecond, GateSecret: []byte("reboot-secret"), Logger: quiet(),
		})
		if err != nil {
			t.Fatal(err)
		}
		m.Start(mCtx)
		eventually(t, "w1 online", func() bool {
			info, ok := m.Machine("w1")
			return ok && info.State == fleet.StateOnline
		})

		pastMarker := filepath.Join(outDir, "past-window.ran")
		midMarker := filepath.Join(outDir, "mid-window.ran")
		futureMarker := filepath.Join(outDir, "future-window.ran")
		run, err := m.StartManeuver(mCtx, fleet.StartManeuverRequest{
			Plan: &maneuver.MaterializedPlan{
				ManeuverID: "mv-reboot",
				Name:       "Reboot rejoin",
				Target:     maneuver.Target{IP: "198.51.100.10"},
				TotalSec:   6,
				Pattern:    maneuver.PatternConfig{Type: maneuver.PatternAllInOne},
				Items: []maneuver.MaterializedItem{
					// Window [0, 1s): runs only at start; after reboot at ~T+1.5s it must NOT re-run.
					{ItemID: "past", BaselineID: "b1", Command: "touch " + pastMarker, StartSec: 0, DurationSec: 1, Replicas: 1},
					// Window [0, 5s): spans across reboot; must resume immediately on reboot.
					{ItemID: "mid", BaselineID: "b2", Command: "echo rejoin-resumed >> " + midMarker + "; sleep 10", StartSec: 0, DurationSec: 5, Replicas: 1},
					// Window [2, 5s): starts at T+2s; must run on schedule after reboot.
					{ItemID: "future", BaselineID: "b3", Command: "touch " + futureMarker, StartSec: 2, DurationSec: 3, Replicas: 1},
				},
				Agents: []string{"w1"},
			},
			LeadTime: 250 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("StartManeuver: %v", err)
		}

		// Wait until past window [0,1s) has run and elapsed (~1.3s after T).
		eventually(t, "past marker created", func() bool {
			_, err := os.Stat(pastMarker)
			return err == nil
		})
		time.Sleep(1200 * time.Millisecond)

		// Simulate reboot: stop agent1 and remove pastMarker to prove reboot doesn't replay [0,1s).
		if err := ra1.Stop(); err != nil {
			t.Fatalf("stop agent1: %v", err)
		}
		_ = os.Remove(pastMarker)

		// Also shut down server temporarily to prove rebooted agent resumes even while server is away,
		// or reconnects later.
		mCancel()
		m.Close()

		// Boot fresh agent2 on the same agentDataDir.
		ra2 := startAgent(t, mt, "w1", func(c *agent.Config) { c.DataDir = agentDataDir })
		_ = ra2

		// Mid-window item and future item must run at wall-clock position, and past item must NOT re-run.
		eventually(t, "mid and future items ran after reboot", func() bool {
			b, err1 := os.ReadFile(midMarker)
			_, err2 := os.Stat(futureMarker)
			return err1 == nil && stringsContains(string(b), "rejoin-resumed") && err2 == nil
		})
		if _, err := os.Stat(pastMarker); !os.IsNotExist(err) {
			t.Errorf("past-window item re-ran after its wall-clock window ended!")
		}
		_ = run
	})
}

func stringsContains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexString(s, sub) >= 0)
}

func indexString(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
