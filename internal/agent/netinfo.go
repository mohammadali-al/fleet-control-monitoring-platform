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

const netInfoSendTimeout = 10 * time.Second

// netInfoInterval is how often the primary private IP is re-resolved so a
// change (DHCP, new default route) is reported.
var netInfoInterval = 30 * time.Second

// primaryIPFunc resolves the default-route source IP; tests substitute it.
var primaryIPFunc = capture.DefaultRouteSourceIP

// netInfoReporter reports the agent's primary private IP (source IP of the
// default route) on connect and whenever it changes. Shared by worker and
// monitor agents.
type netInfoReporter struct {
	sender exec.Sender
	log    *slog.Logger

	mu   sync.Mutex
	last string
}

func newNetInfoReporter(sender exec.Sender, log *slog.Logger) *netInfoReporter {
	return &netInfoReporter{sender: sender, log: log}
}

// reportNow resolves and sends the IP unconditionally (used on connect).
func (r *netInfoReporter) reportNow(ctx context.Context) { r.report(ctx, true) }

func (r *netInfoReporter) report(ctx context.Context, force bool) {
	ip, err := primaryIPFunc()
	if err != nil || ip == "" {
		r.log.Debug("resolve primary private ip", "component", "fleet-agent", "err", err)
		return
	}
	r.mu.Lock()
	same := ip == r.last
	r.mu.Unlock()
	if same && !force {
		return
	}
	env, err := channel.NewEnvelope(channel.TypeNetInfo, "", channel.NetInfoPayload{PrimaryPrivateIP: ip, At: time.Now()})
	if err != nil {
		r.log.Warn("build net-info envelope", "component", "fleet-agent", "err", err)
		return
	}
	sctx, cancel := context.WithTimeout(ctx, netInfoSendTimeout)
	defer cancel()
	if err := r.sender.Send(sctx, env); err != nil {
		r.log.Debug("send net-info", "component", "fleet-agent", "err", err)
		return
	}
	r.mu.Lock()
	r.last = ip
	r.mu.Unlock()
}

// run re-checks periodically (reporting only changes) until ctx ends.
func (r *netInfoReporter) run(ctx context.Context) {
	t := time.NewTicker(netInfoInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.report(ctx, false)
		}
	}
}
