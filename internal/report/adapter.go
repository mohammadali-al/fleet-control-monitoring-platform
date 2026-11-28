package report

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
)

// ProjectTestOptions supplies optional control-plane details alongside a
// project.Test when building a report Spec.
type ProjectTestOptions struct {
	Plan      *Plan
	Paths     []string
	Workers   []Worker
	Usage     []UsageSample
	Commands  []Command
	Alignment *Alignment
	Watchdog  []WatchdogEvent
	Now       func() time.Time
}

// SpecFromProjectTest adapts a finished project.Test and its parent Project
// into a report.Spec using the test's ResultPointers and fallback timestamps.
func SpecFromProjectTest(proj project.Project, test project.Test, opts ProjectTestOptions) Spec {
	projName := strings.TrimSpace(proj.Name)
	if projName == "" {
		projName = proj.ID
	}

	targetIP := strings.TrimSpace(test.Target.IP)
	if targetIP == "" {
		targetIP = strings.TrimSpace(test.Monitor.IP)
	}
	targetOrigin := strings.TrimSpace(test.Target.Origin)
	if targetOrigin == "" {
		targetOrigin = strings.TrimSpace(test.Monitor.Origin)
	}

	start, end := projectTestWindow(test)

	workerByID := map[string]Worker{}
	var workerOrder []string
	addWorker := func(w Worker) {
		id := strings.TrimSpace(w.Machine)
		if id == "" {
			return
		}
		if prev, exists := workerByID[id]; exists {
			if prev.Label == "" && w.Label != "" {
				prev.Label = w.Label
				workerByID[id] = prev
			}
			return
		}
		w.Machine = id
		workerByID[id] = w
		workerOrder = append(workerOrder, id)
	}
	for _, w := range opts.Workers {
		addWorker(w)
	}
	if test.ResultPointers != nil && test.ResultPointers.Evidence != nil {
		for _, m := range test.ResultPointers.Evidence.Machines {
			addWorker(Worker{Machine: m})
		}
	}
	for _, m := range test.Agents {
		addWorker(Worker{Machine: m})
	}
	for _, c := range opts.Commands {
		addWorker(Worker{Machine: c.Machine})
	}
	workers := make([]Worker, 0, len(workerOrder))
	for _, id := range workerOrder {
		workers = append(workers, workerByID[id])
	}

	var paths []string
	if len(opts.Paths) > 0 {
		paths = append([]string(nil), opts.Paths...)
	}

	return Spec{
		Project:   projName,
		TestID:    test.ID,
		TestName:  test.Name,
		Target:    Target{IP: targetIP, Origin: targetOrigin, Paths: paths},
		Start:     start,
		End:       end,
		Plan:      opts.Plan,
		Workers:   workers,
		Usage:     opts.Usage,
		Commands:  opts.Commands,
		Alignment: opts.Alignment,
		Watchdog:  opts.Watchdog,
		Now:       opts.Now,
	}
}

func projectTestWindow(test project.Test) (start, end time.Time) {
	if test.StartedAt != nil && !test.StartedAt.IsZero() {
		start = test.StartedAt.UTC()
	} else if test.ResultPointers != nil && test.ResultPointers.Evidence != nil && !test.ResultPointers.Evidence.From.IsZero() {
		start = test.ResultPointers.Evidence.From.UTC()
	} else if test.ResultPointers != nil && test.ResultPointers.Monitor != nil && !test.ResultPointers.Monitor.From.IsZero() {
		start = test.ResultPointers.Monitor.From.UTC()
	} else if !test.CreatedAt.IsZero() {
		start = test.CreatedAt.UTC()
	}

	if test.EndedAt != nil && !test.EndedAt.IsZero() {
		end = test.EndedAt.UTC()
	} else if test.ResultPointers != nil && test.ResultPointers.Evidence != nil && test.ResultPointers.Evidence.To != nil && !test.ResultPointers.Evidence.To.IsZero() {
		end = test.ResultPointers.Evidence.To.UTC()
	} else if test.ResultPointers != nil && test.ResultPointers.Monitor != nil && test.ResultPointers.Monitor.To != nil && !test.ResultPointers.Monitor.To.IsZero() {
		end = test.ResultPointers.Monitor.To.UTC()
	} else if !test.UpdatedAt.IsZero() {
		end = test.UpdatedAt.UTC()
	}

	if !start.IsZero() {
		start = start.Truncate(time.Millisecond)
	}
	if !end.IsZero() && end.Nanosecond()%int(time.Millisecond) != 0 {
		end = end.Truncate(time.Millisecond).Add(time.Millisecond)
	}
	if !start.IsZero() && (end.IsZero() || end.Before(start)) {
		end = start
	}
	return start, end
}

// PlanFromManeuver adapts a maneuver.Maneuver (and optional materialized plan)
// into a report.Plan.
func PlanFromManeuver(mv maneuver.Maneuver, mat *maneuver.MaterializedPlan) *Plan {
	pattern := mv.Pattern.Type
	switch pattern {
	case "", maneuver.PatternAllInOne:
		pattern = PatternAllInOne
	case maneuver.PatternLinear:
		pattern = PatternLinear
	case maneuver.PatternSpikes:
		pattern = PatternSpikes
	}

	p := &Plan{
		Name:         mv.Name,
		Pattern:      pattern,
		TotalTime:    time.Duration(mv.TotalSec) * time.Second,
		WindowLength: time.Duration(mv.Pattern.WindowLengthSec) * time.Second,
		PatternLen:   time.Duration(mv.Pattern.PatternLengthSec) * time.Second,
		SpikeLen:     time.Duration(mv.Pattern.SpikeDurationSec) * time.Second,
		MaxCores:     mv.Pattern.MaxCPUCores,
		MaxRAMGB:     mv.Pattern.MaxRAMGB,
	}

	if mat != nil {
		if p.Name == "" {
			p.Name = mat.Name
		}
		p.Items = make([]PlanItem, 0, len(mat.Items))
		for _, it := range mat.Items {
			name := it.Name
			if name == "" {
				name = it.BaselineID
			}
			p.Items = append(p.Items, PlanItem{
				Name:     name,
				Command:  it.Command,
				Offset:   time.Duration(it.StartSec) * time.Second,
				Duration: time.Duration(it.DurationSec) * time.Second,
			})
		}
		return p
	}

	p.Items = make([]PlanItem, 0, len(mv.Timeline))
	for _, it := range mv.Timeline {
		name := it.Name
		if name == "" {
			name = it.BaselineID
		}
		p.Items = append(p.Items, PlanItem{
			Name:     name,
			Offset:   time.Duration(it.StartSec) * time.Second,
			Duration: time.Duration(it.DurationSec) * time.Second,
		})
	}
	return p
}

// SamplesFromHistory reconstructs []monitor.Sample from HistoryStore query
// series (MetricOK and MetricLatencyMs) so report.Gather can compute
// availability, response-time percentiles, and series charts. When historyKey
// is non-empty, it overrides each series' HistoryKey so it matches the report's
// normalized target key.
func SamplesFromHistory(series []monitor.HistorySeries, historyKey string) []monitor.Sample {
	if len(series) == 0 {
		return nil
	}
	type ptKey struct {
		agent      string
		target     string
		probeType  probe.Type
		historyKey string
		tsMs       int64
	}
	type ptAcc struct {
		hasOK  bool
		ok     bool
		hasLat bool
		latMS  float64
	}

	acc := map[ptKey]*ptAcc{}
	var order []ptKey

	for _, s := range series {
		if s.Metric != monitor.MetricOK && s.Metric != monitor.MetricLatencyMs {
			continue
		}
		hk := historyKey
		if hk == "" {
			hk = s.HistoryKey
		}
		for _, p := range s.Points {
			k := ptKey{
				agent:      s.Agent,
				target:     s.Target,
				probeType:  s.Type,
				historyKey: hk,
				tsMs:       p.T.UTC().UnixMilli(),
			}
			a := acc[k]
			if a == nil {
				a = &ptAcc{}
				acc[k] = a
				order = append(order, k)
			}
			switch s.Metric {
			case monitor.MetricOK:
				a.hasOK = true
				a.ok = p.Value >= 0.5
			case monitor.MetricLatencyMs:
				a.hasLat = true
				a.latMS = p.Value
			}
		}
	}

	out := make([]monitor.Sample, 0, len(order))
	for _, k := range order {
		a := acc[k]
		ok := a.ok
		if !a.hasOK {
			ok = a.hasLat && a.latMS > 0
		}
		var lat time.Duration
		if ok && a.hasLat && a.latMS > 0 {
			lat = time.Duration(math.Round(a.latMS * float64(time.Millisecond)))
		}
		out = append(out, monitor.Sample{
			Agent:      k.agent,
			HistoryKey: k.historyKey,
			Result: probe.Result{
				TargetID: k.target,
				Type:     k.probeType,
				Time:     time.UnixMilli(k.tsMs).UTC(),
				OK:       ok,
				Latency:  lat,
			},
		})
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].Result.Time.Equal(out[j].Result.Time) {
			return out[i].Result.Time.Before(out[j].Result.Time)
		}
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		if out[i].Result.TargetID != out[j].Result.TargetID {
			return out[i].Result.TargetID < out[j].Result.TargetID
		}
		return out[i].Result.Type < out[j].Result.Type
	})
	return out
}
