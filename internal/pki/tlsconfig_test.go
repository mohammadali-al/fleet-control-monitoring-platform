package pki

import (
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// peer is a throwaway issued identity for tests.
type peer struct{ cert, key []byte }

func issue(t *testing.T, ca *CA, id, ip string) peer {
	t.Helper()
	c, k, err := ca.IssuePeer(id, []net.IP{net.ParseIP(ip)}, time.Hour)
	if err != nil {
		t.Fatalf("IssuePeer(%s): %v", id, err)
	}
	return peer{c, k}
}

// asyncConn makes Write non-blocking on top of net.Pipe. net.Pipe is
// unbuffered, so when one side aborts a handshake and writes a TLS alert while
// the other is mid-write, both block forever. Queueing writes breaks that.
type asyncConn struct {
	net.Conn
	ch      chan []byte
	done    chan struct{}
	flushed chan struct{}
	once    sync.Once
}

func newAsyncConn(c net.Conn) *asyncConn {
	a := &asyncConn{Conn: c, ch: make(chan []byte, 256), done: make(chan struct{}), flushed: make(chan struct{})}
	go func() {
		defer close(a.flushed)
		for {
			select {
			case b := <-a.ch:
				if _, err := a.Conn.Write(b); err != nil {
					return
				}
			case <-a.done:
				// Closing: flush whatever is still queued, then stop.
				for {
					select {
					case b := <-a.ch:
						if _, err := a.Conn.Write(b); err != nil {
							return
						}
					default:
						return
					}
				}
			}
		}
	}()
	return a
}

func (a *asyncConn) Write(b []byte) (int, error) {
	cp := append([]byte(nil), b...)
	select {
	case a.ch <- cp:
		return len(b), nil
	case <-a.done:
		return 0, net.ErrClosed
	}
}

func (a *asyncConn) Close() error {
	a.once.Do(func() { close(a.done) })
	<-a.flushed // bounded by the pipe deadline set in handshake
	return a.Conn.Close()
}

// handshake runs a TLS handshake between srv and cli over an in-memory pipe
// and returns each side's outcome plus the connection states.
//
// TLS 1.3 detail: the client finishes its handshake before the server has
// verified the client's certificate, so a client-cert rejection surfaces on
// the client only at its first Read. Each side therefore does one Read/Write
// after Handshake, which also drains the server's session tickets (net.Pipe
// is unbuffered and would otherwise block).
func handshake(t *testing.T, srv, cli *tls.Config) (srvErr, cliErr error, srvState, cliState tls.ConnectionState) {
	t.Helper()
	p1, p2 := net.Pipe()
	sc, cc := newAsyncConn(p1), newAsyncConn(p2)
	deadline := time.Now().Add(10 * time.Second)
	_ = sc.SetDeadline(deadline)
	_ = cc.SetDeadline(deadline)

	type result struct {
		err   error
		state tls.ConnectionState
	}
	sres, cres := make(chan result, 1), make(chan result, 1)

	go func() {
		s := tls.Server(sc, srv)
		err := s.Handshake()
		if err == nil {
			_, err = s.Write([]byte("x"))
		}
		st := s.ConnectionState()
		_ = sc.Close()
		sres <- result{err, st}
	}()
	go func() {
		c := tls.Client(cc, cli)
		err := c.Handshake()
		if err == nil {
			_, err = c.Read(make([]byte, 1))
		}
		st := c.ConnectionState()
		_ = cc.Close()
		cres <- result{err, st}
	}()

	s, c := <-sres, <-cres
	return s.err, c.err, s.state, c.state
}

type env struct {
	ca       *CA
	srv, cli peer
	srvIP    net.IP
}

func newEnv(t *testing.T) env {
	t.Helper()
	ca := mustCA(t)
	return env{
		ca:    ca,
		srv:   issue(t, ca, "main-server", "10.1.1.1"),
		cli:   issue(t, ca, "agent-7", "10.1.1.7"),
		srvIP: net.ParseIP("10.1.1.1"),
	}
}

func TestMTLSHandshakeSucceedsAndPeerID(t *testing.T) {
	e := newEnv(t)
	srvCfg, err := ServerTLSConfig(e.srv.cert, e.srv.key, e.ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	cliCfg, err := DialTLSConfig(e.cli.cert, e.cli.key, e.ca.CertPEM(), e.srvIP)
	if err != nil {
		t.Fatal(err)
	}
	serr, cerr, sst, cst := handshake(t, srvCfg, cliCfg)
	if serr != nil || cerr != nil {
		t.Fatalf("handshake failed: server=%v client=%v", serr, cerr)
	}
	if id, err := PeerID(&sst); err != nil || id != "agent-7" {
		t.Fatalf("server sees PeerID = %q, %v; want agent-7", id, err)
	}
	if id, err := PeerID(&cst); err != nil || id != "main-server" {
		t.Fatalf("client sees PeerID = %q, %v; want main-server", id, err)
	}
}

// Both-EKU certs must work with the roles reversed (agent is the TLS server,
// the main server dials it: the default server_dial mode).
func TestMTLSReversedRoles(t *testing.T) {
	e := newEnv(t)
	srvCfg, err := ServerTLSConfig(e.cli.cert, e.cli.key, e.ca.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	cliCfg, err := DialTLSConfig(e.srv.cert, e.srv.key, e.ca.CertPEM(), net.ParseIP("10.1.1.7"))
	if err != nil {
		t.Fatal(err)
	}
	serr, cerr, sst, _ := handshake(t, srvCfg, cliCfg)
	if serr != nil || cerr != nil {
		t.Fatalf("reversed handshake failed: server=%v client=%v", serr, cerr)
	}
	if id, err := PeerID(&sst); err != nil || id != "main-server" {
		t.Fatalf("PeerID = %q, %v", id, err)
	}
}

func TestIPSANMismatchFails(t *testing.T) {
	e := newEnv(t)
	srvCfg, _ := ServerTLSConfig(e.srv.cert, e.srv.key, e.ca.CertPEM())
	// Dial expecting a different IP than the server cert covers.
	cliCfg, err := DialTLSConfig(e.cli.cert, e.cli.key, e.ca.CertPEM(), net.ParseIP("10.9.9.9"))
	if err != nil {
		t.Fatal(err)
	}
	_, cerr, _, _ := handshake(t, srvCfg, cliCfg)
	if cerr == nil {
		t.Fatal("handshake must fail when the expected IP is not in the cert SANs")
	}
	if !strings.Contains(cerr.Error(), "10.9.9.9") {
		t.Fatalf("expected an IP-SAN mismatch error naming 10.9.9.9, got: %v", cerr)
	}
}

func TestDifferentCAServerCertRejectedByClient(t *testing.T) {
	e := newEnv(t)
	rogue := mustCA(t)
	rogueSrv := issue(t, rogue, "main-server", "10.1.1.1") // right IP, wrong CA
	srvCfg, err := ServerTLSConfig(rogueSrv.cert, rogueSrv.key, rogue.CertPEM())
	if err != nil {
		t.Fatal(err)
	}
	cliCfg, _ := DialTLSConfig(e.cli.cert, e.cli.key, e.ca.CertPEM(), e.srvIP)
	_, cerr, _, _ := handshake(t, srvCfg, cliCfg)
	if cerr == nil {
		t.Fatal("client must reject a server cert from a different CA")
	}
}

func TestDifferentCAClientCertRejectedByServer(t *testing.T) {
	e := newEnv(t)
	rogue := mustCA(t)
	rogueCli := issue(t, rogue, "agent-7", "10.1.1.7")
	srvCfg, _ := ServerTLSConfig(e.srv.cert, e.srv.key, e.ca.CertPEM())
	// The rogue client trusts the real CA for the server, but presents its own CA's cert.
	cliCfg, err := DialTLSConfig(rogueCli.cert, rogueCli.key, e.ca.CertPEM(), e.srvIP)
	if err != nil {
		t.Fatal(err)
	}
	serr, cerr, _, _ := handshake(t, srvCfg, cliCfg)
	t.Logf("serr=%v cerr=%v", serr, cerr)
	if serr == nil {
		t.Fatal("server must reject a client cert from a different CA")
	}
	if cerr == nil {
		t.Fatal("client must also see the connection fail")
	}
}

func TestNoClientCertRejectedByServer(t *testing.T) {
	e := newEnv(t)
	srvCfg, _ := ServerTLSConfig(e.srv.cert, e.srv.key, e.ca.CertPEM())
	cliCfg, err := DialTLSConfig(e.cli.cert, e.cli.key, e.ca.CertPEM(), e.srvIP)
	if err != nil {
		t.Fatal(err)
	}
	cliCfg.Certificates = nil // present no client cert
	serr, cerr, _, _ := handshake(t, srvCfg, cliCfg)
	t.Logf("serr=%v cerr=%v", serr, cerr)
	if serr == nil {
		t.Fatal("server must reject a client that presents no cert")
	}
	if cerr == nil {
		t.Fatal("client must also see the connection fail")
	}
}

func TestSystemRootsNotUsed(t *testing.T) {
	e := newEnv(t)
	cfg, err := DialTLSConfig(e.cli.cert, e.cli.key, e.ca.CertPEM(), e.srvIP)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RootCAs == nil {
		t.Fatal("RootCAs must be set explicitly (nil would fall back to system roots)")
	}
	if n := len(cfg.RootCAs.Subjects()); n != 1 { //nolint:staticcheck // counting our own pool
		t.Fatalf("RootCAs has %d certs, want exactly our CA", n)
	}
	if cfg.ServerName != "10.1.1.1" {
		t.Fatalf("ServerName = %q", cfg.ServerName)
	}
}

func TestConfigConstructorErrors(t *testing.T) {
	e := newEnv(t)
	other := issue(t, e.ca, "x", "10.0.0.9")
	if _, err := ServerTLSConfig(e.srv.cert, other.key, e.ca.CertPEM()); err == nil {
		t.Error("mismatched cert/key should fail")
	}
	if _, err := ServerTLSConfig(e.srv.cert, e.srv.key, []byte("junk")); err == nil {
		t.Error("bad CA PEM should fail")
	}
	if _, err := DialTLSConfig(e.srv.cert, e.srv.key, e.ca.CertPEM(), nil); err == nil {
		t.Error("nil expected IP should fail")
	}
}

func TestPeerIDErrors(t *testing.T) {
	if _, err := PeerID(nil); err == nil {
		t.Error("nil state should fail")
	}
	if _, err := PeerID(&tls.ConnectionState{}); err == nil {
		t.Error("state without verified chain should fail")
	}
}
