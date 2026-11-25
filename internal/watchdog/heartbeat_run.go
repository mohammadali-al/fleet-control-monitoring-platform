package watchdog

import (
	"context"
	"time"
)

// RunWithHeartbeat is Run with the heartbeat written from the loop itself: on
// every wake-up it beats when `heartbeat_write` is due, then samples when the
// sample interval is due, and sleeps until the earlier of the two. A hang in
// sampling or in the ladder therefore stops the beats, the file goes stale and
// the liveness timer restarts the service. The write interval and threshold
// follow live policy changes. This is what `fleet-agent watchdog` runs.
func (w *Watchdog) RunWithHeartbeat(ctx context.Context) error {
	hb := NewHeartbeat(HeartbeatPath(w.cfg.DataDir, HeartbeatWatchdog))
	var nextSample time.Time
	failing := false
	for {
		pol := w.engine.Policy()
		nextBeat, err := hb.Tick(pol.HeartbeatWrite.D(), pol.HeartbeatThreshold.D())
		switch {
		case err != nil && !failing:
			w.log.Warn("heartbeat write failed", "component", "fleet-watchdog", "err", err)
			failing = true
		case err == nil && failing:
			w.log.Info("heartbeat writes recovered", "component", "fleet-watchdog")
			failing = false
		}
		if !time.Now().Before(nextSample) {
			w.Step(ctx)
			nextSample = time.Now().Add(w.engine.Policy().SampleInterval.D())
		}
		wait := min(nextBeat, time.Until(nextSample))
		timer := time.NewTimer(max(wait, 10*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
