package report

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
)

func TestAdapterProjectTestAndHistorySamples(t *testing.T) {
	start := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	end := start.Add(30 * time.Second)

	proj := project.Project{
		ID:   "acme-eng",
		Name: "Acme Engagement",
	}
	test := project.Test{
		ID:           "t-1",
		ProjectID:    "acme-eng",
		Name:         "Spike Check #1",
		ManeuverID:   "mv-1",
		ManeuverName: "Spike Maneuver",
		Target:       project.TargetRef{IP: "198.51.100.10", Origin: "https://shop.example.test"},
		Agents:       []string{"w1", "w2"},
		Monitor: project.MonitorRef{
			JobID:      "mj-1",
			HistoryKey: "198.51.100.10|https://shop.example.test:443",
			IP:         "198.51.100.10",
			Origin:     "https://shop.example.test:443",
		},
		Status:        project.StatusCompleted,
		ManeuverRunID: "m-run-1",
		StartedAt:     &start,
		EndedAt:       &end,
		ResultPointers: &project.ResultPointers{
			Evidence: &project.EvidenceWindow{
				From:          start,
				To:            &end,
				Machines:      []string{"w1", "w2"},
				ManeuverRunID: "m-run-1",
			},
			Monitor: &project.MonitorWindow{
				JobID:      "mj-1",
				HistoryKey: "198.51.100.10|https://shop.example.test:443",
				From:       start,
				To:         &end,
			},
		},
	}

	mv := maneuver.Maneuver{
		ID:       "mv-1",
		Name:     "Spike Maneuver",
		TotalSec: 30,
		Pattern:  maneuver.PatternConfig{Type: maneuver.PatternAllInOne, MaxCPUCores: 4, MaxRAMGB: 8},
		Timeline: []maneuver.TimelineItem{{ID: "item-1", BaselineID: "b1", Name: "wrk", StartSec: 2, DurationSec: 20}},
	}
	mat := maneuver.MaterializedPlan{
		ManeuverID: "mv-1",
		Name:       "Spike Maneuver",
		TotalSec:   30,
		Pattern:    mv.Pattern,
		Items: []maneuver.MaterializedItem{
			{ItemID: "item-1", BaselineID: "b1", Name: "wrk", Command: "wrk -t2 https://shop.example.test/", StartSec: 2, DurationSec: 20},
		},
	}
	plan := PlanFromManeuver(mv, &mat)
	if plan.Pattern != PatternAllInOne || len(plan.Items) != 1 || plan.Items[0].Command == "" {
		t.Fatalf("unexpected plan: %+v", plan)
	}

	histSeries := []monitor.HistorySeries{
		{
			Metric:     monitor.MetricOK,
			Agent:      "mon1",
			Target:     "mj-1/http/0",
			HistoryKey: "198.51.100.10|https://shop.example.test:443",
			Type:       probe.TypeHTTP,
			Points: []monitor.HistoryPoint{
				{T: start.Add(5 * time.Second), Value: 1},
				{T: start.Add(10 * time.Second), Value: 0},
			},
		},
		{
			Metric:     monitor.MetricLatencyMs,
			Agent:      "mon1",
			Target:     "mj-1/http/0",
			HistoryKey: "198.51.100.10|https://shop.example.test:443",
			Type:       probe.TypeHTTP,
			Points: []monitor.HistoryPoint{
				{T: start.Add(5 * time.Second), Value: 42.5},
				{T: start.Add(10 * time.Second), Value: 0},
			},
		},
	}

	targetKey := monitor.HistoryKey(test.Target.IP, test.Target.Origin)
	samples := SamplesFromHistory(histSeries, targetKey)
	if len(samples) != 2 {
		t.Fatalf("samples = %d, want 2", len(samples))
	}
	if !samples[0].Result.OK || samples[0].Result.Latency != 42500*time.Microsecond {
		t.Fatalf("sample[0] = %+v", samples[0])
	}
	if samples[1].Result.OK || samples[1].Result.Latency != 0 {
		t.Fatalf("sample[1] = %+v", samples[1])
	}

	spec := SpecFromProjectTest(proj, test, ProjectTestOptions{
		Plan:  plan,
		Paths: []string{"/"},
		Now:   func() time.Time { return end.Add(time.Minute) },
	})
	d, err := Gather(spec, Sources{Samples: samples})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if !d.Monitor.Available || d.Monitor.Probes != 2 || d.Monitor.OK != 1 || d.Monitor.Availability != 50 {
		t.Fatalf("monitor = %+v, want 2 probes / 1 ok / 50%%", d.Monitor)
	}
	var buf bytes.Buffer
	if err := Render(&buf, d); err != nil {
		t.Fatalf("Render: %v", err)
	}
	html := buf.String()
	for _, want := range []string{"Acme Engagement", "Spike Check #1", "198.51.100.10", "50.00 %", "wrk -t2"} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML missing %q", want)
		}
	}
}
