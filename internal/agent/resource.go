package agent

import (
	"context"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

const resourceSendTimeout = 10 * time.Second

// resourceReportInterval is how often an online worker samples and reports
// machine-level CPU, memory, load, and egress telemetry.
var resourceReportInterval = 15 * time.Second

// startResourceMonitor periodically samples and reports machine-level resource
// telemetry while the worker is running.
func (w *worker) startResourceMonitor() {
	ctx := w.base
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(resourceReportInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				w.sampleAndReportResource(ctx)
			}
		}
	}()
}

// reportResourceOnConnect samples machine-level resource telemetry and reports
// it as soon as a control-channel connection is established.
func (w *worker) reportResourceOnConnect() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	ctx := w.base
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		w.sampleAndReportResource(ctx)
	}()
}

func (w *worker) sampleAndReportResource(ctx context.Context) {
	sampler := w.resSampler
	if sampler == nil {
		return
	}
	if ctx == nil {
		ctx = w.base
	}
	// Sample machine-level metrics only (no units, no session cgroup root).
	sctx, cancel := context.WithTimeout(ctx, resourceSendTimeout)
	defer cancel()
	sample, err := sampler.Sample(sctx, nil)
	if err != nil {
		w.log.Debug("sample resource state", "component", "fleet-agent", "err", err)
		return
	}
	w.sendResourceState(ctx, sample)
}

func (w *worker) sendResourceState(ctx context.Context, sample watchdog.Sample) {
	at := sample.Time
	if at.IsZero() {
		at = time.Now()
	}
	p := channel.ResourceStatePayload{
		CPUPercent: sample.Machine.CPUPercent,
		MemPercent: sample.Machine.MemoryPercent,
		LoadPerCPU: sample.Machine.LoadPerCPU,
		EgressMbps: sample.Machine.EgressMbps,
		At:         at,
	}
	env, err := channel.NewEnvelope(channel.TypeResourceState, "", p)
	if err != nil {
		w.log.Warn("build resource-state envelope", "component", "fleet-agent", "err", err)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, resourceSendTimeout)
	defer cancel()
	if err := w.sender.Send(sctx, env); err != nil {
		w.log.Debug("send resource-state", "component", "fleet-agent", "err", err)
		return
	}
}
