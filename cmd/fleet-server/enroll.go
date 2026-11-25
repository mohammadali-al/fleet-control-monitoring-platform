package main

// Token enrollment wiring (T-0026): the enrollment endpoint on its own TLS
// listener (it bypasses the operator auth proxy; the token is its
// authentication), and the operator API mounted next to the panel.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strconv"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/enroll"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// enrollHost is what token enrollment needs from the fleet manager:
// *fleet.Manager implements it.
type enrollHost interface {
	Registry() *registry.Registry
	CA() *pki.CA
	Reconcile()
	Submit(req fleet.SubmitRequest) (string, error)
}

// fleetRunner sends Stop to an agent as a one-shot command through the
// manager's queue. The operator has confirmed Stop in the panel, so the
// dangerous-command gate is confirmed too. A monitor agent has no command
// role, so it cannot be stopped this way.
type fleetRunner struct{ h enrollHost }

func (r fleetRunner) Run(_ context.Context, machine, command string) error {
	_, err := r.h.Submit(fleet.SubmitRequest{Machine: machine, Command: command, Confirm: true})
	switch {
	case errors.Is(err, fleet.ErrNoShell):
		return enroll.ErrStopUnsupported
	case errors.Is(err, fleet.ErrOffline):
		return fmt.Errorf("the agent is offline, so it cannot be told to stop (it is not running, or not connected): %w", err)
	}
	return err
}

// newEnroll builds the enrollment service from the server configuration.
func newEnroll(s config.ServerConfig, dataDir string, h enrollHost, log *slog.Logger) (*enroll.Service, error) {
	if s.EnrollHost == "" {
		return nil, errors.New("enroll_addr is set but enroll_host is not (the name or IP address machines reach this server at)")
	}
	if s.AgentDialAddr == "" {
		return nil, errors.New("token enrollment needs the agent_dial listener: set agent_dial_addr")
	}
	enrollPort, err := portOf(s.EnrollAddr)
	if err != nil {
		return nil, fmt.Errorf("enroll_addr: %w", err)
	}
	dialPort, err := portOf(s.AgentDialAddr)
	if err != nil {
		return nil, fmt.Errorf("agent_dial_addr: %w", err)
	}
	ca := h.CA()
	if ca == nil {
		return nil, errors.New("token enrollment needs the internal CA, which is unavailable")
	}
	store := registry.NewStore(dataDir)
	reg := h.Registry()
	return enroll.New(enroll.Config{
		CA: ca, Registry: reg,
		Persist:    func() error { return store.Save(reg) },
		Reconcile:  h.Reconcile,
		Runner:     fleetRunner{h},
		Binary:     enroll.BinaryDir(s.AgentBinaryDir, s.AgentBinaryPath, runtime.GOOS, runtime.GOARCH),
		PublicHost: s.EnrollHost, EnrollPort: enrollPort, AgentDialPort: dialPort,
		ServerID: s.ServerIDOrDefault(), Logger: log,
	})
}

func portOf(addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(p)
	if err != nil || n <= 0 || n > 65535 {
		return 0, fmt.Errorf("bad port in %q", addr)
	}
	return n, nil
}

// withEnroll returns the panel with the operator enrollment API mounted at
// /api/enroll/ (behind the same proxy as the panel).
func withEnroll(panel http.Handler, svc *enroll.Service) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(enroll.APIPrefix, svc.APIHandler())
	mux.Handle("/", panel)
	return mux
}

// enableEnroll turns token enrollment on when server.enroll_addr is set: it
// starts the enrollment endpoint (TLS, token-gated, outside the auth proxy)
// and returns the panel handler with the operator API added. Without
// enroll_addr, or when the manager cannot host it, the panel is returned
// unchanged.
func enableEnroll(ctx context.Context, s config.ServerConfig, dataDir string, mgr any, panel http.Handler, log *slog.Logger) (http.Handler, error) {
	if s.EnrollAddr == "" {
		return panel, nil
	}
	h, ok := mgr.(enrollHost)
	if !ok {
		return nil, errors.New("enroll_addr is set but this build's fleet manager cannot host token enrollment")
	}
	svc, err := newEnroll(s, dataDir, h, log)
	if err != nil {
		return nil, err
	}
	ln, err := svc.Listen(s.EnrollAddr)
	if err != nil {
		return nil, fmt.Errorf("enrollment listener %s: %w", s.EnrollAddr, err)
	}
	go func() {
		if err := svc.Serve(ctx, ln); err != nil {
			log.Error("enrollment endpoint", "component", "fleet-server", "err", err)
		}
	}()
	log.Info("token enrollment listening", "component", "fleet-server", "addr", ln.Addr().String(), "url", svc.BaseURL())
	return withEnroll(panel, svc), nil
}
