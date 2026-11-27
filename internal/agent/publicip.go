package agent

import (
	"context"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
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

	w.pipMu.Lock()
	old := w.pipCache
	if old == cache {
		w.pipMu.Unlock()
		return
	}
	w.pipCache = cache
	w.pipMu.Unlock()

	if old != nil {
		old.SetOnRefresh(nil)
	}
	if cache == nil {
		return
	}
	cur := cache.SetOnRefresh(func(res capture.PublicIPResult) {
		w.reportPublicIP(res, false)
	})
	if cur.IP != "" {
		w.wg.Add(1)
		go func() {
			defer w.wg.Done()
			w.reportPublicIP(cur, false)
		}()
	}
}

// reportPublicIPOnConnect sends the currently cached public IP (if resolved)
// as soon as a control-channel connection is established.
func (w *worker) reportPublicIPOnConnect() {
	w.pipMu.Lock()
	cache := w.pipCache
	w.pipMu.Unlock()
	if cache == nil {
		return
	}
	res := cache.Result()
	if res.IP == "" {
		return
	}
	w.sendPublicIP(res)
}

// handlePublicIPRefresh handles an inbound TypePublicIPRefresh by running a
// synchronous reflector refresh and immediately reporting the fresh result.
func (w *worker) handlePublicIPRefresh() {
	w.pipMu.Lock()
	cache := w.pipCache
	w.pipMu.Unlock()
	if cache == nil {
		w.log.Debug("public-ip-refresh ignored: reflector cache not active", "component", "fleet-agent")
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		ctx, cancel := context.WithTimeout(w.base, publicIPRefreshTimeout)
		defer cancel()
		res, err := cache.RefreshNow(ctx)
		if err != nil {
			w.log.Warn("public IP refresh failed", "component", "fleet-agent", "err", err)
			return
		}
		w.reportPublicIP(res, true)
	}()
}

func (w *worker) reportPublicIP(res capture.PublicIPResult, force bool) {
	if res.IP == "" {
		return
	}
	if !force {
		w.pipMu.Lock()
		dup := w.pipReported &&
			res.IP == w.lastPIP.IP &&
			res.Confirmed == w.lastPIP.Confirmed &&
			res.At.Sub(w.lastPIP.At) < capture.PublicIPRefreshInterval
		w.pipMu.Unlock()
		if dup {
			return
		}
	}
	w.sendPublicIP(res)
}

func (w *worker) sendPublicIP(res capture.PublicIPResult) {
	p := channel.PublicIPPayload{
		IP:        res.IP,
		At:        res.At,
		Confirmed: res.Confirmed,
	}
	env, err := channel.NewEnvelope(channel.TypePublicIP, "", p)
	if err != nil {
		w.log.Warn("build public-ip envelope", "component", "fleet-agent", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(w.base, publicIPSendTimeout)
	defer cancel()
	if err := w.sender.Send(ctx, env); err != nil {
		w.log.Debug("send public-ip", "component", "fleet-agent", "err", err)
		return
	}
	w.pipMu.Lock()
	w.lastPIP = res
	w.pipReported = true
	w.pipMu.Unlock()
}

func (w *worker) unbindPublicIP() {
	w.pipMu.Lock()
	cache := w.pipCache
	w.pipCache = nil
	w.pipMu.Unlock()
	if cache != nil {
		cache.SetOnRefresh(nil)
	}
}
