package monitor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

// Metric names written to and queried from the persistent time-series store.
const (
	MetricLatencyMs = "fleet_probe_latency_ms"
	MetricOK        = "fleet_probe_ok"
	MetricStatus    = "fleet_probe_status_code"
	MetricLoss      = "fleet_probe_loss_ratio"
)

const (
	defaultWriteTimeout = 5 * time.Second
	defaultQueryTimeout = 15 * time.Second
	maxExportLineBytes  = 16 << 20
)

// ErrBadQuery is returned when a history query has invalid parameters.
var ErrBadQuery = errors.New("monitor: invalid history query")

// NormalizeMetric maps short aliases ("latency_ms", "ok", "status", "loss") or
// full metric names to a validated Prometheus metric name. Empty selects
// MetricLatencyMs.
func NormalizeMetric(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", "latency_ms", MetricLatencyMs:
		return MetricLatencyMs, nil
	case "ok", MetricOK:
		return MetricOK, nil
	case "status", "status_code", MetricStatus:
		return MetricStatus, nil
	case "loss", "loss_ratio", MetricLoss:
		return MetricLoss, nil
	}
	name := strings.TrimSpace(raw)
	if !isValidMetricName(name) {
		return "", fmt.Errorf("%w: invalid metric %q", ErrBadQuery, raw)
	}
	return name, nil
}

func isValidMetricName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' || c == ':'
		digit := c >= '0' && c <= '9'
		if i == 0 && !letter {
			return false
		}
		if i > 0 && !letter && !digit {
			return false
		}
	}
	return true
}

// HistoryQuery selects persisted time-series samples over [Start, End].
type HistoryQuery struct {
	Metric     string
	Agent      string
	Target     string
	HistoryKey string
	Type       probe.Type
	Start      time.Time
	End        time.Time
	// Step selects downsampled Prometheus range evaluation when > 0; zero
	// returns raw persisted points via /api/v1/export.
	Step time.Duration
}

// HistoryPoint is one persisted sample point returned by a history query.
type HistoryPoint struct {
	T     time.Time `json:"t"`
	Value float64   `json:"value"`
}

// HistorySeries is one labeled time series returned by a history query.
type HistorySeries struct {
	Metric     string         `json:"metric"`
	Agent      string         `json:"agent"`
	Target     string         `json:"target"`
	HistoryKey string         `json:"history_key,omitempty"`
	Type       probe.Type     `json:"type,omitempty"`
	Points     []HistoryPoint `json:"points"`
}

// HistoryReader reads persisted monitor samples by metric and time range.
type HistoryReader interface {
	QueryRange(ctx context.Context, q HistoryQuery) ([]HistorySeries, error)
}

// HistoryStore is a persistent time-series store that supports both ingest
// writes and time-range history queries.
type HistoryStore interface {
	Sink
	HistoryReader
}

// MultiSink fans every Write out to multiple Sinks. All non-nil sinks are
// attempted even if one fails; any errors are joined.
type MultiSink struct {
	sinks []Sink
}

// NewMultiSink builds a Sink that writes to each non-nil sink in order.
func NewMultiSink(sinks ...Sink) *MultiSink {
	out := make([]Sink, 0, len(sinks))
	for _, s := range sinks {
		if s != nil {
			out = append(out, s)
		}
	}
	return &MultiSink{sinks: out}
}

// Write implements Sink.
func (m *MultiSink) Write(ctx context.Context, samples []Sample) error {
	var errs []error
	for _, s := range m.sinks {
		if err := s.Write(ctx, samples); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// VMStoreConfig configures a VictoriaMetrics-backed HistoryStore.
type VMStoreConfig struct {
	Addr         string
	Retention    string
	HTTPClient   *http.Client
	WriteTimeout time.Duration
	QueryTimeout time.Duration
}

// VMStore persists monitor samples to VictoriaMetrics over HTTP (/api/v1/import)
// and reads historical ranges back via /api/v1/export and /api/v1/query_range.
type VMStore struct {
	baseURL      string
	retention    string
	client       *http.Client
	writeTimeout time.Duration
	queryTimeout time.Duration
	now          func() time.Time
}

// NewVMStore validates cfg and returns a VictoriaMetrics store client.
func NewVMStore(cfg VMStoreConfig) (*VMStore, error) {
	raw := strings.TrimSpace(cfg.Addr)
	if raw == "" {
		return nil, errors.New("monitor: store address is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("monitor: invalid store address %q (want http(s)://host:port)", cfg.Addr)
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	wt := cfg.WriteTimeout
	if wt <= 0 {
		wt = defaultWriteTimeout
	}
	qt := cfg.QueryTimeout
	if qt <= 0 {
		qt = defaultQueryTimeout
	}
	return &VMStore{
		baseURL:      strings.TrimRight(u.String(), "/"),
		retention:    cfg.Retention,
		client:       client,
		writeTimeout: wt,
		queryTimeout: qt,
		now:          time.Now,
	}, nil
}

// Addr returns the configured base HTTP URL.
func (s *VMStore) Addr() string { return s.baseURL }

// Retention returns the configured retention label (informational).
func (s *VMStore) Retention() string { return s.retention }

type vmImportLine struct {
	Metric     map[string]string `json:"metric"`
	Values     []float64         `json:"values"`
	Timestamps []int64           `json:"timestamps"`
}

type vmSeriesKey struct {
	metric     string
	agent      string
	target     string
	historyKey string
	probeType  probe.Type
}

// Write implements Sink by encoding samples into VictoriaMetrics JSON lines
// and posting them to /api/v1/import.
func (s *VMStore) Write(ctx context.Context, samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	now := s.now().UTC()
	grouped := make(map[vmSeriesKey]*vmImportLine)
	var order []vmSeriesKey

	addPoint := func(k vmSeriesKey, val float64, tsMs int64) {
		line := grouped[k]
		if line == nil {
			labels := map[string]string{
				"__name__": k.metric,
				"agent":    k.agent,
				"target":   k.target,
				"type":     string(k.probeType),
			}
			if k.historyKey != "" {
				labels["history_key"] = k.historyKey
			}
			line = &vmImportLine{Metric: labels}
			grouped[k] = line
			order = append(order, k)
		}
		line.Values = append(line.Values, val)
		line.Timestamps = append(line.Timestamps, tsMs)
	}

	for _, sm := range samples {
		r := sm.Result
		t := r.Time
		if t.IsZero() {
			t = now
		}
		tsMs := t.UTC().UnixMilli()
		base := vmSeriesKey{
			agent:      sm.Agent,
			target:     r.TargetID,
			historyKey: sm.HistoryKey,
			probeType:  r.Type,
		}

		ms := 0.0
		if r.OK {
			ms = float64(r.Latency) / float64(time.Millisecond)
		}
		kLat := base
		kLat.metric = MetricLatencyMs
		addPoint(kLat, ms, tsMs)

		okVal := 0.0
		if r.OK {
			okVal = 1.0
		}
		kOK := base
		kOK.metric = MetricOK
		addPoint(kOK, okVal, tsMs)

		if r.Status > 0 {
			kSt := base
			kSt.metric = MetricStatus
			addPoint(kSt, float64(r.Status), tsMs)
		}
		if r.Type == probe.TypeICMP {
			kLoss := base
			kLoss.metric = MetricLoss
			addPoint(kLoss, r.Loss, tsMs)
		}
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, k := range order {
		if err := enc.Encode(grouped[k]); err != nil {
			return fmt.Errorf("encode metrics: %w", err)
		}
	}

	wctx, cancel := withBoundedTimeout(ctx, s.writeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(wctx, http.MethodPost, s.baseURL+"/api/v1/import", &buf)
	if err != nil {
		return fmt.Errorf("build import request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-ndjson")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("victoriametrics import: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("victoriametrics import status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// QueryRange implements HistoryReader. When q.Step <= 0 it queries raw stored
// points via /api/v1/export; when q.Step > 0 it queries downsampled matrix
// series via /api/v1/query_range.
func (s *VMStore) QueryRange(ctx context.Context, q HistoryQuery) ([]HistorySeries, error) {
	metric, err := NormalizeMetric(q.Metric)
	if err != nil {
		return nil, err
	}
	if q.Type != "" {
		switch q.Type {
		case probe.TypeHTTP, probe.TypeTCP, probe.TypeICMP, probe.TypeDNS:
		default:
			return nil, fmt.Errorf("%w: unknown probe type %q", ErrBadQuery, q.Type)
		}
	}
	if q.Step < 0 {
		return nil, fmt.Errorf("%w: step must be non-negative", ErrBadQuery)
	}
	end := q.End
	if end.IsZero() {
		end = s.now().UTC()
	}
	start := q.Start
	if start.IsZero() {
		start = end.Add(-time.Hour)
	}
	if end.Before(start) {
		return nil, fmt.Errorf("%w: end (%s) is before start (%s)", ErrBadQuery, end.Format(time.RFC3339), start.Format(time.RFC3339))
	}

	sel := buildSelector(metric, q)
	qctx, cancel := withBoundedTimeout(ctx, s.queryTimeout)
	defer cancel()

	if q.Step > 0 {
		return s.queryMatrix(qctx, metric, sel, start, end, q.Step)
	}
	return s.queryExport(qctx, metric, sel, start, end)
}

func (s *VMStore) queryExport(ctx context.Context, metric, sel string, start, end time.Time) ([]HistorySeries, error) {
	params := url.Values{}
	params.Set("match[]", sel)
	params.Set("start", start.UTC().Format(time.RFC3339Nano))
	params.Set("end", end.UTC().Format(time.RFC3339Nano))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/api/v1/export?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build export request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("victoriametrics export: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("victoriametrics export status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	startMs := start.UTC().UnixMilli()
	endMs := end.UTC().UnixMilli()
	byKey := make(map[vmSeriesKey]*HistorySeries)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), maxExportLineBytes)
	for sc.Scan() {
		raw := bytes.TrimSpace(sc.Bytes())
		if len(raw) == 0 {
			continue
		}
		var line vmImportLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return nil, fmt.Errorf("decode export line: %w", err)
		}
		n := min(len(line.Values), len(line.Timestamps))
		if n == 0 {
			continue
		}
		mName := line.Metric["__name__"]
		if mName == "" {
			mName = metric
		}
		k := vmSeriesKey{
			metric:     mName,
			agent:      line.Metric["agent"],
			target:     line.Metric["target"],
			historyKey: line.Metric["history_key"],
			probeType:  probe.Type(line.Metric["type"]),
		}
		hs := byKey[k]
		for i := 0; i < n; i++ {
			ts := line.Timestamps[i]
			if ts < startMs || ts > endMs {
				continue
			}
			if hs == nil {
				hs = &HistorySeries{
					Metric:     k.metric,
					Agent:      k.agent,
					Target:     k.target,
					HistoryKey: k.historyKey,
					Type:       k.probeType,
				}
				byKey[k] = hs
			}
			hs.Points = append(hs.Points, HistoryPoint{
				T:     time.UnixMilli(ts).UTC(),
				Value: line.Values[i],
			})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read export response: %w", err)
	}

	return sortHistorySeries(byKey), nil
}

func (s *VMStore) queryMatrix(ctx context.Context, metric, sel string, start, end time.Time, step time.Duration) ([]HistorySeries, error) {
	params := url.Values{}
	params.Set("query", sel)
	params.Set("start", start.UTC().Format(time.RFC3339Nano))
	params.Set("end", end.UTC().Format(time.RFC3339Nano))
	params.Set("step", formatStepSeconds(step))
	params.Set("nocache", "1")
	params.Set("latency_offset", "0s")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/api/v1/query_range?"+params.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("build query_range request: %w", err)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("victoriametrics query_range: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("victoriametrics query_range status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}

	var qr struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string `json:"metric"`
				Values [][]any           `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return nil, fmt.Errorf("decode query_range response: %w", err)
	}
	if qr.Status != "success" {
		return nil, fmt.Errorf("victoriametrics query_range error: %s", qr.Error)
	}

	byKey := make(map[vmSeriesKey]*HistorySeries)
	for _, item := range qr.Data.Result {
		mName := item.Metric["__name__"]
		if mName == "" {
			mName = metric
		}
		k := vmSeriesKey{
			metric:     mName,
			agent:      item.Metric["agent"],
			target:     item.Metric["target"],
			historyKey: item.Metric["history_key"],
			probeType:  probe.Type(item.Metric["type"]),
		}
		pts := make([]HistoryPoint, 0, len(item.Values))
		for _, pair := range item.Values {
			if len(pair) != 2 {
				continue
			}
			tsFloat, ok := pair[0].(float64)
			if !ok {
				continue
			}
			valStr, ok := pair[1].(string)
			if !ok {
				continue
			}
			val, err := strconv.ParseFloat(valStr, 64)
			if err != nil {
				continue
			}
			pts = append(pts, HistoryPoint{
				T:     time.UnixMilli(int64(tsFloat * 1000)).UTC(),
				Value: val,
			})
		}
		if len(pts) == 0 {
			continue
		}
		hs := byKey[k]
		if hs == nil {
			hs = &HistorySeries{
				Metric:     k.metric,
				Agent:      k.agent,
				Target:     k.target,
				HistoryKey: k.historyKey,
				Type:       k.probeType,
			}
			byKey[k] = hs
		}
		hs.Points = append(hs.Points, pts...)
	}
	return sortHistorySeries(byKey), nil
}

func sortHistorySeries(byKey map[vmSeriesKey]*HistorySeries) []HistorySeries {
	out := make([]HistorySeries, 0, len(byKey))
	for _, hs := range byKey {
		sort.SliceStable(hs.Points, func(i, j int) bool {
			return hs.Points[i].T.Before(hs.Points[j].T)
		})
		out = append(out, *hs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Metric != out[j].Metric {
			return out[i].Metric < out[j].Metric
		}
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].HistoryKey < out[j].HistoryKey
	})
	return out
}

func buildSelector(metric string, q HistoryQuery) string {
	var matchers []string
	if q.Agent != "" {
		matchers = append(matchers, `agent="`+escapePromLabel(q.Agent)+`"`)
	}
	if q.Target != "" {
		matchers = append(matchers, `target="`+escapePromLabel(q.Target)+`"`)
	}
	if q.HistoryKey != "" {
		matchers = append(matchers, `history_key="`+escapePromLabel(q.HistoryKey)+`"`)
	}
	if q.Type != "" {
		matchers = append(matchers, `type="`+escapePromLabel(string(q.Type))+`"`)
	}
	if len(matchers) == 0 {
		return metric
	}
	return metric + "{" + strings.Join(matchers, ",") + "}"
}

func escapePromLabel(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

func formatStepSeconds(d time.Duration) string {
	sec := d.Seconds()
	if sec < 1 {
		sec = 1
	}
	return strconv.FormatFloat(sec, 'f', -1, 64) + "s"
}

func withBoundedTimeout(ctx context.Context, maxWait time.Duration) (context.Context, context.CancelFunc) {
	if maxWait <= 0 {
		return ctx, func() {}
	}
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= maxWait {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, maxWait)
}
