package panel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"golang.org/x/net/dns/dnsmessage"
)

func startPanelFakeDNS(t *testing.T, records map[string]net.IP) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			var msg dnsmessage.Message
			if err := msg.Unpack(buf[:n]); err != nil || len(msg.Questions) == 0 {
				continue
			}
			q := msg.Questions[0]
			name := strings.TrimSuffix(q.Name.String(), ".")
			resp := dnsmessage.Message{
				Header: dnsmessage.Header{
					ID:                 msg.Header.ID,
					Response:           true,
					RecursionDesired:   msg.Header.RecursionDesired,
					RecursionAvailable: true,
				},
				Questions: []dnsmessage.Question{q},
			}
			if ip, ok := records[name]; ok && q.Type == dnsmessage.TypeA {
				resp.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: [4]byte(ip.To4())},
				}}
			} else if !ok {
				resp.Header.RCode = dnsmessage.RCodeNameError
			}
			if out, err := resp.Pack(); err == nil {
				_, _ = pc.WriteTo(out, from)
			}
		}
	}()
	return pc.LocalAddr().String()
}

func TestMonitorCreateJobDNSAndProbeTypes(t *testing.T) {
	e := newObsEnv(t, nil)
	e.waitState("mon1", fleet.StateOnline)

	dnsAddr := startPanelFakeDNS(t, map[string]net.IP{
		"dns.example.test": net.ParseIP("198.51.100.12"),
	})

	// Validation errors for probe_types / missing dns name.
	for _, bad := range []map[string]any{
		{"ip": "198.51.100.12", "probe_types": []string{"bogus"}},
		{"ip": "198.51.100.12", "probe_types": []string{"dns"}},
		{"ip": "198.51.100.12", "probe_types": []string{"tcp"}},
	} {
		if code, _ := e.req("POST", "/api/monitor/jobs", bad, nil); code != http.StatusBadRequest {
			t.Errorf("POST /api/monitor/jobs %v = %d, want 400", bad, code)
		}
	}

	// Create a DNS target from Origin + probe_types: ["dns"] + dns_server.
	var created struct{ Job monitor.Job }
	code, _ := e.req("POST", "/api/monitor/jobs", map[string]any{
		"url":         "https://dns.example.test/health",
		"ip":          "198.51.100.12",
		"probe_types": []string{"dns"},
		"dns_server":  dnsAddr,
	}, &created)
	if code != http.StatusCreated {
		t.Fatalf("create dns job = %d", code)
	}
	job := created.Job
	if len(job.Targets) != 1 || job.Targets[0].Target.Type != probe.TypeDNS {
		t.Fatalf("dns job targets = %+v, want 1 dns target", job.Targets)
	}
	tgt := job.Targets[0].Target
	if tgt.Address != "dns.example.test" || tgt.Server != dnsAddr {
		t.Errorf("dns target = %+v, want address=dns.example.test server=%s", tgt, dnsAddr)
	}

	// Config pushed to mon1 carries the DNS target, and probe.NewDNS() probes it successfully.
	eventually(t, "mon-config with dns target", func() bool {
		return len(e.link("mon1").sentOfType(channel.TypeMonConfig)) > 0
	})
	cfgs := e.link("mon1").sentOfType(channel.TypeMonConfig)
	var pushed probe.Config
	if err := json.Unmarshal(cfgs[len(cfgs)-1].Payload, &pushed); err != nil || len(pushed.Targets) != 1 {
		t.Fatalf("pushed config = %+v %v", pushed, err)
	}
	res := probe.NewDNS().Probe(context.Background(), pushed.Targets[0], 2*time.Second)
	if !res.OK || len(res.Answer) != 1 || res.Answer[0] != "198.51.100.12" {
		t.Fatalf("dns probe result = %+v", res)
	}
	res.TargetID = pushed.Targets[0].ID
	e.link("mon1").h(context.Background(), monDataEnvelope(t, 1, res))

	var live struct {
		Series []fleet.SeriesData `json:"series"`
	}
	if code, _ := e.req("GET", "/api/monitor/live?window=60", nil, &live); code != http.StatusOK || len(live.Series) != 1 || live.Series[0].Type != probe.TypeDNS {
		t.Errorf("live dns series = %d %+v", code, live.Series)
	}

	// DNS-only job via singular probe_type + dns_name (no URL).
	var createdNoURL struct{ Job monitor.Job }
	if code, _ := e.req("POST", "/api/monitor/jobs", map[string]any{
		"ip":         "198.51.100.13",
		"probe_type": "dns",
		"dns_name":   "svc.example.test",
	}, &createdNoURL); code != http.StatusCreated {
		t.Fatalf("create dns-only job = %d", code)
	}
	if len(createdNoURL.Job.Targets) != 1 || createdNoURL.Job.Targets[0].Target.Type != probe.TypeDNS || createdNoURL.Job.Targets[0].Target.Address != "svc.example.test" {
		t.Errorf("dns-only job targets = %+v", createdNoURL.Job.Targets)
	}
}

func TestMonitorOverlaySharedSegmentAndIntervalCounts(t *testing.T) {
	e := newEnv(t)
	o, err := fleet.OpenObserve(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	e.m.SetObserve(o)

	var subA, subB submitResponse
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "echo cmd-alpha"}, &subA); st != http.StatusAccepted {
		t.Fatalf("submit alpha = %d", st)
	}
	if st := e.call(t, "POST", "/api/commands", map[string]any{"machines": []string{"w1"}, "command": "echo cmd-beta"}, &subB); st != http.StatusAccepted {
		t.Fatalf("submit beta = %d", st)
	}
	sessA := subA.Results[0].Session
	sessB := subB.Results[0].Session

	type liveResp struct {
		Now       time.Time     `json:"now"`
		WindowSec int           `json:"window_sec"`
		Commands  []monCommand  `json:"commands"`
		Intervals []monInterval `json:"intervals"`
	}
	var live liveResp
	eventually(t, "both commands on overlay", func() bool {
		live = liveResp{}
		e.call(t, "GET", "/api/monitor/live?window=60", nil, &live)
		return len(live.Commands) == 2 && live.Commands[0].End != nil && live.Commands[1].End != nil
	})
	for _, c := range live.Commands {
		if c.Packets != nil || c.IntervalPackets != nil {
			t.Errorf("before pull command = %+v, want nil packets and interval_packets", c)
		}
	}

	// Build a shared segment mixing sessA (3 records: 2 in -18s bin, 1 in -2s bin)
	// and sessB (2 records in -2s bin), plus a dedicated segment for sessA (1 record in -18s bin).
	now := time.Now().UTC().Truncate(time.Millisecond)
	tOlder := now.Add(-18 * time.Second)
	tRecent := now.Add(-2 * time.Second)

	commitRecs := func(file string, recs []capture.Record) {
		t.Helper()
		var data []byte
		for _, r := range recs {
			b, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			data = append(append(data, b...), '\n')
		}
		part := filepath.Join(t.TempDir(), file+".part")
		if err := os.WriteFile(part, data, 0o640); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		entry := capture.SegmentEntry{
			File: file, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(len(data)),
			Records: len(recs), StartTime: recs[0].Timestamp, EndTime: recs[len(recs)-1].Timestamp,
			SchemaVersion: capture.SchemaVersion, Compression: capture.CompressionNone,
		}
		if _, err := o.Archive.Commit("w1", entry, part); err != nil {
			t.Fatal(err)
		}
	}

	commitRecs("seg-shared.ndjson", []capture.Record{
		synthRecord(t, sessA, tOlder, 0),
		synthRecord(t, sessA, tOlder.Add(time.Millisecond), 1),
		synthRecord(t, sessB, tRecent, 2),
		synthRecord(t, sessA, tRecent.Add(time.Millisecond), 3),
		synthRecord(t, sessB, tRecent.Add(2*time.Millisecond), 4),
	})
	commitRecs("seg-alpha.ndjson", []capture.Record{
		synthRecord(t, sessA, tOlder.Add(2*time.Millisecond), 5),
	})

	// archivedPackets itself must return exact per-session counts, never whole shared segment total.
	if got, ok := archivedPackets(o, "w1", sessA); !ok || got != 4 {
		t.Errorf("archivedPackets(%s) = (%d, %v), want (4, true)", sessA, got, ok)
	}
	if got, ok := archivedPackets(o, "w1", sessB); !ok || got != 2 {
		t.Errorf("archivedPackets(%s) = (%d, %v), want (2, true)", sessB, got, ok)
	}

	live = liveResp{}
	e.call(t, "GET", "/api/monitor/live?window=60", nil, &live)
	if len(live.Intervals) != overlayIntervals {
		t.Fatalf("len(intervals) = %d, want %d", len(live.Intervals), overlayIntervals)
	}
	bySess := map[string]monCommand{}
	for _, c := range live.Commands {
		bySess[c.Session] = c
	}

	sumInts := func(xs []int) int {
		s := 0
		for _, v := range xs {
			s += v
		}
		return s
	}
	nonZeroCount := func(xs []int) int {
		n := 0
		for _, v := range xs {
			if v > 0 {
				n++
			}
		}
		return n
	}

	ca := bySess[sessA]
	if ca.Packets == nil || *ca.Packets != 4 {
		t.Errorf("alpha packets = %v, want 4", ca.Packets)
	}
	if len(ca.IntervalPackets) != overlayIntervals || sumInts(ca.IntervalPackets) != 4 || nonZeroCount(ca.IntervalPackets) != 2 {
		t.Errorf("alpha interval_packets = %v, want len=12 sum=4 across 2 intervals", ca.IntervalPackets)
	}
	// Recent interval (last bin) has 1 packet for alpha, older bin has 3.
	if ca.IntervalPackets[overlayIntervals-1] != 1 {
		t.Errorf("alpha last interval packets = %d, want 1 (%v)", ca.IntervalPackets[overlayIntervals-1], ca.IntervalPackets)
	}

	cb := bySess[sessB]
	if cb.Packets == nil || *cb.Packets != 2 {
		t.Errorf("beta packets = %v, want 2", cb.Packets)
	}
	if len(cb.IntervalPackets) != overlayIntervals || sumInts(cb.IntervalPackets) != 2 || cb.IntervalPackets[overlayIntervals-1] != 2 {
		t.Errorf("beta interval_packets = %v, want sum=2 in last bin", cb.IntervalPackets)
	}

	// Top-level intervals aggregate across commands in each bin.
	totalIvPkts := 0
	for _, iv := range live.Intervals {
		totalIvPkts += iv.Packets
	}
	if totalIvPkts != 6 || live.Intervals[overlayIntervals-1].Packets != 3 {
		t.Errorf("intervals = %+v, want total=6 and last=3", live.Intervals)
	}
}

func TestMonitorSPARouteInBundle(t *testing.T) {
	srv := noBackend(t)
	status, _, page := body(t, srv.URL+"/monitor")
	if status != http.StatusOK {
		t.Fatalf("GET /monitor = %d", status)
	}
	for _, want := range []string{`<div id="root"></div>`, `/assets/dist/app.css`, `/assets/dist/app.js`} {
		if !strings.Contains(page, want) {
			t.Errorf("/monitor SPA shell lacks %q", want)
		}
	}

	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != http.StatusOK || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d (len=%d)", status, len(js))
	}
	for _, want := range []string{
		"Monitor data",
		"/api/monitor/live",
		"/api/monitor/config",
		"/api/monitor/jobs",
		"/api/monitor/settings",
		"/ws/monitor",
		"Probe types",
		"DNS query name",
		"Packets in interval",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
	for _, outOfScope := range []string{
		"Preview with sample data",
		"Embedded Grafana frame placeholder",
	} {
		if strings.Contains(js, outOfScope) {
			t.Errorf("SPA bundle unexpectedly contains out-of-scope copy %q", outOfScope)
		}
	}
}
