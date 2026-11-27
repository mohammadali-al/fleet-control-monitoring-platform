package capture

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Source-address determination (§6.4):
//
//   - Private target: the machine's private IP on the NIC owning the route to
//     the target, looked up at capture time (routeSourceIP).
//   - Public target: the agent's public IP via reflector consensus over 6 HTTP
//     "what-is-my-IP" services (>=2 agree -> confirmed, exactly 1 alive ->
//     adopt). The value is cached and refreshed at boot, every 15 min, on
//     network change, and on demand; each record stores the cached value.

// DefaultReflectors is the built-in reflector set (6 independent HTTP services
// that echo the caller's public IP as plaintext). Configurable per node.
var DefaultReflectors = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
	"https://ipinfo.io/ip",
	"https://checkip.amazonaws.com",
	"https://ident.me",
}

// PublicIPRefreshInterval is the periodic reflector refresh cadence (§6.4).
const PublicIPRefreshInterval = 15 * time.Minute

// isPrivateIP reports whether ip is a private/local target for which the source
// address is the route's NIC IP rather than the public reflector result. This
// covers RFC1918 / ULA (net.IP.IsPrivate) plus loopback, link-local and the
// unspecified address.
func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	return ip.IsPrivate() ||
		ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified()
}

// routeDialer lets tests substitute the route lookup. The production value uses
// a connectionless UDP socket: connecting a UDP socket performs the kernel
// route lookup and binds the local address without sending any packet, which is
// exactly "the machine's private IP on the NIC owning the route to the target".
type routeDialer func(target net.IP) (net.IP, error)

func kernelRouteSourceIP(target net.IP) (net.IP, error) {
	network := "udp4"
	if target.To4() == nil {
		network = "udp6"
	}
	c, err := net.Dial(network, net.JoinHostPort(target.String(), "9"))
	if err != nil {
		return nil, fmt.Errorf("capture: route lookup to %s: %w", target, err)
	}
	defer c.Close()
	la, ok := c.LocalAddr().(*net.UDPAddr)
	if !ok || la.IP == nil || la.IP.IsUnspecified() {
		return nil, fmt.Errorf("capture: no route source IP for %s", target)
	}
	return la.IP, nil
}

// PublicIPResult is the outcome of one reflector-consensus resolution.
type PublicIPResult struct {
	IP        string    // the chosen public IP ("" when unresolved)
	Confirmed bool      // true when >=2 reflectors agreed
	Alive     int       // reflectors that answered a valid IP
	At        time.Time // when it was resolved
}

// Reflector resolves the agent's public IP by consensus over HTTP services.
type Reflector struct {
	URLs   []string
	Client *http.Client
}

// NewReflector builds a Reflector. Empty urls selects DefaultReflectors.
func NewReflector(urls []string) *Reflector {
	if len(urls) == 0 {
		urls = DefaultReflectors
	}
	return &Reflector{
		URLs:   urls,
		Client: &http.Client{Timeout: 5 * time.Second},
	}
}

// Resolve queries every reflector concurrently and applies §6.4 consensus:
// >=2 agree -> confirmed; exactly 1 alive -> adopt (unconfirmed); none alive ->
// error. The most-agreed IP wins; ties break deterministically by IP string.
func (r *Reflector) Resolve(ctx context.Context) (PublicIPResult, error) {
	type one struct {
		ip  string
		err error
	}
	results := make(chan one, len(r.URLs))
	var wg sync.WaitGroup
	for _, u := range r.URLs {
		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			ip, err := r.fetch(ctx, url)
			results <- one{ip, err}
		}(u)
	}
	go func() { wg.Wait(); close(results) }()

	counts := map[string]int{}
	alive := 0
	for res := range results {
		if res.err != nil || res.ip == "" {
			continue
		}
		alive++
		counts[res.ip]++
	}
	if alive == 0 {
		return PublicIPResult{At: time.Now()}, errors.New("capture: no reflector answered")
	}

	bestIP, best := "", 0
	for ip, n := range counts {
		if n > best || (n == best && ip < bestIP) {
			bestIP, best = ip, n
		}
	}
	return PublicIPResult{
		IP:        bestIP,
		Confirmed: best >= 2,
		Alive:     alive,
		At:        time.Now(),
	}, nil
}

// fetch retrieves and validates a single reflector's reported IP.
func (r *Reflector) fetch(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := r.Client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("capture: reflector %s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128))
	if err != nil {
		return "", err
	}
	ip := net.ParseIP(strings.TrimSpace(string(body)))
	if ip == nil {
		return "", fmt.Errorf("capture: reflector %s: not an IP", url)
	}
	return ip.String(), nil
}

// PublicIPCache holds the last reflector result and refreshes it on a timer and
// on demand (boot/15 min/network-change/manual). Current() is safe for the
// per-packet hot path. Start launches the refresh loop; Stop ends it, so the
// cache leaves no goroutine behind.
type PublicIPCache struct {
	ref      *Reflector
	interval time.Duration
	watchNet func(ctx context.Context, onChange func()) error
	log      *slog.Logger

	mu     sync.RWMutex
	result PublicIPResult
	cancel context.CancelFunc

	refreshC chan struct{}
	stopOnce sync.Once
	stop     chan struct{}
	done     chan struct{}

	onRefresh func(PublicIPResult)
}

// NewPublicIPCache builds a cache over ref. interval <=0 selects
// PublicIPRefreshInterval.
func NewPublicIPCache(ref *Reflector, interval time.Duration) *PublicIPCache {
	if interval <= 0 {
		interval = PublicIPRefreshInterval
	}
	return &PublicIPCache{
		ref:      ref,
		interval: interval,
		watchNet: watchNetworkChanges,
		log:      slog.Default(),
		refreshC: make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Current returns the cached public IP ("" until the first successful refresh).
func (c *PublicIPCache) Current() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.result.IP
}

// Result returns the full cached resolution.
func (c *PublicIPCache) Result() PublicIPResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.result
}

// RefreshNow resolves synchronously and updates the cache (manual button / boot
// / network-change entry point).
func (c *PublicIPCache) RefreshNow(ctx context.Context) (PublicIPResult, error) {
	res, err := c.ref.Resolve(ctx)
	if err != nil {
		return res, err
	}
	c.mu.Lock()
	c.result = res
	cb := c.onRefresh
	c.mu.Unlock()
	if cb != nil {
		cb(res)
	}
	return res, nil
}

// TriggerRefresh asks the background loop to refresh (non-blocking, coalesced).
func (c *PublicIPCache) TriggerRefresh() {
	select {
	case c.refreshC <- struct{}{}:
	default:
	}
}

// Start does an initial boot refresh, then refreshes every interval, on OS
// network-change events (best-effort), and on each TriggerRefresh until Stop.
// It is safe to call once.
func (c *PublicIPCache) Start(ctx context.Context) {
	runCtx, runCancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancel = runCancel
	c.mu.Unlock()

	var wg sync.WaitGroup
	if c.watchNet != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := c.watchNet(runCtx, c.TriggerRefresh); err != nil && runCtx.Err() == nil {
				log := c.log
				if log == nil {
					log = slog.Default()
				}
				log.Info("capture: network-change public-IP trigger unavailable; continuing with boot/periodic/manual triggers", "err", err)
			}
		}()
	}

	go func() {
		defer func() {
			runCancel()
			wg.Wait()
			close(c.done)
		}()
		_, _ = c.RefreshNow(runCtx)
		t := time.NewTicker(c.interval)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-runCtx.Done():
				return
			case <-t.C:
				_, _ = c.RefreshNow(runCtx)
			case <-c.refreshC:
				_, _ = c.RefreshNow(runCtx)
			}
		}
	}()
}

// Stop ends the refresh loop and waits for it (and any network watcher) to exit.
func (c *PublicIPCache) Stop() {
	c.stopOnce.Do(func() {
		close(c.stop)
		c.mu.Lock()
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
	<-c.done
}

// SetOnRefresh registers a callback invoked after each successful RefreshNow
// and returns the current cached result atomically under the cache lock.
func (c *PublicIPCache) SetOnRefresh(fn func(PublicIPResult)) PublicIPResult {
	c.mu.Lock()
	c.onRefresh = fn
	res := c.result
	c.mu.Unlock()
	return res
}

// SourceIPResolver picks the attribution source address for a target (§6.4):
// private targets use the route's NIC IP (looked up per call), public targets
// use the cached reflector IP.
type SourceIPResolver struct {
	cache *PublicIPCache
	route routeDialer
}

// NewSourceIPResolver builds a resolver. A nil cache means no public IP is
// known (public targets resolve to "").
func NewSourceIPResolver(cache *PublicIPCache) *SourceIPResolver {
	return &SourceIPResolver{cache: cache, route: kernelRouteSourceIP}
}

// For returns the source address to record for a packet to target. On a private
// target it returns the route NIC IP (empty on lookup failure); on a public
// target it returns the cached public IP (empty until first resolution).
func (r *SourceIPResolver) For(target net.IP) string {
	if target == nil {
		return ""
	}
	if isPrivateIP(target) {
		ip, err := r.route(target)
		if err != nil || ip == nil {
			return ""
		}
		return ip.String()
	}
	if r.cache == nil {
		return ""
	}
	return r.cache.Current()
}
