package panel

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/agent"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet/fleettest"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/report"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

func TestProjectReportWithoutBackend(t *testing.T) {
	srv := noBackend(t)
	status, _, raw := body(t, srv.URL+"/api/projects/p1/tests/t1/report")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("GET report without backend = %d (%s), want 503", status, raw)
	}
}

func TestProjectReportGenerationAndErrors(t *testing.T) {
	mt, err := fleettest.NewMTLS()
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	ra, err := mt.StartAgent("w1", agent.Config{DataDir: t.TempDir(), TermGrace: 200 * time.Millisecond, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ra.Stop() })

	vmSrv := startPanelFakeVM(t)
	vmStore, err := monitor.NewVMStore(monitor.VMStoreConfig{Addr: vmSrv.URL, Retention: "90d"})
	if err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	reg := registry.New()
	for _, mc := range []registry.Machine{
		{ID: "w1", Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.1"}, Provisioned: true},
		{ID: "mon1", Role: registry.RoleMonitor, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"127.0.0.2"}, Provisioned: true},
	} {
		if err := reg.Add(mc); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("panel-secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	mgr, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		NewLink:      mt.Factory(),
		PollInterval: 10 * time.Millisecond,
		GateSecret:   []byte("panel-secret"),
		DataDir:      dataDir,
		Logger:       quiet,
	})
	if err != nil {
		t.Fatal(err)
	}
	obs, err := fleet.OpenObserve(dataDir, quiet)
	if err != nil {
		t.Fatalf("OpenObserve: %v", err)
	}
	obs.Store = vmStore
	mgr.SetObserve(obs)

	ctx, cancel := context.WithCancel(context.Background())
	mgr.Start(ctx)
	t.Cleanup(func() { cancel(); mgr.Close() })

	h, err := New(Options{Assets: web.FS, Templates: web.Templates, Backend: mgr, Logger: quiet})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if info, ok := mgr.Machine("w1"); ok && info.State == fleet.StateOnline {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Seed baseline, maneuver, project, and tests.
	status, raw := doJSON(t, http.MethodPost, srv.URL+"/api/baseline", map[string]any{
		"id":               "sleep-probe",
		"name":             "Sleep probe",
		"command_template": "echo target={ip}; sleep {duration}",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create baseline = %d (%s)", status, raw)
	}
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/maneuvers", map[string]any{
		"id":        "mv-rep",
		"name":      "Report Probe Maneuver",
		"target":    map[string]string{"ip": "198.51.100.10", "origin": "https://shop.example.test"},
		"total_sec": 20,
		"pattern":   map[string]any{"type": "all"},
		"timeline": []map[string]any{
			{"id": "item-1", "baseline_id": "sleep-probe", "start_sec": 0, "duration_sec": 15, "replicas": 1},
		},
		"agents": []string{"w1"},
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create maneuver = %d (%s)", status, raw)
	}

	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects", map[string]any{
		"id":   "acme",
		"name": "Acme Report Test",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create project = %d (%s)", status, raw)
	}

	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/acme/tests", map[string]any{
		"id":          "t-planned",
		"name":        "Planned Test",
		"maneuver_id": "mv-rep",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create planned test = %d (%s)", status, raw)
	}
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/acme/tests", map[string]any{
		"id":          "t-run",
		"name":        "Completed Test #1",
		"maneuver_id": "mv-rep",
	}, nil)
	if status != http.StatusCreated {
		t.Fatalf("create run test = %d (%s)", status, raw)
	}

	// Unknown project / unknown test / planned (incomplete) test all return 404.
	for _, path := range []string{
		"/api/projects/nope/tests/t-planned/report",
		"/api/projects/acme/tests/nope/report",
		"/api/projects/acme/tests/t-planned/report",
	} {
		code, _, respBody := body(t, srv.URL+path)
		if code != http.StatusNotFound {
			t.Errorf("GET %s = %d (%s), want 404", path, code, respBody)
		}
	}

	// Start t-run -> running (incomplete) must also return 404.
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/acme/tests/t-run/start", map[string]any{
		"create_monitor": true,
		"lead_ms":        250,
	}, map[string]string{"Remote-User": "alice"})
	if status != http.StatusAccepted {
		t.Fatalf("start t-run = %d (%s)", status, raw)
	}
	var startRes fleet.StartProjectTestResult
	if err := json.Unmarshal(raw, &startRes); err != nil {
		t.Fatal(err)
	}
	if code, _, respBody := body(t, srv.URL+"/api/projects/acme/tests/t-run/report"); code != http.StatusNotFound {
		t.Fatalf("GET running test report = %d (%s), want 404", code, respBody)
	}

	// Wait slightly so the test window spans >0 ms, then stop to complete the test.
	time.Sleep(150 * time.Millisecond)
	status, raw = doJSON(t, http.MethodPost, srv.URL+"/api/projects/acme/tests/t-run/stop", map[string]any{}, map[string]string{"Remote-User": "alice"})
	if status != http.StatusOK {
		t.Fatalf("stop t-run = %d (%s)", status, raw)
	}
	var stopRes struct {
		Test project.Test `json:"test"`
	}
	if err := json.Unmarshal(raw, &stopRes); err != nil {
		t.Fatal(err)
	}
	finished := stopRes.Test
	if finished.ResultPointers == nil || finished.ResultPointers.Evidence == nil || finished.ResultPointers.Monitor == nil {
		t.Fatalf("missing result_pointers on finished test: %+v", finished)
	}

	// Seed monitor history samples in VMStore inside and outside the test window.
	winStart := finished.ResultPointers.Monitor.From
	winEnd := *finished.ResultPointers.Monitor.To
	mid := winStart.Add(winEnd.Sub(winStart) / 2)
	monKey := finished.ResultPointers.Monitor.HistoryKey
	targetID := finished.ResultPointers.Monitor.JobID + "/http/0"
	if err := vmStore.Write(context.Background(), []monitor.Sample{
		{
			Agent:      "mon1",
			HistoryKey: monKey,
			Result:     probe.Result{TargetID: targetID, Type: probe.TypeHTTP, Time: winStart.Add(-10 * time.Minute), OK: false, Latency: 0},
		},
		{
			Agent:      "mon1",
			HistoryKey: monKey,
			Result:     probe.Result{TargetID: targetID, Type: probe.TypeHTTP, Time: winStart, OK: true, Latency: 25 * time.Millisecond, Status: 200},
		},
		{
			Agent:      "mon1",
			HistoryKey: monKey,
			Result:     probe.Result{TargetID: targetID, Type: probe.TypeHTTP, Time: mid, OK: false, Latency: 0, Status: 503},
		},
		{
			Agent:      "mon1",
			HistoryKey: monKey,
			Result:     probe.Result{TargetID: targetID, Type: probe.TypeHTTP, Time: winEnd, OK: true, Latency: 35 * time.Millisecond, Status: 200},
		},
		{
			Agent:      "mon1",
			HistoryKey: "203.0.113.99|https://unrelated.test:443",
			Result:     probe.Result{TargetID: "other/http/0", Type: probe.TypeHTTP, Time: mid, OK: false},
		},
	}); err != nil {
		t.Fatalf("vmStore.Write: %v", err)
	}

	// Commit archived evidence segments for w1: one matching this test's ManeuverRunID,
	// and one belonging to an unrelated run that must NOT appear in the report.
	commitReportSegment := func(file string, recs []capture.Record, dropped uint64) string {
		t.Helper()
		var buf bytes.Buffer
		for _, rec := range recs {
			b, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		sum := sha256.Sum256(buf.Bytes())
		shaHex := hex.EncodeToString(sum[:])
		part := filepath.Join(t.TempDir(), file+".part")
		if err := os.WriteFile(part, buf.Bytes(), 0o640); err != nil {
			t.Fatal(err)
		}
		entry := capture.SegmentEntry{
			File:          file,
			SHA256:        shaHex,
			Bytes:         int64(buf.Len()),
			Records:       len(recs),
			Dropped:       dropped,
			StartTime:     recs[0].Timestamp,
			EndTime:       recs[len(recs)-1].Timestamp,
			SchemaVersion: capture.SchemaVersion,
			Compression:   capture.CompressionNone,
		}
		if _, err := obs.Archive.Commit("w1", entry, part); err != nil {
			t.Fatalf("Archive.Commit: %v", err)
		}
		return shaHex
	}

	goodSHA := commitReportSegment("seg-test.ndjson", []capture.Record{
		{
			SessionID:        "S-rep-1",
			Timestamp:        winStart.Add(10 * time.Millisecond),
			Protocol:         capture.ProtoTCP,
			Src:              "10.0.0.1:41000",
			Dst:              "198.51.100.10:443",
			Flags:            "SYN",
			PayloadStatus:    capture.PayloadFull,
			PayloadBytesFull: 120,
			PayloadSHA256:    strings.Repeat("ab", 32),
			SrcIPUsed:        "10.0.0.1",
			ManeuverID:       "mv-rep",
			RunID:            finished.ManeuverRunID,
			ItemID:           "item-1",
			Replica:          1,
		},
		{
			SessionID:        "S-rep-1",
			Timestamp:        winStart.Add(20 * time.Millisecond),
			Protocol:         capture.ProtoTCP,
			Src:              "10.0.0.1:41001",
			Dst:              "198.51.100.10:443",
			Flags:            "SYN",
			PayloadStatus:    capture.PayloadFull,
			PayloadBytesFull: 80,
			PayloadSHA256:    strings.Repeat("cd", 32),
			SrcIPUsed:        "10.0.0.1",
			ManeuverID:       "mv-rep",
			RunID:            finished.ManeuverRunID,
			ItemID:           "item-1",
			Replica:          1,
		},
	}, 4)

	otherSHA := commitReportSegment("seg-other.ndjson", []capture.Record{
		{
			SessionID:        "S-other-99",
			Timestamp:        mid,
			Protocol:         capture.ProtoTCP,
			Src:              "10.0.0.1:42000",
			Dst:              "203.0.113.50:80",
			Flags:            "SYN",
			PayloadStatus:    capture.PayloadFull,
			PayloadBytesFull: 50,
			PayloadSHA256:    strings.Repeat("ef", 32),
			SrcIPUsed:        "10.0.0.1",
			ManeuverID:       "mv-other",
			RunID:            "m-unrelated",
		},
	}, 99)

	// Request inline report for the completed test.
	code, hdr, html := body(t, srv.URL+"/api/projects/acme/tests/t-run/report")
	if code != http.StatusOK {
		t.Fatalf("GET completed test report = %d (%s)", code, html)
	}
	if ct := hdr.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html...", ct)
	}
	if cd := hdr.Get("Content-Disposition"); !strings.Contains(cd, "inline") || !strings.Contains(cd, `filename="report-acme-t-run.html"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	if !strings.Contains(html, "<!DOCTYPE html>") {
		t.Fatalf("report does not look like an HTML document")
	}

	for _, want := range []string{
		"Acme Report Test",
		"Completed Test #1",
		"Report Probe Maneuver",
		"198.51.100.10",
		"https://shop.example.test",
		"S-rep-1",
		goodSHA,
		"66.67 %",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("generated report HTML missing %q", want)
		}
	}
	if strings.Contains(html, otherSHA) || strings.Contains(html, "S-other-99") {
		t.Errorf("unrelated run's evidence leaked into report")
	}

	// Verify embedded JSON data inside the self-contained HTML report.
	re := regexp.MustCompile(`(?s)<script type="application/json" id="report-data">(.*?)</script>`)
	m := re.FindStringSubmatch(html)
	if m == nil {
		t.Fatal("missing embedded #report-data JSON in HTML report")
	}
	var embedded report.Data
	if err := json.Unmarshal([]byte(m[1]), &embedded); err != nil {
		t.Fatalf("unmarshal embedded report data: %v", err)
	}
	if embedded.TestID != "t-run" || embedded.Totals.Packets != 2 || embedded.Totals.Flows != 2 || embedded.Totals.Dropped != 4 {
		t.Errorf("unexpected embedded totals: %+v", embedded.Totals)
	}
	if !embedded.Monitor.Available || embedded.Monitor.Probes != 3 || embedded.Monitor.OK != 2 {
		t.Errorf("unexpected embedded monitor stats: %+v", embedded.Monitor)
	}

	// Request report as download attachment.
	code, hdr, dlBody := body(t, srv.URL+"/api/projects/acme/tests/t-run/report?download=1")
	if code != http.StatusOK {
		t.Fatalf("GET download report = %d", code)
	}
	if cd := hdr.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, `filename="report-acme-t-run.html"`) {
		t.Errorf("download Content-Disposition = %q", cd)
	}
	if !strings.Contains(dlBody, "<!DOCTYPE html>") {
		t.Errorf("download body missing HTML doctype")
	}
}

func TestProjectsBundleHasReportWiring(t *testing.T) {
	srv := noBackend(t)
	status, _, js := body(t, srv.URL+"/assets/dist/app.js")
	if status != http.StatusOK || len(js) == 0 {
		t.Fatalf("GET /assets/dist/app.js = %d", status)
	}
	for _, want := range []string{
		"/report",
		"download=1",
		"Open self-contained interactive HTML report in a new tab",
	} {
		if !strings.Contains(js, want) {
			t.Errorf("SPA bundle lacks %q", want)
		}
	}
}
