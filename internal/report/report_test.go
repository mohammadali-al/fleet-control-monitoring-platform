package report

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

var t0 = time.Date(2026, 3, 9, 10, 0, 0, 0, time.UTC)

const origin = "http://shop.example:8080"

// fileSegment writes recs as an NDJSON segment and files it into the archive
// through the real Commit path. It returns the manifest entry.
func fileSegment(t *testing.T, a *evidence.Archive, machine, name string, recs []capture.Record, dropped uint64) capture.SegmentEntry {
	t.Helper()
	var buf bytes.Buffer
	for _, r := range recs {
		line, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	sum := sha256.Sum256(buf.Bytes())
	e := capture.SegmentEntry{
		File: name, SHA256: hex.EncodeToString(sum[:]), Bytes: int64(buf.Len()), Records: len(recs),
		Dropped: dropped, StartTime: recs[0].Timestamp, EndTime: recs[len(recs)-1].Timestamp,
		SchemaVersion: capture.SchemaVersion, Compression: capture.CompressionNone,
	}
	part := filepath.Join(t.TempDir(), name+".part")
	if err := os.WriteFile(part, buf.Bytes(), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Commit(machine, e, part); err != nil {
		t.Fatal(err)
	}
	return e
}

// pkts builds n records of one session; flow i%flows picks the destination port.
func pkts(session string, start time.Time, n, flows int) []capture.Record {
	out := make([]capture.Record, n)
	for i := range out {
		out[i] = capture.Record{
			SessionID: session, Timestamp: start.Add(time.Duration(i) * 100 * time.Millisecond),
			Protocol: capture.ProtoTCP, Src: "10.0.0.5:40000", Dst: "203.0.113.7:" + string(rune('0'+8+i%flows)),
			Flags: "SYN", PayloadStatus: capture.PayloadFull, PayloadBytesFull: 100,
			PayloadSHA256: strings.Repeat("ab", 32), SrcIPUsed: "10.0.0.5",
		}
	}
	return out
}

type fixture struct {
	spec Spec
	src  Sources
	segs map[string]capture.SegmentEntry
}

func newFixture(t *testing.T, withPlan bool) fixture {
	t.Helper()
	a, err := evidence.OpenArchive(t.TempDir(), evidence.ArchiveOptions{DiskUsage: func(string) (float64, error) { return 1, nil }})
	if err != nil {
		t.Fatal(err)
	}
	segs := map[string]capture.SegmentEntry{
		"w1/a": fileSegment(t, a, "worker-1", "seg-a.ndjson", pkts("S-w1-1", t0.Add(5*time.Second), 5, 2), 7),
		"w1/x": fileSegment(t, a, "worker-1", "seg-other.ndjson", pkts("S-unrelated", t0.Add(6*time.Second), 9, 1), 99),
		"w2/a": fileSegment(t, a, "worker-2", "seg-a.ndjson", pkts("S-w2-1", t0.Add(5*time.Second), 4, 3), 3),
	}

	end := t0.Add(60 * time.Second)
	spec := Spec{
		Project: "Acme Retail", TestID: "T-2026-0042", TestName: "Black Friday soak",
		Target: Target{IP: "203.0.113.7", Origin: origin, Paths: []string{"/", "/checkout"}},
		Start:  t0, End: end,
		Workers: []Worker{{Machine: "worker-1", Label: "rack A"}, {Machine: "worker-2"}, {Machine: "worker-3", Label: "late"}},
		Commands: []Command{
			{SessionID: "S-w1-1", Machine: "worker-1", Command: "wrk -t4 -c64 -d20s http://shop.example:8080/", User: "op", Mode: "oneshot", Start: t0.Add(5 * time.Second), End: t0.Add(25 * time.Second), ExitCode: 0},
			{SessionID: "S-w2-1", Machine: "worker-2", Command: "curl -s $T", User: "op", Mode: "oneshot", Start: t0.Add(10 * time.Second), End: t0.Add(40 * time.Second), ExitCode: 3},
		},
		Usage: []UsageSample{
			{Machine: "worker-1", Time: t0.Add(10 * time.Second), CoresUsed: 2.5, RAMGB: 1.25},
			{Machine: "worker-1", Time: t0.Add(20 * time.Second), CoresUsed: 1.5, RAMGB: 1.0},
			{Machine: "worker-2", Time: t0.Add(10 * time.Second), CoresUsed: 1.5, RAMGB: 0.75},
			{Machine: "worker-2", Time: t0.Add(20 * time.Second), CoresUsed: 1.0, RAMGB: 0.5},
			{Machine: "worker-1", Time: t0.Add(2 * time.Hour), CoresUsed: 64, RAMGB: 64}, // outside the window
		},
		Alignment: &Alignment{Intended: t0, Agents: []AgentStart{
			{Machine: "worker-1", StartedAt: t0.Add(12 * time.Millisecond)},
			{Machine: "worker-2", StartedAt: t0.Add(-8 * time.Millisecond)},
			{Machine: "worker-3", Missed: true, Note: "order arrived after T"},
		}},
		Watchdog: []WatchdogEvent{
			{Time: t0.Add(30 * time.Second), Machine: "worker-1", Action: "throttle", Detail: "cpu above 90 %", SessionID: "S-w1-1"},
			{Time: t0.Add(3 * time.Hour), Machine: "worker-2", Action: "reboot", Detail: "long after the test"},
		},
		Now: func() time.Time { return t0.Add(time.Hour) },
	}
	if withPlan {
		spec.Plan = &Plan{Name: "Checkout spikes", Pattern: PatternSpikes, TotalTime: 60 * time.Second,
			PatternLen: 6 * time.Second, SpikeLen: 2 * time.Second, MaxCores: 4, MaxRAMGB: 8,
			Items: []PlanItem{{Name: "wrk checkout", Command: "wrk -t4", Offset: 5 * time.Second, Duration: 20 * time.Second}}}
	}

	// Monitor: 60 http probes, one failed (index 20), plus 12 tcp probes; and
	// samples of another history and outside the window that must be ignored.
	key := monitor.HistoryKey(spec.Target.IP, origin)
	var samples []monitor.Sample
	for i := 0; i < 60; i++ {
		res := probe.Result{TargetID: "tg1", Type: probe.TypeHTTP, Time: t0.Add(time.Duration(i) * time.Second), OK: i != 20, Latency: time.Duration(20+i) * time.Millisecond, Status: 200}
		samples = append(samples, monitor.Sample{Agent: "mon-1", HistoryKey: key, Result: res})
	}
	for i := 0; i < 12; i++ {
		samples = append(samples, monitor.Sample{Agent: "mon-1", HistoryKey: key,
			Result: probe.Result{TargetID: "tg2", Type: probe.TypeTCP, Time: t0.Add(time.Duration(i*5) * time.Second), OK: true, Latency: 3 * time.Millisecond}})
	}
	samples = append(samples,
		monitor.Sample{Agent: "mon-1", HistoryKey: "198.51.100.1|", Result: probe.Result{Type: probe.TypeHTTP, Time: t0.Add(time.Second), OK: false}},
		monitor.Sample{Agent: "mon-1", HistoryKey: key, Result: probe.Result{Type: probe.TypeHTTP, Time: t0.Add(-time.Hour), OK: false}})
	return fixture{spec: spec, src: Sources{Archive: a, Samples: samples}, segs: segs}
}

func render(t *testing.T, f fixture) (string, *Data) {
	t.Helper()
	d, err := Gather(f.spec, f.src)
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var buf bytes.Buffer
	if err := Render(&buf, d); err != nil {
		t.Fatalf("Render: %v", err)
	}
	return buf.String(), d
}

func mustContain(t *testing.T, html string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(html, p) {
			t.Errorf("report is missing %q", p)
		}
	}
}

func TestReportHasEverySectionAndCorrectNumbers(t *testing.T) {
	f := newFixture(t, true)
	html, d := render(t, f)

	// one standalone document
	if n := strings.Count(html, "<!DOCTYPE html>"); n != 1 {
		t.Fatalf("want exactly one document, got %d", n)
	}
	// every section is present and reachable from the nav
	for _, id := range []string{"overview", "maneuver", "workers", "usage", "commands", "monitor", "evidence", "alignment", "watchdog"} {
		mustContain(t, html, `id="`+id+`"`, `href="#`+id+`"`)
	}

	// identity + target
	mustContain(t, html, "Acme Retail", "T-2026-0042", "Black Friday soak", "203.0.113.7", origin, "/checkout")

	// Maneuver plan + pattern
	mustContain(t, html, "Checkout spikes", "spikes", "pattern_length", "spike_duration", "wrk checkout", `data-chart="pattern"`)

	// workers: the declared three, each present
	if len(d.Workers) != 3 {
		t.Fatalf("workers = %d, want 3", len(d.Workers))
	}
	mustContain(t, html, "worker-1", "worker-2", "worker-3", "rack A")

	// resource usage: cores and GB units, aggregate = sum over workers
	if d.Usage.PeakCores != 4 || d.Usage.PeakRAMGB != 2 {
		t.Fatalf("aggregate peak = %v cores / %v GB, want 4 / 2", d.Usage.PeakCores, d.Usage.PeakRAMGB)
	}
	if d.Usage.MeanCores != 3.25 || d.Usage.MeanRAMGB != 1.75 { // t+10s: 2.5+1.5, 1.25+0.75; t+20s: 1.5+1.0, 1.0+0.5
		t.Errorf("aggregate mean = %v cores / %v GB, want 3.25 / 1.75", d.Usage.MeanCores, d.Usage.MeanRAMGB)
	}
	mustContain(t, html, "4.00 cores", "2.00 GB", "2.50 cores", "1.25 GB", "1.50 cores", "0.75 GB", "cores used", "GB RAM")
	if strings.Contains(html, "64.00 cores") {
		t.Error("an out-of-window usage sample leaked into the report")
	}
	wr := map[string]WorkerRow{}
	for _, w := range d.Workers {
		wr[w.Machine] = w
	}
	if w := wr["worker-1"]; w.PeakCores != 2.5 || w.PeakRAMGB != 1.25 {
		t.Errorf("worker-1 usage = %+v", w)
	}

	// commands with packet / flow counts (per command, per machine, total)
	if d.Commands[0].Packets != 5 || d.Commands[0].Flows != 2 || d.Commands[0].PayloadBytes != 500 {
		t.Errorf("command 1 = %+v, want 5 packets / 2 flows / 500 B", d.Commands[0])
	}
	if d.Commands[1].Packets != 4 || d.Commands[1].Flows != 3 {
		t.Errorf("command 2 = %+v, want 4 packets / 3 flows", d.Commands[1])
	}
	if d.Totals.Packets != 9 || d.Totals.Flows != 5 {
		t.Errorf("totals = %d packets / %d flows, want 9 / 5", d.Totals.Packets, d.Totals.Flows)
	}
	mustContain(t, html, "wrk -t4 -c64 -d20s", "S-w1-1", `<td class="num">5</td><td class="num">2</td><td class="num">500 B</td>`)

	// monitor: stats, both charts, command overlay + watchdog marker
	if !d.Monitor.Available || d.Monitor.Probes != 72 || d.Monitor.OK != 71 {
		t.Fatalf("monitor = %+v, want 72 probes / 71 ok (foreign history and out-of-window samples ignored)", d.Monitor)
	}
	if d.Monitor.Availability != 98.61 {
		t.Errorf("availability = %v, want 98.61", d.Monitor.Availability)
	}
	mustContain(t, html, `data-chart="availability"`, `data-chart="response"`, "98.61 %")
	for _, id := range []string{"availability", "response", "cores", "ram"} {
		svg := chartSVG(t, html, id)
		if n := strings.Count(svg, `class="ov ov`); n != 2 {
			t.Errorf("chart %s has %d overlay bands, want 2 (one per command)", id, n)
		}
		if !strings.Contains(svg, `data-label="worker-1: wrk -t4`) {
			t.Errorf("chart %s: overlay does not label its command", id)
		}
		if !strings.Contains(svg, `class="line s0"`) || !strings.Contains(svg, `data-pts="`) {
			t.Errorf("chart %s draws no series", id)
		}
		if !strings.Contains(svg, `class="mark"`) {
			t.Errorf("chart %s has no watchdog marker", id)
		}
	}

	// evidence: hashes, manifest refs, drop counts; the unrelated segment stays out
	if d.Evidence.Segments != 2 {
		t.Fatalf("evidence segments = %d, want 2 (the unrelated session's segment is excluded)", d.Evidence.Segments)
	}
	mustContain(t, html, f.segs["w1/a"].SHA256, f.segs["w2/a"].SHA256, "worker-1/manifest.json", "worker-2/manifest.json",
		"S-w1-1/seg-a.ndjson", d.Evidence.Digest, d.Evidence.Machines[0].Digest)
	if strings.Contains(html, f.segs["w1/x"].SHA256) {
		t.Error("an unrelated segment's hash is listed")
	}
	if d.Evidence.Dropped != 10 || d.Totals.Dropped != 10 {
		t.Errorf("drops = %d, want 7+3 = 10 (the unrelated segment's 99 excluded)", d.Evidence.Dropped)
	}
	mustContain(t, html, "Capture drops")

	// alignment + missed
	if d.Align == nil || len(d.Align.Missed) != 1 || d.Align.Missed[0] != "worker-3" {
		t.Fatalf("alignment = %+v", d.Align)
	}
	if d.Align.SpreadMS != 20 {
		t.Errorf("spread = %v ms, want 20 (+12 and -8)", d.Align.SpreadMS)
	}
	mustContain(t, html, "&#43;12.00 ms", "-8.00 ms", "order arrived after T", "missed")
	if !wr["worker-3"].Missed {
		t.Error("worker-3 not flagged missed in the workers table")
	}

	// watchdog: in-window event shown, out-of-window counted but not listed
	mustContain(t, html, "throttle", "cpu above 90 %")
	if strings.Contains(html, "long after the test") {
		t.Error("an out-of-window watchdog event is listed")
	}
	if d.WatchdogOutside != 1 {
		t.Errorf("WatchdogOutside = %d, want 1", d.WatchdogOutside)
	}
}

// chartSVG returns the <svg> of the chart with the given id.
func chartSVG(t *testing.T, html, id string) string {
	t.Helper()
	i := strings.Index(html, `data-chart="`+id+`"`)
	if i < 0 {
		t.Fatalf("no chart %q", id)
	}
	j := strings.Index(html[i:], "</svg>")
	if j < 0 {
		t.Fatalf("chart %q is not closed", id)
	}
	return html[i : i+j]
}

func TestReportIsSelfContained(t *testing.T) {
	html, d := render(t, newFixture(t, true))

	// No attribute may point at a network resource, and nothing may pull code
	// or style from elsewhere.
	for _, re := range []string{
		`(?i)\b(?:src|href|action|poster|data|srcset)\s*=\s*["']?\s*(?:https?:)?//`,
		`(?i)url\(\s*["']?\s*(?:https?:)?//`,
		`(?i)@import`,
		`(?i)<link\b`,
		`(?i)<script[^>]*\bsrc\s*=`,
		`(?i)<iframe|<img\b|<object|<embed|<video|<audio`,
		`(?i)\b(?:fetch|XMLHttpRequest|WebSocket|EventSource|sendBeacon|importScripts)\b`,
		`(?i)<meta[^>]+http-equiv=["']?refresh`,
	} {
		if m := regexp.MustCompile(re).FindString(html); m != "" {
			t.Errorf("external reference / network API found by %s: %q", re, m)
		}
	}
	// Apart from the target origin (data), no http(s):// string exists anywhere
	// in the file, including the inline CSS and JS.
	stripped := strings.ReplaceAll(html, d.Target.Origin, "ORIGIN")
	stripped = strings.ReplaceAll(stripped, "http://shop.example:8080", "ORIGIN") // from the fixture command text
	if m := regexp.MustCompile(`(?i)https?://`).FindString(stripped); m != "" {
		i := strings.Index(strings.ToLower(stripped), m)
		t.Errorf("found %q outside the target data: ...%s...", m, stripped[max(0, i-60):min(len(stripped), i+80)])
	}
	// CSS and JS are inline.
	mustContain(t, html, "<style>", "(function () {", `id="report-data"`)
}

func TestEmbeddedDataRoundTrips(t *testing.T) {
	html, d := render(t, newFixture(t, true))
	re := regexp.MustCompile(`(?s)<script type="application/json" id="report-data">(.*?)</script>`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no embedded data block")
	}
	var got Data
	if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
		t.Fatalf("embedded data is not valid JSON: %v", err)
	}
	if got.TestID != d.TestID || got.Totals != d.Totals || len(got.Commands) != 2 || got.Evidence.Digest != d.Evidence.Digest {
		t.Errorf("embedded data differs from the gathered data: %+v", got.Totals)
	}
}

func TestNonManeuverTestOmitsPlanSection(t *testing.T) {
	f := newFixture(t, false)
	f.spec.Alignment = nil // manual commands have no synchronized start
	html, d := render(t, f)

	if d.Plan != nil {
		t.Fatal("plan should be nil")
	}
	for _, gone := range []string{`id="maneuver"`, `href="#maneuver"`, `data-chart="pattern"`, "Maneuver plan", "Load pattern"} {
		if strings.Contains(html, gone) {
			t.Errorf("non-Maneuver report still contains %q", gone)
		}
	}
	// everything else still works
	for _, id := range []string{"overview", "workers", "usage", "commands", "monitor", "evidence", "alignment", "watchdog"} {
		mustContain(t, html, `id="`+id+`"`)
	}
	mustContain(t, html, "Manual commands only", "No synchronized start was recorded", "wrk -t4", f.segs["w1/a"].SHA256)
}

func TestMissingStoresAreSaidNotFaked(t *testing.T) {
	f := newFixture(t, false)
	f.src = Sources{}
	html, d := render(t, f)
	mustContain(t, html, "No evidence archive was available", "No monitor samples for this target", "No data in this window")
	if d.Totals.Packets != 0 || d.Totals.Availability != -1 {
		t.Errorf("totals = %+v", d.Totals)
	}
	// Still a valid, complete page.
	mustContain(t, html, `id="evidence"`, `id="monitor"`, "</html>")
}

func TestWindowDerivedFromDataAndRequiresID(t *testing.T) {
	f := newFixture(t, false)
	f.spec.Start, f.spec.End = time.Time{}, time.Time{}
	d, err := Gather(f.spec, f.src)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Start.Equal(t0) {
		t.Errorf("derived start = %v, want the earliest datum %v", d.Start, t0)
	}
	if !d.End.After(d.Start) {
		t.Errorf("derived window = %v .. %v", d.Start, d.End)
	}
	if _, err := Gather(Spec{}, Sources{}); err == nil {
		t.Error("Gather accepted a spec without a test id")
	}
	if _, err := Gather(Spec{TestID: "x"}, Sources{}); err == nil {
		t.Error("Gather accepted a spec with no window and no data")
	}
}

func TestHostileTextCannotBreakOutOfTheDocument(t *testing.T) {
	f := newFixture(t, true)
	evil := `</script><script>alert(1)</script><img src=x onerror=alert(2)>`
	f.spec.Commands[0].Command = evil
	f.spec.Project = evil
	f.spec.Watchdog[0].Detail = evil
	f.spec.Plan.Name = evil
	html, _ := render(t, f)
	if strings.Contains(html, "<script>alert") || strings.Contains(html, "<img src=x") {
		t.Fatal("unescaped markup from data reached the document")
	}
	if n := strings.Count(html, "</script>"); n != 2 {
		t.Errorf("found %d closing script tags, want exactly the 2 of the template", n)
	}
}

func TestWriteFileProducesOneFileAndLeavesNothing(t *testing.T) {
	f := newFixture(t, true)
	dir := t.TempDir()
	path := filepath.Join(dir, "report.html")
	if err := WriteFile(path, f.spec, f.src); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "report.html" {
		t.Fatalf("directory holds %v, want only report.html", ents)
	}
	b, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(b, []byte("T-2026-0042")) {
		t.Fatalf("report not written: %v", err)
	}
	// Replacing works too.
	if err := WriteFile(path, f.spec, f.src); err != nil {
		t.Fatal(err)
	}
}

func TestGatherDoesNotModifyTheArchive(t *testing.T) {
	f := newFixture(t, true)
	snap := func() string {
		var sb strings.Builder
		_ = filepath.Walk(f.src.Archive.Root(), func(p string, info os.FileInfo, err error) error {
			if err == nil {
				sb.WriteString(p + "|" + info.ModTime().String() + "|" + string(rune(info.Size())) + "\n")
			}
			return nil
		})
		return sb.String()
	}
	before := snap()
	if _, err := Gather(f.spec, f.src); err != nil {
		t.Fatal(err)
	}
	if after := snap(); after != before {
		t.Error("Gather changed the archive")
	}
}

func TestBucketingBoundsPoints(t *testing.T) {
	if b := bucketFor(30 * time.Second); b != time.Second {
		t.Errorf("short window bucket = %v", b)
	}
	if b := bucketFor(24 * time.Hour); 24*time.Hour/b > maxChartPoints {
		t.Errorf("24h window yields %d points", 24*time.Hour/b)
	}
}

func TestPatternShapes(t *testing.T) {
	start := t0
	if pts := patternShape(&Plan{Pattern: PatternAllInOne}, start, time.Minute); len(pts) != 2 || pts[0].V != 100 {
		t.Errorf("all-in-one = %v", pts)
	}
	lin := patternShape(&Plan{Pattern: PatternLinear, WindowLength: 10 * time.Second}, start, 30*time.Second)
	if len(lin) != 9 || lin[1].V != 100 || lin[2].V != 0 {
		t.Errorf("linear = %v", lin)
	}
	sp := patternShape(&Plan{Pattern: PatternSpikes, PatternLen: 6 * time.Second, SpikeLen: 2 * time.Second}, start, 12*time.Second)
	if len(sp) != 12 {
		t.Fatalf("spikes = %v", sp)
	}
	// spike centered in the period: 2 s gap, 2 s spike, 2 s gap
	if sp[2].T != start.Add(2*time.Second).UnixMilli() || sp[2].V != 100 || sp[3].T != start.Add(4*time.Second).UnixMilli() {
		t.Errorf("spike not centered: %v", sp[:6])
	}
	if patternShape(&Plan{Pattern: PatternSpikes, PatternLen: time.Second, SpikeLen: 2 * time.Second}, start, time.Minute) != nil {
		t.Error("an invalid spike pattern produced a shape")
	}
}

// TestWriteSample writes the fixture report to $REPORT_SAMPLE_OUT, for looking
// at it in a browser. It does nothing when the variable is unset.
func TestWriteSample(t *testing.T) {
	out := os.Getenv("REPORT_SAMPLE_OUT")
	if out == "" {
		t.Skip("REPORT_SAMPLE_OUT not set")
	}
	f := newFixture(t, true)
	if err := WriteFile(out, f.spec, f.src); err != nil {
		t.Fatal(err)
	}
}

func TestUnits(t *testing.T) {
	if GBFromBytes(2_500_000_000) != 2.5 {
		t.Error("GBFromBytes is not decimal GB")
	}
	if got := humanBytes(1234567); got != "1.23 MB" {
		t.Errorf("humanBytes = %q", got)
	}
}
