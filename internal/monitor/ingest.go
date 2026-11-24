// Package monitor is the server side of monitor data (architecture chapter 8):
// the stateful monitor jobs, and the flow that carries probe results from a
// monitor agent over the control channel into a time-series sink.
//
// Monitor data is a separate path from worker evidence logs: its own
// envelope types (mon-config, mon-data), its own storage interface, its own
// package.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

// Flush timing (architecture 8.4: every 2-3 s).
const (
	MinFlushInterval     = 2 * time.Second
	MaxFlushInterval     = 3 * time.Second
	DefaultFlushInterval = 2500 * time.Millisecond

	// DefaultMaxBuffered bounds the agent's local backlog during an outage
	// (oldest results are dropped first, and counted). At the fastest probe
	// rate (300 ms) with 3 targets this is hours of outage.
	DefaultMaxBuffered = 100_000
	// DefaultMaxBatch bounds one mon-data envelope (well under the channel's
	// per-message cap).
	DefaultMaxBatch = 500
)

// Batch is the payload of a mon-data envelope: results in probe order. The
// agent identity is the channel's verified peer, never a field in the batch.
type Batch struct {
	// Seq numbers batches from this Flusher, starting at 1.
	Seq     uint64         `json:"seq"`
	Results []probe.Result `json:"results"`
}

// SendFunc delivers one envelope to the main server. It returns an error
// (typically channel.ErrNotConnected) while the channel is down.
type SendFunc func(ctx context.Context, env channel.Envelope) error

// FlusherOptions tunes a Flusher; zero values select the defaults.
type FlusherOptions struct {
	Interval    time.Duration // flush period, clamped to 2-3 s
	MaxBuffered int
	MaxBatch    int
}

// Flusher is the monitor-agent half of the data flow. It collects probe
// results (Add is the probe engine's out callback) and sends them to the
// server every 2-3 s. While the channel is down results stay buffered and go
// out, oldest first, on the first flush after it returns. It never gives up:
// only the buffer cap (oldest dropped, counted) bounds memory.
type Flusher struct {
	send     SendFunc
	interval time.Duration
	maxBuf   int
	maxBatch int

	flushMu sync.Mutex // serialises flushes so batches stay ordered

	mu      sync.Mutex
	buf     []probe.Result
	seq     uint64
	dropped uint64
}

// NewFlusher returns a Flusher sending through send.
func NewFlusher(send SendFunc, opt FlusherOptions) *Flusher {
	iv := opt.Interval
	if iv <= 0 {
		iv = DefaultFlushInterval
	}
	f := &Flusher{
		send:     send,
		interval: min(max(iv, MinFlushInterval), MaxFlushInterval),
		maxBuf:   opt.MaxBuffered,
		maxBatch: opt.MaxBatch,
	}
	if f.maxBuf <= 0 {
		f.maxBuf = DefaultMaxBuffered
	}
	if f.maxBatch <= 0 {
		f.maxBatch = DefaultMaxBatch
	}
	return f
}

// Interval is the effective flush period.
func (f *Flusher) Interval() time.Duration { return f.interval }

// Add buffers one result. It never blocks on the network.
func (f *Flusher) Add(r probe.Result) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf = append(f.buf, r)
	f.trimLocked()
}

func (f *Flusher) trimLocked() {
	if over := len(f.buf) - f.maxBuf; over > 0 {
		f.buf = append([]probe.Result(nil), f.buf[over:]...)
		f.dropped += uint64(over)
	}
}

// Pending is the number of buffered, unsent results.
func (f *Flusher) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.buf)
}

// Dropped is the number of results discarded because the buffer was full.
func (f *Flusher) Dropped() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped
}

// Flush sends the whole backlog in batches. On the first send error the
// unsent results are put back (ahead of any newer ones) and the error is
// returned; nothing is lost and the next Flush retries.
func (f *Flusher) Flush(ctx context.Context) error {
	f.flushMu.Lock()
	defer f.flushMu.Unlock()
	for {
		f.mu.Lock()
		n := min(len(f.buf), f.maxBatch)
		if n == 0 {
			f.mu.Unlock()
			return nil
		}
		batch := append([]probe.Result(nil), f.buf[:n]...)
		f.buf = f.buf[n:]
		f.seq++
		seq := f.seq
		f.mu.Unlock()

		env, err := channel.NewEnvelope(channel.TypeMonData, fmt.Sprintf("mon-%d", seq), Batch{Seq: seq, Results: batch})
		if err == nil {
			err = f.send(ctx, env)
		}
		if err != nil {
			f.mu.Lock()
			f.buf = append(batch, f.buf...)
			f.trimLocked()
			f.mu.Unlock()
			return err
		}
	}
}

// Run flushes every Interval until ctx is done, then makes one last
// best-effort flush. Send failures are expected during an outage and are not
// fatal: the loop just keeps going.
func (f *Flusher) Run(ctx context.Context) {
	t := time.NewTicker(f.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = f.Flush(fctx)
			cancel()
			return
		case <-t.C:
			if err := f.Flush(ctx); err != nil && ctx.Err() == nil {
				slog.Debug("monitor flush deferred", "pending", f.Pending(), "err", err)
			}
		}
	}
}

// ConfigEnvelope wraps a probe.Config as a mon-config envelope.
func ConfigEnvelope(cfg probe.Config) (channel.Envelope, error) {
	return channel.NewEnvelope(channel.TypeMonConfig, "mon-config", cfg)
}

// DecodeConfig reads the probe.Config out of a mon-config envelope (the agent
// calls this from its channel handler and passes the result to Engine.Apply).
func DecodeConfig(env channel.Envelope) (probe.Config, error) {
	var cfg probe.Config
	if env.Type != channel.TypeMonConfig {
		return cfg, fmt.Errorf("monitor: envelope type %q is not %q", env.Type, channel.TypeMonConfig)
	}
	if err := json.Unmarshal(env.Payload, &cfg); err != nil {
		return cfg, fmt.Errorf("monitor: bad mon-config payload: %w", err)
	}
	return cfg, nil
}

// Sample is one stored point: a probe result with the identity the server
// attaches (never trusted from the agent).
type Sample struct {
	// Agent is the monitor agent's verified machine id.
	Agent string
	// HistoryKey is the unique (ip, origin) history this target belongs to;
	// empty if the target maps to no known job.
	HistoryKey string
	Result     probe.Result
}

// Sink is the time-series store behind the ingest path. The real store
// (Prometheus / VictoriaMetrics) implements this later; MemorySink serves
// tests. Write must be safe for concurrent use.
type Sink interface {
	Write(ctx context.Context, samples []Sample) error
}

// MemorySink is a simple in-memory Sink.
type MemorySink struct {
	mu      sync.Mutex
	samples []Sample
}

// Write implements Sink.
func (m *MemorySink) Write(_ context.Context, s []Sample) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.samples = append(m.samples, s...)
	return nil
}

// Samples returns a copy of everything written so far.
func (m *MemorySink) Samples() []Sample {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Sample(nil), m.samples...)
}

// HistoryResolver maps a target id to its (ip, origin) history key. Jobs
// implements it via HistoryKeyFor.
type HistoryResolver func(targetID string) (key string, ok bool)

// Ingestor is the server half of the data flow: it decodes mon-data
// envelopes and writes them to the Sink.
type Ingestor struct {
	sink    Sink
	resolve HistoryResolver
}

// NewIngestor returns an Ingestor. resolve may be nil.
func NewIngestor(sink Sink, resolve HistoryResolver) *Ingestor {
	return &Ingestor{sink: sink, resolve: resolve}
}

// Handle ingests one envelope from the monitor agent agentID (the verified
// peer of the connection it arrived on). Envelopes of other types are
// ignored (nil), so Handle can sit in a multi-type channel handler.
func (i *Ingestor) Handle(ctx context.Context, agentID string, env channel.Envelope) error {
	if env.Type != channel.TypeMonData {
		return nil
	}
	if agentID == "" {
		return errors.New("monitor: mon-data from an unidentified agent")
	}
	var b Batch
	if err := json.Unmarshal(env.Payload, &b); err != nil {
		return fmt.Errorf("monitor: bad mon-data payload: %w", err)
	}
	samples := make([]Sample, 0, len(b.Results))
	for _, r := range b.Results {
		s := Sample{Agent: agentID, Result: r}
		if i.resolve != nil {
			if key, ok := i.resolve(r.TargetID); ok {
				s.HistoryKey = key
			}
		}
		samples = append(samples, s)
	}
	if len(samples) == 0 {
		return nil
	}
	if err := i.sink.Write(ctx, samples); err != nil {
		return fmt.Errorf("monitor: write samples: %w", err)
	}
	return nil
}
