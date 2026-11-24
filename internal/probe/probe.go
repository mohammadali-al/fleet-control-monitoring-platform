// Package probe is the monitor-agent side probe engine (architecture 8.1): one
// pluggable Prober per probe type (HTTP(S), TCP connect, ICMP, DNS), driven by
// a configuration the main server pushes. Frequency and timeouts come from
// that pushed Config; the agent-local constants here are last-resort fallbacks
// that only fill a value the server left out.
//
// The engine never stops on its own: probe failures are results, not errors,
// and results are handed to a callback (internal/monitor buffers them while
// the channel is down).
package probe

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Type is a probe type, chosen per target.
type Type string

// Probe types.
const (
	TypeHTTP Type = "http"
	TypeTCP  Type = "tcp"
	TypeICMP Type = "icmp"
	TypeDNS  Type = "dns"
)

// Interval bounds and agent-local fallbacks (architecture 8.1, decision table
// rows 16 and 17). The server's pushed values win; these apply only to a value
// the server did not supply.
const (
	// MinInterval is the lowest probe period (300 ms).
	MinInterval = 300 * time.Millisecond
	// MaxInterval caps the probe period ("several seconds", not minutes).
	MaxInterval = 60 * time.Second
	// FallbackInterval is used when no interval was pushed.
	FallbackInterval = time.Second

	FallbackHTTPTimeout = 2 * time.Second
	FallbackTCPTimeout  = 1500 * time.Millisecond
	FallbackICMPTimeout = time.Second
	FallbackDNSTimeout  = 2 * time.Second
)

// FallbackTimeout is the agent-local timeout for a probe type.
func FallbackTimeout(t Type) time.Duration {
	switch t {
	case TypeHTTP:
		return FallbackHTTPTimeout
	case TypeTCP:
		return FallbackTCPTimeout
	case TypeICMP:
		return FallbackICMPTimeout
	case TypeDNS:
		return FallbackDNSTimeout
	}
	return FallbackHTTPTimeout
}

// Target is one thing to probe, as pushed by the server.
type Target struct {
	// ID is unique per agent; results carry it.
	ID   string `json:"id"`
	Type Type   `json:"type"`
	// Address is what to probe: a URL for http, host:port for tcp, a host for
	// icmp, a query name for dns.
	Address string `json:"address"`
	// IP, if set, is the address to connect to for http and tcp while the
	// request still carries the Address's host (Host header, TLS SNI).
	IP string `json:"ip,omitempty"`
	// Server is the resolver (host:port) for dns; empty means the system one.
	Server string `json:"server,omitempty"`
	// Count is the number of echo requests per ICMP probe (default 1).
	Count int `json:"count,omitempty"`
	// IntervalMS and TimeoutMS override the Config values for this target.
	IntervalMS int `json:"interval_ms,omitempty"`
	TimeoutMS  int `json:"timeout_ms,omitempty"`
}

// TLSInfo is what an HTTPS probe learns about the peer's TLS session.
type TLSInfo struct {
	Version  string    `json:"version"`
	Cipher   string    `json:"cipher,omitempty"`
	Subject  string    `json:"subject,omitempty"`
	Issuer   string    `json:"issuer,omitempty"`
	NotAfter time.Time `json:"not_after"`
	DNSNames []string  `json:"dns_names,omitempty"`
	ALPN     string    `json:"alpn,omitempty"`
}

// Result is one probe outcome. Failure is a normal result (OK false, Error
// set), never a Go error.
type Result struct {
	TargetID string        `json:"target_id"`
	Type     Type          `json:"type"`
	Time     time.Time     `json:"time"`
	OK       bool          `json:"ok"`
	Latency  time.Duration `json:"latency_ns"`
	Error    string        `json:"error,omitempty"`

	// HTTP.
	Status int      `json:"status,omitempty"`
	TLS    *TLSInfo `json:"tls,omitempty"`
	// ICMP: Sent and Received echo counts; Loss is the lost fraction 0..1.
	Sent     int     `json:"sent,omitempty"`
	Received int     `json:"received,omitempty"`
	Loss     float64 `json:"loss,omitempty"`
	// DNS: the resolved answers.
	Answer []string `json:"answer,omitempty"`
}

// Prober probes targets of one type. Implementations must be safe for
// concurrent use and must return when ctx is done or timeout elapses.
type Prober interface {
	Type() Type
	Probe(ctx context.Context, t Target, timeout time.Duration) Result
}

// Config is the server-pushed monitor configuration (the TypeMonConfig
// payload). A new Config replaces the previous one wholesale.
type Config struct {
	// IntervalMS is the global default probe period.
	IntervalMS int `json:"interval_ms,omitempty"`
	// TimeoutMS holds the pushed timeout per probe type.
	TimeoutMS map[Type]int `json:"timeout_ms,omitempty"`
	Targets   []Target     `json:"targets"`
}

// Interval is the effective probe period for t, clamped to the allowed range.
func (c Config) Interval(t Target) time.Duration {
	d := time.Duration(t.IntervalMS) * time.Millisecond
	if d <= 0 {
		d = time.Duration(c.IntervalMS) * time.Millisecond
	}
	if d <= 0 {
		d = FallbackInterval
	}
	return min(max(d, MinInterval), MaxInterval)
}

// Timeout is the effective timeout for t: the target override, else the
// pushed per-type value, else the local fallback.
func (c Config) Timeout(t Target) time.Duration {
	if t.TimeoutMS > 0 {
		return time.Duration(t.TimeoutMS) * time.Millisecond
	}
	if ms := c.TimeoutMS[t.Type]; ms > 0 {
		return time.Duration(ms) * time.Millisecond
	}
	return FallbackTimeout(t.Type)
}

// Engine runs one probe loop per configured target and reports every result
// to out. It runs until its context is cancelled and never stops otherwise.
type Engine struct {
	out func(Result)
	now func() time.Time

	mu      sync.Mutex
	probers map[Type]Prober
	cfg     Config
	ctx     context.Context // set by Run; nil before
	loops   map[string]*loop
	wg      sync.WaitGroup
}

type loop struct {
	key    string
	cancel context.CancelFunc
}

// NewEngine returns an engine with the four built-in probers registered. out
// is called from probe goroutines and must be safe for concurrent use.
func NewEngine(out func(Result)) *Engine {
	e := &Engine{
		out:     out,
		now:     time.Now,
		probers: map[Type]Prober{},
		loops:   map[string]*loop{},
	}
	for _, p := range []Prober{NewHTTP(), NewTCP(), NewICMP(), NewDNS()} {
		e.probers[p.Type()] = p
	}
	return e
}

// Register adds or replaces the prober for its type.
func (e *Engine) Register(p Prober) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.probers[p.Type()] = p
}

// Apply installs a pushed Config. Targets that are new or changed (including
// a changed effective interval or timeout) get a fresh loop; removed targets
// stop. Unchanged targets are left alone. Safe before or during Run.
func (e *Engine) Apply(cfg Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = cfg
	e.reconcileLocked()
}

// Config returns the currently applied configuration.
func (e *Engine) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Run starts the loops and blocks until ctx is cancelled, then waits for them.
func (e *Engine) Run(ctx context.Context) {
	e.mu.Lock()
	e.ctx = ctx
	e.reconcileLocked()
	e.mu.Unlock()

	<-ctx.Done()

	e.mu.Lock()
	for id, l := range e.loops {
		l.cancel()
		delete(e.loops, id)
	}
	e.ctx = nil
	e.mu.Unlock()
	e.wg.Wait()
}

func (e *Engine) reconcileLocked() {
	if e.ctx == nil {
		return
	}
	want := map[string]Target{}
	for _, t := range e.cfg.Targets {
		if t.ID != "" {
			want[t.ID] = t
		}
	}
	for id, l := range e.loops {
		if _, ok := want[id]; !ok {
			l.cancel()
			delete(e.loops, id)
		}
	}
	for id, t := range want {
		interval, timeout := e.cfg.Interval(t), e.cfg.Timeout(t)
		// Everything that, if changed, restarts the target's loop.
		key := fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s", t.Type, t.Address, t.IP, t.Server, t.Count, interval, timeout)
		if old, ok := e.loops[id]; ok {
			if old.key == key {
				continue
			}
			old.cancel()
		}
		lctx, cancel := context.WithCancel(e.ctx)
		e.loops[id] = &loop{key: key, cancel: cancel}
		e.wg.Add(1)
		go e.runLoop(lctx, t, interval, timeout)
	}
}

func (e *Engine) runLoop(ctx context.Context, t Target, interval, timeout time.Duration) {
	defer e.wg.Done()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		e.probeOnce(ctx, t, timeout)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) probeOnce(ctx context.Context, t Target, timeout time.Duration) {
	e.mu.Lock()
	p := e.probers[t.Type]
	e.mu.Unlock()
	start := e.now()
	var r Result
	if p == nil {
		r = Result{OK: false, Error: "no prober for type " + string(t.Type)}
	} else {
		pctx, cancel := context.WithTimeout(ctx, timeout)
		r = p.Probe(pctx, t, timeout)
		cancel()
	}
	if ctx.Err() != nil {
		return // cancelled mid-probe: not a real measurement
	}
	r.TargetID = t.ID
	r.Type = t.Type
	if r.Time.IsZero() {
		r.Time = start
	}
	if e.out != nil {
		e.out(r)
	}
}
