package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	osexec "os/exec"
	"reflect"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

// ErrCaptureUnavailable means live evidence capture cannot run on this host
// (it needs root and the nft binary). Commands still run, uncaptured.
var ErrCaptureUnavailable = errors.New("agent: live evidence capture is unavailable")

// handleConfig applies a config envelope and acknowledges it. A config the
// agent cannot apply is reported to the server and changes nothing.
func (w *worker) handleConfig(env channel.Envelope) {
	var p channel.ConfigPayload
	err := json.Unmarshal(env.Payload, &p)
	if err != nil {
		err = fmt.Errorf("bad config payload: %w", err)
	} else {
		err = w.apply(p)
	}
	ack := channel.ConfigAckPayload{OK: err == nil}
	if err != nil {
		ack.Error = err.Error()
		w.log.Warn("config rejected", "component", "fleet-agent", "err", err)
	}
	out, nerr := channel.NewEnvelope(channel.TypeConfigAck, env.ID, ack)
	if nerr == nil {
		ctx, cancel := context.WithTimeout(w.base, 10*time.Second)
		defer cancel()
		nerr = w.sender.Send(ctx, out)
	}
	if nerr != nil {
		w.log.Warn("send config ack", "component", "fleet-agent", "err", nerr)
	}
}

// apply puts a config payload into effect. It is the generic applier: each
// section of channel.ConfigPayload has one apply step, and a section that is
// absent is left alone. Applying the same payload again changes nothing, so the
// server may push on every connection.
func (w *worker) apply(p channel.ConfigPayload) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errors.New("agent is shutting down")
	}
	// Capture first: a new executor's session hooks read the capturer.
	w.applyCaptureLocked(p.Capture)
	return w.applyGateLocked(p.GateSecret)
}

// applyGateLocked installs the gate secret. The secret is fixed inside an
// exec.Gate, so a changed secret starts a new executor for new runs; the old
// one stays reachable until its sessions end.
func (w *worker) applyGateLocked(secret string) error {
	if secret == "" {
		if w.ex == nil {
			return errors.New("config carries no gate secret; commands stay disabled")
		}
		return nil // the section is absent: keep the current secret
	}
	if w.ex != nil && secret == w.secret {
		return nil
	}
	gate, err := exec.NewGate([]byte(secret), nil)
	if err != nil {
		return err
	}
	ex, err := exec.New(exec.Config{
		MachineID:      w.cfg.MachineID,
		Sender:         w.sender,
		Gate:           gate,
		CgroupRoot:     w.cfg.CgroupRoot,
		TermGrace:      w.cfg.TermGrace,
		Shell:          w.cfg.Shell,
		Logger:         w.log,
		OnSessionStart: w.sessionStart,
		OnSessionEnd:   w.sessionEnd,
	})
	if err != nil {
		return err
	}
	if w.ex != nil {
		w.retired = append(w.retired, w.ex)
		w.log.Info("gate secret changed; new commands use the new secret", "component", "fleet-agent")
	} else {
		w.log.Info("gate secret applied", "component", "fleet-agent")
	}
	w.ex, w.secret = ex, secret
	w.readyOnce.Do(func() { close(w.ready) })
	return nil
}

// applyCaptureLocked (re)builds the capture subsystem when the effective
// capture config changes. A pushed section replaces the local one. Capture is
// best-effort: when it cannot run (not root, no nft) the agent logs that and
// runs commands uncaptured; that is not a config error.
func (w *worker) applyCaptureLocked(pushed *config.CaptureConfig) {
	eff := w.cfg.Capture
	if pushed != nil {
		eff = *pushed
	}
	if w.capTried && reflect.DeepEqual(eff, w.capCfg) {
		return
	}
	w.capTried, w.capCfg = true, eff
	if w.capt != nil {
		w.retCapt = append(w.retCapt, w.capt)
		w.capt = nil
	}
	c, err := w.cfg.CaptureFactory(w.base, captureConfig(w.cfg, eff, w.log))
	switch {
	case errors.Is(err, ErrCaptureUnavailable):
		w.log.Warn("evidence capture is off", "component", "fleet-agent", "reason", err.Error())
	case err != nil:
		w.log.Error("evidence capture could not start", "component", "fleet-agent", "err", err)
	default:
		w.capt = c
		if w.cfg.CgroupRoot == "" {
			w.log.Warn("evidence capture has no cgroup root: sessions run without cgroups and are not captured", "component", "fleet-agent")
		}
	}
	w.bindPublicIP(w.capt)
	w.bindDiskState(w.capt)
}

// captureConfig maps the config-file/push shape to capture.Config.
func captureConfig(cfg Config, c config.CaptureConfig, log *slog.Logger) capture.Config {
	return capture.Config{
		DataDir:       cfg.DataDir,
		Machine:       cfg.MachineID,
		TrimThreshold: c.TrimThresholdBytes,
		Segment: capture.SegmentConfig{
			MaxBytes:         c.SegmentMaxBytes,
			MaxAge:           time.Duration(c.SegmentMaxAgeSeconds) * time.Second,
			DiskWarnPercent:  c.DiskWarnPercent,
			DiskPausePercent: c.DiskPausePercent,
		},
		Reflectors: c.Reflectors,
		NFLOGGroup: c.NFLOGGroup,
		Log:        log,
	}
}

// liveFactory is the default CaptureFactory: the real capture Controller, which
// binds nftables/NFLOG per session and so needs root and the nft binary. The
// capability is checked here, up front, so an unprivileged agent says so once
// instead of failing every session (and never starts the public-IP refresh).
func liveFactory(ctx context.Context, cfg capture.Config) (Capturer, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("%w: needs root (CAP_NET_ADMIN)", ErrCaptureUnavailable)
	}
	if _, err := osexec.LookPath("nft"); err != nil {
		return nil, fmt.Errorf("%w: the nft binary was not found", ErrCaptureUnavailable)
	}
	c, err := capture.NewController(cfg)
	if err != nil {
		return nil, err
	}
	if _, err := capture.EnsureEvidenceDir(cfg.DataDir, cfg.Machine); err != nil {
		return nil, err
	}
	c.Start(ctx)
	return liveCapturer{c}, nil
}

// liveCapturer adapts *capture.Controller to Capturer.
type liveCapturer struct{ c *capture.Controller }

func (l liveCapturer) Capture(sessionID, cgroupPath string) (SessionCapture, error) {
	sc, err := l.c.Capture(sessionID, cgroupPath)
	if err != nil {
		return nil, err
	}
	return sc, nil
}

func (l liveCapturer) Close() { l.c.Close() }

func (l liveCapturer) PublicIPCache() *capture.PublicIPCache { return l.c.PublicIPCache() }

func (l liveCapturer) SetOnDiskState(fn func(capture.DiskStatus)) (capture.DiskStatus, bool) {
	return l.c.SetOnDiskState(fn)
}

func (l liveCapturer) CheckDisk() (capture.DiskStatus, error) { return l.c.CheckDisk() }
