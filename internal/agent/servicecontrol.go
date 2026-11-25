package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	osexec "os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

const (
	defaultSystemdUnit    = "fleet-agent.service"
	defaultLaunchdLabel   = "com.fleet.agent"
	defaultLaunchdPlist   = "/Library/LaunchDaemons/com.fleet.agent.plist"
	serviceControlTimeout = 10 * time.Second
)

// ServiceController controls the agent's own system service unit (systemd on
// Linux, launchd on macOS). It is independent of the command executor so both
// worker and monitor agents handle service-control envelopes directly.
type ServiceController interface {
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
	Status(ctx context.Context) (string, error)
}

type cmdRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

type osServiceController struct {
	goos         string
	systemdUnit  string
	launchdLabel string
	launchdPlist string
	run          cmdRunner
}

func defaultServiceController() ServiceController {
	return &osServiceController{
		goos:         runtime.GOOS,
		systemdUnit:  defaultSystemdUnit,
		launchdLabel: defaultLaunchdLabel,
		launchdPlist: defaultLaunchdPlist,
		run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return osexec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}
}

func (c *osServiceController) Stop(ctx context.Context) error {
	switch c.goos {
	case "darwin":
		target := "system/" + c.launchdLabel
		if err := c.exec(ctx, "launchctl", "disable", target); err != nil {
			return err
		}
		return c.exec(ctx, "launchctl", "bootout", target)
	case "linux":
		return c.exec(ctx, "systemctl", "disable", "--now", "--no-block", c.systemdUnit)
	default:
		return fmt.Errorf("service-control stop: unsupported OS %q", c.goos)
	}
}

func (c *osServiceController) Start(ctx context.Context) error {
	switch c.goos {
	case "darwin":
		target := "system/" + c.launchdLabel
		if err := c.exec(ctx, "launchctl", "enable", target); err != nil {
			return err
		}
		return c.exec(ctx, "launchctl", "bootstrap", "system", c.launchdPlist)
	case "linux":
		return c.exec(ctx, "systemctl", "enable", "--now", c.systemdUnit)
	default:
		return fmt.Errorf("service-control start: unsupported OS %q", c.goos)
	}
}

func (c *osServiceController) Status(ctx context.Context) (string, error) {
	switch c.goos {
	case "darwin":
		target := "system/" + c.launchdLabel
		_, err := c.run(ctx, "launchctl", "print", target)
		if err == nil {
			return "running", nil
		}
		var exitErr *osexec.ExitError
		if errors.As(err, &exitErr) {
			return "stopped", nil
		}
		return "", fmt.Errorf("launchctl print %s: %w", target, err)
	case "linux":
		out, err := c.run(ctx, "systemctl", "is-active", c.systemdUnit)
		state := strings.TrimSpace(string(out))
		if err == nil {
			if state == "active" {
				return "running", nil
			}
			return state, nil
		}
		var exitErr *osexec.ExitError
		if state != "" && errors.As(err, &exitErr) {
			if state == "inactive" {
				return "stopped", nil
			}
			return state, nil
		}
		return "", fmt.Errorf("systemctl is-active %s: %w", c.systemdUnit, err)
	default:
		return "", fmt.Errorf("service-control status: unsupported OS %q", c.goos)
	}
}

func (c *osServiceController) exec(ctx context.Context, name string, args ...string) error {
	out, err := c.run(ctx, name, args...)
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, msg)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// SetServiceController replaces the worker agent's ServiceController (for tests).
func (a *Agent) SetServiceController(sc ServiceController) {
	if sc == nil {
		sc = defaultServiceController()
	}
	a.w.svcMu.Lock()
	a.w.svc = sc
	a.w.svcMu.Unlock()
}

// SetServiceController replaces the monitor agent's ServiceController (for tests).
func (m *Monitor) SetServiceController(sc ServiceController) {
	if sc == nil {
		sc = defaultServiceController()
	}
	m.mu.Lock()
	m.svc = sc
	m.mu.Unlock()
}

type serviceSenderFunc func(ctx context.Context, env channel.Envelope) error

func (f serviceSenderFunc) Send(ctx context.Context, env channel.Envelope) error { return f(ctx, env) }

func handleServiceControl(ctx context.Context, log *slog.Logger, send exec.Sender, ctrl ServiceController, env channel.Envelope) {
	var req channel.ServiceControlPayload
	res := channel.ServiceControlResultPayload{}
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		res.Error = fmt.Sprintf("bad service-control payload: %v", err)
		log.Warn("bad service-control payload", "component", "fleet-agent", "err", err)
		sendServiceControlResult(ctx, log, send, env.ID, res)
		return
	}
	res.Action = req.Action
	if ctrl == nil {
		ctrl = defaultServiceController()
	}
	cctx, cancel := context.WithTimeout(ctx, serviceControlTimeout)
	defer cancel()

	switch req.Action {
	case channel.ServiceActionStop:
		if err := ctrl.Stop(cctx); err != nil {
			res.Error = err.Error()
			log.Warn("service-control stop failed", "component", "fleet-agent", "err", err)
		} else {
			res.OK = true
			res.State = "stopped"
			log.Info("service-control stop succeeded", "component", "fleet-agent")
		}
	case channel.ServiceActionStart:
		if err := ctrl.Start(cctx); err != nil {
			res.Error = err.Error()
			log.Warn("service-control start failed", "component", "fleet-agent", "err", err)
		} else {
			res.OK = true
			res.State = "running"
			log.Info("service-control start succeeded", "component", "fleet-agent")
		}
	case channel.ServiceActionStatus:
		state, err := ctrl.Status(cctx)
		if err != nil {
			res.Error = err.Error()
			log.Warn("service-control status failed", "component", "fleet-agent", "err", err)
		} else {
			res.OK = true
			res.State = state
		}
	default:
		res.Error = fmt.Sprintf("unknown service-control action %q", req.Action)
		log.Warn("unknown service-control action", "component", "fleet-agent", "action", req.Action)
	}
	sendServiceControlResult(ctx, log, send, env.ID, res)
}

func sendServiceControlResult(ctx context.Context, log *slog.Logger, send exec.Sender, id string, res channel.ServiceControlResultPayload) {
	out, err := channel.NewEnvelope(channel.TypeServiceControlResult, id, res)
	if err == nil {
		sctx, cancel := context.WithTimeout(ctx, serviceControlTimeout)
		defer cancel()
		err = send.Send(sctx, out)
	}
	if err != nil {
		log.Warn("send service-control result", "component", "fleet-agent", "err", err)
	}
}
