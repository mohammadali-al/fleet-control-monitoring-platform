package fleet_test

import (
	"context"
	"net"
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
