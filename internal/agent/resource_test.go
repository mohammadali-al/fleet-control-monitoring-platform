package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

type resourceSenderStub struct {
	mu   sync.Mutex
	envs []channel.Envelope
}

func (s *resourceSenderStub) Send(_ context.Context, env channel.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envs = append(s.envs, env)
	return nil
}

func (s *resourceSenderStub) payloads() []channel.ResourceStatePayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []channel.ResourceStatePayload
	for _, env := range s.envs {
		if env.Type != channel.TypeResourceState {
			continue
		}
		var p channel.ResourceStatePayload
		if err := json.Unmarshal(env.Payload, &p); err == nil {
			out = append(out, p)
		}
	}
	return out
}

type stubResourceSampler struct {
	mu      sync.Mutex
	calls   int
	unitsIn [][]string
	samples []watchdog.Sample
	err     error
}

func (s *stubResourceSampler) Sample(_ context.Context, units []string) (watchdog.Sample, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.unitsIn = append(s.unitsIn, append([]string(nil), units...))
	if s.err != nil {
		return watchdog.Sample{}, s.err
	}
	idx := s.calls - 1
	if idx >= len(s.samples) {
		idx = len(s.samples) - 1
	}
	return s.samples[idx], nil
}

func TestWorkerResourceReporterOnConnectAndTick(t *testing.T) {
	origInterval := resourceReportInterval
	resourceReportInterval = 20 * time.Millisecond
	t.Cleanup(func() { resourceReportInterval = origInterval })

	t0 := time.Date(2026, 11, 28, 15, 30, 0, 0, time.UTC)
	t1 := t0.Add(20 * time.Millisecond)
	sampler := &stubResourceSampler{
		samples: []watchdog.Sample{
			{
				Time: t0,
				Machine: watchdog.MachineMetrics{
					CPUPercent:    0,
					MemoryPercent: 45.5,
					LoadPerCPU:    0.5,
					EgressMbps:    0,
				},
			},
			{
				Time: t1,
				Machine: watchdog.MachineMetrics{
					CPUPercent:    37.25,
					MemoryPercent: 46.0,
					LoadPerCPU:    0.85,
					EgressMbps:    4.2,
				},
			},
		},
	}

	w := newWorker(Config{
		MachineID: "w1",
		DataDir:   t.TempDir(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	w.resSampler = sampler
	sender := &resourceSenderStub{}
	w.sender.set(sender)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.start(ctx)
	t.Cleanup(func() {
		cancel()
		w.shutdown()
	})

	w.reportResourceOnConnect()

	deadline := time.Now().Add(2 * time.Second)
	for len(sender.payloads()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for connect + tick resource reports; got %d", len(sender.payloads()))
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := sender.payloads()
	if got[0].MemPercent != 45.5 || got[0].LoadPerCPU != 0.5 || !got[0].At.Equal(t0) {
		t.Fatalf("first report = %+v", got[0])
	}
	if got[1].CPUPercent != 37.25 || got[1].MemPercent != 46.0 || got[1].LoadPerCPU != 0.85 || got[1].EgressMbps != 4.2 || !got[1].At.Equal(t1) {
		t.Fatalf("second report = %+v", got[1])
	}

	// Ensure machine-level only sampling (units == nil).
	sampler.mu.Lock()
	for i, u := range sampler.unitsIn {
		if len(u) != 0 {
			t.Errorf("sample call %d passed units %v, want nil/empty", i, u)
		}
	}
	sampler.mu.Unlock()
}

func TestWorkerResourceReporterSkipsOnSampleError(t *testing.T) {
	w := newWorker(Config{
		MachineID: "w1",
		DataDir:   t.TempDir(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	w.resSampler = &stubResourceSampler{err: errors.New("proc unavailable")}
	sender := &resourceSenderStub{}
	w.sender.set(sender)

	w.sampleAndReportResource(context.Background())

	if got := sender.payloads(); len(got) != 0 {
		t.Fatalf("expected 0 payloads on sample error, got %+v", got)
	}
}

// Verifies that holding one SystemSampler on the worker computes CPU% deltas
// between consecutive calls rather than always reporting 0%.
func TestWorkerResourceSamplerHoldsStateForCPUDelta(t *testing.T) {
	procDir := t.TempDir()
	writeProc := func(stat, mem, load string) {
		if err := os.WriteFile(filepath.Join(procDir, "stat"), []byte(stat), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procDir, "meminfo"), []byte(mem), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(procDir, "loadavg"), []byte(load), 0644); err != nil {
			t.Fatal(err)
		}
	}
	mem := "MemTotal:       1000000 kB\nMemAvailable:    250000 kB\n"
	load := "1.50 1.20 1.00 1/100 123\n"
	writeProc("cpu  100 0 0 900 0 0 0 0 0 0\n", mem, load)

	ss := watchdog.NewSystemSampler("", "")
	ss.ProcRoot = procDir
	ss.Now = func() time.Time { return time.Date(2026, 11, 28, 15, 0, 0, 0, time.UTC) }

	w := newWorker(Config{
		MachineID: "w1",
		DataDir:   t.TempDir(),
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	w.resSampler = ss
	sender := &resourceSenderStub{}
	w.sender.set(sender)

	w.sampleAndReportResource(context.Background())
	// Advance +50 busy, +50 idle => 50% CPU over the delta.
	writeProc("cpu  150 0 0 950 0 0 0 0 0 0\n", mem, load)
	w.sampleAndReportResource(context.Background())

	got := sender.payloads()
	if len(got) != 2 {
		t.Fatalf("got %d payloads, want 2", len(got))
	}
	if got[0].CPUPercent != 0 {
		t.Errorf("first CPUPercent = %v, want 0", got[0].CPUPercent)
	}
	if got[1].CPUPercent != 50 {
		t.Errorf("second CPUPercent = %v, want 50", got[1].CPUPercent)
	}
	if got[1].MemPercent != 75 {
		t.Errorf("MemPercent = %v, want 75", got[1].MemPercent)
	}
	if got[1].LoadPerCPU <= 0 {
		t.Errorf("LoadPerCPU = %v, want > 0", got[1].LoadPerCPU)
	}
}
