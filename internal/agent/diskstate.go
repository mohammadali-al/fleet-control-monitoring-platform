package agent

import (
	"context"
	"os"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

const diskSendTimeout = 10 * time.Second

// diskReportInterval is how often an idle worker samples and reports its
// local disk usage and guard state (on top of immediate transition alerts).
var diskReportInterval = time.Minute

// diskStateSource is an optional capability of a Capturer (implemented by
// liveCapturer and test capturers) that surfaces disk-guard transitions and
// disk sampling.
type diskStateSource interface {
	SetOnDiskState(fn func(capture.DiskStatus)) (capture.DiskStatus, bool)
	CheckDisk() (capture.DiskStatus, error)
}

// bindDiskState attaches the worker's disk-state reporter to c (if c exposes
// diskStateSource) and reports any already-sampled state. Caller holds w.mu and
// has verified !w.closed.
func (w *worker) bindDiskState(c Capturer) {
	var src diskStateSource
	if c != nil {
		if ds, ok := c.(diskStateSource); ok {
			src = ds
		}
	}

	w.diskMu.Lock()
	old := w.diskSrc
	if old == src {
		w.diskMu.Unlock()
		return
	}
	w.diskSrc = src
	w.diskMu.Unlock()

	if old != nil {
		old.SetOnDiskState(nil)
	}
	if src == nil {
		return
	}
	cur, ok := src.SetOnDiskState(func(st capture.DiskStatus) {
		w.reportDiskState(st, false)
	})
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		if ok {
			w.reportDiskState(cur, true)
			return
		}
		if st, err := src.CheckDisk(); err == nil {
			w.reportDiskState(st, true)
		}
	}()
}

func (w *worker) unbindDiskState() {
	w.diskMu.Lock()
	src := w.diskSrc
	w.diskSrc = nil
	w.diskMu.Unlock()
	if src != nil {
		src.SetOnDiskState(nil)
	}
}

// startDiskMonitor periodically samples and reports disk state while the
// worker is running.
func (w *worker) startDiskMonitor() {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(diskReportInterval)
		defer t.Stop()
		for {
			select {
			case <-w.base.Done():
				return
			case <-t.C:
				w.sampleAndReportDisk(false)
			}
		}
	}()
}

// reportDiskOnConnect samples disk usage and reports it as soon as a
// control-channel connection is established.
func (w *worker) reportDiskOnConnect() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.wg.Add(1)
	w.mu.Unlock()
	go func() {
		defer w.wg.Done()
		w.sampleAndReportDisk(true)
	}()
}

func (w *worker) sampleAndReportDisk(force bool) {
	st, err := w.sampleDiskState()
	if err != nil {
		w.log.Debug("sample disk state", "component", "fleet-agent", "err", err)
		return
	}
	w.reportDiskState(st, force)
}

func (w *worker) sampleDiskState() (capture.DiskStatus, error) {
	w.diskMu.Lock()
	src := w.diskSrc
	w.diskMu.Unlock()
	if src != nil {
		return src.CheckDisk()
	}
	w.mu.Lock()
	eff := w.capCfg
	if !w.capTried {
		eff = w.cfg.Capture
	}
	w.mu.Unlock()
	warnPct := eff.DiskWarnPercent
	pausePct := eff.DiskPausePercent
	dir := capture.EvidenceMachineDir(w.cfg.DataDir, w.cfg.MachineID)
	if _, err := os.Stat(dir); err != nil {
		dir = w.cfg.DataDir
	}
	return capture.CheckDisk(dir, warnPct, pausePct)
}

func (w *worker) reportDiskState(st capture.DiskStatus, force bool) {
	if st.Status == "" {
		return
	}
	if st.At.IsZero() {
		st.At = time.Now()
	}
	if !force {
		w.diskMu.Lock()
		dup := w.diskReported &&
			st.Status == w.lastDisk.Status &&
			st.At.Sub(w.lastDisk.At) < diskReportInterval
		w.diskMu.Unlock()
		if dup {
			return
		}
	}
	w.sendDiskState(st)
}

func (w *worker) sendDiskState(st capture.DiskStatus) {
	p := channel.DiskStatePayload{
		Status:     st.Status,
		TotalBytes: st.TotalBytes,
		FreeBytes:  st.FreeBytes,
		UsedPct:    st.UsedPct,
		WarnAt:     st.WarnAt,
		PauseAt:    st.PauseAt,
		At:         st.At,
	}
	env, err := channel.NewEnvelope(channel.TypeDiskState, "", p)
	if err != nil {
		w.log.Warn("build disk-state envelope", "component", "fleet-agent", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(w.base, diskSendTimeout)
	defer cancel()
	if err := w.sender.Send(ctx, env); err != nil {
		w.log.Debug("send disk-state", "component", "fleet-agent", "err", err)
		return
	}
	w.diskMu.Lock()
	w.lastDisk = st
	w.diskReported = true
	w.diskMu.Unlock()
}
