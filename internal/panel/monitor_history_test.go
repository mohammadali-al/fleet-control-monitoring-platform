package panel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

type panelVMLine struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

func startPanelFakeVM(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var lines []panelVMLine
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/import":
			body, _ := io.ReadAll(r.Body)
			sc := bufio.NewScanner(bytes.NewReader(body))
			var batch []panelVMLine
			for sc.Scan() {
				raw := bytes.TrimSpace(sc.Bytes())
				if len(raw) == 0 {
					continue
				}
				var ln panelVMLine
				if err := json.Unmarshal(raw, &ln); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				batch = append(batch, ln)
			}
			mu.Lock()
			lines = append(lines, batch...)
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)

		case "/api/v1/export":
			sel := r.URL.Query().Get("match[]")
			metric, labels := parseTestSelector(sel)
			startT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
			endT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
			mu.Lock()
			cp := append([]panelVMLine(nil), lines...)
			mu.Unlock()
			enc := json.NewEncoder(w)
			for _, ln := range cp {
				if !matchTestLabels(ln.Metric, metric, labels) {
					continue
				}
				var out panelVMLine
				out.Metric = ln.Metric
				for i := 0; i < min(len(ln.Values), len(ln.Timestamps)); i++ {
					ts := ln.Timestamps[i]
					if ts >= startT.UnixMilli() && ts <= endT.UnixMilli() {
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
			metric, labels := parseTestSelector(sel)
			startT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
			endT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
			mu.Lock()
			cp := append([]panelVMLine(nil), lines...)
			mu.Unlock()
			type item struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			}
			var res []item
			for _, ln := range cp {
				if !matchTestLabels(ln.Metric, metric, labels) {
					continue
				}
				var vals [][]any
				for i := 0; i < min(len(ln.Values), len(ln.Timestamps)); i++ {
					ts := ln.Timestamps[i]
					if ts >= startT.UnixMilli() && ts <= endT.UnixMilli() {
						vals = append(vals, []any{float64(ts) / 1000.0, strconv.FormatFloat(ln.Values[i], 'f', -1, 64)})
					}
				}
				if len(vals) > 0 {
					res = append(res, item{Metric: ln.Metric, Values: vals})
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "success",
				"data":   map[string]any{"resultType": "matrix", "result": res},
			})

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func parseTestSelector(sel string) (string, map[string]string) {
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
		labels[k] = strings.Trim(v, `"`)
	}
	return metric, labels
}

func matchTestLabels(m map[string]string, metric string, want map[string]string) bool {
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

func TestMonitorHistoryEndpoint(t *testing.T) {
	// Without Store bound, /api/monitor/history returns 503.
	eNoStore := newObsEnv(t, nil)
	if code, _ := eNoStore.req("GET", "/api/monitor/history", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("history without store = %d, want 503", code)
	}

	vmSrv := startPanelFakeVM(t)
	e := newObsEnv(t, func(_ *obsEnv, o *fleet.Observe) {
		vm, err := monitor.NewVMStore(monitor.VMStoreConfig{Addr: vmSrv.URL, Retention: "90d"})
		if err != nil {
			t.Fatal(err)
		}
		o.Store = vm
	})
	e.waitState("mon1", fleet.StateOnline)

	var created struct{ Job monitor.Job }
	if code, _ := e.req("POST", "/api/monitor/jobs", map[string]any{"url": "https://api.example.test/health", "ip": "198.51.100.20"}, &created); code != http.StatusCreated {
		t.Fatalf("create job = %d", code)
	}
	job := created.Job
	httpTarget := job.ID + "/http/0"

	now := time.Now().UTC().Truncate(time.Millisecond)
	oldTime := now.Add(-2 * time.Hour)
	recentTime := now.Add(-15 * time.Second)
	e.link("mon1").h(context.Background(), monDataEnvelope(t, 1,
		probe.Result{TargetID: httpTarget, Type: probe.TypeHTTP, Time: oldTime, OK: true, Latency: 88 * time.Millisecond, Status: 200},
		probe.Result{TargetID: httpTarget, Type: probe.TypeHTTP, Time: recentTime, OK: true, Latency: 21 * time.Millisecond, Status: 200},
		probe.Result{TargetID: job.ID + "/icmp", Type: probe.TypeICMP, Time: recentTime, OK: true, Latency: 5 * time.Millisecond},
	))

	// Live window still holds only the recent points.
	var live struct {
		Series []fleet.SeriesData `json:"series"`
	}
	if code, _ := e.req("GET", "/api/monitor/live?window=60", nil, &live); code != http.StatusOK || len(live.Series) != 2 {
		t.Fatalf("live = %d %+v", code, live)
	}
	for _, sd := range live.Series {
		if sd.Target == httpTarget && len(sd.Points) != 1 {
			t.Errorf("live window http points = %+v, want only the recent point", sd.Points)
		}
	}

	// History query over 3h returns both the old (2h ago) and recent HTTP latency points.
	q := url.Values{}
	q.Set("metric", "latency_ms")
	q.Set("target", httpTarget)
	q.Set("from", now.Add(-3*time.Hour).Format(time.RFC3339Nano))
	q.Set("to", now.Add(time.Minute).Format(time.RFC3339Nano))
	var hist struct {
		Metric  string                  `json:"metric"`
		StepSec float64                 `json:"step_sec"`
		Series  []monitor.HistorySeries `json:"series"`
	}
	if code, _ := e.req("GET", "/api/monitor/history?"+q.Encode(), nil, &hist); code != http.StatusOK {
		t.Fatalf("history = %d", code)
	}
	if hist.Metric != monitor.MetricLatencyMs || hist.StepSec != 0 || len(hist.Series) != 1 || len(hist.Series[0].Points) != 2 {
		t.Fatalf("unexpected history response = %+v", hist)
	}
	if hist.Series[0].Points[0].Value != 88 || hist.Series[0].Points[1].Value != 21 {
		t.Errorf("history points = %+v, want [88, 21]", hist.Series[0].Points)
	}
	if hist.Series[0].HistoryKey != job.HistoryKey || hist.Series[0].Agent != "mon1" {
		t.Errorf("series metadata = %+v", hist.Series[0])
	}

	// Step query (downsampled /api/v1/query_range path) works too.
	q.Set("step", "60s")
	hist.Series = nil
	if code, _ := e.req("GET", "/api/monitor/history?"+q.Encode(), nil, &hist); code != http.StatusOK || hist.StepSec != 60 || len(hist.Series) != 1 {
		t.Fatalf("step history = %d %+v", code, hist)
	}

	// Validation errors return 400.
	for _, bad := range []string{
		"/api/monitor/history?from=not-a-time",
		"/api/monitor/history?to=not-a-time",
		"/api/monitor/history?from=" + url.QueryEscape(now.Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)),
		"/api/monitor/history?step=-5s",
		"/api/monitor/history?metric=bad%20name",
		"/api/monitor/history?type=bogus",
	} {
		if code, _ := e.req("GET", bad, nil, nil); code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", bad, code)
		}
	}
}
