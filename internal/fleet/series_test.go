package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

func sample(agent, target string, at time.Time, ok bool, lat time.Duration) monitor.Sample {
	return monitor.Sample{Agent: agent, HistoryKey: "1.2.3.4|", Result: probe.Result{TargetID: target, Type: probe.TypeICMP, Time: at, OK: ok, Latency: lat}}
}

func TestSeriesKeepsRetentionAndSnapshotsByWindow(t *testing.T) {
	s := NewSeries(time.Minute)
	now := time.Now()
	if err := s.Write(context.Background(), []monitor.Sample{
		sample("m1", "t1", now.Add(-2*time.Minute), true, time.Millisecond), // older than the retention
		sample("m1", "t1", now.Add(-30*time.Second), true, 10*time.Millisecond),
		sample("m1", "t1", now.Add(-5*time.Second), false, 0),
		sample("m2", "t1", now.Add(-time.Second), true, 20*time.Millisecond),
	}); err != nil {
		t.Fatal(err)
	}
	all := s.Snapshot(now.Add(-time.Hour))
	if len(all) != 2 || len(all[0].Points) != 2 || all[0].Agent != "m1" || all[1].Agent != "m2" {
		t.Fatalf("snapshot = %+v (the 2-minute-old point is beyond the retention)", all)
	}
	win := s.Snapshot(now.Add(-10 * time.Second))
	if len(win) != 2 || len(win[0].Points) != 1 || win[0].Points[0].OK {
		t.Errorf("10 s window = %+v", win)
	}
	if p := all[1].Points[0]; p.Ms != 20 || !p.OK || all[1].HistoryKey != "1.2.3.4|" || all[1].Type != probe.TypeICMP {
		t.Errorf("point = %+v key = %+v", p, all[1].SeriesKey)
	}
}

func TestSeriesToleratesOutOfOrderBatches(t *testing.T) {
	s := NewSeries(0)
	now := time.Now()
	_ = s.Write(context.Background(), []monitor.Sample{sample("m", "t", now, true, time.Millisecond)})
	_ = s.Write(context.Background(), []monitor.Sample{sample("m", "t", now.Add(-time.Hour), true, time.Millisecond), sample("m", "t", now.Add(-20*time.Second), true, 2*time.Millisecond)})
	got := s.Snapshot(now.Add(-time.Minute))
	if len(got) != 1 || len(got[0].Points) != 2 || !got[0].Points[0].T.Before(got[0].Points[1].T) {
		t.Fatalf("snapshot = %+v, want the two recent points in time order", got)
	}
}

func TestSeriesFeedAndBound(t *testing.T) {
	s := NewSeries(0)
	s.maxPoints = 3
	ch, cancel := s.Subscribe()
	now := time.Now()
	for i := 0; i < 5; i++ {
		_ = s.Write(context.Background(), []monitor.Sample{sample("m", "t", now.Add(time.Duration(i)*time.Millisecond), true, time.Duration(i+1)*time.Millisecond)})
	}
	for i := 0; i < 5; i++ {
		select {
		case b := <-ch:
			if len(b) != 1 || b[0].Agent != "m" || b[0].Target != "t" || b[0].Ms != float64(i+1) {
				t.Fatalf("batch %d = %+v", i, b)
			}
		case <-time.After(time.Second):
			t.Fatalf("no batch %d", i)
		}
	}
	if got := s.Snapshot(now.Add(-time.Minute)); len(got[0].Points) != 3 || got[0].Points[0].Ms != 3 {
		t.Errorf("bound = %+v, want the newest 3", got[0].Points)
	}
	cancel()
	cancel() // safe twice
	_ = s.Write(context.Background(), []monitor.Sample{sample("m", "t", now, true, time.Millisecond)})
	select {
	case b := <-ch:
		t.Errorf("a cancelled subscriber got %+v", b)
	default:
	}
}
