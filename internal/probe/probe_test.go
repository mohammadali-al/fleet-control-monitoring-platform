package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestHTTPProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			time.Sleep(150 * time.Millisecond)
		case "/missing":
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	res := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: srv.URL + "/"}, time.Second)
	if !res.OK || res.Status != 200 || res.Error != "" {
		t.Fatalf("200 probe = %+v", res)
	}
	if res.TLS != nil {
		t.Errorf("plain http must have no TLS info: %+v", res.TLS)
	}

	slow := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: srv.URL + "/slow"}, time.Second)
	if !slow.OK || slow.Latency < 150*time.Millisecond || slow.Latency > time.Second {
		t.Errorf("slow latency = %v (ok=%v), want >= 150ms", slow.Latency, slow.OK)
	}

	nf := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: srv.URL + "/missing"}, time.Second)
	if nf.OK || nf.Status != 404 {
		t.Errorf("404 probe = %+v, want not OK with status 404", nf)
	}

	// A hostname that cannot be dialled, steered to the server by Target.IP.
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	via := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: "http://monitored.invalid:" + port + "/", IP: host}, time.Second)
	if !via.OK || via.Status != 200 {
		t.Errorf("probe with IP override = %+v", via)
	}

	// Timeout -> failure result, not a hang.
	start := time.Now()
	to := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: srv.URL + "/slow"}, 40*time.Millisecond)
	if to.OK || to.Error == "" || time.Since(start) > time.Second {
		t.Errorf("timeout probe = %+v after %v", to, time.Since(start))
	}

	// Unreachable.
	srv.Close()
	down := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: srv.URL}, time.Second)
	if down.OK || down.Error == "" {
		t.Errorf("closed server probe = %+v", down)
	}

	if bad := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: "ftp://x"}, time.Second); bad.OK || bad.Error == "" {
		t.Errorf("bad address = %+v", bad)
	}
}

func TestHTTPSProbeTLSInfo(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())

	p := &HTTP{TLSConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	res := p.Probe(context.Background(), Target{Type: TypeHTTP, Address: srv.URL}, 2*time.Second)
	if !res.OK || res.Status != 200 {
		t.Fatalf("https probe = %+v", res)
	}
	if res.TLS == nil || !strings.HasPrefix(res.TLS.Version, "TLS 1.") || res.TLS.NotAfter.Before(time.Now()) || res.TLS.Subject == "" {
		t.Errorf("TLS info = %+v", res.TLS)
	}

	// Untrusted cert (default roots): a failed result, not a success.
	bad := NewHTTP().Probe(context.Background(), Target{Type: TypeHTTP, Address: srv.URL}, 2*time.Second)
	if bad.OK {
		t.Errorf("untrusted certificate probe should fail: %+v", bad)
	}
}

func TestTCPProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	res := NewTCP().Probe(context.Background(), Target{Type: TypeTCP, Address: ln.Addr().String()}, time.Second)
	if !res.OK || res.Error != "" || res.Latency <= 0 || res.Latency > time.Second {
		t.Fatalf("tcp probe = %+v", res)
	}

	// IP override keeps the port of Address.
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	via := NewTCP().Probe(context.Background(), Target{Type: TypeTCP, Address: "monitored.invalid:" + port, IP: "127.0.0.1"}, time.Second)
	if !via.OK {
		t.Errorf("tcp probe with IP override = %+v", via)
	}

	addr := ln.Addr().String()
	ln.Close()
	down := NewTCP().Probe(context.Background(), Target{Type: TypeTCP, Address: addr}, time.Second)
	if down.OK || down.Error == "" {
		t.Errorf("closed port probe = %+v, want failure", down)
	}
}

func TestICMPProbeLoopback(t *testing.T) {
	if err := ICMPAvailable(); err != nil {
		t.Skipf("SKIPPED (live ICMP needs an ICMP socket: root/CAP_NET_RAW or net.ipv4.ping_group_range): %v", err)
	}
	res := NewICMP().Probe(context.Background(), Target{Type: TypeICMP, Address: "127.0.0.1", Count: 3}, 2*time.Second)
	if !res.OK || res.Sent != 3 || res.Received != 3 || res.Loss != 0 {
		t.Fatalf("icmp loopback = %+v", res)
	}
	if res.Latency <= 0 || res.Latency > time.Second {
		t.Errorf("icmp RTT = %v", res.Latency)
	}
}

func TestICMPProbeFailsWithoutSocketOrReply(t *testing.T) {
	// A target that cannot be resolved is a failure result in any environment.
	res := NewICMP().Probe(context.Background(), Target{Type: TypeICMP, Address: "no-such-host.invalid"}, 300*time.Millisecond)
	if res.OK || res.Error == "" {
		t.Errorf("unresolvable icmp target = %+v", res)
	}
}

// dnsTestServer answers A queries for the names in zone over UDP and counts
// the queries it received.
type dnsTestServer struct {
	pc      net.PacketConn
	mu      sync.Mutex
	queries int
}

func newDNSTestServer(t *testing.T, zone map[string]netip.Addr, delay time.Duration) *dnsTestServer {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &dnsTestServer{pc: pc}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var p dnsmessage.Parser
			h, err := p.Start(buf[:n])
			if err != nil {
				continue
			}
			q, err := p.Question()
			if err != nil {
				continue
			}
			s.mu.Lock()
			s.queries++
			s.mu.Unlock()
			time.Sleep(delay)
			resp := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: h.ID, Response: true, Authoritative: true, RecursionAvailable: true},
				Questions: []dnsmessage.Question{q},
			}
			name := strings.TrimSuffix(q.Name.String(), ".")
			if ip, ok := zone[name]; ok && q.Type == dnsmessage.TypeA {
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: ip.As4()},
				}}
			} else if !ok {
				resp.Header.RCode = dnsmessage.RCodeNameError
			}
			out, err := resp.Pack()
			if err != nil {
				continue
			}
			_, _ = pc.WriteTo(out, from)
		}
	}()
	return s
}

func TestDNSProbe(t *testing.T) {
	srv := newDNSTestServer(t, map[string]netip.Addr{"svc.test": netip.MustParseAddr("10.1.2.3")}, 80*time.Millisecond)
	server := srv.pc.LocalAddr().String()

	res := NewDNS().Probe(context.Background(), Target{Type: TypeDNS, Address: "svc.test", Server: server}, 2*time.Second)
	if !res.OK || !slices.Equal(res.Answer, []string{"10.1.2.3"}) {
		t.Fatalf("dns probe = %+v", res)
	}
	if res.Latency < 80*time.Millisecond || res.Latency > 2*time.Second {
		t.Errorf("dns latency = %v, want >= 80ms (server delay)", res.Latency)
	}

	nx := NewDNS().Probe(context.Background(), Target{Type: TypeDNS, Address: "missing.test", Server: server}, 2*time.Second)
	if nx.OK || nx.Error == "" {
		t.Errorf("NXDOMAIN probe = %+v, want failure", nx)
	}

	to := NewDNS().Probe(context.Background(), Target{Type: TypeDNS, Address: "svc.test", Server: server}, 20*time.Millisecond)
	if to.OK {
		t.Errorf("dns probe with a 20ms timeout against an 80ms server = %+v, want failure", to)
	}
}

// collector gathers engine results.
type collector struct {
	mu  sync.Mutex
	res []Result
}

func (c *collector) add(r Result) {
	c.mu.Lock()
	c.res = append(c.res, r)
	c.mu.Unlock()
}

func (c *collector) forTarget(id string) []Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Result
	for _, r := range c.res {
		if r.TargetID == id {
			out = append(out, r)
		}
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startEngine(t *testing.T, out func(Result)) *Engine {
	t.Helper()
	e := NewEngine(out)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return e
}

func TestConfigResolution(t *testing.T) {
	cfg := Config{IntervalMS: 500, TimeoutMS: map[Type]int{TypeHTTP: 700}}
	if got := cfg.Interval(Target{}); got != 500*time.Millisecond {
		t.Errorf("global interval = %v", got)
	}
	if got := cfg.Interval(Target{IntervalMS: 1200}); got != 1200*time.Millisecond {
		t.Errorf("override interval = %v", got)
	}
	if got := cfg.Interval(Target{IntervalMS: 50}); got != MinInterval {
		t.Errorf("clamped low interval = %v, want %v", got, MinInterval)
	}
	if got := (Config{}).Interval(Target{}); got != FallbackInterval {
		t.Errorf("fallback interval = %v", got)
	}
	if got := cfg.Timeout(Target{Type: TypeHTTP}); got != 700*time.Millisecond {
		t.Errorf("pushed timeout = %v", got)
	}
	if got := cfg.Timeout(Target{Type: TypeHTTP, TimeoutMS: 90}); got != 90*time.Millisecond {
		t.Errorf("override timeout = %v", got)
	}
	if got := cfg.Timeout(Target{Type: TypeTCP}); got != FallbackTCPTimeout {
		t.Errorf("fallback timeout = %v", got)
	}
}

func TestEngineFrequencyComesFromPushedConfig(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	var col collector
	e := startEngine(t, col.add)
	tgt := Target{ID: "t1", Type: TypeTCP, Address: ln.Addr().String()}

	// Fast config: 300 ms (the minimum) -> several results in ~1.3 s.
	e.Apply(Config{IntervalMS: 300, Targets: []Target{tgt}})
	waitFor(t, "4 results at 300ms", func() bool { return len(col.forTarget("t1")) >= 4 })
	rs := col.forTarget("t1")
	for i := 1; i < len(rs); i++ {
		if gap := rs[i].Time.Sub(rs[i-1].Time); gap < 250*time.Millisecond || gap > 600*time.Millisecond {
			t.Errorf("gap %d = %v, want about 300ms", i, gap)
		}
	}
	for _, r := range rs {
		if !r.OK || r.Type != TypeTCP || r.TargetID != "t1" {
			t.Errorf("result %+v", r)
		}
	}

	// A pushed slower per-target override takes effect: far fewer results.
	e.Apply(Config{IntervalMS: 300, Targets: []Target{{ID: "t1", Type: TypeTCP, Address: tgt.Address, IntervalMS: 5000}}})
	time.Sleep(100 * time.Millisecond) // let the old loop drain
	n := len(col.forTarget("t1"))
	time.Sleep(900 * time.Millisecond)
	if got := len(col.forTarget("t1")) - n; got > 2 {
		t.Errorf("after pushing a 5s interval, %d results in 0.9s; want at most 2 (restart probe + none)", got)
	}

	// Removing the target stops its loop.
	e.Apply(Config{})
	time.Sleep(100 * time.Millisecond)
	n = len(col.forTarget("t1"))
	time.Sleep(400 * time.Millisecond)
	if got := len(col.forTarget("t1")); got != n {
		t.Errorf("target removed but results kept coming (%d -> %d)", n, got)
	}
}

func TestEngineTimeoutComesFromPushedConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
	}))
	defer srv.Close()

	var col collector
	e := startEngine(t, col.add)
	tgt := Target{ID: "h", Type: TypeHTTP, Address: srv.URL}

	// Pushed 100 ms timeout (server sleeps 400 ms): failure. The local HTTP
	// fallback (2 s) would have succeeded, so this proves the pushed value wins.
	e.Apply(Config{IntervalMS: 5000, TimeoutMS: map[Type]int{TypeHTTP: 100}, Targets: []Target{tgt}})
	waitFor(t, "first result", func() bool { return len(col.forTarget("h")) >= 1 })
	if r := col.forTarget("h")[0]; r.OK {
		t.Errorf("with pushed 100ms timeout the probe should fail: %+v", r)
	}

	// Push 3 s: succeeds.
	e.Apply(Config{IntervalMS: 5000, TimeoutMS: map[Type]int{TypeHTTP: 3000}, Targets: []Target{tgt}})
	waitFor(t, "result after timeout change", func() bool {
		rs := col.forTarget("h")
		return len(rs) >= 2 && rs[len(rs)-1].OK
	})
}

func TestEngineNeverStopsOnFailingTargets(t *testing.T) {
	var col collector
	e := startEngine(t, col.add)
	// Closed port, unknown type, unresolvable name: all just failing results.
	e.Apply(Config{IntervalMS: 300, TimeoutMS: map[Type]int{TypeTCP: 200}, Targets: []Target{
		{ID: "down", Type: TypeTCP, Address: "127.0.0.1:1"},
		{ID: "weird", Type: "gopher", Address: "x"},
	}})
	waitFor(t, "repeated failures", func() bool {
		return len(col.forTarget("down")) >= 3 && len(col.forTarget("weird")) >= 2
	})
	for _, r := range col.forTarget("down") {
		if r.OK {
			t.Errorf("closed port result OK: %+v", r)
		}
	}
}
