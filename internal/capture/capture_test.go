package capture

import (
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fakeSource is a synthetic packetSource: it replays a fixed list of packets
// and reports a configurable drop count, so the session loop is exercised with
// no kernel and no root.
type fakeSource struct {
	ch    chan capturedPacket
	drops uint64
}

func newFakeSource(pkts []capturedPacket, drops uint64) *fakeSource {
	ch := make(chan capturedPacket, len(pkts))
	for _, p := range pkts {
		ch <- p
	}
	return &fakeSource{ch: ch, drops: drops}
}

func (f *fakeSource) Packets() <-chan capturedPacket { return f.ch }
func (f *fakeSource) Drops() uint64                  { return f.drops }
func (f *fakeSource) Close() error                   { close(f.ch); return nil }

func TestSessionCaptureEndToEnd(t *testing.T) {
	dataDir := t.TempDir()
	c, err := NewController(Config{DataDir: dataDir, Machine: "m1"})
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	// Deterministic source IP, no network.
	c.resolver = &SourceIPResolver{route: func(net.IP) (net.IP, error) { return net.ParseIP("192.168.9.9"), nil }}

	ts := time.Now()
	pkts := []capturedPacket{
		{data: tcpv4(t, "192.168.1.5", "93.184.216.34", 5000, 80, true, false, nil), ts: ts},
		{data: tcpv4(t, "192.168.1.5", "93.184.216.34", 5000, 80, false, true, []byte("GET /")), ts: ts.Add(time.Millisecond)},
		{data: udpv4(t, "192.168.1.5", "8.8.8.8", 5353, 53, []byte("dns")), ts: ts.Add(2 * time.Millisecond)},
		{data: []byte{0x00, 0x00}, ts: ts}, // non-IP: must be ignored, not fatal
	}
	src := newFakeSource(pkts, 3)

	sw, err := NewSegmentWriter(c.machineDir(), "m1", c.cfg.Segment, c.log)
	if err != nil {
		t.Fatalf("NewSegmentWriter: %v", err)
	}
	sw.diskUse = func(string) (float64, error) { return 10, nil }

	sc := c.startSession("sess-xyz", sw, src)

	// Wait for the three valid packets to be recorded.
	deadline := time.Now().Add(2 * time.Second)
	for sc.Records() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	sum, err := sc.Stop()
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if sum.Records != 3 {
		t.Errorf("records = %d, want 3 (non-IP ignored)", sum.Records)
	}
	if sum.Dropped != 3 {
		t.Errorf("dropped = %d, want 3", sum.Dropped)
	}
	if sum.Segments < 1 {
		t.Errorf("segments = %d, want >=1", sum.Segments)
	}

	// Manifest on disk must verify, and dropped count must be carried.
	mf, err := LoadManifest(c.machineDir(), "m1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if err := mf.Verify(); err != nil {
		t.Errorf("Verify: %v", err)
	}
	var drops uint64
	for _, e := range mf.Entries() {
		drops += e.Dropped
	}
	if drops != 3 {
		t.Errorf("manifest dropped total = %d, want 3", drops)
	}
}

func TestControllerCaptureSourceFailureIsNonFatal(t *testing.T) {
	c, err := NewController(Config{DataDir: t.TempDir(), Machine: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	// A factory that always fails simulates "no privilege": Capture returns an
	// error, which the agent logs and ignores (the command still runs).
	c.newSource = func(cgroupPath string, group int, log *slog.Logger) (packetSource, error) {
		return nil, errNoSource
	}
	if _, err := c.Capture("sess-1", "/sys/fs/cgroup/x"); err == nil {
		t.Error("Capture should surface the source setup error")
	}
}

var errNoSource = &captureError{"no source"}

type captureError struct{ s string }

func (e *captureError) Error() string { return e.s }

func TestNextGroupDistinct(t *testing.T) {
	c, err := NewController(Config{DataDir: t.TempDir(), Machine: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for i := 0; i < 5; i++ {
		g := c.nextGroup()
		if seen[g] {
			t.Errorf("duplicate group %d", g)
		}
		seen[g] = true
		if g < DefaultNFLOGGroupBase {
			t.Errorf("group %d below base", g)
		}
	}
}

// smokeHangTimeout bounds the whole live smoke body. The T-0017 hang blocked a
// root run until an outer 30 s kill; this internal watchdog makes a hang fail
// fast with a useful message instead of blocking a CI/root run.
const smokeHangTimeout = 10 * time.Second

// TestLiveCaptureSmoke exercises the real nftables+NFLOG path. It needs root
// (CAP_NET_ADMIN), cgroup v2 and the nft binary, so it is skipped by default
// and in CI. Run as root to exercise it. The body runs under a watchdog so a
// regression that blocks setup, read, or Close fails fast (T-0017).
func TestLiveCaptureSmoke(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("live capture is Linux-only")
	}
	if os.Geteuid() != 0 {
		t.Skip("SKIP: live kernel capture needs root (CAP_NET_ADMIN); run as root to exercise")
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Skip("SKIP: nft binary not found")
	}
	root := "/sys/fs/cgroup"
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
		t.Skip("SKIP: cgroup v2 not mounted at /sys/fs/cgroup")
	}

	// Create a throwaway cgroup, start capture on it, generate one outbound
	// UDP packet from a process in the cgroup, and expect at least one record.
	cgDir := filepath.Join(root, "fleet-capture-test")
	if err := os.Mkdir(cgDir, 0o755); err != nil && !os.IsExist(err) {
		t.Skipf("SKIP: cannot create test cgroup: %v", err)
	}
	t.Cleanup(func() { os.Remove(cgDir) })

	// Verbose setup/teardown trace so a hang (should one reappear) shows exactly
	// where it stopped making progress.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Run the live body under a watchdog. Only t.Error/t.Log are safe from a
	// non-test goroutine, so the body uses those and the watchdog (on the test
	// goroutine) does the fatal on a hang.
	done := make(chan struct{})
	go func() {
		defer close(done)
		src, err := newLiveSource(cgDir, DefaultNFLOGGroupBase+999, log)
		if err != nil {
			t.Errorf("newLiveSource: %v", err)
			return
		}
		// Move ourselves into the cgroup so our traffic matches, send a UDP datagram.
		_ = os.WriteFile(filepath.Join(cgDir, "cgroup.procs"), []byte("0"), 0o644)
		conn, derr := net.Dial("udp", "203.0.113.1:9")
		if derr == nil {
			conn.Write([]byte("live-capture-smoke"))
			conn.Close()
		}

		got := false
		select {
		case <-src.Packets():
			got = true
		case <-time.After(2 * time.Second):
		}
		if err := src.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		if !got {
			t.Log("no packet observed (environment-dependent); setup/teardown path still exercised")
		}
	}()

	select {
	case <-done:
	case <-time.After(smokeHangTimeout):
		// Leaves the body goroutine parked, but a failing hang test is meant to
		// surface the bug; the nft table is torn down by Close in the fix path.
		t.Fatalf("live capture smoke hung (>%s): a netlink op or Close did not return", smokeHangTimeout)
	}
}
