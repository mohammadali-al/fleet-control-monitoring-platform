// Package enroll is token enrollment (T-0026): an SSH-free, firewall-friendly
// way to add a worker or monitor machine. The operator mints a one-time token
// in the panel; a person at the machine runs one `curl ... | sudo bash`; the
// machine generates its own key, sends only a certificate request with the
// token, receives a certificate signed by the internal CA, installs a permanent
// service and dials OUT to the main server (agent_dial). The private key never
// leaves the machine.
//
// Two HTTP surfaces, deliberately separate:
//
//   - Handler (server.go) is the enrollment endpoint. It is served on its own
//     TLS listener that bypasses the operator auth proxy: the token is the only
//     authentication there. It verifies its own TLS (the one-liner pins its
//     public key) so the pipe into bash cannot be intercepted.
//   - APIHandler (api.go) is the operator side the Machines page calls (mint a
//     token, show the script, list, Stop, Detach). It sits behind the proxy,
//     like the rest of the panel.
//
// A token is never logged; the short code is a credential too and is not
// logged either.
package enroll

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// DefaultCertValidity is the lifetime of an enrolled machine's certificate.
const DefaultCertValidity = 365 * 24 * time.Hour

// Errors the panel maps to HTTP statuses.
var (
	// ErrStopUnsupported is returned (by a Runner) when the machine cannot be
	// told to stop over the control channel, e.g. a monitor agent, which has no
	// command role. The service can still be stopped on the machine itself.
	ErrStopUnsupported = errors.New("enroll: this agent cannot be stopped remotely")
	ErrBadRequest      = errors.New("enroll: bad request")
	ErrNotEnrolled     = errors.New("enroll: machine is not a token-enrolled agent")
)

// Runner runs a shell command as root on an enrolled machine through the
// control channel (the fleet manager's command queue in production). It is how
// Stop reaches the machine.
type Runner interface {
	Run(ctx context.Context, machineID, command string) error
}

// Config configures a Service.
type Config struct {
	CA       *pki.CA
	Registry *registry.Registry
	// Persist saves the registry (registry.Store.Save); nil means no
	// persistence (tests).
	Persist func() error
	// Reconcile tells the fleet manager the registry changed so it starts or
	// stops the machine's link (fleet.Manager.Reconcile); nil does nothing.
	Reconcile func()
	// Runner executes Stop on the machine; nil makes Stop report that remote
	// stop is unavailable (unless ServiceControl is set).
	Runner Runner
	// ServiceControl sends a service-control action (stop | start | status) to
	// an agent of any role over the control channel. When set, Stop and Start
	// use it instead of Runner.
	ServiceControl func(ctx context.Context, machineID, action string) error
	// Binary returns the path of the agent binary for an OS and architecture
	// (see BinaryDir). Nil serves no binary.
	Binary func(os, arch string) (path string, err error)

	// PublicHost is the host name or IP address machines reach this server at;
	// EnrollPort is the enrollment endpoint's port, AgentDialPort the
	// agent_dial listener's. ServerID is the main server's channel identity.
	PublicHost    string
	EnrollPort    int
	AgentDialPort int
	ServerID      string

	TokenTTL     time.Duration // default 10 minutes
	CertValidity time.Duration // default one year
	Logger       *slog.Logger
	Now          func() time.Time
}

// Service is token enrollment: the token store, the CSR exchange, the machine
// lifecycle (Stop, Detach) and the HTTP handlers.
type Service struct {
	cfg    Config
	log    *slog.Logger
	tokens *tokenStore
	lim    *limiter

	// The enrollment endpoint's own TLS identity, issued by the CA; Pin is
	// what the one-liner pins.
	tlsCert tls.Certificate
	pin     string
	caFP    string

	mu sync.Mutex // serialises registry changes made here
}

// New builds a Service and issues the enrollment endpoint's TLS certificate.
func New(cfg Config) (*Service, error) {
	if cfg.CA == nil || cfg.Registry == nil {
		return nil, errors.New("enroll: a CA and a registry are required")
	}
	if cfg.PublicHost == "" || cfg.EnrollPort <= 0 || cfg.AgentDialPort <= 0 {
		return nil, errors.New("enroll: public host, enrollment port and agent_dial port are required")
	}
	if cfg.ServerID == "" {
		cfg.ServerID = config.DefaultServerID
	}
	if cfg.CertValidity <= 0 {
		cfg.CertValidity = DefaultCertValidity
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Service{cfg: cfg, log: cfg.Logger, tokens: newTokenStore(cfg.Now, cfg.TokenTTL), lim: newLimiter(cfg.Now)}

	id, ips := "fleet-enroll", []net.IP(nil)
	if ip := net.ParseIP(cfg.PublicHost); ip != nil {
		ips = []net.IP{ip}
	} else {
		id = cfg.PublicHost
	}
	certPEM, keyPEM, err := cfg.CA.IssuePeer(id, ips, 365*24*time.Hour)
	if err != nil {
		return nil, fmt.Errorf("enroll: issue the endpoint certificate: %w", err)
	}
	if s.tlsCert, err = tls.X509KeyPair(certPEM, keyPEM); err != nil {
		return nil, fmt.Errorf("enroll: endpoint key pair: %w", err)
	}
	leaf, err := x509.ParseCertificate(s.tlsCert.Certificate[0])
	if err != nil {
		return nil, err
	}
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	s.pin = base64.StdEncoding.EncodeToString(spki[:])
	caBlock, _ := pem.Decode(cfg.CA.CertPEM())
	if caBlock == nil {
		return nil, errors.New("enroll: the CA has no certificate")
	}
	ca := sha256.Sum256(caBlock.Bytes)
	s.caFP = hex.EncodeToString(ca[:])
	return s, nil
}

// BaseURL is the enrollment endpoint's URL as machines reach it.
func (s *Service) BaseURL() string {
	return "https://" + net.JoinHostPort(s.cfg.PublicHost, fmt.Sprint(s.cfg.EnrollPort))
}

// Pin is the base64 SHA-256 of the endpoint's public key.
func (s *Service) Pin() string { return s.pin }

func (s *Service) serverAddr() string {
	return net.JoinHostPort(s.cfg.PublicHost, fmt.Sprint(s.cfg.AgentDialPort))
}

func (s *Service) scriptParams(token, role string) ScriptParams {
	return ScriptParams{
		BaseURL: s.BaseURL(), Pin: s.pin, CASHA256: s.caFP, Token: token, Role: role,
		ServerAddr: s.serverAddr(), ServerID: s.cfg.ServerID,
	}
}

// Minted is what the panel shows after "Add agent (token)".
type Minted struct {
	Token Token
	// Linux and MacOS are the one-liners; MacOS is empty for a worker (macOS
	// machines can only be monitors).
	Linux, MacOS string
}

// Mint creates a one-time, single-use token (valid for the configured TTL,
// 10 minutes by default) for a machine that will enroll as role, with its
// short code and the install one-liners. by names the operator, for the log.
func (s *Service) Mint(role, by string) (Minted, error) {
	if err := CheckRoleOS(role, ""); err != nil {
		return Minted{}, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	t, err := s.tokens.mint(role, by)
	if err != nil {
		return Minted{}, err
	}
	m := Minted{Token: t, Linux: OneLiner(s.BaseURL(), s.pin, t.Code)}
	if role == config.RoleMonitor {
		m.MacOS = m.Linux
	}
	s.log.Info("enrollment token minted", "component", "enroll", "role", role, "by", by, "expires", t.ExpiresAt.Format(time.RFC3339))
	return m, nil
}

// Script renders the bootstrap script for role: the read-only "show script"
// view when token is empty.
func (s *Service) Script(role, token string) (string, error) {
	if err := CheckRoleOS(role, ""); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return RenderScript(s.scriptParams(token, role)), nil
}

// Request is one machine's enrollment: the token, its certificate request and
// what it reports about itself.
type Request struct {
	Token  string // the long secret; the short code cannot redeem
	CSRPEM []byte
	// What the machine reports (untrusted, informational).
	Hostname, OS, OSVersion, Kernel, Arch string
	CPUs                                  int
	MemBytes                              uint64
	// RemoteIP is where the request came from (recorded as the address).
	RemoteIP string
}

// Result is what the machine gets back.
type Result struct {
	MachineID   string
	CertPEM     []byte
	Fingerprint string
	Role        string
}

// Enroll redeems the token with a machine's request: it signs the machine's
// public key (bound to the id the server assigns; the request's own subject is
// ignored), records the machine and its specs in the registry, and consumes
// the token. A failed request (bad CSR, macOS worker, unknown or used token)
// issues nothing and registers nothing.
func (s *Service) Enroll(req Request) (Result, error) {
	var res Result
	err := s.tokens.redeem(req.Token, func(t Token) (string, error) {
		os := strings.ToLower(strings.TrimSpace(req.OS))
		if os != OSLinux && os != OSDarwin {
			return "", fmt.Errorf("%w: unsupported operating system %q (linux and macOS only)", ErrBadRequest, clipText(req.OS, 20))
		}
		if err := CheckRoleOS(t.Role, os); err != nil {
			return "", err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		id := s.pickID(req.Hostname)
		certPEM, err := s.cfg.CA.SignCSR(req.CSRPEM, id, nil, s.cfg.CertValidity)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrBadRequest, err)
		}
		fp, err := fingerprint(certPEM)
		if err != nil {
			return "", err
		}
		now := s.cfg.Now()
		m := registry.Machine{
			ID: id, Role: t.Role, ConnectMode: registry.ConnectModeAgentDial,
			CertFingerprint: fp, Provisioned: true, EnrolledVia: registry.EnrolledToken,
			Specs: &registry.Specs{
				Hostname: clipText(req.Hostname, 100), OS: os, OSVersion: clipText(req.OSVersion, 100),
				Kernel: clipText(req.Kernel, 100), Arch: clipText(req.Arch, 20), CPUs: req.CPUs, MemBytes: req.MemBytes,
			},
		}
		if ip := net.ParseIP(req.RemoteIP); ip != nil {
			if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				m.PrivateIPs = []string{ip.String()}
			} else {
				m.PublicIP, m.PublicIPAt = ip.String(), &now
			}
		}
		if err := s.cfg.Registry.Add(m); err != nil {
			return "", err
		}
		if err := s.persist(); err != nil {
			_ = s.cfg.Registry.Remove(id) // not recorded durably: do not hand out the identity
			return "", err
		}
		res = Result{MachineID: id, CertPEM: certPEM, Fingerprint: fp, Role: t.Role}
		return id, nil
	})
	if err != nil {
		return Result{}, err
	}
	s.reconcile()
	s.log.Info("machine enrolled", "component", "enroll", "machine", res.MachineID, "role", res.Role)
	return res, nil
}

// pickID derives a machine id from the reported host name: lower-case letters,
// digits, '.', '_' and '-', unique among every registry entry (a detached
// machine's id is never reused, so an old certificate can never match a new
// machine). s.mu is held.
func (s *Service) pickID(hostname string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(hostname) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ':
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), ".-_")
	if len(base) > 48 {
		base = base[:48]
	}
	if base == "" {
		var raw [3]byte
		_, _ = rand.Read(raw[:])
		base = "agent-" + hex.EncodeToString(raw[:])
	}
	id := base
	for n := 2; ; n++ {
		if _, taken := s.cfg.Registry.Get(id); !taken {
			return id
		}
		id = fmt.Sprintf("%s-%d", base, n)
	}
}

func fingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("enroll: no PEM certificate")
	}
	sum := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(sum[:]), nil
}

func clipText(s string, n int) string {
	var b strings.Builder
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			continue // no control characters from an untrusted machine
		}
		b.WriteRune(r)
	}
	out := b.String()
	if len(out) > n {
		out = out[:n]
	}
	return out
}

func (s *Service) persist() error {
	if s.cfg.Persist == nil {
		return nil
	}
	return s.cfg.Persist()
}

func (s *Service) reconcile() {
	if s.cfg.Reconcile != nil {
		s.cfg.Reconcile()
	}
}

// Machine is an enrolled agent as the Machines page lists it.
type Machine struct {
	registry.Machine
	// Online is the channel's live state (the registry type hides it from JSON).
	Online bool `json:"online"`
	// Stopped is true while the operator-stopped agent is down (it is cleared
	// as soon as the agent is back online, e.g. after a local `fleet-agent start`).
	Stopped bool `json:"stopped"`
}

// Machines lists the token-enrolled machines that are still attached.
func (s *Service) Machines() []Machine {
	var out []Machine
	for _, m := range s.cfg.Registry.List() {
		if m.EnrolledVia != registry.EnrolledToken || m.Removed {
			continue
		}
		stopped := m.Stopped && !m.Online
		out = append(out, Machine{Machine: m, Online: m.Online, Stopped: stopped})
	}
	return out
}

// Stop disables the machine's service so it stays down across a reboot (the
// machine's own `fleet-agent start` or remote Start re-enables it). The machine
// stays registered.
func (s *Service) Stop(ctx context.Context, id string) error {
	m, err := s.attached(id)
	if err != nil {
		return err
	}
	if s.cfg.ServiceControl != nil {
		if err := s.cfg.ServiceControl(ctx, id, "stop"); err != nil {
			return err
		}
	} else {
		if s.cfg.Runner == nil {
			return ErrStopUnsupported
		}
		os := OSLinux
		if m.Specs != nil && m.Specs.OS != "" {
			os = m.Specs.OS
		}
		if err := s.cfg.Runner.Run(ctx, id, StopCommand(os)); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.Registry.Update(id, func(m *registry.Machine) { m.Stopped = true }); err != nil {
		return err
	}
	s.log.Info("agent stopped", "component", "enroll", "machine", id)
	return s.persist()
}

// Start re-enables and starts the machine's service over the control channel.
func (s *Service) Start(ctx context.Context, id string) error {
	if _, err := s.attached(id); err != nil {
		return err
	}
	if s.cfg.ServiceControl == nil {
		return ErrStopUnsupported
	}
	if err := s.cfg.ServiceControl(ctx, id, "start"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cfg.Registry.Update(id, func(m *registry.Machine) { m.Stopped = false }); err != nil {
		return err
	}
	s.log.Info("agent started", "component", "enroll", "machine", id)
	return s.persist()
}

// Detach revokes the machine's client certificate and forgets the machine: the
// registry entry is kept only as a removed record carrying the revoked
// fingerprint (its id is never issued again, so the old certificate can match
// no machine), the manager drops its link and the channel listener refuses its
// certificate from then on. Re-adding the machine needs a fresh token.
func (s *Service) Detach(_ context.Context, id string) error {
	if _, err := s.attached(id); err != nil {
		return err
	}
	s.mu.Lock()
	now := s.cfg.Now()
	err := s.cfg.Registry.Update(id, func(m *registry.Machine) {
		m.Removed, m.RemovedAt = true, &now
		m.Provisioned, m.Online, m.Stopped = false, false, false
		m.RevokedFingerprint = m.CertFingerprint
	})
	if err == nil {
		err = s.persist()
	}
	s.mu.Unlock()
	s.reconcile() // even on a persist error: the in-memory revocation must take effect
	if err != nil {
		return err
	}
	s.log.Info("agent detached, certificate revoked", "component", "enroll", "machine", id)
	return nil
}

// Revoked reports whether cert belongs to a detached machine.
func (s *Service) Revoked(cert *x509.Certificate) bool {
	sum := sha256.Sum256(cert.Raw)
	fp := hex.EncodeToString(sum[:])
	for _, m := range s.cfg.Registry.List() {
		if m.Removed && m.RevokedFingerprint == fp {
			return true
		}
	}
	return false
}

func (s *Service) attached(id string) (registry.Machine, error) {
	m, ok := s.cfg.Registry.Get(id)
	if !ok {
		return registry.Machine{}, fmt.Errorf("%w: %q", registry.ErrNotFound, id)
	}
	if m.EnrolledVia != registry.EnrolledToken || m.Removed {
		return registry.Machine{}, fmt.Errorf("%w: %q", ErrNotEnrolled, id)
	}
	return m, nil
}
