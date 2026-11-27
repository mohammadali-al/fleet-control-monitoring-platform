package onboard

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
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
	mu          sync.Mutex
	cmds        []string // every command, sudo prefix stripped
	rawCmds     []string
	files       map[string]fakeFile
	dirs        map[string]bool
	users       map[string]bool
	enabled     bool
	active      bool
	unitEnabled map[string]bool
	unitActive  map[string]bool
	failCmd     string // if non-empty, commands containing this exit 1
	sudoSeen    bool
	unameOS     string // defaults to Linux (or host OS)
	unameArch   string // defaults to host arch
}

func newFakeMachine() *fakeMachine {
	return &fakeMachine{
		files:       map[string]fakeFile{},
		dirs:        map[string]bool{},
		users:       map[string]bool{},
		unitEnabled: map[string]bool{},
		unitActive:  map[string]bool{},
	}
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
	case a[0] == "uname":
		uOS := f.unameOS
		if uOS == "" {
			uOS = runtime.GOOS
			if uOS == "linux" {
				uOS = "Linux"
			} else if uOS == "darwin" {
				uOS = "Darwin"
			}
		}
		uArch := f.unameArch
		if uArch == "" {
			uArch = runtime.GOARCH
			if uArch == "amd64" {
				uArch = "x86_64"
			} else if uArch == "arm64" {
				uArch = "aarch64"
			}
		}
		return uOS + " " + uArch + "\n", 0
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
		unit := a[len(a)-1]
		now := len(a) > 2 && a[2] == "--now"
		switch a[1] {
		case "daemon-reload":
		case "enable":
			f.enabled = true
			f.unitEnabled[unit] = true
			if now {
				f.active = true
				f.unitActive[unit] = true
			}
		case "restart":
			f.active = true
			f.unitActive[unit] = true
		case "stop":
			f.active = false
			f.unitActive[unit] = false
		case "disable":
			f.enabled = false
			f.unitEnabled[unit] = false
			if now {
				f.active = false
				f.unitActive[unit] = false
			}
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
	f.p.ServerAddr = "fleet.example.com:7444"
	f.p.ServerID = "fleet-server"
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
	for _, want := range []string{
		`"connect_mode": "agent_dial"`,
		`"server_addr": "fleet.example.com:7444"`,
		`"server_id": "fleet-server"`,
	} {
		if !strings.Contains(string(cfgFile.data), want) {
			t.Fatalf("config missing %s:\n%s", want, cfgFile.data)
		}
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
	for _, u := range []string{"fleet-agent.service", "fleet-watchdog.service", "fleet-agent-liveness.timer", "fleet-watchdog-liveness.timer"} {
		if f.fm.unitEnabled[u] || f.fm.unitActive[u] {
			t.Fatalf("%s still enabled=%v active=%v", u, f.fm.unitEnabled[u], f.fm.unitActive[u])
		}
	}
	// The liveness timers go first (T-0027/T-0030), or they would restart the stopped services.
	wantPrefixes := []string{
		"systemctl disable --now fleet-agent-liveness.timer",
		"systemctl disable --now fleet-watchdog-liveness.timer",
		"systemctl stop fleet-watchdog.service",
		"systemctl disable fleet-watchdog.service",
		"systemctl stop fleet-agent.service",
		"systemctl disable fleet-agent.service",
	}
	if got := f.fm.cmds; len(got) != len(wantPrefixes) {
		t.Fatalf("cmds = %v, want %d commands", got, len(wantPrefixes))
	} else {
		for i, want := range wantPrefixes {
			if !strings.HasPrefix(got[i], want) {
				t.Fatalf("cmds[%d] = %q, want prefix %q (all: %v)", i, got[i], want, got)
			}
		}
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

func TestProvisionInstallsWatchdogAndLiveness(t *testing.T) {
	l := DefaultLayout()

	t.Run("worker_full", func(t *testing.T) {
		f := newFixture(t, "root")
		if _, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker)); err != nil {
			t.Fatal(err)
		}
		wd, ok := f.fm.file(l.watchdogUnitPath())
		if !ok || wd.mode != "0644" || wd.owner != "root" {
			t.Fatalf("watchdog unit not installed: %+v", wd)
		}
		for _, want := range []string{
			"Description=Fleet watchdog (load-safety net)\n",
			"User=root\n",
			"Environment=FLEET_DATA_DIR=/var/lib/fleet-agent\n",
			"ExecStart=/usr/local/bin/fleet-agent watchdog --config /etc/fleet-agent/config.json\n",
			"WorkingDirectory=/var/lib/fleet-agent\n",
			"Restart=always\n",
			"OOMScoreAdjust=-900\n",
			"Nice=-5\n",
			"WantedBy=multi-user.target\n",
		} {
			if !strings.Contains(string(wd.data), want) {
				t.Errorf("worker watchdog unit missing %q:\n%s", want, wd.data)
			}
		}
		if strings.Contains(string(wd.data), "FLEET_WATCHDOG_MODE=headless") {
			t.Errorf("worker watchdog unit must not be headless:\n%s", wd.data)
		}
		if _, ok := f.fm.file(l.watchdogPolicyPath()); ok {
			t.Error("worker must not be seeded with stripped monitor policy.yaml")
		}

		for _, u := range []string{"fleet-agent.service", "fleet-watchdog.service", "fleet-agent-liveness.timer", "fleet-watchdog-liveness.timer"} {
			if !f.fm.unitEnabled[u] || !f.fm.unitActive[u] {
				t.Errorf("%s enabled=%v active=%v, want true/true", u, f.fm.unitEnabled[u], f.fm.unitActive[u])
			}
		}

		for deployPath, installedPath := range map[string]string{
			"../../deploy/systemd/fleet-watchdog-liveness.service": l.watchdogLivenessServicePath(),
			"../../deploy/systemd/fleet-watchdog-liveness.timer":   l.watchdogLivenessTimerPath(),
		} {
			got, ok := f.fm.file(installedPath)
			if !ok || got.mode != "0644" || got.owner != "root" {
				t.Fatalf("%s not installed: %+v", installedPath, got)
			}
			wantBytes, err := os.ReadFile(deployPath)
			if err != nil {
				t.Fatal(err)
			}
			want := string(wantBytes)
			if i := strings.Index(want, "[Unit]"); i >= 0 {
				want = want[i:]
			}
			if strings.TrimSpace(string(got.data)) != strings.TrimSpace(want) {
				t.Errorf("%s differs from %s:\n got: %s\nwant: %s", installedPath, deployPath, got.data, want)
			}
		}
	})

	t.Run("monitor_headless_stripped", func(t *testing.T) {
		f := newFixture(t, "root")
		if _, err := f.p.Provision(context.Background(), f.target("root", registry.RoleMonitor)); err != nil {
			t.Fatal(err)
		}
		wd, ok := f.fm.file(l.watchdogUnitPath())
		if !ok || wd.mode != "0644" || wd.owner != "root" {
			t.Fatalf("monitor watchdog unit not installed: %+v", wd)
		}
		for _, want := range []string{
			"Description=Fleet watchdog (headless)\n",
			"Environment=FLEET_WATCHDOG_MODE=headless\n",
			"Environment=FLEET_DATA_DIR=/var/lib/fleet-agent\n",
			"ExecStart=/usr/local/bin/fleet-agent watchdog --config /etc/fleet-agent/config.json\n",
			"Restart=always\n",
			"WantedBy=multi-user.target\n",
		} {
			if !strings.Contains(string(wd.data), want) {
				t.Errorf("monitor watchdog unit missing %q:\n%s", want, wd.data)
			}
		}
		polFile, ok := f.fm.file(l.watchdogPolicyPath())
		if !ok || polFile.mode != "0640" || polFile.owner != l.ServiceUser {
			t.Fatalf("monitor policy.yaml not installed properly: %+v", polFile)
		}
		pf, err := watchdog.ParseProfile(polFile.data)
		if err != nil {
			t.Fatalf("monitor policy.yaml invalid: %v\n%s", err, polFile.data)
		}
		pol, err := pf.Effective("mon-1", nil)
		if err != nil {
			t.Fatal(err)
		}
		if pol.Steps.Throttle || pol.Steps.KillSession || !pol.Steps.Restart || !pol.Steps.Reboot ||
			pol.LoadController.CPUPercent != 0 || pol.LoadController.MemoryPercent != 0 {
			t.Errorf("monitor stripped policy = %+v", pol)
		}
		for _, u := range []string{"fleet-agent.service", "fleet-watchdog.service", "fleet-agent-liveness.timer", "fleet-watchdog-liveness.timer"} {
			if !f.fm.unitEnabled[u] || !f.fm.unitActive[u] {
				t.Errorf("%s enabled=%v active=%v, want true/true", u, f.fm.unitEnabled[u], f.fm.unitActive[u])
			}
		}
	})
}

func TestProvisionSelectsBinaryFromAgentBinaryDirAndErrorsClearly(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		if err := os.WriteFile(filepath.Join(dir, "fleet-agent-"+p), []byte("BIN-"+p), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Prefers AgentBinaryDir over AgentBinaryPath and picks the binary matching uname -sm.
	for _, tc := range []struct {
		unameOS, unameArch, wantBin string
	}{
		{"Linux", "x86_64", "BIN-linux-amd64"},
		{"Linux", "aarch64", "BIN-linux-arm64"},
		{"Darwin", "x86_64", "BIN-darwin-amd64"},
		{"Darwin", "arm64", "BIN-darwin-arm64"},
	} {
		t.Run(tc.unameOS+"_"+tc.unameArch, func(t *testing.T) {
			f := newFixture(t, "root")
			f.fm.unameOS = tc.unameOS
			f.fm.unameArch = tc.unameArch
			f.p.AgentBinaryDir = dir
			// f.p.AgentBinaryPath is still set to the fake host binary; dir must win.
			if _, err := f.p.Provision(context.Background(), f.target("root", registry.RoleMonitor)); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			got, ok := f.fm.file(DefaultLayout().Binary)
			if !ok {
				t.Fatalf("binary not installed at %s", DefaultLayout().Binary)
			}
			if string(got.data) != tc.wantBin {
				t.Errorf("installed binary = %q, want %q", got.data, tc.wantBin)
			}
		})
	}

	// Host fallback works when agent_binary_dir lacks the host binary, but a non-host
	// platform without a binary in agent_binary_dir fails clearly BEFORE installing anything.
	t.Run("missing_target_platform_errors_before_install", func(t *testing.T) {
		f := newFixture(t, "root")
		partialDir := t.TempDir()
		f.p.AgentBinaryDir = partialDir
		// Target is linux/arm64 while host is runtime.GOOS/runtime.GOARCH (or darwin/arm64 if host is linux/arm64).
		f.fm.unameOS = "Linux"
		f.fm.unameArch = "aarch64"
		if runtime.GOOS == "linux" && runtime.GOARCH == "arm64" {
			f.fm.unameOS = "Darwin"
			f.fm.unameArch = "x86_64"
		}

		_, err := f.p.Provision(context.Background(), f.target("root", registry.RoleWorker))
		if !errors.Is(err, ErrAgentBinaryMissing) {
			t.Fatalf("err = %v, want ErrAgentBinaryMissing", err)
		}
		// Only the read-only uname probe may have run; nothing was installed or registered.
		for _, c := range f.fm.cmds {
			if !strings.HasPrefix(c, "uname ") {
				t.Errorf("install command ran despite missing binary: %q", c)
			}
		}
		if len(f.fm.files) != 0 || len(f.fm.dirs) != 0 || len(f.fm.users) != 0 {
			t.Errorf("remote state modified despite missing binary: files=%d dirs=%d users=%d",
				len(f.fm.files), len(f.fm.dirs), len(f.fm.users))
		}
		if len(f.reg.List()) != 0 {
			t.Errorf("registry entry created despite missing binary: %+v", f.reg.List())
		}
		if _, err := os.Stat(f.store.Path()); !errors.Is(err, os.ErrNotExist) {
			t.Error("registry file written despite missing binary")
		}
	})

	// Explicit Target.OS/Target.Arch fails before even dialing SSH.
	t.Run("explicit_target_platform_fails_before_dial", func(t *testing.T) {
		f := newFixture(t, "root")
		f.p.AgentBinaryDir = t.TempDir()
		tgt := f.target("root", registry.RoleWorker)
		tgt.OS, tgt.Arch = "darwin", "amd64"
		_, err := f.p.Provision(context.Background(), tgt)
		if !errors.Is(err, ErrAgentBinaryMissing) || !strings.Contains(err.Error(), "darwin/amd64") {
			t.Fatalf("err = %v", err)
		}
		if len(f.fm.rawCmds) != 0 {
			t.Errorf("commands ran despite preflight error: %v", f.fm.rawCmds)
		}
	})
}

func TestRenderAgentConfigAgentDialVsServerDial(t *testing.T) {
	l := DefaultLayout()

	t.Run("agent_dial_emits_server_addr_and_server_id", func(t *testing.T) {
		m := registry.Machine{ID: "m-ad1", Role: config.RoleWorker, ConnectMode: config.ConnectModeAgentDial}
		got, err := renderAgentConfig(m, l, "fleet.example.com:7444", "srv-custom")
		if err != nil {
			t.Fatalf("renderAgentConfig: %v", err)
		}
		var doc struct {
			Agent map[string]any `json:"agent"`
		}
		if err := json.Unmarshal(got, &doc); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, got)
		}
		if doc.Agent["server_addr"] != "fleet.example.com:7444" {
			t.Errorf("server_addr = %v, want fleet.example.com:7444", doc.Agent["server_addr"])
		}
		if doc.Agent["server_id"] != "srv-custom" {
			t.Errorf("server_id = %v, want srv-custom", doc.Agent["server_id"])
		}
		cfgPath := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(cfgPath, got, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("config.Load: %v", err)
		}
		if err := loaded.Agent.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if loaded.Agent.ServerAddr != "fleet.example.com:7444" || loaded.Agent.ServerID != "srv-custom" {
			t.Errorf("loaded agent config = %+v", loaded.Agent)
		}
	})

	t.Run("agent_dial_defaults_empty_server_id", func(t *testing.T) {
		m := registry.Machine{ID: "m-ad2", Role: config.RoleMonitor, ConnectMode: config.ConnectModeAgentDial}
		got, err := renderAgentConfig(m, l, "fleet.example.com:7444", "")
		if err != nil {
			t.Fatalf("renderAgentConfig: %v", err)
		}
		if !strings.Contains(string(got), `"server_id": "`+config.DefaultServerID+`"`) {
			t.Fatalf("missing default server_id:\n%s", got)
		}
	})

	t.Run("agent_dial_without_server_addr_errors", func(t *testing.T) {
		m := registry.Machine{ID: "m-ad3", Role: config.RoleWorker, ConnectMode: config.ConnectModeAgentDial}
		if _, err := renderAgentConfig(m, l, "", "fleet-server"); !errors.Is(err, ErrAgentDialNotConfigured) {
			t.Fatalf("err = %v, want ErrAgentDialNotConfigured", err)
		}
	})

	t.Run("server_dial_omits_server_addr_and_server_id_byte_identical", func(t *testing.T) {
		for _, mode := range []string{config.ConnectModeServerDial, ""} {
			m := registry.Machine{ID: "m-sd1", Role: config.RoleWorker, ConnectMode: mode}
			got, err := renderAgentConfig(m, l, "fleet.example.com:7444", "srv-custom")
			if err != nil {
				t.Fatalf("renderAgentConfig(%q): %v", mode, err)
			}
			for _, forbidden := range []string{"server_addr", "server_id", "fleet.example.com", "srv-custom"} {
				if strings.Contains(string(got), forbidden) {
					t.Errorf("mode %q output unexpectedly contains %q:\n%s", mode, forbidden, got)
				}
			}
			legacyDoc := struct {
				Agent config.AgentConfig `json:"agent"`
			}{Agent: config.AgentConfig{
				Role:        m.Role,
				MachineID:   m.ID,
				ConnectMode: m.ConnectMode,
				CAFile:      l.caFile(),
				CertFile:    l.certFile(),
				KeyFile:     l.keyFile(),
			}}
			wantBytes, err := json.MarshalIndent(legacyDoc, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			wantBytes = append(wantBytes, '\n')
			if string(got) != string(wantBytes) {
				t.Errorf("mode %q output differs from byte-identical baseline:\n got:\n%s\nwant:\n%s", mode, got, wantBytes)
			}
		}
	})
}

func TestProvisionAgentDialRequiresEndpointAndConnects(t *testing.T) {
	t.Run("missing_endpoint_fails_before_dial_or_registry", func(t *testing.T) {
		f := newFixture(t, "root")
		tgt := f.target("root", registry.RoleWorker)
		tgt.ConnectMode = config.ConnectModeAgentDial
		_, err := f.p.Provision(context.Background(), tgt)
		if !errors.Is(err, ErrAgentDialNotConfigured) {
			t.Fatalf("err = %v, want ErrAgentDialNotConfigured", err)
		}
		if len(f.fm.rawCmds) != 0 {
			t.Errorf("SSH commands ran despite missing endpoint: %v", f.fm.rawCmds)
		}
		if len(f.reg.List()) != 0 {
			t.Errorf("registry modified despite missing endpoint: %+v", f.reg.List())
		}
		if _, err := os.Stat(f.store.Path()); !errors.Is(err, os.ErrNotExist) {
			t.Error("registry store written despite missing endpoint")
		}
	})

	t.Run("reprovision_existing_agent_dial_without_endpoint_fails_early", func(t *testing.T) {
		f := newFixture(t, "root")
		f.p.ServerAddr = "fleet.example.com:7444"
		tgt := f.target("root", registry.RoleWorker)
		tgt.ConnectMode = config.ConnectModeAgentDial
		if _, err := f.p.Provision(context.Background(), tgt); err != nil {
			t.Fatal(err)
		}
		f.fm.resetLog()
		f.p.ServerAddr = ""
		reprov := f.target("root", registry.RoleWorker)
		reprov.ConnectMode = "" // empty keeps existing agent_dial mode
		if _, err := f.p.Provision(context.Background(), reprov); !errors.Is(err, ErrAgentDialNotConfigured) {
			t.Fatalf("err = %v, want ErrAgentDialNotConfigured", err)
		}
		if len(f.fm.rawCmds) != 0 {
			t.Errorf("SSH commands ran on reprovision despite missing endpoint: %v", f.fm.rawCmds)
		}
	})

	t.Run("onboarded_agent_dial_config_connects_end_to_end", func(t *testing.T) {
		f := newFixture(t, "root")
		const serverID = "fleet-srv-e2e"
		srvCert, srvKey, err := f.ca.IssuePeer(serverID, nil, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		srvTLS, err := pki.ServerTLSConfig(srvCert, srvKey, f.ca.CertPEM())
		if err != nil {
			t.Fatal(err)
		}
		attached := make(chan string, 2)
		acc, err := channel.Accept(channel.AcceptorConfig{
			Addr: "127.0.0.1:0", TLS: srvTLS, ServerID: serverID,
			Attach: func(id string, _ *channel.Conn) (channel.Handler, func(), error) {
				attached <- id
				return nil, nil, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		sctx, scancel := context.WithCancel(context.Background())
		defer scancel()
		go func() { _ = acc.Serve(sctx) }()

		f.p.ServerAddr = acc.Addr().String()
		f.p.ServerID = serverID
		tgt := f.target("root", registry.RoleMonitor)
		tgt.ConnectMode = config.ConnectModeAgentDial
		res, err := f.p.Provision(context.Background(), tgt)
		if err != nil {
			t.Fatalf("Provision: %v", err)
		}

		l := DefaultLayout()
		cfgFile, ok := f.fm.file(l.configFile())
		if !ok {
			t.Fatal("agent config.json not written")
		}
		localDir := t.TempDir()
		cfgPath := filepath.Join(localDir, "config.json")
		if err := os.WriteFile(cfgPath, cfgFile.data, 0o600); err != nil {
			t.Fatal(err)
		}
		loaded, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("config.Load: %v\n%s", err, cfgFile.data)
		}
		if loaded.Agent.ServerAddr != f.p.ServerAddr || loaded.Agent.ServerID != serverID {
			t.Fatalf("agent config = %+v", loaded.Agent)
		}
		for remotePath, localName := range map[string]string{
			l.caFile():   "ca.crt",
			l.certFile(): "agent.crt",
			l.keyFile():  "agent.key",
		} {
			ff, ok := f.fm.file(remotePath)
			if !ok {
				t.Fatalf("missing remote file %s", remotePath)
			}
			if err := os.WriteFile(filepath.Join(localDir, localName), ff.data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		loaded.Agent.CAFile = filepath.Join(localDir, "ca.crt")
		loaded.Agent.CertFile = filepath.Join(localDir, "agent.crt")
		loaded.Agent.KeyFile = filepath.Join(localDir, "agent.key")

		runner, err := agent.DialFromConfig(loaded.Agent, filepath.Join(localDir, "data"), slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatalf("agent.DialFromConfig: %v", err)
		}
		actx, acancel := context.WithCancel(context.Background())
		defer acancel()
		done := make(chan error, 1)
		go func() { done <- runner.Run(actx) }()

		select {
		case id := <-attached:
			if id != res.Machine.ID {
				t.Errorf("server saw agent %q, want %q", id, res.Machine.ID)
			}
		case err := <-done:
			t.Fatalf("agent exited early: %v", err)
		case <-time.After(5 * time.Second):
			t.Fatal("onboarded agent_dial agent did not connect to server")
		}
		acancel()
		<-done
	})
}
