package agent_test

import (
	"context"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

func TestAgentVersionReportedOnConnect(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	wrk, err := mt.StartAgent("w1", agent.Config{
		DataDir: t.TempDir(),
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		AgentVersion: "2.1.0-worker",
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	t.Cleanup(func() { _ = wrk.Stop() })

	mon, err := mt.StartMonitor("mon1", agent.MonitorConfig{
		AgentVersion: "2.1.0-mon",
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatalf("StartMonitor: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "mon1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("test-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		GateSecret:   []byte("test-secret"),
		Capture:      config.CaptureConfig{},
		NewLink:      mt.Factory(),
		PollInterval: 10 * time.Millisecond,
		Logger:       quiet(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	eventually(t, "w1 version in registry", func() bool {
		info, ok := mgr.Machine("w1")
		return ok && info.AgentVersion == "2.1.0-worker"
	})
	eventually(t, "mon1 version in registry", func() bool {
		info, ok := mgr.Machine("mon1")
		return ok && info.AgentVersion == "2.1.0-mon"
	})
}
