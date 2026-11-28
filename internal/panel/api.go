package panel

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/backup"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/onboard"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// Backend is what the live pages need from the main server. *fleet.Manager
// implements it.
type Backend interface {
	Machines() []fleet.MachineInfo
	Machine(id string) (fleet.MachineInfo, bool)
	Onboard(ctx context.Context, req fleet.OnboardRequest) (fleet.MachineInfo, *onboard.Result, error)
	Reprovision(ctx context.Context, id string, creds fleet.Credentials) (fleet.MachineInfo, *onboard.Result, error)
	Remove(ctx context.Context, id string, creds fleet.Credentials, skipRemote bool) error
	RefreshPublicIP(ctx context.Context, machine string) error

	Preflight(machine, command string) error
	Submit(req fleet.SubmitRequest) (string, error)
	OpenShell(req fleet.ShellRequest) (string, error)
	AttachShell(req fleet.AttachRequest) (*fleet.ShellAttachment, error)
	Cancel(ctx context.Context, session string) error
	ShellInput(ctx context.Context, session string, data []byte) error
	ShellResize(ctx context.Context, session string, cols, rows uint16) error
	ShellConfirm(ctx context.Context, session, line string) error
	Commands() []fleet.Command
	Command(session string) (fleet.Command, []fleet.OutputSegment, bool)

	Bus() *fleet.Bus
}

const (
	maxBodyBytes = 64 << 10
	// provisionTimeout bounds one onboarding / re-provision / removal. It is
	// not tied to the browser connection: a half-finished provisioning is
	// harder to recover from than a slow one.
	provisionTimeout = 10 * time.Minute
)

func (s *server) routesAPI(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/machines", s.live(s.listMachines))
	mux.HandleFunc("POST /api/machines", s.live(s.onboardMachine))
	mux.HandleFunc("GET /api/machines/{id}", s.live(s.getMachine))
	mux.HandleFunc("POST /api/machines/{id}/groups", s.live(s.setMachineGroups))
	mux.HandleFunc("POST /api/machines/{id}/reprovision", s.live(s.reprovisionMachine))
	mux.HandleFunc("POST /api/machines/{id}/remove", s.live(s.removeMachine))
	mux.HandleFunc("POST /api/machines/{id}/refresh-public-ip", s.live(s.refreshPublicIP))

	mux.HandleFunc("GET /api/commands", s.live(s.listCommands))
	mux.HandleFunc("POST /api/commands", s.live(s.submitCommands))
	mux.HandleFunc("GET /api/commands/{id}", s.live(s.getCommand))
	mux.HandleFunc("POST /api/commands/{id}/cancel", s.live(s.cancelCommand))

	s.routesBaseline(mux)
	s.routesManeuver(mux)
	s.routesObserve(mux)
}

// live wraps an API handler: no backend is a 503, and a state-changing request
// must be JSON (a cross-site HTML form cannot send that without a preflight).
func (s *server) live(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.be == nil {
			writeJSON(w, http.StatusServiceUnavailable, errBody("the panel has no backend configured"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeJSON(w, http.StatusUnsupportedMediaType, errBody("Content-Type must be application/json"))
				return
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r)
	}
}

// operatorFrom names the signed-in operator for the audit trail. The panel
// keeps no accounts: it trusts the headers the auth proxy forwards (Authelia's
// Remote-User, then the common alternatives). With none it is "operator".
func operatorFrom(r *http.Request) string {
	for _, h := range []string{"Remote-User", "X-Forwarded-User", "X-Auth-Request-User", "X-Pomerium-Claim-Email"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return clip(v, 64)
		}
	}
	return "operator"
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error string `json:"error"`
}

func errBody(msg string) errorBody { return errorBody{Error: msg} }

// decode reads a JSON request body strictly: bounded, one value, no unknown fields.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody("bad request body: "+err.Error()))
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, errBody("bad request body: trailing data"))
		return false
	}
	return true
}

// statusFor maps a backend error to an HTTP status.
func statusFor(err error) int {
	var cr *fleet.ConfirmRequiredError
	switch {
	case errors.As(err, &cr):
		return http.StatusConflict
	case errors.Is(err, fleet.ErrUnavailable), errors.Is(err, onboard.ErrAgentBinaryMissing):
		return http.StatusServiceUnavailable
	case errors.Is(err, fleet.ErrUnknown), errors.Is(err, registry.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, fleet.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, fleet.ErrNoShell):
		return http.StatusUnprocessableEntity
	case errors.Is(err, fleet.ErrOffline), errors.Is(err, fleet.ErrBusy),
		errors.Is(err, backup.ErrNoServer), errors.Is(err, backup.ErrBackupRunning):
		return http.StatusConflict
	case errors.Is(err, fleet.ErrBadRequest):
		return http.StatusBadRequest
	default:
		return http.StatusBadGateway
	}
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, statusFor(err), errBody(err.Error()))
}

// ---- machines ----

type machinesResponse struct {
	Machines []fleet.MachineInfo `json:"machines"`
	Summary  fleet.Summary       `json:"summary"`
}

func (s *server) listMachines(w http.ResponseWriter, _ *http.Request) {
	ms := s.be.Machines()
	if ms == nil {
		ms = []fleet.MachineInfo{}
	}
	writeJSON(w, http.StatusOK, machinesResponse{Machines: ms, Summary: fleet.Summarize(ms)})
}

type credentialsBody struct {
	User       string `json:"user"`
	Password   string `json:"password"`
	PrivateKey string `json:"private_key"`
	Port       int    `json:"port"`
}

func (c credentialsBody) validate() error {
	if strings.TrimSpace(c.User) == "" {
		return errors.New("SSH username is required")
	}
	if c.Password == "" && strings.TrimSpace(c.PrivateKey) == "" {
		return errors.New("an SSH password or private key is required")
	}
	if c.Port < 0 || c.Port > 65535 {
		return errors.New("SSH port is out of range")
	}
	return nil
}

func (c credentialsBody) creds() fleet.Credentials {
	return fleet.Credentials{User: strings.TrimSpace(c.User), Password: c.Password, PrivateKey: c.PrivateKey, Port: c.Port}
}

type onboardBody struct {
	Host        string `json:"host"`
	Role        string `json:"role"`
	ConnectMode string `json:"connect_mode"`
	credentialsBody
}

type provisionResponse struct {
	Machine    fleet.MachineInfo `json:"machine"`
	Created    bool              `json:"created"`
	CertIssued bool              `json:"cert_issued"`
	Steps      []string          `json:"steps"`
}

func (s *server) onboardMachine(w http.ResponseWriter, r *http.Request) {
	var b onboardBody
	if !decode(w, r, &b) {
		return
	}
	b.Host = strings.TrimSpace(b.Host)
	if net.ParseIP(b.Host) == nil {
		writeJSON(w, http.StatusBadRequest, errBody("the machine address must be an IP address"))
		return
	}
	if b.Role != registry.RoleWorker && b.Role != registry.RoleMonitor {
		writeJSON(w, http.StatusBadRequest, errBody(`role must be "worker" or "monitor"`))
		return
	}
	if b.ConnectMode == "" {
		b.ConnectMode = registry.ConnectModeServerDial
	}
	if b.ConnectMode != registry.ConnectModeServerDial && b.ConnectMode != registry.ConnectModeAgentDial {
		writeJSON(w, http.StatusBadRequest, errBody(`connect_mode must be "server_dial" or "agent_dial"`))
		return
	}
	if err := b.credentialsBody.validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), provisionTimeout)
	defer cancel()
	s.log.Info("onboard machine", "component", "panel", "operator", operatorFrom(r), "host", b.Host, "role", b.Role, "connect_mode", b.ConnectMode)
	info, res, err := s.be.Onboard(ctx, fleet.OnboardRequest{
		Host: b.Host, Role: b.Role, ConnectMode: b.ConnectMode, Credentials: b.credentialsBody.creds(),
	})
	if err != nil {
		s.log.Warn("onboard failed", "component", "panel", "host", b.Host, "err", err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, provisionResponse{Machine: info, Created: res.Created, CertIssued: res.CertIssued, Steps: res.Steps})
}

func (s *server) reprovisionMachine(w http.ResponseWriter, r *http.Request) {
	var b credentialsBody
	if !decode(w, r, &b) {
		return
	}
	if err := b.validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), provisionTimeout)
	defer cancel()
	s.log.Info("re-provision machine", "component", "panel", "operator", operatorFrom(r), "machine", id)
	info, res, err := s.be.Reprovision(ctx, id, b.creds())
	if err != nil {
		s.log.Warn("re-provision failed", "component", "panel", "machine", id, "err", err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, provisionResponse{Machine: info, Created: res.Created, CertIssued: res.CertIssued, Steps: res.Steps})
}

type removeBody struct {
	credentialsBody
	SkipRemote bool `json:"skip_remote"`
}

func (s *server) removeMachine(w http.ResponseWriter, r *http.Request) {
	var b removeBody
	if !decode(w, r, &b) {
		return
	}
	if !b.SkipRemote {
		if err := b.credentialsBody.validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
			return
		}
	}
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), provisionTimeout)
	defer cancel()
	s.log.Info("remove machine", "component", "panel", "operator", operatorFrom(r), "machine", id, "skip_remote", b.SkipRemote)
	if err := s.be.Remove(ctx, id, b.credentialsBody.creds(), b.SkipRemote); err != nil {
		s.log.Warn("remove failed", "component", "panel", "machine", id, "err", err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": id})
}

type registryProvider interface {
	Registry() *registry.Registry
}

type machineGroupsBody struct {
	Groups []string `json:"groups"`
}

func (s *server) registryDataDir() string {
	if dp, ok := s.be.(dataDirProvider); ok {
		if d := strings.TrimSpace(dp.DataDir()); d != "" {
			return d
		}
	}
	if ob, ok := s.be.(observer); ok {
		if o := ob.Observe(); o != nil {
			switch {
			case o.Jobs != nil && o.Jobs.Dir() != "":
				return filepath.Dir(o.Jobs.Dir())
			case o.Archive != nil && o.Archive.Root() != "":
				return filepath.Dir(o.Archive.Root())
			}
		}
	}
	return ""
}

func (s *server) setMachineGroups(w http.ResponseWriter, r *http.Request) {
	rp, ok := s.be.(registryProvider)
	if !ok || rp.Registry() == nil {
		notConfigured(w, "the machine registry")
		return
	}
	var b machineGroupsBody
	if !decode(w, r, &b) {
		return
	}
	groups := make([]string, 0, len(b.Groups))
	for _, g := range b.Groups {
		groups = append(groups, strings.TrimSpace(g))
	}
	if err := registry.ValidateGroups(groups); err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	if len(groups) == 0 {
		groups = nil
	}

	id := r.PathValue("id")
	reg := rp.Registry()
	var removed bool
	var prevGroups []string
	err := reg.Update(id, func(m *registry.Machine) {
		if m.Removed {
			removed = true
			return
		}
		prevGroups = append([]string(nil), m.Groups...)
		m.Groups = append([]string(nil), groups...)
	})
	if errors.Is(err, registry.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, errBody("unknown machine: "+id))
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errBody(err.Error()))
		return
	}
	if removed {
		writeJSON(w, http.StatusConflict, errBody("cannot update watchdog groups of a removed machine"))
		return
	}
	if dataDir := s.registryDataDir(); dataDir != "" {
		if err := registry.NewStore(dataDir).Save(reg); err != nil {
			_ = reg.Update(id, func(m *registry.Machine) {
				m.Groups = prevGroups
			})
			s.log.Warn("persist machine groups", "component", "panel", "machine", id, "err", err)
			writeJSON(w, http.StatusInternalServerError, errBody("could not save registry: "+err.Error()))
			return
		}
	}
	s.log.Info("machine watchdog groups updated", "component", "panel", "operator", operatorFrom(r), "machine", id, "groups", groups)
	info, _ := s.be.Machine(id)
	writeJSON(w, http.StatusOK, map[string]any{"machine": info})
}

// ---- commands ----

type commandsResponse struct {
	Commands []fleet.Command `json:"commands"`
}

// listCommands returns the serial queue and history, newest first. ?mode=
// oneshot or pty filters; ?active=1 keeps only queued and running entries.
func (s *server) listCommands(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	activeOnly := r.URL.Query().Get("active") != ""
	out := []fleet.Command{}
	for _, c := range s.be.Commands() {
		if mode != "" && c.Mode != mode {
			continue
		}
		if activeOnly && c.State == "finished" {
			continue
		}
		out = append(out, c)
	}
	writeJSON(w, http.StatusOK, commandsResponse{Commands: out})
}

type submitBody struct {
	Machines   []string `json:"machines"`
	Command    string   `json:"command"`
	TimeoutSec int      `json:"timeout_sec"`
	Confirm    bool     `json:"confirm"`
}

// submitResult is one machine's outcome in a fan-out submission.
type submitResult struct {
	Machine string `json:"machine"`
	Session string `json:"session,omitempty"`
	Error   string `json:"error,omitempty"`
}

type submitResponse struct {
	Results []submitResult `json:"results"`
}

// gateResponse is the 409 body when a dangerous command needs the operator's
// confirmation first. Nothing has been submitted when it is returned.
type gateResponse struct {
	Error             string   `json:"error"`
	NeedsConfirmation bool     `json:"needs_confirmation"`
	Pattern           string   `json:"pattern"`
	Command           string   `json:"command"`
	Machines          []string `json:"machines"`
}

func (s *server) submitCommands(w http.ResponseWriter, r *http.Request) {
	var b submitBody
	if !decode(w, r, &b) {
		return
	}
	if len(b.Machines) == 0 {
		writeJSON(w, http.StatusBadRequest, errBody("choose at least one worker agent"))
		return
	}
	if strings.TrimSpace(b.Command) == "" {
		writeJSON(w, http.StatusBadRequest, errBody("the command is empty"))
		return
	}
	// The dangerous-command gate: if any target needs confirmation and the
	// operator has not given it, nothing is submitted anywhere. The agent
	// enforces the gate regardless; this is the UX half.
	if !b.Confirm {
		var gate *gateResponse
		for _, m := range b.Machines {
			var cr *fleet.ConfirmRequiredError
			if err := s.be.Preflight(m, b.Command); errors.As(err, &cr) {
				if gate == nil {
					gate = &gateResponse{Error: cr.Error(), NeedsConfirmation: true, Pattern: cr.Pattern, Command: b.Command}
				}
				gate.Machines = append(gate.Machines, m)
			}
		}
		if gate != nil {
			s.log.Warn("dangerous command awaiting confirmation", "component", "panel", "operator", operatorFrom(r), "command", b.Command, "machines", gate.Machines)
			writeJSON(w, http.StatusConflict, gate)
			return
		}
	}
	user := operatorFrom(r)
	resp := submitResponse{Results: make([]submitResult, 0, len(b.Machines))}
	var firstErr error
	submitted := 0
	for _, m := range b.Machines {
		id, err := s.be.Submit(fleet.SubmitRequest{Machine: m, Command: b.Command, User: user, TimeoutSec: b.TimeoutSec, Confirm: b.Confirm})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			resp.Results = append(resp.Results, submitResult{Machine: m, Error: err.Error()})
			continue
		}
		submitted++
		resp.Results = append(resp.Results, submitResult{Machine: m, Session: id})
	}
	if b.Confirm {
		s.log.Warn("dangerous command confirmed", "component", "panel", "operator", user, "command", b.Command, "machines", b.Machines)
	}
	status := http.StatusAccepted
	if submitted == 0 {
		status = statusFor(firstErr)
	}
	writeJSON(w, status, resp)
}

type commandResponse struct {
	Command fleet.Command         `json:"command"`
	Output  []fleet.OutputSegment `json:"output"`
}

func (s *server) getCommand(w http.ResponseWriter, r *http.Request) {
	c, out, ok := s.be.Command(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("unknown session"))
		return
	}
	if out == nil {
		out = []fleet.OutputSegment{}
	}
	writeJSON(w, http.StatusOK, commandResponse{Command: c, Output: out})
}

func (s *server) cancelCommand(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	id := r.PathValue("id")
	s.log.Info("cancel session", "component", "panel", "operator", operatorFrom(r), "session", id)
	if err := s.be.Cancel(ctx, id); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"cancelled": id})
}
