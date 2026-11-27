package fleet_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

type vmLine struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

func startTestVM(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	var lines []vmLine
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/import":
			body, _ := io.ReadAll(r.Body)
			sc := bufio.NewScanner(bytes.NewReader(body))
			var batch []vmLine
			for sc.Scan() {
				raw := bytes.TrimSpace(sc.Bytes())
				if len(raw) == 0 {
					continue
				}
				var ln vmLine
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
			metric := sel
			if i := strings.IndexByte(sel, '{'); i >= 0 {
				metric = sel[:i]
			}
			startT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("start"))
			endT, _ := time.Parse(time.RFC3339Nano, r.URL.Query().Get("end"))
			mu.Lock()
			cp := append([]vmLine(nil), lines...)
			mu.Unlock()
			enc := json.NewEncoder(w)
			for _, ln := range cp {
				if ln.Metric["__name__"] != metric {
					continue
				}
				var out vmLine
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
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEnableObserveWiresPersistentStoreAndPreservesLiveWindow(t *testing.T) {
	vmSrv := startTestVM(t)
	reg := registry.New()
	if err := reg.Add(registry.Machine{ID: "mon1", Role: registry.RoleMonitor, PrivateIPs: []string{"10.0.0.3"}, Provisioned: true}); err != nil {
		t.Fatal(err)
	}
	gate, err := exec.NewGate([]byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var monHandler channel.Handler
	m, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		PollInterval: 10 * time.Millisecond,
		MonitorStore: config.MonitorStoreConfig{Addr: vmSrv.URL, Retention: "90d"},
		NewLink: func(_ registry.Machine, h channel.Handler) (fleet.Link, error) {
			monHandler = h
			return &recLink{h: h}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.EnableObserve(t.TempDir()); err != nil {
		t.Fatalf("EnableObserve: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })

	eventually(t, "mon1 link", func() bool { return monHandler != nil })

	obs := m.Observe()
	if obs == nil || obs.Store == nil || obs.Series == nil {
		t.Fatalf("expected Observe.Store and Observe.Series to be wired: %+v", obs)
	}
	job, err := obs.Jobs.Create(monitor.Spec{IP: "203.0.113.5"}, []string{"mon1"})
	if err != nil {
		t.Fatal(err)
	}

	feed, unsub := obs.Series.Subscribe()
	defer unsub()

	now := time.Now().UTC()
	oldT := now.Add(-2 * time.Hour)
	recentT := now.Add(-5 * time.Second)
	env := envOf(t, channel.TypeMonData, monitor.Batch{
		Seq: 1,
		Results: []probe.Result{
			{TargetID: job.Targets[0].Target.ID, Type: probe.TypeICMP, Time: oldT, OK: true, Latency: 25 * time.Millisecond},
			{TargetID: job.Targets[0].Target.ID, Type: probe.TypeICMP, Time: recentT, OK: true, Latency: 9 * time.Millisecond},
		},
	})
	monHandler(ctx, env)

	// Live subscriber received the batch immediately.
	select {
	case pts := <-feed:
		if len(pts) != 2 {
			t.Fatalf("live feed points = %d, want 2", len(pts))
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for live feed points")
	}

	// Live 10-minute in-memory window holds only the recent point (the 2h-old point aged out).
	live := obs.Series.Snapshot(now.Add(-fleet.DefaultSeriesRetention))
	if len(live) != 1 || len(live[0].Points) != 1 || live[0].Points[0].Ms != 9 {
		t.Fatalf("live snapshot = %+v, want only the 9ms recent point", live)
	}

	// Persistent store retains BOTH the 2h-old point and the recent point across time-range queries.
	hist, err := obs.Store.QueryRange(context.Background(), monitor.HistoryQuery{
		Metric: monitor.MetricLatencyMs,
		Start:  now.Add(-3 * time.Hour),
		End:    now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(hist) != 1 || len(hist[0].Points) != 2 || hist[0].Points[0].Value != 25 || hist[0].Points[1].Value != 9 {
		t.Fatalf("history = %+v, want [25, 9]", hist)
	}
}
