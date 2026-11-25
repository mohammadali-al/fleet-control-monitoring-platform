package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

type fakeServiceController struct {
	mu      sync.Mutex
	actions []string
	state   string
	err     error
}

func (f *fakeServiceController) Stop(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "stop")
	if f.err != nil {
		return f.err
	}
	f.state = "stopped"
	return nil
}

func (f *fakeServiceController) Start(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "start")
	if f.err != nil {
		return f.err
	}
	f.state = "running"
	return nil
}

func (f *fakeServiceController) Status(_ context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, "status")
	if f.err != nil {
		return "", f.err
	}
	if f.state == "" {
		return "running", nil
	}
	return f.state, nil
}

func (f *fakeServiceController) snapshot() (actions []string, state string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...), f.state
}

func TestServiceControlMonitorAndWorkerFleettest(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}

	monCtrl := &fakeServiceController{state: "running"}
	mon, err := mt.StartMonitor("mon1", agent.MonitorConfig{
		ServiceController: monCtrl,
		Logger:            quiet(),
	})
	if err != nil {
		t.Fatalf("StartMonitor: %v", err)
	}
	t.Cleanup(func() { _ = mon.Stop() })

	wrkCtrl := &fakeServiceController{state: "running"}
	wrk, err := mt.StartAgent("w1", agent.Config{
		DataDir: t.TempDir(),
		CaptureFactory: func(context.Context, capture.Config) (agent.Capturer, error) {
			return nil, agent.ErrCaptureUnavailable
		},
		Logger: quiet(),
	})
	if err != nil {
		t.Fatalf("StartAgent: %v", err)
	}
	wrk.SetServiceController(wrkCtrl)
	t.Cleanup(func() { _ = wrk.Stop() })

	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "mon1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "off1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.2"}, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	// GateSecret is intentionally omitted: the worker has no command executor,
	// proving service-control does not depend on the command execution path.
	gate, err := exec.NewGate([]byte("test-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
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

	eventually(t, "mon1 and w1 online", func() bool {
		m1, _ := mgr.Machine("mon1")
		w1, _ := mgr.Machine("w1")
		return m1.State == fleet.StateOnline && w1.State == fleet.StateOnline
	})

	// Monitor: stop disables and reports stopped, status confirms, start re-enables.
	res, err := mgr.ServiceControl(ctx, "mon1", channel.ServiceActionStop)
	if err != nil || !res.OK || res.State != "stopped" || res.Action != channel.ServiceActionStop {
		t.Fatalf("monitor stop = (%+v, %v), want ok stopped", res, err)
	}
	if mc, _ := reg.Get("mon1"); !mc.Stopped {
		t.Error("registry Stopped not set after monitor stop")
	}
	res, err = mgr.ServiceControl(ctx, "mon1", channel.ServiceActionStatus)
	if err != nil || !res.OK || res.State != "stopped" {
		t.Fatalf("monitor status = (%+v, %v), want stopped", res, err)
	}
	res, err = mgr.ServiceControl(ctx, "mon1", channel.ServiceActionStart)
	if err != nil || !res.OK || res.State != "running" || res.Action != channel.ServiceActionStart {
		t.Fatalf("monitor start = (%+v, %v), want ok running", res, err)
	}
	if mc, _ := reg.Get("mon1"); mc.Stopped {
		t.Error("registry Stopped not cleared after monitor start")
	}
	if acts, st := monCtrl.snapshot(); strings.Join(acts, ",") != "stop,status,start" || st != "running" {
		t.Errorf("monitor controller actions=%v state=%q, want [stop status start] running", acts, st)
	}

	// Worker: uses the exact same service-control path without a command executor.
	res, err = mgr.ServiceControl(ctx, "w1", channel.ServiceActionStop)
	if err != nil || !res.OK || res.State != "stopped" {
		t.Fatalf("worker stop = (%+v, %v), want ok stopped", res, err)
	}
	res, err = mgr.ServiceControl(ctx, "w1", channel.ServiceActionStart)
	if err != nil || !res.OK || res.State != "running" {
		t.Fatalf("worker start = (%+v, %v), want ok running", res, err)
	}
	if acts, st := wrkCtrl.snapshot(); strings.Join(acts, ",") != "stop,start" || st != "running" {
		t.Errorf("worker controller actions=%v state=%q, want [stop start] running", acts, st)
	}

	// Agent-side failure propagates back cleanly.
	monCtrl.mu.Lock()
	monCtrl.err = errors.New("systemctl unavailable")
	monCtrl.mu.Unlock()
	if res, err := mgr.ServiceControl(ctx, "mon1", channel.ServiceActionStop); err == nil || res.OK || !strings.Contains(err.Error(), "systemctl unavailable") {
		t.Errorf("failing monitor stop = (%+v, %v), want error containing systemctl unavailable", res, err)
	}

	// Validation and offline/unknown handling.
	if _, err := mgr.ServiceControl(ctx, "mon1", "restart"); !errors.Is(err, fleet.ErrBadRequest) {
		t.Errorf("invalid action err = %v, want ErrBadRequest", err)
	}
	if _, err := mgr.ServiceControl(ctx, "off1", channel.ServiceActionStop); !errors.Is(err, fleet.ErrOffline) {
		t.Errorf("offline machine err = %v, want ErrOffline", err)
	}
	if _, err := mgr.ServiceControl(ctx, "missing", channel.ServiceActionStop); !errors.Is(err, fleet.ErrUnknown) {
		t.Errorf("unknown machine err = %v, want ErrUnknown", err)
	}
}
