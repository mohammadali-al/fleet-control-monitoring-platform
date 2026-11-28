package enroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeRunner struct {
	mu   sync.Mutex
	runs [][2]string
	err  error
}

func (f *fakeRunner) Run(_ context.Context, id, cmd string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs = append(f.runs, [2]string{id, cmd})
	return f.err
}

type env struct {
	t         *testing.T
	svc       *Service
	reg       *registry.Registry
	ca        *pki.CA
	clk       *clock
	runner    *fakeRunner
	persists  int
	reconcile int
	binary    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, reg: registry.New(), ca: ca, clk: &clock{t: time.Now()}, runner: &fakeRunner{}}
	e.binary = filepath.Join(t.TempDir(), "fleet-agent")
	if err := os.WriteFile(e.binary, []byte("#!fake-agent-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{
		CA: ca, Registry: e.reg,
		Persist:    func() error { e.persists++; return nil },
		Reconcile:  func() { e.reconcile++ },
		Runner:     e.runner,
		Binary:     BinaryDir("", e.binary, OSLinux, "amd64"),
		PublicHost: "127.0.0.1", EnrollPort: 7445, AgentDialPort: 7444,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: e.clk.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

// csr returns a PEM certificate request for a fresh key, and the key: the
// machine's side of the exchange. Its subject claims to be someone else, to
// show that the server ignores it.
func csr(t *testing.T) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "some-other-machine"}, DNSNames: []string{"evil.example"},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), key
}

func linuxReq(tok string, csrPEM []byte) Request {
	return Request{
		Token: tok, CSRPEM: csrPEM, Hostname: "Build Box 7", OS: "linux", OSVersion: "Ubuntu 24.04 LTS",
		Kernel: "6.8.0", Arch: "amd64", CPUs: 8, MemBytes: 16 << 30, RemoteIP: "203.0.113.9",
	}
}

func TestMintReturnsCodeAndOneLiners(t *testing.T) {
	e := newEnv(t)

	m, err := e.svc.Mint("monitor", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Token.Code) != codeLen || strings.Trim(m.Token.Code, codeAlphabet) != "" {
		t.Errorf("code %q: want %d characters from the unambiguous alphabet", m.Token.Code, codeLen)
	}
	if got := m.Token.ExpiresAt.Sub(m.Token.CreatedAt); got != 10*time.Minute {
		t.Errorf("token lifetime = %v, want 10m", got)
	}
	if len(m.Token.Token) < 40 {
		t.Errorf("token %q is too short to be a secret", m.Token.Token)
	}
	for name, line := range map[string]string{"linux": m.Linux, "macos": m.MacOS} {
		if !strings.Contains(line, "curl ") || !strings.HasSuffix(line, "| sudo bash") ||
			!strings.Contains(line, "https://127.0.0.1:7445/enroll/e/"+m.Token.Code) ||
			!strings.Contains(line, "--pinnedpubkey 'sha256//"+e.svc.Pin()+"'") {
			t.Errorf("%s one-liner is not a pinned curl|sudo bash for the code: %s", name, line)
		}
		if strings.Contains(line, m.Token.Token) {
			t.Errorf("%s one-liner carries the long token; only the short code may travel in it", name)
		}
	}

	// A worker has no macOS one-liner: macOS is monitor-only.
	w, err := e.svc.Mint("worker", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if w.Linux == "" || w.MacOS != "" {
		t.Errorf("worker one-liners = linux %q macos %q, want a Linux one only", w.Linux, w.MacOS)
	}
	if w.Token.Code == m.Token.Code || w.Token.Token == m.Token.Token {
		t.Error("two mints returned the same credentials")
	}
	if _, err := e.svc.Mint("admin", "alice"); !errors.Is(err, ErrBadRequest) {
		t.Errorf("Mint(admin) err = %v, want ErrBadRequest", err)
	}
}

func TestTokenIsSingleUse(t *testing.T) {
	e := newEnv(t)
	m, _ := e.svc.Mint("worker", "alice")
	c1, _ := csr(t)
	c2, _ := csr(t)

	res, err := e.svc.Enroll(linuxReq(m.Token.Token, c1))
	if err != nil {
		t.Fatalf("first redemption: %v", err)
	}
	if _, err := e.svc.Enroll(linuxReq(m.Token.Token, c2)); !errors.Is(err, ErrTokenUsed) {
		t.Errorf("second redemption err = %v, want ErrTokenUsed", err)
	}
	if got := len(e.reg.List()); got != 1 {
		t.Errorf("registry has %d machines after a refused second redemption, want 1", got)
	}
	if res.MachineID != "build-box-7" {
		t.Errorf("machine id = %q, want one derived from the host name", res.MachineID)
	}
	// The token no longer authorises downloads either.
	if _, err := e.svc.tokens.check(m.Token.Token, false); !errors.Is(err, ErrTokenUsed) {
		t.Errorf("check after use = %v, want ErrTokenUsed", err)
	}
	// The short code cannot redeem, only fetch the script.
	m2, _ := e.svc.Mint("worker", "alice")
	if _, err := e.svc.Enroll(linuxReq(m2.Token.Code, c2)); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("redeeming with the short code err = %v, want ErrTokenInvalid", err)
	}
	if _, err := e.svc.Enroll(linuxReq("nonsense", c2)); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("unknown token err = %v, want ErrTokenInvalid", err)
	}
}

func TestTokenExpires(t *testing.T) {
	e := newEnv(t)
	m, _ := e.svc.Mint("worker", "alice")
	c, _ := csr(t)

	e.clk.advance(9*time.Minute + 59*time.Second)
	if _, err := e.svc.tokens.check(m.Token.Token, false); err != nil {
		t.Fatalf("token should still be valid just before 10 minutes: %v", err)
	}
	e.clk.advance(2 * time.Second)
	if _, err := e.svc.Enroll(linuxReq(m.Token.Token, c)); !errors.Is(err, ErrTokenExpired) {
		t.Errorf("redeeming an expired token err = %v, want ErrTokenExpired", err)
	}
	if len(e.reg.List()) != 0 {
		t.Error("an expired token registered a machine")
	}
}

func TestEnrollBindsCertToMachineAndRecordsSpecs(t *testing.T) {
	e := newEnv(t)
	m, _ := e.svc.Mint("worker", "alice")
	csrPEM, key := csr(t)

	res, err := e.svc.Enroll(linuxReq(m.Token.Token, csrPEM))
	if err != nil {
		t.Fatal(err)
	}

	// The certificate is for the machine's own key (it never left the machine)
	// and carries the id the server assigned, not what the request claimed.
	block, _ := pem.Decode(res.CertPEM)
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		t.Error("the certificate is not for the machine's own public key")
	}
	if cert.Subject.CommonName != res.MachineID || len(cert.DNSNames) != 1 || cert.DNSNames[0] != res.MachineID {
		t.Errorf("certificate names CN %q SANs %v, want only %q", cert.Subject.CommonName, cert.DNSNames, res.MachineID)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(e.ca.CertPEM())
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("certificate does not verify against the CA as a client certificate: %v", err)
	}
	// ...and it works as the agent's TLS identity with the machine's key.
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if _, err := pki.ServerTLSConfig(res.CertPEM, keyPEM, e.ca.CertPEM()); err != nil {
		t.Errorf("certificate and the machine's key do not form a TLS identity: %v", err)
	}

	got, ok := e.reg.Get(res.MachineID)
	if !ok {
		t.Fatal("machine is not in the registry")
	}
	sum := sha256.Sum256(cert.Raw)
	if got.Role != "worker" || got.ConnectMode != registry.ConnectModeAgentDial || !got.Provisioned ||
		got.EnrolledVia != registry.EnrolledToken || got.CertFingerprint != res.Fingerprint ||
		res.Fingerprint != hexOf(sum[:]) {
		t.Errorf("registry entry = %+v", got)
	}
	if sp := got.Specs; sp == nil || sp.Hostname != "Build Box 7" || sp.OS != "linux" || sp.Arch != "amd64" ||
		sp.CPUs != 8 || sp.MemBytes != 16<<30 || sp.OSVersion != "Ubuntu 24.04 LTS" || sp.Kernel != "6.8.0" {
		t.Errorf("specs = %+v", got.Specs)
	}
	if got.PublicIP != "203.0.113.9" {
		t.Errorf("public ip = %q, want the address the request came from", got.PublicIP)
	}
	if e.persists != 1 || e.reconcile != 1 {
		t.Errorf("persisted %d times, reconciled %d times; want 1 and 1", e.persists, e.reconcile)
	}
	if err := e.reg.Validate(); err != nil {
		t.Errorf("registry no longer validates: %v", err)
	}

	// A second machine with the same host name gets its own id.
	m2, _ := e.svc.Mint("monitor", "alice")
	c2, _ := csr(t)
	res2, err := e.svc.Enroll(linuxReq(m2.Token.Token, c2))
	if err != nil {
		t.Fatal(err)
	}
	if res2.MachineID == res.MachineID {
		t.Errorf("two machines share the id %q", res.MachineID)
	}
}

func hexOf(b []byte) string {
	const h = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, h[x>>4], h[x&15])
	}
	return string(out)
}

func TestEnrollRefusesBadRequestsWithoutBurningTheToken(t *testing.T) {
	e := newEnv(t)
	m, _ := e.svc.Mint("monitor", "alice")

	if _, err := e.svc.Enroll(linuxReq(m.Token.Token, []byte("not a csr"))); !errors.Is(err, ErrBadRequest) {
		t.Errorf("garbage CSR err = %v, want ErrBadRequest", err)
	}
	// A request signed by a different key than the one it names is a forgery.
	good, _ := csr(t)
	block, _ := pem.Decode(good)
	block.Bytes[len(block.Bytes)-3] ^= 0xff
	if _, err := e.svc.Enroll(linuxReq(m.Token.Token, pem.EncodeToMemory(block))); !errors.Is(err, ErrBadRequest) {
		t.Errorf("tampered CSR err = %v, want ErrBadRequest", err)
	}
	r := linuxReq(m.Token.Token, good)
	r.OS = "windows"
	if _, err := e.svc.Enroll(r); !errors.Is(err, ErrBadRequest) {
		t.Errorf("windows err = %v, want ErrBadRequest", err)
	}
	if len(e.reg.List()) != 0 {
		t.Fatal("a refused request registered a machine")
	}
	// The token survived all that and still works.
	if _, err := e.svc.Enroll(linuxReq(m.Token.Token, good)); err != nil {
		t.Errorf("token was burned by refused requests: %v", err)
	}
}

func TestMacOSWorkerIsRefusedMonitorAllowed(t *testing.T) {
	e := newEnv(t)
	w, _ := e.svc.Mint("worker", "alice")
	c, _ := csr(t)
	mac := linuxReq(w.Token.Token, c)
	mac.OS, mac.Arch = "darwin", "arm64"

	if _, err := e.svc.Enroll(mac); !errors.Is(err, ErrMacWorker) {
		t.Fatalf("macOS worker err = %v, want ErrMacWorker", err)
	}
	if len(e.reg.List()) != 0 {
		t.Error("a refused macOS worker was registered")
	}
	// The binary download is refused too, before anything is installed.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/enroll/agent?os=darwin&arch=arm64", nil)
	req.Header.Set("Authorization", "Bearer "+w.Token.Token)
	e.svc.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("macOS worker binary download = %d, want 400", rec.Code)
	}
	// The same token still enrolls a Linux worker.
	if _, err := e.svc.Enroll(linuxReq(w.Token.Token, c)); err != nil {
		t.Errorf("Linux worker with the same token: %v", err)
	}

	mon, _ := e.svc.Mint("monitor", "alice")
	c2, _ := csr(t)
	mm := linuxReq(mon.Token.Token, c2)
	mm.OS, mm.Hostname = "darwin", "mac-mini"
	res, err := e.svc.Enroll(mm)
	if err != nil {
		t.Fatalf("macOS monitor: %v", err)
	}
	if got, _ := e.reg.Get(res.MachineID); got.Specs.OS != "darwin" || got.Role != "monitor" {
		t.Errorf("macOS monitor entry = %+v", got)
	}
	if err := CheckRoleOS("worker", "darwin"); !errors.Is(err, ErrMacWorker) {
		t.Errorf("CheckRoleOS(worker, darwin) = %v", err)
	}
}

func enrolled(t *testing.T, e *env, role, os, host string) Result {
	t.Helper()
	m, _ := e.svc.Mint(role, "alice")
	c, _ := csr(t)
	r := linuxReq(m.Token.Token, c)
	r.OS, r.Hostname = os, host
	res, err := e.svc.Enroll(r)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestStopDisablesTheUnit(t *testing.T) {
	e := newEnv(t)
	lin := enrolled(t, e, "worker", "linux", "lin")
	mac := enrolled(t, e, "monitor", "darwin", "mac")
	_ = e.reg.Update(lin.MachineID, func(m *registry.Machine) { m.Online = false })

	if err := e.svc.Stop(context.Background(), lin.MachineID); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Stop(context.Background(), mac.MachineID); err != nil {
		t.Fatal(err)
	}
	want := [][2]string{
		{lin.MachineID, "systemctl disable --now --no-block fleet-agent.service"},
		{mac.MachineID, "launchctl disable system/com.fleet.agent && launchctl bootout system/com.fleet.agent"},
	}
	if len(e.runner.runs) != 2 || e.runner.runs[0] != want[0] || e.runner.runs[1] != want[1] {
		t.Errorf("commands sent = %q, want %q", e.runner.runs, want)
	}
	// Disable (not just stop): the command survives a reboot.
	if !strings.Contains(StopCommand(OSLinux), "disable") || !strings.Contains(StopCommand(OSDarwin), "launchctl disable") {
		t.Error("the stop commands do not disable the service")
	}

	var stopped *Machine
	for _, m := range e.svc.Machines() {
		m := m
		if m.ID == lin.MachineID {
			stopped = &m
		}
	}
	if stopped == nil || !stopped.Stopped {
		t.Fatalf("stopped machine = %+v, want it listed as stopped", stopped)
	}
	// A local `fleet-agent start` brings it back online: it is no longer "stopped".
	_ = e.reg.SetOnline(lin.MachineID, true)
	for _, m := range e.svc.Machines() {
		if m.ID == lin.MachineID && m.Stopped {
			t.Error("an agent that is back online is still shown as stopped")
		}
	}

	// A failure to reach the agent leaves the state alone.
	e.runner.err = errors.New("offline")
	other := enrolled(t, e, "worker", "linux", "other")
	if err := e.svc.Stop(context.Background(), other.MachineID); err == nil {
		t.Error("Stop with an unreachable agent succeeded")
	}
	if got, _ := e.reg.Get(other.MachineID); got.Stopped {
		t.Error("a failed stop marked the machine stopped")
	}
	// A monitor over the control channel cannot be told anything.
	e.runner.err = ErrStopUnsupported
	if err := e.svc.Stop(context.Background(), other.MachineID); !errors.Is(err, ErrStopUnsupported) {
		t.Errorf("err = %v, want ErrStopUnsupported", err)
	}
	if err := e.svc.Stop(context.Background(), "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Stop(unknown) = %v", err)
	}
}

func TestDetachRevokesAndForgets(t *testing.T) {
	e := newEnv(t)
	a := enrolled(t, e, "worker", "linux", "box")
	before := e.reconcile
	persists := e.persists

	block, _ := pem.Decode(a.CertPEM)
	cert, _ := x509.ParseCertificate(block.Bytes)
	if e.svc.Revoked(cert) {
		t.Fatal("a live certificate is reported revoked")
	}

	if err := e.svc.Detach(context.Background(), a.MachineID); err != nil {
		t.Fatal(err)
	}
	got, _ := e.reg.Get(a.MachineID)
	if !got.Removed || got.Provisioned || got.RevokedFingerprint != a.Fingerprint {
		t.Errorf("detached entry = %+v, want removed, de-provisioned, fingerprint recorded for revocation", got)
	}
	if !e.svc.Revoked(cert) {
		t.Error("the detached machine's certificate is not reported revoked")
	}
	if e.reconcile != before+1 || e.persists != persists+1 {
		t.Errorf("reconciled %d (want %d) persisted %d (want %d): the link must be dropped and the revocation saved",
			e.reconcile, before+1, e.persists, persists+1)
	}
	if len(e.svc.Machines()) != 0 {
		t.Error("a detached machine is still listed")
	}
	// Detached means detached: no Stop, no second Detach.
	if err := e.svc.Detach(context.Background(), a.MachineID); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("second Detach = %v, want ErrNotEnrolled", err)
	}
	if err := e.svc.Stop(context.Background(), a.MachineID); !errors.Is(err, ErrNotEnrolled) {
		t.Errorf("Stop after Detach = %v, want ErrNotEnrolled", err)
	}
	// Re-adding needs a fresh token and yields a new identity, never the old id.
	b := enrolled(t, e, "worker", "linux", "box")
	if b.MachineID == a.MachineID || b.Fingerprint == a.Fingerprint {
		t.Errorf("re-enrolled machine reused identity %q / %s", b.MachineID, b.Fingerprint)
	}
	if e.svc.Revoked(parse(t, b.CertPEM)) {
		t.Error("the new certificate is revoked")
	}
	if err := e.svc.Detach(context.Background(), "nope"); !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("Detach(unknown) = %v", err)
	}
}

func parse(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRenderedService(t *testing.T) {
	unit := RenderSystemdUnit()
	for _, want := range []string{"Restart=always", "WantedBy=multi-user.target", "ExecStart=" + BinaryPath + " --config " + ConfigDir + "/config.json", "User=root"} {
		if !strings.Contains(unit, want) {
			t.Errorf("systemd unit lacks %q:\n%s", want, unit)
		}
	}
	plist := RenderLaunchdPlist()
	for _, want := range []string{"<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>", "<string>" + LaunchdLabel + "</string>", BinaryPath} {
		if !strings.Contains(plist, want) {
			t.Errorf("launchd plist lacks %q", want)
		}
	}
	// The baked config is accepted by the agent's own loader, with the role,
	// connect_mode and server address in it.
	cfgText := strings.ReplaceAll(RenderAgentConfig("monitor", "fleet.example:7444", "fleet-server"), machineIDPlaceholder, "m1")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ac := cfg.Agent
	if err := ac.Validate(); err != nil {
		t.Fatal(err)
	}
	if ac.Role != "monitor" || !ac.Dials() || ac.ServerAddr != "fleet.example:7444" || ac.MachineID != "m1" ||
		ac.CertFile != ConfigDir+"/agent.crt" || ac.KeyFile != ConfigDir+"/agent.key" || ac.CAFile != ConfigDir+"/ca.crt" {
		t.Errorf("agent config = %+v", ac)
	}
	// The wrapper maps start/stop to the same commands Stop sends.
	wrap := RenderWrapper()
	for _, c := range []string{StopCommand(OSLinux), StartCommand(OSLinux), StopCommand(OSDarwin), StartCommand(OSDarwin)} {
		if !strings.Contains(wrap, c) {
			t.Errorf("wrapper lacks %q", c)
		}
	}
}

func TestScriptIsValidShellAndRefusesMacWorker(t *testing.T) {
	e := newEnv(t)
	m, _ := e.svc.Mint("worker", "alice")
	script, err := e.svc.Script("worker", m.Token.Token)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"--pinnedpubkey", "sha256//" + e.svc.Pin(), e.svc.caFP, "ROLE='worker'", "SERVER_ADDR='127.0.0.1:7444'",
		"macOS machines can only be monitor agents", "id -u", "systemctl enable " + SystemdUnit, "launchctl bootstrap",
		"Restart=always", "KeepAlive", "agent.key", "openssl req -new",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	// The private key is generated on the machine and never sent.
	if strings.Contains(script, "data-urlencode \"key") || strings.Contains(script, "agent.key\" -X") {
		t.Error("the script appears to send the key")
	}
	// The read-only view carries no token.
	view, _ := e.svc.Script("worker", "")
	if strings.Contains(view, m.Token.Token) || !strings.Contains(view, "one-time token") {
		t.Error("the show-script view leaks a token or lacks the placeholder")
	}
	if _, err := e.svc.Script("nope", ""); !errors.Is(err, ErrBadRequest) {
		t.Errorf("Script(nope) = %v", err)
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		for _, s := range []string{script, view} {
			cmd := exec.Command(bash, "-n")
			cmd.Stdin = strings.NewReader(s)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("bash -n rejects the script: %v\n%s", err, out)
			}
		}
	}
}

// ---- the HTTP endpoint, end to end --------------------------------------------

// pinnedClient is what the curl one-liner does: TLS that trusts nothing but the
// endpoint's pinned public key.
func pinnedClient(pin string) *http.Client {
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
		InsecureSkipVerify: true, // trust comes from the pin below, as with curl -k --pinnedpubkey
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
			if base64.StdEncoding.EncodeToString(sum[:]) != pin {
				return errors.New("pin mismatch")
			}
			return nil
		},
	}}}
}

func startEndpoint(t *testing.T, e *env) (base string, sawBody func() string) {
	t.Helper()
	ln, err := e.svc.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.svc.cfg.PublicHost, e.svc.cfg.EnrollPort = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
	var mu sync.Mutex
	var bodies []string
	h := e.svc.Handler()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(b))
		h.ServeHTTP(w, r)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String(), func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.Join(bodies, "\n")
	}
}

func get(t *testing.T, c *http.Client, u, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", u, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestEndpointFullFlowOverPinnedTLS(t *testing.T) {
	e := newEnv(t)
	base, sawBody := startEndpoint(t, e)
	c := pinnedClient(e.svc.Pin())
	m, _ := e.svc.Mint("worker", "alice")

	// The one-liner's request: the code fetches the script, which carries the token.
	code, script := get(t, c, base+"/enroll/e/"+strings.ToLower(m.Token.Code), "")
	if code != 200 || !strings.Contains(script, "TOKEN='"+m.Token.Token+"'") {
		t.Fatalf("script by code = %d, token in script: %v", code, strings.Contains(script, m.Token.Token))
	}
	// The CA and the agent need the token; they do not consume it.
	if code, _ := get(t, c, base+"/enroll/ca.crt", ""); code != http.StatusUnauthorized {
		t.Errorf("ca.crt without a token = %d, want 401", code)
	}
	if code, _ := get(t, c, base+"/enroll/agent?os=linux&arch=amd64", m.Token.Code); code != http.StatusUnauthorized {
		t.Errorf("agent with the short code = %d, want 401 (only the secret authorises downloads)", code)
	}
	code, ca := get(t, c, base+"/enroll/ca.crt", m.Token.Token)
	if code != 200 || ca != string(e.ca.CertPEM()) {
		t.Errorf("ca.crt = %d, matches CA: %v", code, ca == string(e.ca.CertPEM()))
	}
	code, bin := get(t, c, base+"/enroll/agent?os=linux&arch=amd64", m.Token.Token)
	if code != 200 || bin != "#!fake-agent-binary" {
		t.Errorf("agent = %d %q", code, bin)
	}
	if code, _ := get(t, c, base+"/enroll/agent?os=linux&arch=arm64", m.Token.Token); code != http.StatusNotFound {
		t.Errorf("agent for a platform with no binary = %d, want 404", code)
	}
	if code, _ := get(t, c, base+"/enroll/agent?os=../../etc&arch=amd64", m.Token.Token); code != http.StatusBadRequest {
		t.Errorf("agent with a path in os = %d, want 400", code)
	}

	// The exchange: CSR + specs + token in, id + certificate out.
	csrPEM, key := csr(t)
	form := url.Values{"csr": {string(csrPEM)}, "hostname": {"edge-1"}, "os": {"linux"}, "arch": {"amd64"},
		"os_version": {"Debian 12"}, "kernel": {"6.1"}, "cpus": {"4"}, "mem_bytes": {"8589934592"}}
	post := func(token string) (int, string) {
		req, _ := http.NewRequest("POST", base+"/enroll/csr", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	status, body := post(m.Token.Token)
	if status != 200 {
		t.Fatalf("csr = %d %s", status, body)
	}
	id, certPEM, _ := strings.Cut(body, "\n")
	if id != "edge-1" {
		t.Errorf("assigned id = %q", id)
	}
	if cert := parse(t, []byte(certPEM)); !key.PublicKey.Equal(cert.PublicKey) || cert.Subject.CommonName != "edge-1" {
		t.Error("the returned certificate is not for the machine's key and assigned id")
	}
	if got, ok := e.reg.Get("edge-1"); !ok || got.Specs == nil || got.Specs.CPUs != 4 || got.Specs.MemBytes != 8<<30 || got.Specs.OSVersion != "Debian 12" {
		t.Errorf("registry entry = %+v", got)
	}
	// Nothing sent contains a private key.
	if strings.Contains(sawBody(), "PRIVATE KEY") {
		t.Error("a private key crossed the wire")
	}
	// The token is spent: a second redemption and further downloads are refused.
	if status, _ := post(m.Token.Token); status != http.StatusUnauthorized {
		t.Errorf("second redemption over HTTP = %d, want 401", status)
	}
	if code, _ := get(t, c, base+"/enroll/ca.crt", m.Token.Token); code != http.StatusUnauthorized {
		t.Errorf("download with a spent token = %d, want 401", code)
	}
	if code, _ := get(t, c, base+"/enroll/e/"+m.Token.Code, ""); code != http.StatusUnauthorized {
		t.Errorf("script by a spent code = %d, want 401", code)
	}

	// A client that does not hold the pin (a man in the middle) cannot even connect.
	if _, err := pinnedClient("AAAA").Get(base + "/enroll/ca.crt"); err == nil {
		t.Error("a client with the wrong pin connected to the endpoint")
	}
}

func TestEndpointLimitsGuessing(t *testing.T) {
	e := newEnv(t)
	base, _ := startEndpoint(t, e)
	c := pinnedClient(e.svc.Pin())
	m, _ := e.svc.Mint("monitor", "alice")

	for i := 0; i < failLimit; i++ {
		if code, _ := get(t, c, base+"/enroll/e/AAAAAAAA", ""); code != http.StatusUnauthorized {
			t.Fatalf("guess %d = %d, want 401", i, code)
		}
	}
	// Past the limit even the right code is refused, until the window passes.
	if code, _ := get(t, c, base+"/enroll/e/"+m.Token.Code, ""); code != http.StatusTooManyRequests {
		t.Errorf("after %d failures = %d, want 429", failLimit, code)
	}
	e.clk.advance(failWindow + time.Second)
	if code, _ := get(t, c, base+"/enroll/e/"+m.Token.Code, ""); code != http.StatusOK {
		t.Errorf("after the window = %d, want 200", code)
	}
}

func TestOperatorAPI(t *testing.T) {
	e := newEnv(t)
	h := e.svc.APIHandler()
	do := func(method, path, ct, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := do("POST", "/api/enroll/tokens", "text/plain", `{"role":"worker"}`); rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("non-JSON POST = %d, want 415", rec.Code)
	}
	if rec := do("POST", "/api/enroll/tokens", "application/json", `{"role":"admin"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bad role = %d, want 400", rec.Code)
	}
	rec := do("POST", "/api/enroll/tokens", "application/json", `{"role":"monitor"}`)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"code":"`) || !strings.Contains(rec.Body.String(), "sudo bash") ||
		!strings.Contains(rec.Body.String(), `"macos":"curl`) || !strings.Contains(rec.Body.String(), `"ttl_seconds":600`) {
		t.Errorf("mint = %d %s", rec.Code, rec.Body)
	}
	for tok := range e.svc.tokens.tokens {
		if strings.Contains(rec.Body.String(), tok) {
			t.Error("the mint response contains the long token")
		}
	}
	if rec := do("POST", "/api/enroll/tokens", "application/json", `{"role":"worker"}`); strings.Contains(rec.Body.String(), `"macos"`) {
		t.Errorf("a worker mint offers a macOS one-liner: %s", rec.Body)
	}
	if rec := do("GET", "/api/enroll/script?role=worker", "", ""); rec.Code != 200 || !strings.HasPrefix(rec.Body.String(), "#!/bin/bash") {
		t.Errorf("script = %d", rec.Code)
	}
	if rec := do("GET", "/api/enroll/script?role=x", "", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("script(bad role) = %d", rec.Code)
	}

	r := enrolled(t, e, "worker", "linux", "api-box")
	rec = do("GET", "/api/enroll/machines", "", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"api-box"`) || !strings.Contains(rec.Body.String(), `"cpus":8`) ||
		!strings.Contains(rec.Body.String(), `"online":false`) || !strings.Contains(rec.Body.String(), `"stopped":false`) {
		t.Errorf("machines = %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/enroll/machines/"+r.MachineID+"/stop", "application/json", `{}`); rec.Code != 200 {
		t.Errorf("stop = %d %s", rec.Code, rec.Body)
	}
	e.runner.err = ErrStopUnsupported
	if rec := do("POST", "/api/enroll/machines/"+r.MachineID+"/stop", "application/json", `{}`); rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), "fleet-agent stop") {
		t.Errorf("unsupported stop = %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/enroll/machines/"+r.MachineID+"/detach", "application/json", `{}`); rec.Code != 200 {
		t.Errorf("detach = %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/enroll/machines/"+r.MachineID+"/detach", "application/json", `{}`); rec.Code != http.StatusConflict {
		t.Errorf("second detach = %d", rec.Code)
	}
	if rec := do("POST", "/api/enroll/machines/zzz/detach", "application/json", `{}`); rec.Code != http.StatusNotFound {
		t.Errorf("detach unknown = %d", rec.Code)
	}
}

func TestBinaryDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fleet-agent-darwin-arm64"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	b := BinaryDir(dir, "/opt/fleet-agent", "linux", "amd64")
	if p, err := b("darwin", "arm64"); err != nil || filepath.Base(p) != "fleet-agent-darwin-arm64" {
		t.Errorf("darwin/arm64 = %q %v", p, err)
	}
	if p, err := b("linux", "amd64"); err != nil || p != "/opt/fleet-agent" {
		t.Errorf("host fallback = %q %v", p, err)
	}
	for _, bad := range [][2]string{{"linux", "arm64"}, {"windows", "amd64"}, {"../x", "amd64"}, {"linux", "../x"}} {
		if p, err := b(bad[0], bad[1]); err == nil {
			t.Errorf("%v resolved to %q", bad, p)
		}
	}
}

func TestNewValidates(t *testing.T) {
	ca, _ := pki.NewCA(pki.Options{})
	if _, err := New(Config{CA: ca, Registry: registry.New()}); err == nil {
		t.Error("New without a public host succeeded")
	}
	if _, err := New(Config{PublicHost: "h", EnrollPort: 1, AgentDialPort: 2}); err == nil {
		t.Error("New without a CA succeeded")
	}
	// A host name (not an IP) is a DNS name in the endpoint certificate.
	s, err := New(Config{CA: ca, Registry: registry.New(), PublicHost: "fleet.example.com", EnrollPort: 7445, AgentDialPort: 7444})
	if err != nil {
		t.Fatal(err)
	}
	if leaf := parse(t, pemOf(s.tlsCert.Certificate[0])); leaf.DNSNames[0] != "fleet.example.com" {
		t.Errorf("endpoint cert names %v", leaf.DNSNames)
	}
	_ = net.IPv4len
}

func pemOf(der []byte) []byte { return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}) }

func TestServeUpdateAgentAndSHA256(t *testing.T) {
	dir := t.TempDir()
	linuxBin := []byte("fake-linux-amd64-binary-payload")
	if err := os.WriteFile(filepath.Join(dir, "fleet-agent-linux-amd64"), linuxBin, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	e.svc.cfg.Binary = BinaryDir(dir, "", "linux", "amd64")

	// Start TLS listener using the service's TLSConfig (VerifyClientCertIfGiven).
	ln, err := e.svc.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = e.svc.Serve(ctx, ln) }()

	baseURL := "https://" + ln.Addr().String()
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(e.ca.CertPEM())

	// Add an active machine and issue its client cert, plus a removed machine.
	if err := e.reg.Add(registry.Machine{ID: "w-upd", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeAgentDial, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := e.ca.IssuePeer("w-upd", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	clientCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	mTLSClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				RootCAs:            pool,
				Certificates:       []tls.Certificate{clientCert},
				InsecureSkipVerify: true, // test listener cert SAN is PublicHost, not 127.0.0.1
			},
		},
	}

	// Unauthenticated client (no cert) -> 401.
	noCertClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	resp, err := noCertClient.Get(baseURL + "/enroll/update/agent?os=linux&arch=amd64")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated update agent = %d, want 401", resp.StatusCode)
	}

	// Authenticated client -> 200 + matching SHA256 header & body.
	resp, err = mTLSClient.Get(baseURL + "/enroll/update/agent?os=linux&arch=amd64")
	if err != nil {
		t.Fatal(err)
	}
	gotBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(gotBody) != string(linuxBin) {
		t.Fatalf("update agent = %d %q, want 200 %q", resp.StatusCode, gotBody, linuxBin)
	}
	info, err := ResolveBinary(e.svc.cfg.Binary, "linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if gotHdr := resp.Header.Get("X-Checksum-Sha256"); gotHdr != info.SHA256 {
		t.Errorf("X-Checksum-Sha256 = %q, want %q", gotHdr, info.SHA256)
	}

	// SHA256 endpoint returns "<hex>  fleet-agent-linux-amd64".
	resp, err = mTLSClient.Get(baseURL + "/enroll/update/agent.sha256?os=linux&arch=amd64")
	if err != nil {
		t.Fatal(err)
	}
	sumBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(sumBody), info.SHA256) {
		t.Fatalf("update sha256 = %d %q, want %s", resp.StatusCode, sumBody, info.SHA256)
	}

	// Missing platform binary -> 404.
	resp, err = mTLSClient.Get(baseURL + "/enroll/update/agent?os=darwin&arch=arm64")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing platform = %d, want 404", resp.StatusCode)
	}

	// Removed machine -> 403.
	_ = e.reg.Update("w-upd", func(m *registry.Machine) { m.Removed = true })
	resp, err = mTLSClient.Get(baseURL + "/enroll/update/agent?os=linux&arch=amd64")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("removed machine = %d, want 403", resp.StatusCode)
	}
}

func TestTokenStatusLinksMachine(t *testing.T) {
	e := newEnv(t)
	h := e.svc.APIHandler()
	get := func(code string) (int, string) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/enroll/tokens/"+code, nil))
		return rec.Code, rec.Body.String()
	}
	m, _ := e.svc.Mint("worker", "alice")
	if c, b := get(m.Token.Code); c != 200 || !strings.Contains(b, `"state":"pending"`) || strings.Contains(b, "machine_id") {
		t.Errorf("pending = %d %s", c, b)
	}
	c1, _ := csr(t)
	res, err := e.svc.Enroll(linuxReq(m.Token.Token, c1))
	if err != nil {
		t.Fatal(err)
	}
	if c, b := get(strings.ToLower(m.Token.Code)); c != 200 || !strings.Contains(b, `"state":"used"`) || !strings.Contains(b, `"machine_id":"`+res.MachineID+`"`) {
		t.Errorf("used = %d %s", c, b)
	}
	m2, _ := e.svc.Mint("worker", "alice")
	e.clk.advance(11 * time.Minute)
	if c, b := get(m2.Token.Code); c != 200 || !strings.Contains(b, `"state":"expired"`) {
		t.Errorf("expired = %d %s", c, b)
	}
	if c, _ := get("NOPE"); c != 404 {
		t.Errorf("unknown = %d, want 404", c)
	}
}
