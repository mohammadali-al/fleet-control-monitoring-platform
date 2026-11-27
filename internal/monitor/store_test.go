package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

// fakeVM implements the VictoriaMetrics HTTP endpoints used by VMStore
// (/api/v1/import, /api/v1/export, /api/v1/query_range) and persists series
// lines to disk so tests can prove persistence across store restarts.
type fakeVM struct {
	mu      sync.Mutex
	walPath string
	lines   []vmImportLine
}

func newFakeVM(t *testing.T, walPath string) (*fakeVM, *httptest.Server) {
	t.Helper()
	vm := &fakeVM{walPath: walPath}
	if err := vm.load(); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(vm.serveHTTP))
	t.Cleanup(srv.Close)
	return vm, srv
}

func (vm *fakeVM) load() error {
	data, err := os.ReadFile(vm.walPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var ln vmImportLine
		if err := json.Unmarshal(raw, &ln); err != nil {
			return err
		}
		vm.lines = append(vm.lines, ln)
	}
	return sc.Err()
}

func (vm *fakeVM) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/v1/import":
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var incoming []vmImportLine
		sc := bufio.NewScanner(bytes.NewReader(body))
		for sc.Scan() {
			raw := bytes.TrimSpace(sc.Bytes())
			if len(raw) == 0 {
				continue
			}
			var ln vmImportLine
			if err := json.Unmarshal(raw, &ln); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			incoming = append(incoming, ln)
		}
		vm.mu.Lock()
		vm.lines = append(vm.lines, incoming...)
		if vm.walPath != "" && len(body) > 0 {
			f, err := os.OpenFile(vm.walPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err == nil {
				_, _ = f.Write(body)
				if !bytes.HasSuffix(body, []byte("\n")) {
					_, _ = f.WriteString("\n")
				}
				_ = f.Close()
			}
		}
		vm.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case "/api/v1/export":
		sel := r.URL.Query().Get("match[]")
		metric, labels := parseSimpleSelector(sel)
		startT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
		endT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
		startMs, endMs := startT.UnixMilli(), endT.UnixMilli()

		vm.mu.Lock()
		lines := append([]vmImportLine(nil), vm.lines...)
		vm.mu.Unlock()

		w.Header().Set("Content-Type", "application/x-ndjson")
		enc := json.NewEncoder(w)
		for _, ln := range lines {
			if !matchLabels(ln.Metric, metric, labels) {
				continue
			}
			var out vmImportLine
			out.Metric = ln.Metric
			for i := 0; i < min(len(ln.Values), len(ln.Timestamps)); i++ {
				ts := ln.Timestamps[i]
				if ts >= startMs && ts <= endMs {
					out.Values = append(out.Values, ln.Values[i])
					out.Timestamps = append(out.Timestamps, ts)
				}
			}
			if len(out.Values) > 0 {
				_ = enc.Encode(out)
			}
		}

	case "/api/v1/query_range":
		sel := r.URL.Query().Get("query")
		metric, labels := parseSimpleSelector(sel)
		startT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
		endT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
		startMs, endMs := startT.UnixMilli(), endT.UnixMilli()

		vm.mu.Lock()
		lines := append([]vmImportLine(nil), vm.lines...)
		vm.mu.Unlock()

		type resItem struct {
			Metric map[string]string `json:"metric"`
			Values [][]any           `json:"values"`
		}
		var results []resItem
		for _, ln := range lines {
			if !matchLabels(ln.Metric, metric, labels) {
				continue
			}
			var vals [][]any
			for i := 0; i < min(len(ln.Values), len(ln.Timestamps)); i++ {
				ts := ln.Timestamps[i]
				if ts >= startMs && ts <= endMs {
					vals = append(vals, []any{float64(ts) / 1000.0, strconv.FormatFloat(ln.Values[i], 'f', -1, 64)})
				}
			}
			if len(vals) > 0 {
				results = append(results, resItem{Metric: ln.Metric, Values: vals})
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "success",
			"data": map[string]any{
				"resultType": "matrix",
				"result":     results,
			},
		})

	default:
		http.NotFound(w, r)
	}
}

func parseSimpleSelector(sel string) (string, map[string]string) {
	labels := map[string]string{}
	idx := strings.IndexByte(sel, '{')
	if idx < 0 {
		return sel, labels
	}
	metric := sel[:idx]
	body := strings.TrimSuffix(sel[idx+1:], "}")
	for _, part := range strings.Split(body, ",") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"`)
		v = strings.ReplaceAll(v, `\"`, `"`)
		v = strings.ReplaceAll(v, `\\`, `\`)
		labels[k] = v
	}
	return metric, labels
}

func matchLabels(m map[string]string, metric string, want map[string]string) bool {
	if m["__name__"] != metric {
		return false
	}
	for k, v := range want {
		if m[k] != v {
			return false
		}
	}
	return true
}

func TestIngestorToVMStoreRangeQueryAndRestart(t *testing.T) {
	wal := filepath.Join(t.TempDir(), "vm.ndjson")
	_, srv1 := newFakeVM(t, wal)

	store1, err := NewVMStore(VMStoreConfig{Addr: srv1.URL, Retention: "90d"})
	if err != nil {
		t.Fatal(err)
	}
	mem := &MemorySink{}
	ing := NewIngestor(NewMultiSink(mem, store1), func(targetID string) (string, bool) {
		if strings.HasPrefix(targetID, "mj-0001/") {
			return "198.51.100.10|https://shop.example.test:443", true
		}
		return "", false
	})

	base := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	env, err := channel.NewEnvelope(channel.TypeMonData, "mon-1", Batch{
		Seq: 1,
		Results: []probe.Result{
			{TargetID: "mj-0001/http/0", Type: probe.TypeHTTP, Time: base, OK: true, Latency: 42 * time.Millisecond, Status: 200},
			{TargetID: "mj-0001/http/0", Type: probe.TypeHTTP, Time: base.Add(10 * time.Second), OK: false, Error: "timeout"},
			{TargetID: "mj-0001/icmp", Type: probe.TypeICMP, Time: base.Add(20 * time.Second), OK: true, Latency: 7 * time.Millisecond, Sent: 3, Received: 2, Loss: 0.33},
			{TargetID: "mj-0002/tcp", Type: probe.TypeTCP, Time: base.Add(3 * time.Hour), OK: true, Latency: 15 * time.Millisecond},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ing.Handle(context.Background(), "mon1", env); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(mem.Samples()) != 4 {
		t.Fatalf("MemorySink got %d samples, want 4", len(mem.Samples()))
	}

	// Simulate VictoriaMetrics + fleet-server restart by closing srv1 and
	// bringing up a fresh fakeVM + VMStore backed by the same WAL file.
	srv1.Close()
	_, srv2 := newFakeVM(t, wal)
	store2, err := NewVMStore(VMStoreConfig{Addr: srv2.URL})
	if err != nil {
		t.Fatal(err)
	}

	// Raw export query for latency_ms in [base-1m, base+1m] excludes the +3h sample.
	got, err := store2.QueryRange(context.Background(), HistoryQuery{
		Metric: "latency_ms",
		Agent:  "mon1",
		Start:  base.Add(-time.Minute),
		End:    base.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("QueryRange raw: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("raw series count = %d, want 2: %+v", len(got), got)
	}
	if got[0].Target != "mj-0001/http/0" || got[0].HistoryKey != "198.51.100.10|https://shop.example.test:443" || len(got[0].Points) != 2 {
		t.Fatalf("first series = %+v", got[0])
	}
	if got[0].Points[0].Value != 42 || got[0].Points[1].Value != 0 {
		t.Errorf("http latency points = %+v, want [42, 0]", got[0].Points)
	}
	if got[1].Target != "mj-0001/icmp" || got[1].Points[0].Value != 7 {
		t.Errorf("icmp series = %+v", got[1])
	}

	// Query ok metric filtered by target.
	okGot, err := store2.QueryRange(context.Background(), HistoryQuery{
		Metric: "ok",
		Target: "mj-0001/http/0",
		Start:  base.Add(-time.Minute),
		End:    base.Add(time.Minute),
	})
	if err != nil || len(okGot) != 1 || len(okGot[0].Points) != 2 {
		t.Fatalf("ok query = %+v err=%v", okGot, err)
	}
	if okGot[0].Points[0].Value != 1 || okGot[0].Points[1].Value != 0 {
		t.Errorf("ok values = %+v, want [1, 0]", okGot[0].Points)
	}

	// Query status and loss metrics.
	stGot, err := store2.QueryRange(context.Background(), HistoryQuery{
		Metric: MetricStatus,
		Start:  base.Add(-time.Minute),
		End:    base.Add(time.Minute),
	})
	if err != nil || len(stGot) != 1 || stGot[0].Points[0].Value != 200 {
		t.Fatalf("status query = %+v err=%v", stGot, err)
	}
	lossGot, err := store2.QueryRange(context.Background(), HistoryQuery{
		Metric: MetricLoss,
		Type:   probe.TypeICMP,
		Start:  base.Add(-time.Minute),
		End:    base.Add(time.Minute),
	})
	if err != nil || len(lossGot) != 1 || lossGot[0].Points[0].Value != 0.33 {
		t.Fatalf("loss query = %+v err=%v", lossGot, err)
	}

	// Downsampled query_range path (Step > 0) returns the +3h sample too when range includes it.
	stepGot, err := store2.QueryRange(context.Background(), HistoryQuery{
		Metric: MetricLatencyMs,
		Target: "mj-0002/tcp",
		Start:  base.Add(2 * time.Hour),
		End:    base.Add(4 * time.Hour),
		Step:   time.Minute,
	})
	if err != nil || len(stepGot) != 1 || stepGot[0].Points[0].Value != 15 {
		t.Fatalf("step query = %+v err=%v", stepGot, err)
	}
}

func TestMultiSinkIsolatesStoreErrors(t *testing.T) {
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "disk full", http.StatusInternalServerError)
	}))
	defer badSrv.Close()

	vm, err := NewVMStore(VMStoreConfig{Addr: badSrv.URL})
	if err != nil {
		t.Fatal(err)
	}
	mem := &MemorySink{}
	ms := NewMultiSink(mem, vm)

	err = ms.Write(context.Background(), []Sample{{
		Agent:  "mon1",
		Result: probe.Result{TargetID: "mj-0001/icmp", Type: probe.TypeICMP, Time: time.Now(), OK: true, Latency: 5 * time.Millisecond},
	}})
	if err == nil {
		t.Fatal("expected error from failing VMStore")
	}
	if len(mem.Samples()) != 1 {
		t.Errorf("MemorySink got %d samples, want 1 even when second sink failed", len(mem.Samples()))
	}
}

func TestVMStoreValidation(t *testing.T) {
	if _, err := NewVMStore(VMStoreConfig{}); err == nil {
		t.Error("empty addr should fail")
	}
	if _, err := NewVMStore(VMStoreConfig{Addr: "ftp://localhost:8428"}); err == nil {
		t.Error("non-http scheme should fail")
	}
	vm, err := NewVMStore(VMStoreConfig{Addr: "http://127.0.0.1:8428"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for name, q := range map[string]HistoryQuery{
		"bad metric": {Metric: "bad metric!", Start: now.Add(-time.Minute), End: now},
		"bad type":   {Type: "ftp", Start: now.Add(-time.Minute), End: now},
		"end<start":  {Start: now, End: now.Add(-time.Minute)},
		"neg step":   {Start: now.Add(-time.Minute), End: now, Step: -time.Second},
	} {
		if _, err := vm.QueryRange(context.Background(), q); !errors.Is(err, ErrBadQuery) {
			t.Errorf("%s: got err=%v, want ErrBadQuery", name, err)
		}
	}
}
