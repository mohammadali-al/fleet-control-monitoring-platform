package fleet

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

// Series keeps the most recent probe results in memory for the panel's live
// charts and fans new ones out to subscribers. It is a monitor.Sink (the
// ingest path writes into it). It is not the long-term store: that is a
// time-series database behind the same Sink interface, later.
type Series struct {
	retention time.Duration
	maxPoints int
	now       func() time.Time

	mu     sync.Mutex
	byKey  map[string]*seriesBuf
	subs   map[int]*feedSub
	nextID int
}

// Retention and size bounds of a Series.
const (
	DefaultSeriesRetention = 10 * time.Minute
	maxSeriesPoints        = 4000 // per (agent, target); 10 min at the fastest 300 ms rate is 2000
)

// Point is one probe result as the panel shows it.
type Point struct {
	T      time.Time `json:"t"`
	Ms     float64   `json:"ms"` // response time in milliseconds (0 for a failed probe)
	OK     bool      `json:"ok"`
	Status int       `json:"status,omitempty"`
	Error  string    `json:"error,omitempty"`
	Loss   float64   `json:"loss,omitempty"`
}

// SeriesKey identifies one line: a probe target on one monitor agent.
type SeriesKey struct {
	Agent      string     `json:"agent"`
	Target     string     `json:"target"`
	HistoryKey string     `json:"history_key,omitempty"`
	Type       probe.Type `json:"type"`
}

// SeriesData is one line with its points, oldest first.
type SeriesData struct {
	SeriesKey
	Points []Point `json:"points"`
}

// LivePoint is one new result on the live feed.
type LivePoint struct {
	SeriesKey
	Point
}

type seriesBuf struct {
	key SeriesKey
	pts []Point
}

type feedSub struct {
	ch chan []LivePoint
}

// NewSeries returns an empty Series keeping retention of history (default
// DefaultSeriesRetention).
func NewSeries(retention time.Duration) *Series {
	if retention <= 0 {
		retention = DefaultSeriesRetention
	}
	return &Series{retention: retention, maxPoints: maxSeriesPoints, now: time.Now,
		byKey: map[string]*seriesBuf{}, subs: map[int]*feedSub{}}
}

// Write implements monitor.Sink.
func (s *Series) Write(_ context.Context, samples []monitor.Sample) error {
	if len(samples) == 0 {
		return nil
	}
	now := s.now()
	live := make([]LivePoint, 0, len(samples))
	s.mu.Lock()
	for _, sm := range samples {
		r := sm.Result
		t := r.Time
		if t.IsZero() {
			t = now
		}
		p := Point{T: t, OK: r.OK, Status: r.Status, Error: r.Error, Loss: r.Loss}
		if r.OK {
			p.Ms = float64(r.Latency) / float64(time.Millisecond)
		}
		key := SeriesKey{Agent: sm.Agent, Target: r.TargetID, HistoryKey: sm.HistoryKey, Type: r.Type}
		id := key.Agent + "\x00" + key.Target
		b := s.byKey[id]
		if b == nil {
			b = &seriesBuf{key: key}
			s.byKey[id] = b
		}
		if key.HistoryKey != "" {
			b.key.HistoryKey = key.HistoryKey
		}
		b.pts = append(b.pts, p)
		if over := len(b.pts) - s.maxPoints; over > 0 {
			b.pts = append([]Point(nil), b.pts[over:]...)
		}
		live = append(live, LivePoint{SeriesKey: b.key, Point: p})
	}
	// Age out old points of the series that were just written to. Points are
	// appended in arrival order, which is time order per target in practice
	// but is not relied on (a late batch after an outage may be older).
	cut := now.Add(-s.retention)
	for _, sm := range samples {
		b := s.byKey[sm.Agent+"\x00"+sm.Result.TargetID]
		if b == nil {
			continue
		}
		keep := b.pts[:0:0]
		for _, p := range b.pts {
			if !p.T.Before(cut) {
				keep = append(keep, p)
			}
		}
		if len(keep) != len(b.pts) {
			b.pts = keep
		}
	}
	for _, sub := range s.subs {
		select {
		case sub.ch <- live:
		default: // a slow browser misses points; the next snapshot catches up
		}
	}
	s.mu.Unlock()
	return nil
}

// Snapshot returns every series with the points newer than since, ordered by
// agent then target. Series with no points in the range are omitted.
func (s *Series) Snapshot(since time.Time) []SeriesData {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SeriesData, 0, len(s.byKey))
	for _, b := range s.byKey {
		var pts []Point
		for _, p := range b.pts {
			if !p.T.Before(since) {
				pts = append(pts, p)
			}
		}
		if len(pts) == 0 {
			continue
		}
		sort.SliceStable(pts, func(i, j int) bool { return pts[i].T.Before(pts[j].T) })
		out = append(out, SeriesData{SeriesKey: b.key, Points: pts})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Target < out[j].Target
	})
	return out
}

// Subscribe returns a channel of new results (batches as they were ingested)
// and a cancel func. A subscriber that cannot keep up misses batches.
func (s *Series) Subscribe() (<-chan []LivePoint, func()) {
	sub := &feedSub{ch: make(chan []LivePoint, 64)}
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.subs[id] = sub
	s.mu.Unlock()
	var once sync.Once
	return sub.ch, func() {
		once.Do(func() {
			s.mu.Lock()
			delete(s.subs, id)
			s.mu.Unlock()
		})
	}
}
