package capture

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// reflectorServer returns a test server that replies with body (or a 500 when
// body is empty, simulating a dead reflector).
func reflectorServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body == "" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

func urls(ss ...*httptest.Server) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.URL
	}
	return out
}

func TestReflectorConsensusAgree(t *testing.T) {
	// Four agree on .9, one is an outlier, one dead -> confirmed .9.
	ref := NewReflector(urls(
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, "203.0.113.9\n"),
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, "198.51.100.2"),
		reflectorServer(t, ""),
	))
	res, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.IP != "203.0.113.9" || !res.Confirmed {
		t.Errorf("got ip=%q confirmed=%v, want 203.0.113.9 confirmed", res.IP, res.Confirmed)
	}
	if res.Alive != 5 {
		t.Errorf("alive = %d, want 5", res.Alive)
	}
}

func TestReflectorConsensusTwoAgree(t *testing.T) {
	ref := NewReflector(urls(
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, ""),
	))
	res, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.IP != "203.0.113.9" || !res.Confirmed {
		t.Errorf("got ip=%q confirmed=%v, want confirmed", res.IP, res.Confirmed)
	}
}

func TestReflectorConsensusSingleAlive(t *testing.T) {
	// Exactly one alive -> adopt, but not confirmed.
	ref := NewReflector(urls(
		reflectorServer(t, "198.51.100.77"),
		reflectorServer(t, ""),
		reflectorServer(t, ""),
	))
	res, err := ref.Resolve(context.Background())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.IP != "198.51.100.77" {
		t.Errorf("ip = %q, want adopted 198.51.100.77", res.IP)
	}
	if res.Confirmed {
		t.Error("single-alive must not be confirmed")
	}
	if res.Alive != 1 {
		t.Errorf("alive = %d, want 1", res.Alive)
	}
}

func TestReflectorConsensusAllFail(t *testing.T) {
	ref := NewReflector(urls(
		reflectorServer(t, ""),
		reflectorServer(t, ""),
	))
	if _, err := ref.Resolve(context.Background()); err == nil {
		t.Error("expected error when every reflector fails")
	}
}

func TestPublicIPCacheRefresh(t *testing.T) {
	ref := NewReflector(urls(
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, "203.0.113.9"),
	))
	c := NewPublicIPCache(ref, 0)
	if c.Current() != "" {
		t.Errorf("fresh cache should be empty, got %q", c.Current())
	}
	if _, err := c.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	if c.Current() != "203.0.113.9" {
		t.Errorf("after refresh = %q", c.Current())
	}
}

func TestPublicIPCacheSetOnRefresh(t *testing.T) {
	ref := NewReflector(urls(
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, "203.0.113.9"),
	))
	c := NewPublicIPCache(ref, 0)
	got := make(chan PublicIPResult, 2)
	if initial := c.SetOnRefresh(func(r PublicIPResult) { got <- r }); initial.IP != "" {
		t.Fatalf("initial = %+v, want empty", initial)
	}
	if _, err := c.RefreshNow(context.Background()); err != nil {
		t.Fatalf("RefreshNow: %v", err)
	}
	res := <-got
	if res.IP != "203.0.113.9" || !res.Confirmed {
		t.Errorf("callback got %+v", res)
	}
	if cur := c.SetOnRefresh(nil); cur.IP != "203.0.113.9" {
		t.Errorf("SetOnRefresh returned %+v", cur)
	}
}

func TestIsPrivateIP(t *testing.T) {
	cases := map[string]bool{
		"10.1.2.3":     true,
		"192.168.0.1":  true,
		"172.16.5.4":   true,
		"127.0.0.1":    true,
		"169.254.1.1":  true,
		"fd00::1":      true,
		"8.8.8.8":      false,
		"203.0.113.10": false,
		"2606:2800::1": false,
	}
	for ip, want := range cases {
		if got := isPrivateIP(net.ParseIP(ip)); got != want {
			t.Errorf("isPrivateIP(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestSourceIPResolver(t *testing.T) {
	ref := NewReflector(urls(
		reflectorServer(t, "203.0.113.9"),
		reflectorServer(t, "203.0.113.9"),
	))
	cache := NewPublicIPCache(ref, 0)
	if _, err := cache.RefreshNow(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	r := NewSourceIPResolver(cache)
	// Stub the route lookup so the test does not depend on host routing.
	r.route = func(target net.IP) (net.IP, error) { return net.ParseIP("192.168.50.10"), nil }

	if got := r.For(net.ParseIP("10.0.0.5")); got != "192.168.50.10" {
		t.Errorf("private target src = %q, want route NIC IP", got)
	}
	if got := r.For(net.ParseIP("8.8.8.8")); got != "203.0.113.9" {
		t.Errorf("public target src = %q, want cached public IP", got)
	}
}

func TestPublicIPCacheNetworkChangeTriggerAndBestEffortFallback(t *testing.T) {
	var mu sync.Mutex
	curIP := "203.0.113.9"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		ip := curIP
		mu.Unlock()
		_, _ = w.Write([]byte(ip))
	}))
	defer srv.Close()

	// 1. Network-change event triggers refresh and invokes SetOnRefresh.
	ref := NewReflector([]string{srv.URL, srv.URL})
	cache := NewPublicIPCache(ref, time.Hour)
	triggerNetChange := make(chan struct{}, 1)
	cache.watchNet = func(ctx context.Context, onChange func()) error {
		for {
			select {
			case <-ctx.Done():
				return nil
			case <-triggerNetChange:
				onChange()
			}
		}
	}
	got := make(chan PublicIPResult, 4)
	cache.SetOnRefresh(func(r PublicIPResult) { got <- r })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache.Start(ctx)
	defer cache.Stop()

	res := <-got
	if res.IP != "203.0.113.9" {
		t.Fatalf("boot IP = %q, want 203.0.113.9", res.IP)
	}

	mu.Lock()
	curIP = "198.51.100.77"
	mu.Unlock()
	triggerNetChange <- struct{}{}

	select {
	case res = <-got:
		if res.IP != "198.51.100.77" {
			t.Fatalf("network-change IP = %q, want 198.51.100.77", res.IP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for network-change refresh")
	}

	// 2. If network-change watcher fails/is unavailable, boot + manual triggers still work.
	cache2 := NewPublicIPCache(ref, time.Hour)
	cache2.watchNet = func(context.Context, func()) error {
		return errors.New("netlink unavailable")
	}
	got2 := make(chan PublicIPResult, 4)
	cache2.SetOnRefresh(func(r PublicIPResult) { got2 <- r })
	cache2.Start(ctx)
	defer cache2.Stop()

	res2 := <-got2
	if res2.IP != "198.51.100.77" {
		t.Fatalf("fallback boot IP = %q", res2.IP)
	}
	mu.Lock()
	curIP = "203.0.113.44"
	mu.Unlock()
	cache2.TriggerRefresh()
	select {
	case res2 = <-got2:
		if res2.IP != "203.0.113.44" {
			t.Fatalf("fallback manual IP = %q, want 203.0.113.44", res2.IP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for manual refresh after watcher error")
	}
}
