package monitor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

var agents = []string{"mon-a", "mon-b", "mon-c"}

func openJobs(t *testing.T) (*Jobs, string) {
	t.Helper()
	dir := t.TempDir()
	j, err := OpenJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	return j, dir
}

func TestCreateRequiresIPAndAllowsOptionalOrigin(t *testing.T) {
	j, _ := openJobs(t)

	if _, err := j.Create(Spec{Origin: "https://app.example.com"}, agents); !errors.Is(err, ErrIPRequired) {
		t.Errorf("missing ip: %v, want ErrIPRequired", err)
	}
	if _, err := j.Create(Spec{IP: "not-an-ip"}, agents); !errors.Is(err, ErrBadIP) {
		t.Errorf("bad ip: %v, want ErrBadIP", err)
	}
	for _, bad := range []string{"app.example.com", "ftp://x:21", "https://", "https://h:99999", "https://h/path"} {
		if _, err := j.Create(Spec{IP: "10.0.0.1", Origin: bad}, agents); !errors.Is(err, ErrBadOrigin) {
			t.Errorf("origin %q: %v, want ErrBadOrigin", bad, err)
		}
	}
	if _, err := j.Create(Spec{IP: "10.0.0.1", Paths: []string{"/x"}}, agents); !errors.Is(err, ErrPathNeedsOrg) {
		t.Errorf("path without origin: %v", err)
	}
	if _, err := j.Create(Spec{IP: "10.0.0.1", Origin: "https://h", Paths: []string{"nope"}}, agents); !errors.Is(err, ErrBadPath) {
		t.Errorf("bad path: %v", err)
	}
	if len(j.List()) != 0 {
		t.Fatalf("failed creates left jobs behind: %+v", j.List())
	}

	// ip only: valid, ICMP only.
	only, err := j.Create(Spec{IP: "10.0.0.1"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	if only.Origin != "" || len(only.Targets) != 1 || only.Targets[0].Target.Type != probe.TypeICMP {
		t.Errorf("ip-only job = %+v", only)
	}

	// ip + origin: origin normalised (port filled in), tcp + http + icmp.
	full, err := j.Create(Spec{IP: "10.0.0.1", Origin: "HTTPS://App.Example.com"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	if full.Origin != "https://app.example.com:443" || full.HistoryKey != "10.0.0.1|https://app.example.com:443" {
		t.Errorf("origin/key = %q / %q", full.Origin, full.HistoryKey)
	}
	types := map[probe.Type]int{}
	for _, at := range full.Targets {
		types[at.Target.Type]++
		if at.Target.IP != "10.0.0.1" && at.Target.Type != probe.TypeICMP {
			t.Errorf("target %+v should dial the job ip", at.Target)
		}
	}
	if types[probe.TypeICMP] != 1 || types[probe.TypeTCP] != 1 || types[probe.TypeHTTP] != 1 {
		t.Errorf("target types = %v", types)
	}
	if full.Targets[1].Target.Address != "app.example.com:443" || full.Targets[2].Target.Address != "https://app.example.com:443/" {
		t.Errorf("target addresses: %+v", full.Targets)
	}
}

func TestDNSTargetCreationAndValidation(t *testing.T) {
	j, dir := openJobs(t)

	// Validation failures for probe_types / dns fields leave no job behind.
	if _, err := j.Create(Spec{IP: "10.0.0.1", ProbeTypes: []probe.Type{"bogus"}}, agents); !errors.Is(err, ErrBadProbeType) {
		t.Errorf("bad probe type: %v, want ErrBadProbeType", err)
	}
	if _, err := j.Create(Spec{IP: "10.0.0.1", ProbeTypes: []probe.Type{probe.TypeTCP}}, agents); !errors.Is(err, ErrOriginRequired) {
		t.Errorf("tcp without origin: %v, want ErrOriginRequired", err)
	}
	if _, err := j.Create(Spec{IP: "10.0.0.1", ProbeTypes: []probe.Type{probe.TypeHTTP}}, agents); !errors.Is(err, ErrOriginRequired) {
		t.Errorf("http without origin: %v, want ErrOriginRequired", err)
	}
	if _, err := j.Create(Spec{IP: "10.0.0.1", ProbeTypes: []probe.Type{probe.TypeDNS}}, agents); !errors.Is(err, ErrDNSNameRequired) {
		t.Errorf("dns without name or origin: %v, want ErrDNSNameRequired", err)
	}
	if _, err := j.Create(Spec{IP: "10.0.0.1", Origin: "https://shop.example.test", ProbeTypes: []probe.Type{probe.TypeDNS}, DNSServer: "bad:99999"}, agents); !errors.Is(err, ErrBadDNSServer) {
		t.Errorf("bad dns server: %v, want ErrBadDNSServer", err)
	}
	if len(j.List()) != 0 {
		t.Fatalf("failed creates left jobs behind: %+v", j.List())
	}

	// Origin + DNS-only target derives query name from Origin hostname.
	dnsFromOrigin, err := j.Create(Spec{
		IP:         "10.0.0.1",
		Origin:     "https://Shop.Example.test",
		ProbeTypes: []probe.Type{probe.TypeDNS},
	}, agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(dnsFromOrigin.Targets) != 1 {
		t.Fatalf("targets = %+v, want 1 dns target", dnsFromOrigin.Targets)
	}
	tg := dnsFromOrigin.Targets[0].Target
	if tg.Type != probe.TypeDNS || tg.Address != "shop.example.test" || tg.Server != "" || tg.ID != dnsFromOrigin.ID+"/dns" {
		t.Errorf("dns target = %+v", tg)
	}
	if hk, ok := j.HistoryKeyFor(tg.ID); !ok || hk != dnsFromOrigin.HistoryKey {
		t.Errorf("HistoryKeyFor(%s) = %q, %v", tg.ID, hk, ok)
	}

	// DNS-only job without Origin uses DNSName and does not collide with IP-only ICMP job.
	if _, err := j.Create(Spec{IP: "10.0.0.2"}, agents); err != nil {
		t.Fatalf("ip-only icmp job: %v", err)
	}
	dnsNoOrigin, err := j.Create(Spec{
		IP:         "10.0.0.2",
		ProbeTypes: []probe.Type{probe.TypeDNS},
		DNSName:    "DNS.Example.Test.",
		DNSServer:  "10.0.0.2",
	}, agents)
	if err != nil {
		t.Fatalf("dns-only without origin: %v", err)
	}
	if dnsNoOrigin.HistoryKey != "10.0.0.2|dns://dns.example.test" {
		t.Errorf("HistoryKey = %q", dnsNoOrigin.HistoryKey)
	}
	if dt := dnsNoOrigin.Targets[0].Target; dt.Type != probe.TypeDNS || dt.Address != "dns.example.test" || dt.Server != "10.0.0.2:53" {
		t.Errorf("dnsNoOrigin target = %+v", dt)
	}

	// All four probe types (icmp, tcp, http, dns) together across agents.
	all4, err := j.Create(Spec{
		IP:         "10.0.0.3",
		Origin:     "https://api.example.test",
		Paths:      []string{"/health"},
		ProbeTypes: []probe.Type{probe.TypeICMP, probe.TypeTCP, probe.TypeHTTP, probe.TypeDNS, probe.TypeDNS},
		DNSServer:  "198.51.100.53:5353",
	}, []string{"mon-a", "mon-b", "mon-c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all4.Targets) != 4 {
		t.Fatalf("all4 targets = %d (%+v), want 4 (deduped)", len(all4.Targets), all4.Targets)
	}
	gotTypes := map[probe.Type]probe.Target{}
	for _, at := range all4.Targets {
		gotTypes[at.Target.Type] = at.Target
	}
	for _, want := range []probe.Type{probe.TypeICMP, probe.TypeTCP, probe.TypeHTTP, probe.TypeDNS} {
		if _, ok := gotTypes[want]; !ok {
			t.Errorf("missing probe type %s in %+v", want, all4.Targets)
		}
	}
	if gotTypes[probe.TypeDNS].Server != "198.51.100.53:5353" || gotTypes[probe.TypeDNS].Address != "api.example.test" {
		t.Errorf("all4 dns target = %+v", gotTypes[probe.TypeDNS])
	}

	// Persistence round-trip preserves ProbeTypes, DNSName, DNSServer, and DNS targets.
	re, err := OpenJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, ok := re.Get(dnsNoOrigin.ID)
	if !ok || reloaded.DNSName != "dns.example.test" || reloaded.DNSServer != "10.0.0.2:53" || len(reloaded.Targets) != 1 || reloaded.Targets[0].Target.Type != probe.TypeDNS {
		t.Errorf("reloaded dns job = %+v (ok=%v)", reloaded, ok)
	}
}

func TestNoDuplicateIPOriginButOriginsAndPathsDiffer(t *testing.T) {
	j, _ := openJobs(t)
	first, err := j.Create(Spec{IP: "10.0.0.1", Origin: "https://app.example.com"}, agents)
	if err != nil {
		t.Fatal(err)
	}

	// Same (ip, origin) - even spelled differently - is a duplicate.
	for _, dup := range []Spec{
		{IP: "10.0.0.1", Origin: "https://app.example.com"},
		{IP: "10.0.0.1", Origin: "https://APP.example.com:443"},
		{IP: "10.0.0.1", Origin: "https://app.example.com", Paths: []string{"/other"}},
	} {
		if _, err := j.Create(dup, agents); !errors.Is(err, ErrDuplicate) {
			t.Errorf("Create(%+v) = %v, want ErrDuplicate", dup, err)
		}
	}
	// Same origin, different ip; same ip, different origin (subdomain, port,
	// scheme) and origin-less: all distinct identities.
	for _, ok := range []Spec{
		{IP: "10.0.0.2", Origin: "https://app.example.com"},
		{IP: "10.0.0.1", Origin: "https://api.example.com"},
		{IP: "10.0.0.1", Origin: "https://app.example.com:8443"},
		{IP: "10.0.0.1", Origin: "http://app.example.com"},
		{IP: "10.0.0.1"},
	} {
		if _, err := j.Create(ok, []string{"x1", "x2", "x3", "x4", "x5", "x6", "x7", "x8", "x9", "x10", "x11", "x12"}); err != nil {
			t.Errorf("Create(%+v) = %v, want ok", ok, err)
		}
	}

	// Multiple paths of the same origin live in the one job.
	more, err := j.AddPath(first.ID, "/health?full=1", agents)
	if err != nil {
		t.Fatal(err)
	}
	if len(more.Paths) != 2 || more.Paths[1] != "/health?full=1" {
		t.Errorf("paths = %v", more.Paths)
	}
	if again, _ := j.AddPath(first.ID, "/health?full=1", agents); len(again.Paths) != 2 {
		t.Errorf("adding an existing path duplicated it: %v", again.Paths)
	}
	if _, err := j.AddPath("mj-9999", "/x", agents); !errors.Is(err, ErrNotFound) {
		t.Errorf("AddPath unknown job: %v", err)
	}
	if got, ok := j.Find("10.0.0.1", "https://app.example.com:443"); !ok || got.ID != first.ID {
		t.Errorf("Find = %+v, %v", got, ok)
	}
}

func TestSpecFromURL(t *testing.T) {
	s, err := SpecFromURL("10.0.0.1", "https://App.Example.com/a/b?x=1")
	if err != nil {
		t.Fatal(err)
	}
	if s.Origin != "https://app.example.com:443" || len(s.Paths) != 1 || s.Paths[0] != "/a/b?x=1" {
		t.Errorf("SpecFromURL = %+v", s)
	}
	if s, err := SpecFromURL("10.0.0.1", ""); err != nil || s.Origin != "" || len(s.Paths) != 0 {
		t.Errorf("empty url = %+v, %v", s, err)
	}
	if _, err := SpecFromURL("10.0.0.1", "gopher://x"); !errors.Is(err, ErrBadOrigin) {
		t.Errorf("bad scheme: %v", err)
	}
}

func TestAssignsTargetsToAgents(t *testing.T) {
	j, _ := openJobs(t)
	// Job 1: icmp+tcp+http = 3 targets -> fills the first agent.
	a, err := j.Create(Spec{IP: "10.0.0.1", Origin: "https://a.example.com"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range a.Targets {
		if at.Agent != "mon-a" {
			t.Errorf("target %s on %s, want mon-a (packed)", at.Target.ID, at.Agent)
		}
	}
	// Job 2 goes to the next agent.
	b, err := j.Create(Spec{IP: "10.0.0.2", Origin: "https://b.example.com"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range b.Targets {
		if at.Agent != "mon-b" {
			t.Errorf("target %s on %s, want mon-b", at.Target.ID, at.Agent)
		}
	}
	for agent, n := range j.AgentLoad() {
		if n < MinTargetsPerAgent || n > MaxTargetsPerAgent {
			t.Errorf("agent %s carries %d targets, want %d-%d", agent, n, MinTargetsPerAgent, MaxTargetsPerAgent)
		}
	}

	// Capacity: only mon-a, already full.
	if _, err := j.Create(Spec{IP: "10.0.0.3"}, []string{"mon-a"}); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("full agent: %v, want ErrNoCapacity", err)
	}
	if _, err := j.Create(Spec{IP: "10.0.0.3"}, nil); !errors.Is(err, ErrNoMonitorAgent) {
		t.Errorf("no agents: %v, want ErrNoMonitorAgent", err)
	}
	if _, ok := j.Find("10.0.0.3", ""); ok {
		t.Error("failed create left a job")
	}
	for agent, n := range j.AgentLoad() {
		if n > MaxTargetsPerAgent {
			t.Errorf("agent %s overloaded: %d", agent, n)
		}
	}

	// Config for an agent carries exactly its targets plus the server settings.
	cfg := j.ConfigFor("mon-a")
	if len(cfg.Targets) != 3 || cfg.IntervalMS <= 0 || cfg.TimeoutMS[probe.TypeHTTP] <= 0 {
		t.Errorf("ConfigFor(mon-a) = %+v", cfg)
	}
	if got := j.ConfigFor("nobody"); len(got.Targets) != 0 {
		t.Errorf("ConfigFor(nobody) = %+v", got)
	}
}

func TestSettingsAndOverridesFlowIntoConfig(t *testing.T) {
	j, _ := openJobs(t)
	job, err := j.Create(Spec{IP: "10.0.0.1", Origin: "http://h.example.com"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	s := Settings{IntervalMS: 750, TimeoutMS: map[probe.Type]int{probe.TypeHTTP: 1234, probe.TypeTCP: 321}}
	if err := j.SetSettings(s); err != nil {
		t.Fatal(err)
	}
	if err := j.SetTargetInterval(job.ID, job.ID+"/tcp", 400*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := j.SetSettings(Settings{}); err == nil {
		t.Error("zero interval accepted")
	}
	cfg := j.ConfigFor("mon-a")
	if cfg.IntervalMS != 750 || cfg.TimeoutMS[probe.TypeHTTP] != 1234 || cfg.TimeoutMS[probe.TypeTCP] != 321 {
		t.Errorf("config = %+v", cfg)
	}
	var tcp probe.Target
	for _, tg := range cfg.Targets {
		if tg.Type == probe.TypeTCP {
			tcp = tg
		}
	}
	if cfg.Interval(tcp) != 400*time.Millisecond || cfg.Timeout(tcp) != 321*time.Millisecond {
		t.Errorf("tcp target interval/timeout = %v / %v", cfg.Interval(tcp), cfg.Timeout(tcp))
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	j, dir := openJobs(t)
	a, err := j.Create(Spec{IP: "10.0.0.1", Origin: "https://a.example.com", Paths: []string{"/", "/login"}}, agents)
	if err != nil {
		t.Fatal(err)
	}
	b, err := j.Create(Spec{IP: "10.0.0.2"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.AddPath(a.ID, "/api/v1", agents[1:]); err != nil {
		t.Fatal(err)
	}
	if err := j.SetSettings(Settings{IntervalMS: 900, TimeoutMS: map[probe.Type]int{probe.TypeICMP: 777}}); err != nil {
		t.Fatal(err)
	}
	if err := j.Delete(b.ID); err != nil {
		t.Fatal(err)
	}

	// Lives under <DataDir>/monitor/ (D-0006), and nothing else is left there.
	file := filepath.Join(dir, "monitor", JobsFileName)
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("jobs file: %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(dir, "monitor")); len(ents) != 1 {
		t.Errorf("monitor dir has stray files: %v", ents)
	}

	re, err := OpenJobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := re.List()
	if len(got) != 1 {
		t.Fatalf("reloaded %d jobs, want 1: %+v", len(got), got)
	}
	want, _ := j.Get(a.ID)
	if fmt.Sprintf("%+v", got[0]) != fmt.Sprintf("%+v", want) {
		t.Errorf("round trip changed the job:\n got %+v\nwant %+v", got[0], want)
	}
	if len(got[0].Paths) != 3 {
		t.Errorf("paths = %v", got[0].Paths)
	}
	if s := re.Settings(); s.IntervalMS != 900 || s.TimeoutMS[probe.TypeICMP] != 777 {
		t.Errorf("settings = %+v", s)
	}
	// Uniqueness survives a reload.
	if _, err := re.Create(Spec{IP: "10.0.0.1", Origin: "https://a.example.com"}, agents); !errors.Is(err, ErrDuplicate) {
		t.Errorf("duplicate after reload: %v", err)
	}
	// IDs keep counting, not restarting.
	c, err := re.Create(Spec{IP: "10.0.0.9"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == a.ID || c.ID == b.ID {
		t.Errorf("reused id %s", c.ID)
	}
	// The history of the deleted (ip, origin) survives and is continued.
	b2, err := re.Create(Spec{IP: "10.0.0.2"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	h := re.History("10.0.0.2", "")
	kinds := []string{}
	for _, e := range h {
		kinds = append(kinds, e.Kind)
	}
	if fmt.Sprint(kinds) != "[created deleted created]" || h[2].Job != b2.ID {
		t.Errorf("history = %v", kinds)
	}
}

func TestCorruptJobsFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "monitor"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "monitor", JobsFileName), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenJobs(dir); err == nil {
		t.Error("corrupt file opened")
	}
}

func TestFailedSaveRollsBack(t *testing.T) {
	j, dir := openJobs(t)
	if _, err := j.Create(Spec{IP: "10.0.0.1"}, agents); err != nil {
		t.Fatal(err)
	}
	// Make the directory unwritable by replacing it with a file.
	mon := filepath.Join(dir, "monitor")
	if err := os.RemoveAll(mon); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mon, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Create(Spec{IP: "10.0.0.2"}, agents); err == nil {
		t.Fatal("create succeeded although it could not be saved")
	}
	if n := len(j.List()); n != 1 {
		t.Errorf("memory ran ahead of disk: %d jobs", n)
	}
}

// ---- monitor data flow ----

// fakeLink is a SendFunc that can be taken down, and records what arrived.
type fakeLink struct {
	mu   sync.Mutex
	up   bool
	envs []channel.Envelope
}

func (l *fakeLink) send(_ context.Context, env channel.Envelope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.up {
		return channel.ErrNotConnected
	}
	l.envs = append(l.envs, env)
	return nil
}

func (l *fakeLink) setUp(up bool) {
	l.mu.Lock()
	l.up = up
	l.mu.Unlock()
}

func (l *fakeLink) received() []channel.Envelope {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]channel.Envelope(nil), l.envs...)
}

func result(target string, i int) probe.Result {
	return probe.Result{TargetID: target, Type: probe.TypeTCP, Time: time.Unix(1_700_000_000+int64(i), 0).UTC(), OK: true, Latency: time.Duration(i) * time.Millisecond}
}

func TestBacklogFlushesOnReconnectInOrder(t *testing.T) {
	link := &fakeLink{}
	f := NewFlusher(link.send, FlusherOptions{MaxBatch: 4})
	for i := range 10 {
		f.Add(result("mj-0001/tcp", i))
	}
	// Channel down: flush fails, nothing lost, nothing sent.
	if err := f.Flush(context.Background()); !errors.Is(err, channel.ErrNotConnected) {
		t.Fatalf("flush while down = %v", err)
	}
	if f.Pending() != 10 || len(link.received()) != 0 {
		t.Fatalf("pending = %d, sent = %d", f.Pending(), len(link.received()))
	}
	// More results arrive during the outage.
	f.Add(result("mj-0001/tcp", 10))
	f.Add(result("mj-0001/tcp", 11))

	// Reconnect: the whole backlog goes out, oldest first, in 4-result batches.
	link.setUp(true)
	if err := f.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.Pending() != 0 {
		t.Errorf("pending after flush = %d", f.Pending())
	}
	sink := &MemorySink{}
	ing := NewIngestor(sink, nil)
	envs := link.received()
	if len(envs) != 3 {
		t.Fatalf("%d envelopes, want 3 (12 results / 4)", len(envs))
	}
	for _, env := range envs {
		if env.Type != channel.TypeMonData {
			t.Errorf("envelope type %q", env.Type)
		}
		if err := ing.Handle(context.Background(), "mon-a", env); err != nil {
			t.Fatal(err)
		}
	}
	got := sink.Samples()
	if len(got) != 12 {
		t.Fatalf("sink has %d samples, want 12", len(got))
	}
	for i, s := range got {
		if s.Result.Latency != time.Duration(i)*time.Millisecond || s.Agent != "mon-a" {
			t.Errorf("sample %d out of order or mislabelled: %+v", i, s)
		}
	}
}

func TestFlusherKeepsRunningThroughOutageAndCapsBuffer(t *testing.T) {
	link := &fakeLink{}
	f := NewFlusher(link.send, FlusherOptions{MaxBuffered: 5})
	for i := range 8 {
		f.Add(result("t", i))
	}
	if f.Pending() != 5 || f.Dropped() != 3 {
		t.Errorf("pending/dropped = %d/%d, want 5/3 (oldest dropped)", f.Pending(), f.Dropped())
	}
	link.setUp(true)
	if err := f.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	sink := &MemorySink{}
	for _, env := range link.received() {
		_ = NewIngestor(sink, nil).Handle(context.Background(), "m", env)
	}
	if got := sink.Samples(); len(got) != 5 || got[0].Result.Latency != 3*time.Millisecond {
		t.Errorf("samples after cap = %d (first %v), want the newest 5", len(got), got[0].Result.Latency)
	}
}

func TestFlushIntervalIsTwoToThreeSeconds(t *testing.T) {
	for in, want := range map[time.Duration]time.Duration{
		0:                       DefaultFlushInterval,
		100 * time.Millisecond:  MinFlushInterval,
		10 * time.Second:        MaxFlushInterval,
		2200 * time.Millisecond: 2200 * time.Millisecond,
	} {
		if got := NewFlusher(nil, FlusherOptions{Interval: in}).Interval(); got != want {
			t.Errorf("interval(%v) = %v, want %v", in, got, want)
		}
	}
	if DefaultFlushInterval < 2*time.Second || DefaultFlushInterval > 3*time.Second {
		t.Errorf("default flush interval %v outside 2-3s", DefaultFlushInterval)
	}
}

func TestFlusherRunFlushesPeriodicallyAndRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("takes a few seconds")
	}
	link := &fakeLink{}
	f := NewFlusher(link.send, FlusherOptions{Interval: MinFlushInterval})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()

	f.Add(result("t", 1))
	time.Sleep(MinFlushInterval + 500*time.Millisecond)
	if len(link.received()) != 0 || f.Pending() != 1 {
		t.Fatalf("link down: sent %d, pending %d", len(link.received()), f.Pending())
	}
	link.setUp(true)
	deadline := time.Now().Add(MinFlushInterval + 2*time.Second)
	for f.Pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if f.Pending() != 0 || len(link.received()) != 1 {
		t.Errorf("after reconnect: pending %d, sent %d", f.Pending(), len(link.received()))
	}
	cancel()
	<-done
}

func TestIngestLabelsHistoryAndIgnoresOtherTypes(t *testing.T) {
	j, _ := openJobs(t)
	job, err := j.Create(Spec{IP: "10.0.0.1", Origin: "https://a.example.com"}, agents)
	if err != nil {
		t.Fatal(err)
	}
	sink := &MemorySink{}
	ing := NewIngestor(sink, j.HistoryKeyFor)

	env, err := channel.NewEnvelope(channel.TypeMonData, "mon-1", Batch{Seq: 1, Results: []probe.Result{
		result(job.ID+"/tcp", 1),
		result("unknown-target", 2),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ing.Handle(context.Background(), "mon-a", env); err != nil {
		t.Fatal(err)
	}
	got := sink.Samples()
	if len(got) != 2 || got[0].HistoryKey != job.HistoryKey || got[1].HistoryKey != "" {
		t.Errorf("samples = %+v", got)
	}

	if err := ing.Handle(context.Background(), "mon-a", channel.Envelope{Type: channel.TypeEvManifest}); err != nil {
		t.Errorf("evidence envelope must be ignored by the monitor path: %v", err)
	}
	if err := ing.Handle(context.Background(), "", env); err == nil {
		t.Error("mon-data from an unidentified peer accepted")
	}
	if err := ing.Handle(context.Background(), "mon-a", channel.Envelope{Type: channel.TypeMonData, Payload: []byte("{bad")}); err == nil {
		t.Error("garbage payload accepted")
	}
	if len(sink.Samples()) != 2 {
		t.Error("rejected envelopes wrote samples")
	}
}

func TestConfigEnvelopeRoundTrip(t *testing.T) {
	j, _ := openJobs(t)
	if _, err := j.Create(Spec{IP: "10.0.0.1", Origin: "https://a.example.com"}, agents); err != nil {
		t.Fatal(err)
	}
	want := j.ConfigFor("mon-a")
	env, err := ConfigEnvelope(want)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != channel.TypeMonConfig {
		t.Errorf("type = %q", env.Type)
	}
	got, err := DecodeConfig(env)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", want) {
		t.Errorf("config changed on the wire:\n got %+v\nwant %+v", got, want)
	}
	if _, err := DecodeConfig(channel.Envelope{Type: channel.TypeMonData}); err == nil {
		t.Error("wrong type decoded")
	}
}
