package fleet_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/pki"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

type realAgent struct {
	ca   *pki.CA
	gate *exec.Gate
	l    *channel.Listener
	ctx  context.Context
}

// startRealAgent runs an agent (certificate CN = machine) listening on loopback.
func startRealAgent(t *testing.T, machine string) *realAgent {
	t.Helper()
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	loop := []net.IP{net.ParseIP("127.0.0.1")}
	agentCert, agentKey, err := ca.IssuePeer(machine, loop, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srvTLS, err := pki.ServerTLSConfig(agentCert, agentKey, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("shared-secret"), []string{`DANGER`})
	if err != nil {
		t.Fatal(err)
	}
	var ex *exec.Executor
	l, err := channel.Listen(channel.ListenerConfig{
		Addr: "127.0.0.1:0", TLS: srvTLS, MachineID: machine,
		Handler: func(ctx context.Context, env channel.Envelope) { ex.Handle(ctx, env) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ex, err = exec.New(exec.Config{MachineID: machine, Sender: l, Gate: gate, TermGrace: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = l.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served; ex.Shutdown() })
	return &realAgent{ca: ca, gate: gate, l: l, ctx: ctx}
}

func (a *realAgent) manager(t *testing.T, machineID string) *fleet.Manager {
	t.Helper()
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: machineID, Role: registry.RoleWorker, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	links, err := fleet.NewDialerFactory(a.ca, "fleet-server", a.l.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fleet.New(fleet.Config{Registry: reg, Gate: a.gate, NewLink: links, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	m.Start(a.ctx)
	t.Cleanup(m.Close)
	return m
}

// The production path: the manager dials a real agent listener over mTLS with
// the production LinkFactory, and commands go through the real executor and
// its gate (same shared secret on both sides).
func TestManagerOverRealChannel(t *testing.T) {
	const machine = "agent-7"
	a := startRealAgent(t, machine)
	m := a.manager(t, machine)

	eventually(t, "agent online over mTLS", func() bool {
		info, _ := m.Machine(machine)
		return info.State == fleet.StateOnline
	})
	sub := m.Bus().Subscribe(nil)
	defer sub.Close()
	id, err := m.Submit(fleet.SubmitRequest{Machine: machine, Command: "echo over-mtls; exit 5", User: "op"})
	if err != nil {
		t.Fatal(err)
	}
	_, out, x := collect(t, sub, id)
	if x.ExitCode != 5 || !strings.Contains(out, "over-mtls") {
		t.Errorf("exit %+v output %q", x, out)
	}
	id, err = m.Submit(fleet.SubmitRequest{Machine: machine, Command: "echo DANGER", Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, x := collect(t, sub, id); x.Reason != exec.ReasonExited || x.ExitCode != 0 {
		t.Errorf("confirmed command over mTLS: %+v", x)
	}
}

// The registry decides who a peer must be: a valid certificate for another
// identity connects at the TLS level but never shows as the registered machine.
func TestIdentityMismatchStaysOffline(t *testing.T) {
	a := startRealAgent(t, "agent-7")
	m := a.manager(t, "someone-else")
	eventually(t, "the dialer to connect", func() bool { return a.l.Peer() != "" })
	time.Sleep(200 * time.Millisecond)
	if info, _ := m.Machine("someone-else"); info.State == fleet.StateOnline {
		t.Error("a machine whose registry ID differs from the certificate's identity came online")
	}
	if _, err := m.Submit(fleet.SubmitRequest{Machine: "someone-else", Command: "true"}); err == nil {
		t.Error("a command was accepted for a machine that is not who the registry says")
	}
}

func TestOpenCreatesAndReusesCAAndGateKey(t *testing.T) {
	dir := t.TempDir()
	open := func() (*fleet.Manager, error) {
		return fleet.Open(fleet.Options{Config: config.ServerConfig{}, DataDir: dir})
	}
	if _, err := open(); err != nil {
		t.Fatal(err)
	}
	caCert := filepath.Join(dir, config.CADirName, pki.CACertFile)
	caKey := filepath.Join(dir, config.CADirName, pki.CAKeyFile)
	gateKey := filepath.Join(dir, config.ConfigDirName, "gate.key")
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	c1, k1, g1 := read(caCert), read(caKey), read(gateKey)
	if st, _ := os.Stat(caKey); st.Mode().Perm() != 0o600 {
		t.Errorf("CA key mode = %v, want 0600", st.Mode().Perm())
	}
	if st, _ := os.Stat(gateKey); st.Mode().Perm() != 0o600 {
		t.Errorf("gate key mode = %v, want 0600", st.Mode().Perm())
	}
	if len(strings.TrimSpace(g1)) < 32 {
		t.Errorf("gate key looks too short: %d chars", len(strings.TrimSpace(g1)))
	}

	// A second start reuses everything: the identity must not change under the agents.
	if _, err := open(); err != nil {
		t.Fatal(err)
	}
	if read(caCert) != c1 || read(caKey) != k1 || read(gateKey) != g1 {
		t.Error("restart replaced the CA or the gate key")
	}

	// A half-present CA is an error, never silently replaced.
	if err := os.Remove(caKey); err != nil {
		t.Fatal(err)
	}
	if _, err := open(); err == nil {
		t.Error("Open replaced or accepted a CA with a missing key")
	}
	if _, err := os.Stat(caKey); err == nil {
		t.Error("Open wrote a new CA key over a half-present CA")
	}
}

func TestOpenLoadsPersistedRegistry(t *testing.T) {
	dir := t.TempDir()
	store := registry.NewStore(dir)
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "kept", Role: registry.RoleWorker, PrivateIPs: []string{"10.1.1.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	m, err := fleet.Open(fleet.Options{Config: config.ServerConfig{ChannelPort: 1}, DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	got := m.Machines()
	if len(got) != 1 || got[0].ID != "kept" || got[0].State != fleet.StateOffline {
		t.Errorf("machines = %+v", got)
	}
}

// GAP-05: the server's channel certificate must carry IP SANs (mirroring
// agent certs) while remaining compatible with already-enrolled agents whose
// listener verifies only the CA chain and server CN.
func TestServerChannelCertHasIPSANAndStaysCompatible(t *testing.T) {
	ca, err := pki.NewCA(pki.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Pre-enrolled agent cert issued before the server cert exists.
	loop := net.ParseIP("127.0.0.1")
	agentCert, agentKey, err := ca.IssuePeer("agent-enrolled", []net.IP{loop}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	srvTLS, err := pki.ServerTLSConfig(agentCert, agentKey, ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}

	var peerCert *x509.Certificate
	origVerify := srvTLS.VerifyPeerCertificate
	srvTLS.VerifyPeerCertificate = func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(verifiedChains) > 0 && len(verifiedChains[0]) > 0 {
			peerCert = verifiedChains[0][0]
		}
		if origVerify != nil {
			return origVerify(rawCerts, verifiedChains)
		}
		return nil
	}

	gate, err := exec.NewGate([]byte("shared-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var ex *exec.Executor
	ln, err := channel.Listen(channel.ListenerConfig{
		Addr: "127.0.0.1:0", TLS: srvTLS, MachineID: "agent-enrolled",
		Handler: func(ctx context.Context, env channel.Envelope) { ex.Handle(ctx, env) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ex, err = exec.New(exec.Config{MachineID: "agent-enrolled", Sender: ln, Gate: gate, TermGrace: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() { defer close(served); _ = ln.Serve(ctx) }()
	t.Cleanup(func() { cancel(); <-served; ex.Shutdown() })

	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "agent-enrolled", Role: registry.RoleWorker, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	links, err := fleet.NewDialerFactory(ca, "fleet-server", ln.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	m, err := fleet.New(fleet.Config{Registry: reg, Gate: gate, NewLink: links, PollInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	m.Start(ctx)
	t.Cleanup(m.Close)

	eventually(t, "pre-enrolled agent online with server IP SAN cert", func() bool {
		info, _ := m.Machine("agent-enrolled")
		return info.State == fleet.StateOnline
	})
	if peerCert == nil {
		t.Fatal("agent did not observe server certificate")
	}
	if peerCert.Subject.CommonName != "fleet-server" {
		t.Errorf("server cert CN = %q, want fleet-server", peerCert.Subject.CommonName)
	}
	if len(peerCert.IPAddresses) == 0 {
		t.Fatal("server cert has no IP SANs")
	}
	hasLoopback := false
	for _, ip := range peerCert.IPAddresses {
		if ip.Equal(loop) {
			hasLoopback = true
			break
		}
	}
	if !hasLoopback {
		t.Errorf("server cert IP SANs = %v, expected 127.0.0.1", peerCert.IPAddresses)
	}

	// Verify Open() also attaches configured server IPs (e.g. EnrollHost) to the
	// server channel cert while remaining compatible with pre-enrolled agents.
	dir := t.TempDir()
	store := registry.NewStore(dir)
	if err := store.Save(reg); err != nil {
		t.Fatal(err)
	}
	caDir := filepath.Join(dir, config.CADirName)
	if err := ca.Save(caDir); err != nil {
		t.Fatal(err)
	}
	peerCert = nil
	cancel()
	<-served
	ex.Shutdown()

	ln2, err := channel.Listen(channel.ListenerConfig{
		Addr: "127.0.0.1:0", TLS: srvTLS, MachineID: "agent-enrolled",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	served2 := make(chan struct{})
	go func() { defer close(served2); _ = ln2.Serve(ctx2) }()
	t.Cleanup(func() { cancel2(); <-served2 })

	m2, err := fleet.Open(fleet.Options{
		Config:  config.ServerConfig{EnrollHost: "192.0.2.55", ChannelPort: ln2.Addr().(*net.TCPAddr).Port},
		DataDir: dir,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	m2.Start(ctx2)
	t.Cleanup(m2.Close)
	eventually(t, "Open() server cert observed by pre-enrolled agent", func() bool {
		info, _ := m2.Machine("agent-enrolled")
		return info.State == fleet.StateOnline
	})
	if peerCert == nil {
		t.Fatal("agent did not observe Open() server certificate")
	}
	wantConfigured := net.ParseIP("192.0.2.55")
	hasConfigured := false
	for _, ip := range peerCert.IPAddresses {
		if ip.Equal(wantConfigured) {
			hasConfigured = true
			break
		}
	}
	if !hasConfigured {
		t.Errorf("Open() server cert IP SANs = %v, expected 192.0.2.55", peerCert.IPAddresses)
	}

	// Quick TLS sanity check: a client validating IP SAN via DialTLSConfig accepts
	// a cert issued with the peer's IP SAN.
	cliCert, cliKey, err := ca.IssuePeer("probe-client", []net.IP{loop}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dialTLS, err := pki.DialTLSConfig(cliCert, cliKey, ca.CertPEM(), loop)
	if err != nil {
		t.Fatal(err)
	}
	srvCert, srvKey, err := ca.IssuePeer("fleet-server", []net.IP{loop}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	ts.StartTLS()
	defer ts.Close()
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: dialTLS}}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("client validating IP SAN failed: %v", err)
	}
	resp.Body.Close()
}
