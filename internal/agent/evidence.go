package agent

import (
	"context"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
)

// The worker's evidence-sync endpoint. internal/evidence owns the protocol
// (T-0009); this file only plugs it into the worker runtime:
//
//	capture ---writes--> DataDir/evidence/<machine>/{segments, manifest.json}
//	                                   |
//	main server --ev-manifest-req / ev-range / ev-verify--> evidence.Agent
//	main server <--ev-manifest / ev-chunk--------------------'
//
// plus the idle push: when no session is running the agent volunteers its
// manifest (once at connection, then every evidencePushInterval) and the server
// fetches what it lacks over the same exchange as a pull.
//
// The endpoint reads the segment directory, not the capture subsystem, so it
// serves what is already on disk even when live capture is off (an empty
// manifest on a machine that never captured).

// evidencePushInterval is how often an idle agent offers unsynced segments.
var evidencePushInterval = time.Minute

// evidenceOfferTimeout bounds the unprompted manifest offer.
const evidenceOfferTimeout = 10 * time.Second

// initEvidence builds the sync agent over DataDir/evidence/<machine>. A worker
// whose identity or data dir is unset (never the case through New) simply has
// no evidence endpoint.
func (w *worker) initEvidence() {
	ev, err := evidence.NewAgent(evidence.AgentConfig{
		Machine: w.cfg.MachineID,
		Dir:     capture.EvidenceMachineDir(w.cfg.DataDir, w.cfg.MachineID),
		Sender:  w.sender,
		Idle:    w.idle,
		Log:     w.log,
	})
	if err != nil {
		w.log.Warn("evidence sync is off", "component", "fleet-agent", "err", err)
		return
	}
	w.ev = ev
}

// idle reports that no command session is running, the condition for an idle
// push (the agent keeps no queue of its own; the server's queue is not an
// agent-side busy signal).
func (w *worker) idle() bool {
	w.cmu.Lock()
	defer w.cmu.Unlock()
	return len(w.live) == 0
}

// handleEvidence routes an ev-* envelope from the main server to the sync
// agent. It reports whether the envelope was an evidence message.
func (w *worker) handleEvidence(ctx context.Context, env channel.Envelope) bool {
	switch env.Type {
	case channel.TypeEvManifestReq, channel.TypeEvRange, channel.TypeEvVerify:
		if w.ev != nil {
			w.ev.Handle(ctx, env)
		}
		return true
	}
	return false
}

// startEvidence runs the periodic idle push until the worker's context ends.
func (w *worker) startEvidence() {
	if w.ev == nil {
		return
	}
	w.evDone = make(chan struct{})
	ctx, interval := w.base, evidencePushInterval
	go func() {
		defer close(w.evDone)
		w.ev.Run(ctx, interval)
	}()
}

// offerEvidence is the idle push on demand (at connection): it sends nothing
// unless the agent is idle and holds segments the server has not confirmed.
func (w *worker) offerEvidence() {
	if w.ev == nil {
		return
	}
	ctx, cancel := context.WithTimeout(w.base, evidenceOfferTimeout)
	defer cancel()
	if _, err := w.ev.PushIfIdle(ctx); err != nil {
		w.log.Debug("evidence offer not sent", "component", "fleet-agent", "err", err)
	}
}
