package agent

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

const (
	publicIPSendTimeout    = 10 * time.Second
	publicIPRefreshTimeout = 15 * time.Second
)

// publicIPSource is an optional capability of a Capturer (implemented by
// liveCapturer and test capturers) that exposes the reflector cache.
type publicIPSource interface {
	PublicIPCache() *capture.PublicIPCache
}

// publicIPReporter binds to a capture.PublicIPCache and reports resolved public
// IPs (plus manual TypePublicIPRefresh requests) over the control channel. It
// is shared by worker and monitor agents.
type publicIPReporter struct {
	sender exec.Sender
	log    *slog.Logger
	base   func() context.Context

	mu       sync.Mutex
	cache    *capture.PublicIPCache
	last     capture.PublicIPResult
	reported bool
	closed   bool
	wg       sync.WaitGroup
}

func newPublicIPReporter(sender exec.Sender, log *slog.Logger, base func() context.Context) *publicIPReporter {
	return &publicIPReporter{sender: sender, log: log, base: base}
}

func (r *publicIPReporter) bind(cache *capture.PublicIPCache) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	old := r.cache
	if old == cache {
		r.mu.Unlock()
		return
	}
	r.cache = cache
	r.mu.Unlock()

	if old != nil {
		old.SetOnRefresh(nil)
	}
	if cache == nil {
		return
	}
	cur := cache.SetOnRefresh(func(res capture.PublicIPResult) {
		r.report(res, false)
	})
	if cur.IP != "" {
		r.mu.Lock()
		closed := r.closed
		if !closed {
			r.wg.Add(1)
		}
		r.mu.Unlock()
		if !closed {
			go func() {
				defer r.wg.Done()
				r.report(cur, false)
			}()
		}
	}
}

func (r *publicIPReporter) reportOnConnect() {
	r.mu.Lock()
	cache := r.cache
	r.mu.Unlock()
	if cache == nil {
		return
	}
	res := cache.Result()
	if res.IP == "" {
		return
	}
	r.send(res)
}

func (r *publicIPReporter) handleRefresh() {
	r.mu.Lock()
	cache := r.cache
	closed := r.closed
	if cache != nil && !closed {
		r.wg.Add(1)
	}
	r.mu.Unlock()
	if cache == nil {
		r.log.Debug("public-ip-refresh ignored: reflector cache not active", "component", "fleet-agent")
		return
	}
	if closed {
		return
	}
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithTimeout(r.base(), publicIPRefreshTimeout)
		defer cancel()
		res, err := cache.RefreshNow(ctx)
		if err != nil {
			r.log.Warn("public IP refresh failed", "component", "fleet-agent", "err", err)
			return
		}
		r.report(res, true)
	}()
}

func (r *publicIPReporter) report(res capture.PublicIPResult, force bool) {
	if res.IP == "" {
		return
	}
	if !force {
		r.mu.Lock()
		dup := r.reported &&
			res.IP == r.last.IP &&
			res.Confirmed == r.last.Confirmed &&
			res.At.Sub(r.last.At) < capture.PublicIPRefreshInterval
		r.mu.Unlock()
		if dup {
			return
		}
	}
	r.send(res)
}

func (r *publicIPReporter) send(res capture.PublicIPResult) {
	p := channel.PublicIPPayload{
		IP:        res.IP,
		At:        res.At,
		Confirmed: res.Confirmed,
	}
	env, err := channel.NewEnvelope(channel.TypePublicIP, "", p)
	if err != nil {
		r.log.Warn("build public-ip envelope", "component", "fleet-agent", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(r.base(), publicIPSendTimeout)
	defer cancel()
	if err := r.sender.Send(ctx, env); err != nil {
		r.log.Debug("send public-ip", "component", "fleet-agent", "err", err)
		return
	}
	r.mu.Lock()
	r.last = res
	r.reported = true
	r.mu.Unlock()
}

func (r *publicIPReporter) unbind() {
	r.mu.Lock()
	r.closed = true
	cache := r.cache
	r.cache = nil
	r.mu.Unlock()
	if cache != nil {
		cache.SetOnRefresh(nil)
	}
	r.wg.Wait()
}

// bindPublicIP attaches the worker's public-IP reporter to c's reflector cache
// (if c exposes one) and reports any already-resolved IP. Caller holds w.mu
// and has verified !w.closed.
func (w *worker) bindPublicIP(c Capturer) {
	var cache *capture.PublicIPCache
	if c != nil {
		if src, ok := c.(publicIPSource); ok {
			cache = src.PublicIPCache()
		}
	}
	w.pip.bind(cache)
}

func (w *worker) reportPublicIPOnConnect() { w.pip.reportOnConnect() }

func (w *worker) handlePublicIPRefresh() { w.pip.handleRefresh() }

func (w *worker) unbindPublicIP() { w.pip.unbind() }
