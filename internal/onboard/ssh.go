// Package onboard brings a new machine online over SSH (architecture 4.5):
// create a dedicated system user, install the fleet-agent binary and its
// systemd unit, issue the machine's client certificate (internal/pki), write
// the pinned CA and the connect_mode flag into the agent config, then enable
// and start the service. Re-provisioning the same machine is idempotent: the
// ID and identity are reused and a new cert is issued only if missing.
//
// The flow is a list of small steps (see steps / the step* methods), each
// running plain commands through the Remote interface, so the real install can
// be tuned later without a rewrite and the whole flow can be tested against a
// local in-process SSH server.
package onboard

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// Step names, in the order Provision runs them (Result.Steps).
const (
	StepEnsureUser    = "ensure-user"
	StepCreateDirs    = "create-dirs"
	StepInstallBinary = "install-binary"
	StepIdentity      = "identity"
	StepWriteCA       = "write-ca"
	StepWriteConfig   = "write-config"
	StepInstallUnit   = "install-unit"
	StepEnableStart   = "enable-start"
)

// ErrAgentBinaryMissing is returned (wrapped) when no usable fleet-agent
// binary is configured.
var ErrAgentBinaryMissing = errors.New("onboard: fleet-agent binary not available")

// Layout is where things go on the target machine.
type Layout struct {
	ServiceUser string // dedicated system user and group
	Binary      string // installed fleet-agent binary
	ConfigDir   string
	StateDir    string
	UnitName    string
	UnitPath    string
}

// DefaultLayout is the standard Linux/systemd layout.
func DefaultLayout() Layout {
	return Layout{
		ServiceUser: "fleet-agent",
		Binary:      "/usr/local/bin/fleet-agent",
		ConfigDir:   "/etc/fleet-agent",
		StateDir:    "/var/lib/fleet-agent",
		UnitName:    "fleet-agent.service",
		UnitPath:    "/etc/systemd/system/fleet-agent.service",
	}
}

func (l Layout) configFile() string { return l.ConfigDir + "/config.json" }
func (l Layout) caFile() string     { return l.ConfigDir + "/ca.crt" }
func (l Layout) certFile() string   { return l.ConfigDir + "/agent.crt" }
func (l Layout) keyFile() string    { return l.ConfigDir + "/agent.key" }

// Auth is how to log in over SSH: a password or a private key (PEM).
type Auth struct {
	User          string
	Password      string
	PrivateKeyPEM []byte
}

func (a Auth) methods() ([]ssh.AuthMethod, error) {
	var m []ssh.AuthMethod
	if len(a.PrivateKeyPEM) > 0 {
		signer, err := ssh.ParsePrivateKey(a.PrivateKeyPEM)
		if err != nil {
			return nil, fmt.Errorf("onboard: parse SSH private key: %w", err)
		}
		m = append(m, ssh.PublicKeys(signer))
	}
	if a.Password != "" {
		m = append(m, ssh.Password(a.Password))
	}
	if len(m) == 0 {
		return nil, errors.New("onboard: no SSH credentials (need a password or a private key)")
	}
	return m, nil
}

// Target describes the machine to onboard.
type Target struct {
	Host string // IP address of the machine (it becomes a cert SAN)
	Port int    // SSH port, default 22
	Auth
	Role        string // worker|monitor
	ConnectMode string // server_dial|agent_dial; empty keeps the machine's mode, default server_dial
	// MachineID forces an ID (new or existing). Empty: reuse the machine
	// already registered for Host, else generate a fresh one.
	MachineID string
	// HostKeyFingerprint pins the SSH host key ("SHA256:..."). Empty falls
	// back to the fingerprint stored for a known machine, else trust-on-first-
	// use: the observed key is recorded in the registry and pinned afterwards.
	HostKeyFingerprint string
}

func (t Target) addr() string {
	port := t.Port
	if port == 0 {
		port = 22
	}
	return net.JoinHostPort(t.Host, strconv.Itoa(port))
}

// Remote runs commands on the target. stdin may be nil. Non-zero exit is a
// *RemoteError. It must never be handed secrets in cmd (use stdin).
type Remote interface {
	Run(ctx context.Context, cmd string, stdin []byte) ([]byte, error)
	Close() error
}

// RemoteError is a command that exited non-zero.
type RemoteError struct {
	Cmd    string
	Code   int
	Output string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("remote command %q exited %d: %s", e.Cmd, e.Code, strings.TrimSpace(e.Output))
}

// DialFunc opens a Remote and reports the SSH host key fingerprint it saw.
type DialFunc func(ctx context.Context, t Target, pinnedHostKey string) (Remote, string, error)

// Provisioner onboards machines. The zero value is not usable; set CA,
// Registry and AgentBinaryPath (Store is optional but normally set).
type Provisioner struct {
	CA       *pki.CA
	Registry *registry.Registry
	// Store, if set, receives the registry after every change.
	Store *registry.Store
	// AgentBinaryPath is the fleet-agent binary to install (from config,
	// ServerConfig.AgentBinaryPath).
	AgentBinaryPath string
	// CertValidity is the client cert lifetime (default 1 year).
	CertValidity time.Duration
	// Layout overrides DefaultLayout when ServiceUser is set.
	Layout Layout
	// Dial overrides the real SSH dialer (tests).
	Dial DialFunc
	// DialTimeout bounds the SSH connect (default 15s).
	DialTimeout time.Duration

	mu  sync.Mutex // one provisioning/removal at a time
	now func() time.Time
}

// Result describes a completed Provision.
type Result struct {
	Machine    registry.Machine
	Steps      []string // steps run, in order
	Created    bool     // a new registry entry was made (vs re-provision)
	CertIssued bool     // a new client cert was issued and installed
}

func (p *Provisioner) layout() Layout {
	if p.Layout.ServiceUser == "" {
		return DefaultLayout()
	}
	return p.Layout
}

func (p *Provisioner) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// job is the state shared by the steps of one Provision run.
type job struct {
	p      *Provisioner
	t      Target
	l      Layout
	rem    Remote
	m      registry.Machine
	binary []byte

	forceCert bool   // issue a fresh cert even if the remote one looks fine
	certPEM   []byte // set when a new cert was issued
	keyPEM    []byte
	certFP    string
}

type step struct {
	name string
	run  func(ctx context.Context, j *job) error
}

func steps() []step {
	return []step{
		{StepEnsureUser, stepEnsureUser},
		{StepCreateDirs, stepCreateDirs},
		{StepInstallBinary, stepInstallBinary},
		{StepIdentity, stepIdentity},
		{StepWriteCA, stepWriteCA},
		{StepWriteConfig, stepWriteConfig},
		{StepInstallUnit, stepInstallUnit},
		{StepEnableStart, stepEnableStart},
	}
}

// Provision onboards the target, or re-provisions it idempotently.
func (p *Provisioner) Provision(ctx context.Context, t Target) (*Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.CA == nil || p.Registry == nil {
		return nil, errors.New("onboard: provisioner needs a CA and a registry")
	}
	if err := validateTarget(&t); err != nil {
		return nil, err
	}
	// Fail on a missing agent binary before touching the registry or the network.
	binary, err := loadAgentBinary(p.AgentBinaryPath)
	if err != nil {
		return nil, err
	}

	m, created, err := p.resolveMachine(t)
	if err != nil {
		return nil, err
	}
	// Record the machine before changing anything remotely, so a failed run
	// is retried against the same identity.
	if err := p.persist(); err != nil {
		return nil, err
	}

	pinned := t.HostKeyFingerprint
	if pinned == "" {
		pinned = m.SSHHostKey
	}
	dial := p.Dial
	if dial == nil {
		dial = p.dialSSH
	}
	rem, hostKey, err := dial(ctx, t, pinned)
	if err != nil {
		return nil, fmt.Errorf("onboard: connect to %s: %w", t.addr(), err)
	}
	defer rem.Close()

	j := &job{p: p, t: t, l: p.layout(), rem: rem, m: m, binary: binary, forceCert: m.Removed}
	res := &Result{Created: created}
	for _, s := range steps() {
		if err := s.run(ctx, j); err != nil {
			return nil, fmt.Errorf("onboard: machine %s: step %s: %w", m.ID, s.name, err)
		}
		res.Steps = append(res.Steps, s.name)
	}
	res.CertIssued = j.certPEM != nil

	err = p.Registry.Update(m.ID, func(m *registry.Machine) {
		m.Provisioned = true
		m.Removed = false
		m.RemovedAt = nil
		m.CertFingerprint = j.certFP
		if hostKey != "" {
			m.SSHHostKey = hostKey
		}
	})
	if err != nil {
		return nil, err
	}
	if err := p.persist(); err != nil {
		return nil, err
	}
	res.Machine, _ = p.Registry.Get(m.ID)
	return res, nil
}

func validateTarget(t *Target) error {
	switch t.Role {
	case registry.RoleWorker, registry.RoleMonitor:
	default:
		return fmt.Errorf("onboard: invalid role %q (must be %q or %q)", t.Role, registry.RoleWorker, registry.RoleMonitor)
	}
	switch t.ConnectMode {
	case "", registry.ConnectModeServerDial, registry.ConnectModeAgentDial:
	default:
		return fmt.Errorf("onboard: invalid connect_mode %q", t.ConnectMode)
	}
	if net.ParseIP(t.Host) == nil {
		return fmt.Errorf("onboard: host %q is not an IP address", t.Host)
	}
	if t.User == "" {
		return errors.New("onboard: SSH user is required")
	}
	if t.Password == "" && len(t.PrivateKeyPEM) == 0 {
		return errors.New("onboard: no SSH credentials (need a password or a private key)")
	}
	return nil
}

// loadAgentBinary reads the configured fleet-agent binary.
func loadAgentBinary(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: no agent binary path configured (set server.agent_binary_path)", ErrAgentBinaryMissing)
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrAgentBinaryMissing, path, err)
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return nil, fmt.Errorf("%w: %s is not a non-empty regular file", ErrAgentBinaryMissing, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrAgentBinaryMissing, path, err)
	}
	return data, nil
}

// resolveMachine finds or creates the registry entry for the target and
// applies its role / connect mode. It does not save.
func (p *Provisioner) resolveMachine(t Target) (registry.Machine, bool, error) {
	id := t.MachineID
	if id == "" {
		for _, m := range p.Registry.List() {
			for _, ip := range m.PrivateIPs {
				if sameIP(ip, t.Host) {
					id = m.ID
				}
			}
		}
	}
	if id == "" {
		var err error
		if id, err = newMachineID(); err != nil {
			return registry.Machine{}, false, err
		}
	}

	if _, ok := p.Registry.Get(id); !ok {
		err := p.Registry.Add(registry.Machine{
			ID: id, Role: t.Role, ConnectMode: t.ConnectMode, PrivateIPs: []string{t.Host},
		})
		if err != nil {
			return registry.Machine{}, false, err
		}
		m, _ := p.Registry.Get(id)
		return m, true, nil
	}
	err := p.Registry.Update(id, func(m *registry.Machine) {
		m.Role = t.Role
		if t.ConnectMode != "" {
			m.ConnectMode = t.ConnectMode
		}
		if !containsIP(m.PrivateIPs, t.Host) {
			m.PrivateIPs = append(m.PrivateIPs, t.Host)
		}
	})
	if err != nil {
		return registry.Machine{}, false, err
	}
	m, _ := p.Registry.Get(id)
	return m, false, nil
}

func sameIP(a, b string) bool {
	ia, ib := net.ParseIP(a), net.ParseIP(b)
	return ia != nil && ib != nil && ia.Equal(ib)
}

func containsIP(list []string, ip string) bool {
	for _, s := range list {
		if sameIP(s, ip) {
			return true
		}
	}
	return false
}

func newMachineID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("onboard: generate machine id: %w", err)
	}
	return "m-" + hex.EncodeToString(b[:]), nil
}

func (p *Provisioner) persist() error {
	if p.Store == nil {
		return nil
	}
	return p.Store.Save(p.Registry)
}

// --- steps -----------------------------------------------------------------

// sudoPrefix is empty for root, else non-interactive sudo.
func (j *job) sudoPrefix() string {
	if j.t.User == "root" {
		return ""
	}
	return "sudo -n "
}

// priv runs a command with privileges.
func (j *job) priv(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	return j.rem.Run(ctx, j.sudoPrefix()+joinQuoted(args), stdin)
}

// writeFile installs content at path with the given mode and owner.
func (j *job) writeFile(ctx context.Context, path, mode, owner string, content []byte) error {
	_, err := j.priv(ctx, content, "install", "-m", mode, "-o", owner, "-g", owner, "/dev/stdin", path)
	return err
}

func stepEnsureUser(ctx context.Context, j *job) error {
	u := j.l.ServiceUser
	_, err := j.rem.Run(ctx, joinQuoted([]string{"getent", "passwd", u}), nil)
	if err == nil {
		return nil // already there
	}
	var re *RemoteError
	if !errors.As(err, &re) {
		return err // transport failure, not "no such user"
	}
	_, err = j.priv(ctx, nil, "useradd", "--system", "--no-create-home",
		"--home-dir", j.l.StateDir, "--shell", "/usr/sbin/nologin", u)
	return err
}

func stepCreateDirs(ctx context.Context, j *job) error {
	u := j.l.ServiceUser
	for _, d := range []string{j.l.ConfigDir, j.l.StateDir} {
		if _, err := j.priv(ctx, nil, "install", "-d", "-m", "0750", "-o", u, "-g", u, d); err != nil {
			return err
		}
	}
	return nil
}

func stepInstallBinary(ctx context.Context, j *job) error {
	return j.writeFile(ctx, j.l.Binary, "0755", "root", j.binary)
}

// stepIdentity keeps the machine's existing client cert when the remote copy
// is still the registered one and valid; otherwise it issues a new one.
func stepIdentity(ctx context.Context, j *job) error {
	if !j.forceCert && j.remoteCertUsable(ctx) {
		return nil
	}
	ips := make([]net.IP, 0, len(j.m.PrivateIPs))
	for _, s := range j.m.PrivateIPs {
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
		}
	}
	validity := j.p.CertValidity
	if validity == 0 {
		validity = 365 * 24 * time.Hour
	}
	certPEM, keyPEM, err := j.p.CA.IssuePeer(j.m.ID, ips, validity)
	if err != nil {
		return err
	}
	fp, err := CertFingerprint(certPEM)
	if err != nil {
		return err
	}
	u := j.l.ServiceUser
	if err := j.writeFile(ctx, j.l.keyFile(), "0600", u, keyPEM); err != nil {
		return err
	}
	if err := j.writeFile(ctx, j.l.certFile(), "0644", u, certPEM); err != nil {
		return err
	}
	j.certPEM, j.keyPEM, j.certFP = certPEM, keyPEM, fp
	return nil
}

// remoteCertUsable reports whether the machine already has the cert the
// registry knows about (same fingerprint, this ID, not near expiry) and a key.
func (j *job) remoteCertUsable(ctx context.Context) bool {
	if j.m.CertFingerprint == "" {
		return false
	}
	out, err := j.rem.Run(ctx, joinQuoted([]string{"cat", j.l.certFile()}), nil)
	if err != nil {
		return false
	}
	block, _ := pem.Decode(out)
	if block == nil {
		return false
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	fp := fingerprintDER(block.Bytes)
	if fp != j.m.CertFingerprint || cert.Subject.CommonName != j.m.ID {
		return false
	}
	if j.p.clock().Add(30 * 24 * time.Hour).After(cert.NotAfter) {
		return false
	}
	if _, err := j.priv(ctx, nil, "test", "-s", j.l.keyFile()); err != nil {
		return false
	}
	j.certFP = fp
	return true
}

func stepWriteCA(ctx context.Context, j *job) error {
	return j.writeFile(ctx, j.l.caFile(), "0644", "root", j.p.CA.CertPEM())
}

func stepWriteConfig(ctx context.Context, j *job) error {
	data, err := renderAgentConfig(j.m, j.l)
	if err != nil {
		return err
	}
	return j.writeFile(ctx, j.l.configFile(), "0640", j.l.ServiceUser, data)
}

func stepInstallUnit(ctx context.Context, j *job) error {
	if err := j.writeFile(ctx, j.l.UnitPath, "0644", "root", []byte(renderUnit(j.l))); err != nil {
		return err
	}
	_, err := j.priv(ctx, nil, "systemctl", "daemon-reload")
	return err
}

func stepEnableStart(ctx context.Context, j *job) error {
	if _, err := j.priv(ctx, nil, "systemctl", "enable", j.l.UnitName); err != nil {
		return err
	}
	// restart (not start) so a re-provision picks up new config or binary.
	_, err := j.priv(ctx, nil, "systemctl", "restart", j.l.UnitName)
	return err
}

// --- rendering ---------------------------------------------------------------

// renderAgentConfig builds the agent's config file: role, machine id, the
// connect_mode flag and the paths of the pinned CA and the client cert/key.
// It uses config.AgentConfig, so the agent's own loader accepts it.
func renderAgentConfig(m registry.Machine, l Layout) ([]byte, error) {
	doc := struct {
		Agent config.AgentConfig `json:"agent"`
	}{Agent: config.AgentConfig{
		Role:        m.Role,
		MachineID:   m.ID,
		ConnectMode: m.ConnectMode,
		CAFile:      l.caFile(),
		CertFile:    l.certFile(),
		KeyFile:     l.keyFile(),
	}}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("onboard: render agent config: %w", err)
	}
	return append(data, '\n'), nil
}

// renderUnit builds the systemd unit. The agent runs as root (architecture
// section 5): evidence capture, PTY-as-root, cgroup tracking and the watchdog
// all need it, so the unit does not drop to the dedicated user. The same unit
// is used for the worker and the monitor profile; the headless monitor runs as
// root too, for simplicity. The dedicated service user is kept only to own the
// config and state directories. The data dir is set explicitly through
// FLEET_DATA_DIR so evidence and registry state land in StateDir instead of
// depending on the binary's location (D-0006).
func renderUnit(l Layout) string {
	return fmt.Sprintf(`[Unit]
Description=Fleet agent
After=network-online.target
Wants=network-online.target

[Service]
User=root
Group=root
Environment=FLEET_DATA_DIR=%[3]s
ExecStart=%[1]s --config %[2]s
WorkingDirectory=%[3]s
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
`, l.Binary, l.configFile(), l.StateDir)
}

// CertFingerprint is the lowercase hex SHA-256 of the first certificate in a
// PEM block (the value stored in registry.Machine.CertFingerprint).
func CertFingerprint(certPEM []byte) (string, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return "", errors.New("onboard: no PEM certificate")
	}
	return fingerprintDER(block.Bytes), nil
}

func fingerprintDER(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

var safeArg = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

func shQuote(s string) string {
	if safeArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func joinQuoted(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shQuote(a)
	}
	return strings.Join(q, " ")
}

// --- removal -----------------------------------------------------------------

// RemoveOptions tunes Remove.
type RemoveOptions struct {
	Port int // SSH port, default 22
	// SkipRemote decommissions in the registry only (machine unreachable or
	// already wiped); no SSH connection is made.
	SkipRemote bool
	// HostKeyFingerprint overrides the pinned host key.
	HostKeyFingerprint string
}

// Remove decommissions a machine: stop and disable its service over SSH, then
// mark it removed in the registry and record the intent to revoke its cert
// (RevokedFingerprint). The record is kept; use Registry.Remove to delete it.
func (p *Provisioner) Remove(ctx context.Context, id string, auth Auth, opts RemoveOptions) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.Registry == nil {
		return errors.New("onboard: provisioner needs a registry")
	}
	m, ok := p.Registry.Get(id)
	if !ok {
		return fmt.Errorf("%w: %q", registry.ErrNotFound, id)
	}
	if !opts.SkipRemote {
		if len(m.PrivateIPs) == 0 {
			return fmt.Errorf("onboard: machine %s has no known address (use SkipRemote)", id)
		}
		t := Target{Host: m.PrivateIPs[0], Port: opts.Port, Auth: auth}
		pinned := opts.HostKeyFingerprint
		if pinned == "" {
			pinned = m.SSHHostKey
		}
		dial := p.Dial
		if dial == nil {
			dial = p.dialSSH
		}
		rem, _, err := dial(ctx, t, pinned)
		if err != nil {
			return fmt.Errorf("onboard: connect to %s: %w", t.addr(), err)
		}
		defer rem.Close()
		j := &job{p: p, t: t, l: p.layout(), rem: rem, m: m}
		for _, verb := range []string{"stop", "disable"} {
			if _, err := j.priv(ctx, nil, "systemctl", verb, j.l.UnitName); err != nil {
				return fmt.Errorf("onboard: machine %s: systemctl %s: %w", id, verb, err)
			}
		}
	}
	now := p.clock()
	err := p.Registry.Update(id, func(m *registry.Machine) {
		m.Removed = true
		m.RemovedAt = &now
		m.Provisioned = false
		m.Online = false
		if m.CertFingerprint != "" {
			m.RevokedFingerprint = m.CertFingerprint
		}
	})
	if err != nil {
		return err
	}
	return p.persist()
}

// --- real SSH ----------------------------------------------------------------

// dialSSH is the default DialFunc. Host key policy: a pinned fingerprint must
// match; with none, the first-seen key is trusted and returned so the caller
// can record it (trust on first use).
func (p *Provisioner) dialSSH(ctx context.Context, t Target, pinned string) (Remote, string, error) {
	methods, err := t.Auth.methods()
	if err != nil {
		return nil, "", err
	}
	var seen string
	cfg := &ssh.ClientConfig{
		User: t.User,
		Auth: methods,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = ssh.FingerprintSHA256(key)
			if pinned != "" && pinned != seen {
				return fmt.Errorf("SSH host key mismatch: got %s, pinned %s", seen, pinned)
			}
			return nil
		},
	}
	timeout := p.DialTimeout
	if timeout == 0 {
		timeout = 15 * time.Second
	}
	cfg.Timeout = timeout

	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", t.addr())
	if err != nil {
		return nil, "", err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout)) // bound the handshake
	sc, chans, reqs, err := ssh.NewClientConn(conn, t.addr(), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	_ = conn.SetDeadline(time.Time{})
	return &sshRemote{client: ssh.NewClient(sc, chans, reqs)}, seen, nil
}

type sshRemote struct{ client *ssh.Client }

func (r *sshRemote) Close() error { return r.client.Close() }

func (r *sshRemote) Run(ctx context.Context, cmd string, stdin []byte) ([]byte, error) {
	sess, err := r.client.NewSession()
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	out := &syncBuffer{}
	sess.Stdout = out // stdout and stderr share one buffer
	sess.Stderr = out
	if stdin != nil {
		sess.Stdin = bytes.NewReader(stdin)
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Close()
		return nil, ctx.Err() // out may still be written by the goroutine
	case err := <-done:
		if err != nil {
			var ee *ssh.ExitError
			if errors.As(err, &ee) {
				return out.Bytes(), &RemoteError{Cmd: cmd, Code: ee.ExitStatus(), Output: out.String()}
			}
			return out.Bytes(), err
		}
		return out.Bytes(), nil
	}
}

// syncBuffer is a bytes.Buffer safe for the two copier goroutines of a session.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func (b *syncBuffer) String() string { return string(b.Bytes()) }
