package onboard

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// --- a fake machine behind a real in-process SSH server -----------------------
//
// Nothing here runs a real shell: the server records each exec request and a
// small interpreter applies it to an in-memory "filesystem" (files, users,
// service state). Credentials and host keys are generated at runtime.

type fakeFile struct {
	data  []byte
	mode  string
	owner string
}

type fakeMachine struct {
	mu       sync.Mutex
	cmds     []string // every command, sudo prefix stripped
	rawCmds  []string
	files    map[string]fakeFile
	dirs     map[string]bool
	users    map[string]bool
	enabled  bool
	active   bool
	failCmd  string // if non-empty, commands containing this exit 1
	sudoSeen bool
}

func newFakeMachine() *fakeMachine {
	return &fakeMachine{files: map[string]fakeFile{}, dirs: map[string]bool{}, users: map[string]bool{}}
}

func (f *fakeMachine) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *fakeMachine) resetLog() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cmds, f.rawCmds = nil, nil
}

func (f *fakeMachine) file(path string) (fakeFile, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ff, ok := f.files[path]
	return ff, ok
}

// exec interprets one command. Returns output and exit status.
func (f *fakeMachine) exec(cmd string, stdin []byte) (string, uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rawCmds = append(f.rawCmds, cmd)
	if strings.HasPrefix(cmd, "sudo -n ") {
		f.sudoSeen = true
		cmd = strings.TrimPrefix(cmd, "sudo -n ")
	}
	f.cmds = append(f.cmds, cmd)
	if f.failCmd != "" && strings.Contains(cmd, f.failCmd) {
		return "simulated failure\n", 1
	}
	a := strings.Fields(cmd) // fake paths contain no spaces or quotes
	switch {
	case a[0] == "getent" && a[1] == "passwd":
		if f.users[a[2]] {
			return a[2] + ":x:999:999::/var/lib:/usr/sbin/nologin\n", 0
		}
		return "", 2
	case a[0] == "useradd":
		f.users[a[len(a)-1]] = true
		return "", 0
	case a[0] == "install" && a[1] == "-d":
		f.dirs[a[len(a)-1]] = true
		return "", 0
	case a[0] == "install":
		// install -m MODE -o OWNER -g GROUP /dev/stdin PATH
		path := a[len(a)-1]
		d := path[:strings.LastIndex(path, "/")]
		if !f.dirs[d] && d != "/usr/local/bin" && d != "/etc/systemd/system" {
			return "install: cannot create " + path + ": No such file or directory\n", 1
		}
		f.files[path] = fakeFile{data: append([]byte(nil), stdin...), mode: a[2], owner: a[4]}
		return "", 0
	case a[0] == "cat":
		if ff, ok := f.files[a[1]]; ok {
			return string(ff.data), 0
		}
		return "cat: " + a[1] + ": No such file or directory\n", 1
	case a[0] == "test" && a[1] == "-s":
		if ff, ok := f.files[a[2]]; ok && len(ff.data) > 0 {
			return "", 0
		}
		return "", 1
	case a[0] == "systemctl":
		switch a[1] {
		case "daemon-reload":
		case "enable":
			f.enabled = true
		case "restart":
			f.active = true
		case "stop":
			f.active = false
		case "disable":
			f.enabled = false
		default:
			return "unknown verb\n", 1
		}
		return "", 0
	}
	return "command not found: " + a[0] + "\n", 127
}

type sshTestServer struct {
	addr     string
	host     string
	port     int
	hostFP   string
	user     string
	password string
	userKey  ssh.PublicKey
	machine  *fakeMachine
	ln       net.Listener
	wg       sync.WaitGroup
}

func startSSHServer(t *testing.T, machine *fakeMachine, user, password string, userKey ssh.PublicKey) *sshTestServer {
	t.Helper()
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if password != "" && c.User() == user && string(pw) == password {
				return nil, nil
			}
			return nil, errors.New("bad password")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if userKey != nil && c.User() == user && string(k.Marshal()) == string(userKey.Marshal()) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	s := &sshTestServer{addr: ln.Addr().String(), host: host, port: port,
		hostFP: ssh.FingerprintSHA256(hostSigner.PublicKey()), user: user, password: password,
		userKey: userKey, machine: machine, ln: ln}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.serve(c, cfg)
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

func (s *sshTestServer) serve(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go s.session(ch, creqs)
	}
}

func (s *sshTestServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		if len(req.Payload) < 4 {
			_ = req.Reply(false, nil)
			return
		}
		n := binary.BigEndian.Uint32(req.Payload)
		cmd := string(req.Payload[4 : 4+n])
		_ = req.Reply(true, nil)
		var stdin []byte
		buf := make([]byte, 32*1024)
		for {
			k, err := ch.Read(buf)
			stdin = append(stdin, buf[:k]...)
			if err != nil {
				break
			}
		}
		out, status := s.machine.exec(cmd, stdin)
		_, _ = ch.Write([]byte(out))
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

// --- fixtures -------------------------------------------------------------------

type fixture struct {
	p       *Provisioner
	srv     *sshTestServer
	fm      *fakeMachine
	reg     *registry.Registry
	store   *registry.Store
	dataDir string
	agent   string
	ca      *pki.CA
}

func newFixture(t *testing.T, user string) *fixture {
	t.Helper()
	dataDir := t.TempDir()
	agent := filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(agent, []byte("\x7fELF-fake-agent-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fm := newFakeMachine()
	srv := startSSHServer(t, fm, user, "s3cret-throwaway", nil)
	reg := registry.New()
	store := registry.NewStore(dataDir)
	return &fixture{
		p:   &Provisioner{CA: ca, Registry: reg, Store: store, AgentBinaryPath: agent, DialTimeout: 5 * time.Second},
		srv: srv, fm: fm, reg: reg, store: store, dataDir: dataDir, agent: agent, ca: ca,
	}
}

func (f *fixture) target(user, role string) Target {
	return Target{Host: f.srv.host, Port: f.srv.port, Auth: Auth{User: user, Password: "s3cret-throwaway"}, Role: role}
}

var wantSteps = []string{StepEnsureUser, StepCreateDirs, StepInstallBinary, StepIdentity,
	StepWriteCA, StepWriteConfig, StepInstallUnit, StepEnableStart}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- tests ------------------------------------------------------------------------

func TestProvisionRunsStepsInOrderAndInstallsEverything(t *testing.T) {
	f := newFixture(t, "root")
	res, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(res.Steps, wantSteps) {
		t.Fatalf("steps = %v, want %v", res.Steps, wantSteps)
	}
	if !res.Created || !res.CertIssued {
		t.Fatalf("created=%v issued=%v", res.Created, res.CertIssued)
	}
	if f.fm.sudoSeen {
		t.Fatal("root must not use sudo")
	}

	// The remote commands appear in the documented order.
	order := []string{"getent passwd", "useradd", "install -d", "install -m 0755", "install -m 0600", "install -m 0644 -o fleet-agent", "install -m 0640", "daemon-reload", "systemctl enable", "systemctl restart"}
	last := -1
	for _, want := range order {
		idx := -1
		for i, c := range f.fm.cmds {
			if i > last && strings.Contains(c, want) {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("command containing %q not found after position %d in %v", want, last, f.fm.cmds)
		}
		last = idx
	}

	l := DefaultLayout()
	if !f.fm.users[l.ServiceUser] {
		t.Fatal("system user not created")
	}
	bin, ok := f.fm.file(l.Binary)
	if !ok || string(bin.data) != "\x7fELF-fake-agent-binary" || bin.mode != "0755" {
		t.Fatalf("binary: %+v", bin)
	}
	unit, ok := f.fm.file(l.UnitPath)
	if !ok || !strings.Contains(string(unit.data), "ExecStart=/usr/local/bin/fleet-agent --config /etc/fleet-agent/config.json") {
		t.Fatalf("unit: %q", unit.data)
	}
	// Privilege + data dir: the agent runs as root (capture/PTY/cgroup need it)
	// and its data dir is set explicitly, not derived from the binary location.
	for _, want := range []string{"User=root\n", "Group=root\n", "Environment=FLEET_DATA_DIR=/var/lib/fleet-agent\n", "Restart=always\n"} {
		if !strings.Contains(string(unit.data), want) {
			t.Fatalf("unit missing %q: %q", want, unit.data)
		}
	}
	if strings.Contains(string(unit.data), "User=fleet-agent") {
		t.Fatalf("unit must not run as the dedicated user: %q", unit.data)
	}
	if !f.fm.enabled || !f.fm.active {
		t.Fatalf("service enabled=%v active=%v", f.fm.enabled, f.fm.active)
	}

	// Pinned CA is the CA's cert; the key is private and owned by the service user.
	ca, _ := f.fm.file(l.caFile())
	if string(ca.data) != string(f.ca.CertPEM()) {
		t.Fatal("pinned CA mismatch")
	}
	key, ok := f.fm.file(l.keyFile())
	if !ok || key.mode != "0600" || key.owner != "fleet-agent" || !strings.Contains(string(key.data), "PRIVATE KEY") {
		t.Fatalf("key file: mode=%s owner=%s", key.mode, key.owner)
	}
	for _, c := range f.fm.rawCmds {
		if strings.Contains(c, "PRIVATE KEY") || strings.Contains(c, "s3cret") {
			t.Fatalf("secret leaked into a command line: %q", c)
		}
	}

	// The client cert is issued for this machine's ID and verifies against the CA
	// with the machine's IP in the SAN.
	certFile, _ := f.fm.file(l.certFile())
	if _, err := pki.DialTLSConfig(certFile.data, key.data, f.ca.CertPEM(), net.ParseIP(f.srv.host)); err != nil {
		t.Fatalf("issued cert unusable: %v", err)
	}
	fp, _ := CertFingerprint(certFile.data)
	if res.Machine.CertFingerprint != fp {
		t.Fatalf("fingerprint %s != %s", res.Machine.CertFingerprint, fp)
	}

	// The agent config is accepted by the agent's own loader and carries the flag.
	cfgFile, _ := f.fm.file(l.configFile())
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, cfgFile.data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("agent config rejected: %v\n%s", err, cfgFile.data)
	}
	a := cfg.Agent
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Role != "worker" || a.MachineID != res.Machine.ID || a.ConnectMode != "server_dial" || a.CAFile != l.caFile() {
		t.Fatalf("agent config: %+v", a)
	}

	// Registry state: persisted, provisioned, host key pinned, defaults applied.
	m := res.Machine
	if !m.Provisioned || m.ConnectMode != registry.ConnectModeServerDial || m.SSHHostKey != f.srv.hostFP {
		t.Fatalf("machine: %+v", m)
	}
	if len(m.PrivateIPs) != 1 || m.PrivateIPs[0] != f.srv.host {
		t.Fatalf("addresses: %v", m.PrivateIPs)
	}
	loaded, err := registry.NewStore(f.dataDir).Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.Get(m.ID)
	if !ok || !got.Provisioned || got.CertFingerprint != fp {
		t.Fatalf("persisted machine: %+v", got)
	}
}

func TestProvisionIsIdempotent(t *testing.T) {
	f := newFixture(t, "root")
	first, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	certBefore, _ := f.fm.file(DefaultLayout().certFile())
	f.fm.resetLog()

	second, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.CertIssued {
		t.Fatalf("second run created=%v issued=%v, want false/false", second.Created, second.CertIssued)
	}
	if !equalStrings(second.Steps, wantSteps) {
		t.Fatalf("steps = %v", second.Steps)
	}
	if second.Machine.ID != first.Machine.ID || second.Machine.CertFingerprint != first.Machine.CertFingerprint {
		t.Fatalf("identity changed: %+v -> %+v", first.Machine, second.Machine)
	}
	if n := len(f.reg.List()); n != 1 {
		t.Fatalf("registry has %d machines", n)
	}
	certAfter, _ := f.fm.file(DefaultLayout().certFile())
	if string(certAfter.data) != string(certBefore.data) {
		t.Fatal("cert was replaced on re-provision")
	}
	if n := f.fm.count("useradd"); n != 0 {
		t.Fatalf("useradd ran %d times on re-provision", n)
	}
	if n := f.fm.count("install -m 0600"); n != 0 {
		t.Fatalf("key rewritten %d times on re-provision", n)
	}
	if !f.fm.active {
		t.Fatal("service not (re)started")
	}
}

func TestReprovisionIssuesCertOnlyIfMissing(t *testing.T) {
	f := newFixture(t, "root")
	first, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	// Wipe the machine's cert (re-imaged box that kept its IP).
	f.fm.mu.Lock()
	delete(f.fm.files, DefaultLayout().certFile())
	f.fm.mu.Unlock()

	second, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	if !second.CertIssued {
		t.Fatal("missing cert was not re-issued")
	}
	if second.Machine.ID != first.Machine.ID {
		t.Fatal("ID changed")
	}
	if second.Machine.CertFingerprint == first.Machine.CertFingerprint {
		t.Fatal("fingerprint unchanged after re-issue")
	}
}

func TestReprovisionUpdatesRoleAndMode(t *testing.T) {
	f := newFixture(t, "root")
	first, _ := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	tgt := f.target("root", registry.RoleMonitor)
	tgt.ConnectMode = registry.ConnectModeAgentDial
	second, err := f.p.Provision(context.Background(), tgt)
	if err != nil {
		t.Fatal(err)
	}
	if second.Machine.ID != first.Machine.ID || second.Machine.Role != "monitor" || second.Machine.ConnectMode != "agent_dial" {
		t.Fatalf("%+v", second.Machine)
	}
	cfgFile, _ := f.fm.file(DefaultLayout().configFile())
	if !strings.Contains(string(cfgFile.data), `"connect_mode": "agent_dial"`) {
		t.Fatalf("config: %s", cfgFile.data)
	}
}

func TestMissingAgentBinaryIsAClearError(t *testing.T) {
	f := newFixture(t, "root")

	f.p.AgentBinaryPath = filepath.Join(t.TempDir(), "nope", "fleet-agent")
	_, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if !errors.Is(err, ErrAgentBinaryMissing) || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("err = %v", err)
	}
	f.p.AgentBinaryPath = ""
	_, err = f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if !errors.Is(err, ErrAgentBinaryMissing) || !strings.Contains(err.Error(), "agent_binary_path") {
		t.Fatalf("err = %v", err)
	}
	f.p.AgentBinaryPath = t.TempDir() // a directory
	if _, err = f.p.Provision(context.Background(), f.target("root", registry.RoleWorker)); !errors.Is(err, ErrAgentBinaryMissing) {
		t.Fatalf("err = %v", err)
	}

	// Nothing happened: no connection, no registry entry.
	if len(f.fm.rawCmds) != 0 {
		t.Fatalf("commands ran: %v", f.fm.rawCmds)
	}
	if len(f.reg.List()) != 0 {
		t.Fatal("registry entry created despite the early error")
	}
	if _, err := os.Stat(f.store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("registry file written despite the early error")
	}
}

func TestNonRootUsesSudo(t *testing.T) {
	f := newFixture(t, "deploy")
	if _, err := f.p.Provision(context.Background(), f.target("deploy", registry.RoleMonitor)); err != nil {
		t.Fatal(err)
	}
	if !f.fm.sudoSeen {
		t.Fatal("expected sudo for a non-root user")
	}
	for _, c := range f.fm.rawCmds {
		if strings.HasPrefix(c, "useradd") || strings.HasPrefix(c, "install") || strings.HasPrefix(c, "systemctl") {
			t.Fatalf("privileged command without sudo: %q", c)
		}
	}
}

func TestPublicKeyAuth(t *testing.T) {
	f := newFixture(t, "root")
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(block)
	srv := startSSHServer(t, f.fm, "root", "", sshPub)
	tgt := Target{Host: srv.host, Port: srv.port, Auth: Auth{User: "root", PrivateKeyPEM: keyPEM}, Role: registry.RoleWorker}
	if _, err := f.p.Provision(context.Background(), tgt); err != nil {
		t.Fatal(err)
	}
	// A wrong key is refused.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	ob, _ := ssh.MarshalPrivateKey(other, "")
	tgt.PrivateKeyPEM = pem.EncodeToMemory(ob)
	tgt.MachineID = "other-box"
	if _, err := f.p.Provision(context.Background(), tgt); err == nil {
		t.Fatal("wrong key accepted")
	}
}

func TestHostKeyPinning(t *testing.T) {
	f := newFixture(t, "root")
	tgt := f.target("root", registry.RoleWorker)
	tgt.HostKeyFingerprint = "SHA256:definitely-not-the-host-key"
	if _, err := f.p.Provision(context.Background(), tgt); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("err = %v", err)
	}
	if len(f.fm.rawCmds) != 0 {
		t.Fatal("commands ran despite host key mismatch")
	}
	tgt.HostKeyFingerprint = f.srv.hostFP
	if _, err := f.p.Provision(context.Background(), tgt); err != nil {
		t.Fatal(err)
	}
	// The stored fingerprint now pins later runs: a different host at the same
	// address is refused.
	other := startSSHServer(t, newFakeMachine(), "root", "s3cret-throwaway", nil)
	tgt2 := Target{Host: f.srv.host, Port: other.port, Auth: Auth{User: "root", Password: "s3cret-throwaway"}, Role: registry.RoleWorker}
	if _, err := f.p.Provision(context.Background(), tgt2); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedStepIsReportedAndRetryReusesIdentity(t *testing.T) {
	f := newFixture(t, "root")
	f.fm.failCmd = "systemctl enable"
	_, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err == nil || !strings.Contains(err.Error(), StepEnableStart) {
		t.Fatalf("err = %v", err)
	}
	ms := f.reg.List()
	if len(ms) != 1 || ms[0].Provisioned {
		t.Fatalf("registry after failure: %+v", ms)
	}
	id := ms[0].ID

	f.fm.failCmd = ""
	res, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	if res.Machine.ID != id || res.Created || !res.Machine.Provisioned {
		t.Fatalf("retry: %+v", res)
	}
}

func TestRemove(t *testing.T) {
	f := newFixture(t, "root")
	res, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	id := res.Machine.ID
	f.fm.resetLog()

	auth := Auth{User: "root", Password: "s3cret-throwaway"}
	if err := f.p.Remove(context.Background(), id, auth, RemoveOptions{Port: f.srv.port}); err != nil {
		t.Fatal(err)
	}
	if f.fm.active || f.fm.enabled {
		t.Fatalf("service still active=%v enabled=%v", f.fm.active, f.fm.enabled)
	}
	// The liveness timer goes first (T-0027), or it would restart the stopped agent.
	if got := f.fm.cmds; len(got) != 3 || !strings.HasPrefix(got[0], "systemctl disable --now fleet-agent-liveness.timer") ||
		!strings.HasPrefix(got[1], "systemctl stop") || !strings.HasPrefix(got[2], "systemctl disable") {
		t.Fatalf("cmds = %v", got)
	}
	m, _ := f.reg.Get(id)
	if !m.Removed || m.RemovedAt == nil || m.Provisioned || m.RevokedFingerprint != res.Machine.CertFingerprint {
		t.Fatalf("machine after remove: %+v", m)
	}
	loaded, _ := f.store.Load()
	if lm, _ := loaded.Get(id); !lm.Removed {
		t.Fatal("removal not persisted")
	}

	// Re-enrolling the same machine reuses the ID but never the revoked cert.
	again, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	if err != nil {
		t.Fatal(err)
	}
	if again.Machine.ID != id || again.Machine.Removed || !again.CertIssued || again.Machine.CertFingerprint == res.Machine.CertFingerprint {
		t.Fatalf("re-enroll: %+v issued=%v", again.Machine, again.CertIssued)
	}

	if err := f.p.Remove(context.Background(), "ghost", auth, RemoveOptions{SkipRemote: true}); !errors.Is(err, registry.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestRemoveSkipRemoteAndUnreachable(t *testing.T) {
	f := newFixture(t, "root")
	res, _ := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
	id := res.Machine.ID
	f.srv.ln.Close() // machine goes away

	auth := Auth{User: "root", Password: "s3cret-throwaway"}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.p.Remove(ctx, id, auth, RemoveOptions{Port: f.srv.port}); err == nil {
		t.Fatal("remove should fail when the machine is unreachable")
	}
	if m, _ := f.reg.Get(id); m.Removed {
		t.Fatal("marked removed despite the failure")
	}
	if err := f.p.Remove(ctx, id, auth, RemoveOptions{SkipRemote: true}); err != nil {
		t.Fatal(err)
	}
	if m, _ := f.reg.Get(id); !m.Removed {
		t.Fatal("SkipRemote did not mark removed")
	}
}

func TestValidateTarget(t *testing.T) {
	f := newFixture(t, "root")
	bad := map[string]Target{
		"bad role":     {Host: "10.0.0.1", Auth: Auth{User: "u", Password: "p"}, Role: "boss"},
		"hostname":     {Host: "box.local", Auth: Auth{User: "u", Password: "p"}, Role: "worker"},
		"no user":      {Host: "10.0.0.1", Auth: Auth{Password: "p"}, Role: "worker"},
		"no creds":     {Host: "10.0.0.1", Auth: Auth{User: "u"}, Role: "worker"},
		"bad mode":     {Host: "10.0.0.1", Auth: Auth{User: "u", Password: "p"}, Role: "worker", ConnectMode: "x"},
		"bad given id": {Host: "10.0.0.1", Auth: Auth{User: "u", Password: "p"}, Role: "worker", MachineID: "a b"},
	}
	for name, tgt := range bad {
		if _, err := f.p.Provision(context.Background(), tgt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(f.reg.List()) != 0 {
		t.Fatal("invalid targets left registry entries")
	}
}

func TestShQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/etc/fleet-agent": "/etc/fleet-agent",
		"a b":              "'a b'",
		"it's":             `'it'\''s'`,
		"$(x)":             "'$(x)'",
	} {
		if got := shQuote(in); got != want {
			t.Errorf("shQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
