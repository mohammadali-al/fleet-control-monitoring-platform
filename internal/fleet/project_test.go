package fleet_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

func TestStartProjectTestWiringMonitorAndLock(t *testing.T) {
	dataDir := t.TempDir()
	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true},
		{ID: "mon1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.2"}, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}

	var linksMu sync.Mutex
	links := map[string]*scriptedLink{}
	m, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		DataDir:      dataDir,
		PollInterval: 10 * time.Millisecond,
		NewLink: func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
			l := &scriptedLink{id: mc.ID, h: h, online: true}
			l.onSend = func(env channel.Envelope) {
				if env.Type == channel.TypeManeuverPrepare && mc.Role == registry.RoleWorker {
					ack, _ := channel.NewEnvelope(channel.TypeManeuverAck, env.ID, channel.ManeuverAckPayload{
						RunID: env.ID, Status: channel.ManeuverAckReady,
					})
					go h(context.Background(), ack)
				}
			}
			linksMu.Lock()
			links[mc.ID] = l
			linksMu.Unlock()
			return l, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.EnableObserve(dataDir); err != nil {
		t.Fatalf("EnableObserve: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })

	eventually(t, "machines online", func() bool {
		m1, ok1 := m.Machine("w1")
		m2, ok2 := m.Machine("mon1")
		return ok1 && ok2 && m1.State == fleet.StateOnline && m2.State == fleet.StateOnline
	})

	// Seed baseline item + maneuver.
	if _, err := m.Baseline().Create(baseline.ItemSpec{
		ID:              "http-load",
		Name:            "HTTP Load",
		CommandTemplate: "echo {url} {duration}",
	}, "op"); err != nil {
		t.Fatal(err)
	}
	mv, err := m.ManeuverStore().Create(maneuver.Spec{
		ID:       "mv-web",
		Name:     "Web test",
		Target:   maneuver.Target{IP: "198.51.100.10", Origin: "https://shop.example.test"},
		TotalSec: 30,
		Pattern:  maneuver.PatternConfig{Type: maneuver.PatternAllInOne},
		Timeline: []maneuver.TimelineItem{
			{ID: "i1", BaselineID: "http-load", StartSec: 0, DurationSec: 15, Replicas: 1},
		},
		Agents: []string{"w1"},
	}, "op")
	if err != nil {
		t.Fatal(err)
	}

	p1, err := m.ProjectStore().CreateProject(project.ProjectSpec{Name: "Engagement A"}, "op")
	if err != nil {
		t.Fatal(err)
	}
	p2, err := m.ProjectStore().CreateProject(project.ProjectSpec{Name: "Engagement B"}, "op")
	if err != nil {
		t.Fatal(err)
	}
	_, t1, err := m.ProjectStore().CreateTest(p1.ID, project.TestSpec{Name: "Run 1", ManeuverID: mv.ID}, "op")
	if err != nil {
		t.Fatal(err)
	}
	_, t2, err := m.ProjectStore().CreateTest(p2.ID, project.TestSpec{Name: "Run 2", ManeuverID: mv.ID}, "op")
	if err != nil {
		t.Fatal(err)
	}

	// Refuses starting without a monitor.
	if _, err := m.StartProjectTest(ctx, fleet.StartProjectTestRequest{
		ProjectID: p1.ID,
		TestID:    t1.ID,
		User:      "op",
		LeadTime:  300 * time.Millisecond,
	}); !errors.Is(err, fleet.ErrMonitorRequired) {
		t.Fatalf("start without monitor = %v, want ErrMonitorRequired", err)
	}

	// Start with create_monitor: creates monitor job, pushes config, starts Maneuver, acquires lock.
	res, err := m.StartProjectTest(ctx, fleet.StartProjectTestRequest{
		ProjectID:     p1.ID,
		TestID:        t1.ID,
		CreateMonitor: true,
		User:          "op",
		LeadTime:      300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartProjectTest: %v", err)
	}
	if res.Test.Status != project.StatusRunning || res.Test.ManeuverRunID == "" {
		t.Fatalf("unexpected started test: %+v", res.Test)
	}
	if res.Test.Monitor.JobID == "" || res.Test.Monitor.Mode != project.MonitorModeCreated {
		t.Fatalf("expected created monitor job on test, got %+v", res.Test.Monitor)
	}
	if res.Active == nil || res.Active.TestID != t1.ID {
		t.Fatalf("expected active lock for %s, got %+v", t1.ID, res.Active)
	}

	// Verify ManeuverRun was actually created in T-0063's Maneuver runner.
	if _, ok := m.ManeuverRun(res.Test.ManeuverRunID); !ok {
		t.Fatalf("maneuver run %s not found in manager", res.Test.ManeuverRunID)
	}

	// Concurrent start on another project/test must be refused by the server-side lock.
	if _, err := m.StartProjectTest(ctx, fleet.StartProjectTestRequest{
		ProjectID:     p2.ID,
		TestID:        t2.ID,
		CreateMonitor: true,
		User:          "op",
		LeadTime:      300 * time.Millisecond,
	}); !errors.Is(err, project.ErrActiveTestConflict) {
		t.Fatalf("concurrent StartProjectTest = %v, want ErrActiveTestConflict", err)
	}

	// Stop the active test: stops Maneuver, records completion, releases lock.
	stopRes, err := m.StopProjectTest(ctx, p1.ID, t1.ID, "op", "")
	if err != nil {
		t.Fatalf("StopProjectTest: %v", err)
	}
	if stopRes.Test.Status != project.StatusCompleted || stopRes.Test.Outcome != project.OutcomeStopped {
		t.Fatalf("stopped test = %+v", stopRes.Test)
	}
	if stopRes.Test.EndedAt == nil || stopRes.Test.ResultPointers == nil || stopRes.Test.ResultPointers.Evidence.To == nil {
		t.Fatalf("expected ended_at and result pointers closed: %+v", stopRes.Test)
	}
	if _, active := m.ProjectStore().ActiveLock(); active {
		t.Fatal("expected lock released after stop")
	}

	// Starting t2 now attaches the existing monitor job for the same (ip, origin) without duplicate error.
	res2, err := m.StartProjectTest(ctx, fleet.StartProjectTestRequest{
		ProjectID:     p2.ID,
		TestID:        t2.ID,
		CreateMonitor: true,
		User:          "op",
		LeadTime:      300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("StartProjectTest t2: %v", err)
	}
	if res2.Test.Monitor.JobID != res.Test.Monitor.JobID || res2.Test.Monitor.Mode != project.MonitorModeAttached {
		t.Fatalf("expected t2 to reuse monitor %s as attached, got %+v", res.Test.Monitor.JobID, res2.Test.Monitor)
	}
	if _, err := m.StopProjectTest(ctx, p2.ID, t2.ID, "op", ""); err != nil {
		t.Fatalf("StopProjectTest t2: %v", err)
	}
}
