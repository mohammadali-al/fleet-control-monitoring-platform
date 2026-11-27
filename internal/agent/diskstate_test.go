package agent_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
)

type diskCapturerStub struct {
	mu     sync.Mutex
	onDisk func(capture.DiskStatus)
	cur    capture.DiskStatus
	hasCur bool
}

func (s *diskCapturerStub) Capture(string, string) (agent.SessionCapture, error) {
	return fakeSessionStub{}, nil
}
func (s *diskCapturerStub) Close() {}

func (s *diskCapturerStub) SetOnDiskState(fn func(capture.DiskStatus)) (capture.DiskStatus, bool) {
	s.mu.Lock()
	s.onDisk = fn
	cur, ok := s.cur, s.hasCur
	s.mu.Unlock()
	return cur, ok
}

func (s *diskCapturerStub) CheckDisk() (capture.DiskStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasCur {
		s.cur = capture.DiskStatus{
			Status:     capture.DiskStatusOK,
			TotalBytes: 1000,
			FreeBytes:  600,
			UsedPct:    40,
			WarnAt:     80,
			PauseAt:    90,
			At:         time.Now(),
		}
		s.hasCur = true
	}
	return s.cur, nil
}

func (s *diskCapturerStub) emit(st capture.DiskStatus) {
	s.mu.Lock()
	s.cur = st
	s.hasCur = true
	fn := s.onDisk
	s.mu.Unlock()
	if fn != nil {
		fn(st)
	}
}

type fakeSessionStub struct{}

func (fakeSessionStub) Stop() (capture.Summary, error) { return capture.Summary{}, nil }

// Verifies the full GAP-17 round-trip over mTLS: worker agent disk telemetry ->
// channel TypeDiskState -> fleet.Manager -> registry.Machine.Disk + bus event.
func TestAgentToRegistryDiskTelemetryRoundTrip(t *testing.T) {
	stub := &diskCapturerStub{}
	e := newMgrEnv(t, "sec", "sec", config.CaptureConfig{}, func(c *agent.Config) {
		c.CaptureFactory = func(context.Context, capture.Config) (agent.Capturer, error) {
			return stub, nil
		}
	})
	e.waitOnline(t)

	// 1. Initial connect reports disk usage to registry.
	eventually(t, "initial disk state in registry", func() bool {
		info, ok := e.m.Machine("w1")
		return ok && info.Disk.Status == "ok" && info.Disk.TotalBytes == 1000 && info.Disk.FreeBytes == 600 && info.Disk.UpdatedAt != nil
	})

	sub := e.m.Bus().Subscribe(func(ev fleet.Event) bool {
		return ev.Kind == fleet.EventMachine && ev.Machine == "w1"
	})
	defer sub.Close()

	// 2. Guard transition to warn (>=80%) updates registry and emits EventMachine.
	tWarn := time.Now().Add(time.Second)
	stub.emit(capture.DiskStatus{
		Status:     capture.DiskStatusWarn,
		TotalBytes: 1000,
		FreeBytes:  150,
		UsedPct:    85,
		WarnAt:     80,
		PauseAt:    90,
		At:         tWarn,
	})
	eventually(t, "warn disk state in registry", func() bool {
		info, ok := e.m.Machine("w1")
		return ok && info.Disk.Status == "warn" && info.Disk.FreeBytes == 150
	})

	// 3. Guard transition to critical (>=90%) updates registry + Summarize.
	tCrit := tWarn.Add(time.Second)
	stub.emit(capture.DiskStatus{
		Status:     capture.DiskStatusCritical,
		TotalBytes: 1000,
		FreeBytes:  50,
		UsedPct:    95,
		WarnAt:     80,
		PauseAt:    90,
		At:         tCrit,
	})
	eventually(t, "critical disk state in registry", func() bool {
		info, ok := e.m.Machine("w1")
		return ok && info.Disk.Status == "critical" && info.Disk.FreeBytes == 50
	})

	sum := fleet.Summarize(e.m.Machines())
	if len(sum.DiskCritical) != 1 || sum.DiskCritical[0] != "w1" {
		t.Errorf("Summary.DiskCritical = %v, want [w1]", sum.DiskCritical)
	}

	// Verify /ws/events bus observed the disk updates.
	sawCrit := false
	deadline := time.After(wait)
	for !sawCrit {
		select {
		case ev := <-sub.C:
			if ev.Info != nil && ev.Info.Disk.Status == "critical" {
				sawCrit = true
			}
		case <-deadline:
			t.Fatal("timed out waiting for EventMachine with critical disk state")
		}
	}
}
