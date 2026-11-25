package watchdog

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func secs(n int) time.Duration { return time.Duration(n) * time.Second }

func TestValidateHeartbeat(t *testing.T) {
	good := [][2]int{{2, 45}, {5, 90}, {10, 75}, {15, 45}, {15, 300}, {2, 300}, {14, 45}}
	for _, c := range good {
		if err := ValidateHeartbeat(secs(c[0]), secs(c[1])); err != nil {
			t.Errorf("write %ds threshold %ds should be accepted: %v", c[0], c[1], err)
		}
	}
	bad := []struct {
		w, th int
		why   string
	}{
		{50, 75, "write above 15"},
		{1, 90, "write below 2"},
		{16, 300, "write above 15"},
		{5, 44, "threshold below the 45 s check"},
		{5, 301, "threshold above 300"},
		{0, 90, "zero write"},
		{15, 44, "threshold below 45"},
		{10, 29, "threshold below 3 x write and 45"},
	}
	for _, c := range bad {
		if err := ValidateHeartbeat(secs(c.w), secs(c.th)); err == nil {
			t.Errorf("write %ds threshold %ds should be rejected (%s)", c.w, c.th, c.why)
		}
	}
	// With write <= 15 the 45 s floor already implies threshold >= 3 x write, so
	// the factor never binds on its own today. If the bounds ever change so that
	// it can, this reminds whoever does it to give the factor its own case.
	if HeartbeatThresholdFactor*MaxHeartbeatWrite > MinHeartbeatThreshold {
		t.Error("bounds changed: threshold >= 3 x write is now independent of the floor and needs its own test")
	}
}

// The brief's examples, through the policy parser (which the panel's save uses).
func TestPolicyHeartbeatFields(t *testing.T) {
	def := DefaultPolicy()
	if def.HeartbeatWrite.D() != DefaultHeartbeatWrite || def.HeartbeatThreshold.D() != DefaultHeartbeatThreshold {
		t.Fatalf("defaults: %v/%v", def.HeartbeatWrite.D(), def.HeartbeatThreshold.D())
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("default policy invalid: %v", err)
	}
	pf, err := ParseProfile([]byte("version: 1\nheartbeat_write: 10s\nheartbeat_threshold: 75s\n"))
	if err != nil {
		t.Fatalf("10/75 should be accepted: %v", err)
	}
	p, err := pf.Effective("m", nil)
	if err != nil || p.HeartbeatWrite.D() != 10*time.Second || p.HeartbeatThreshold.D() != 75*time.Second {
		t.Fatalf("effective: %v %v", p, err)
	}
	for name, y := range map[string]string{
		"50/75":            "version: 1\nheartbeat_write: 50s\nheartbeat_threshold: 75s\n",
		"write below 2":    "version: 1\nheartbeat_write: 1s\n",
		"threshold < 45":   "version: 1\nheartbeat_threshold: 40s\n",
		"threshold > 300":  "version: 1\nheartbeat_threshold: 301s\n",
		"threshold < 3x":   "version: 1\nheartbeat_write: 15s\nheartbeat_threshold: 44s\n",
		"override too low": "version: 1\noverrides:\n  hosts:\n    wrk-1:\n      heartbeat_threshold: 10s\n",
	} {
		if _, err := ParseProfile([]byte(y)); err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
	// A pushed policy that only lowers write keeps the default threshold, so it
	// must still hold the factor: write 15 with the default 90 is fine.
	if _, err := ParseProfile([]byte("version: 1\nheartbeat_write: 15s\n")); err != nil {
		t.Errorf("15/default: %v", err)
	}
}

func TestHeartbeatTickWritesAndPaces(t *testing.T) {
	dir := t.TempDir()
	h := NewHeartbeat(HeartbeatPath(dir, HeartbeatWatchdog))
	now := time.Unix(1_800_000_000, 0)
	h.Now = func() time.Time { return now }

	next, err := h.Tick(secs(5), secs(90))
	if err != nil || next != secs(5) {
		t.Fatalf("first tick: %v %v", next, err)
	}
	at, th, err := ReadHeartbeat(h.Path())
	if err != nil || !at.Equal(now) || th != secs(90) {
		t.Fatalf("file: %v %v %v", at, th, err)
	}

	// Too soon: no write, and it says how long to wait.
	now = now.Add(secs(2))
	next, _ = h.Tick(secs(5), secs(90))
	if next != secs(3) {
		t.Fatalf("pacing: %v", next)
	}
	if at, _, _ := ReadHeartbeat(h.Path()); !at.Equal(time.Unix(1_800_000_000, 0)) {
		t.Fatal("wrote before the interval elapsed")
	}

	// Due again, with a live threshold change (a pushed policy).
	now = now.Add(secs(3))
	if _, err := h.Tick(secs(5), secs(120)); err != nil {
		t.Fatal(err)
	}
	at, th, _ = ReadHeartbeat(h.Path())
	if !at.Equal(now) || th != secs(120) {
		t.Fatalf("after change: %v %v", at, th)
	}
	if fi, _ := os.Stat(h.Path()); fi.Size() != 17 {
		t.Fatalf("heartbeat file size %d, want a fixed 17 bytes", fi.Size())
	}

	// A removed file is recreated on the next write.
	_ = os.Remove(h.Path())
	now = now.Add(secs(6))
	if _, err := h.Tick(secs(5), secs(120)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Path()); err != nil {
		t.Fatalf("a removed heartbeat file was not recreated: %v", err)
	}
	// Invalid settings fall back to the defaults rather than writing nonsense.
	now = now.Add(secs(60))
	if _, err := h.Tick(secs(1), secs(1)); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatSettingsFollowThePolicyFile(t *testing.T) {
	dir := t.TempDir()
	s := &HeartbeatSettings{DataDir: dir, MachineID: "wrk-1"}
	if w, th := s.Get(); w != DefaultHeartbeatWrite || th != DefaultHeartbeatThreshold {
		t.Fatalf("no policy: %v/%v", w, th)
	}
	pf, err := ParseProfile([]byte("version: 2\nheartbeat_write: 10s\nheartbeat_threshold: 75s\noverrides:\n  hosts:\n    wrk-1:\n      heartbeat_threshold: 120s\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveLastKnown(dir, pf); err != nil {
		t.Fatal(err)
	}
	if w, th := s.Get(); w != secs(10) || th != secs(120) {
		t.Fatalf("with policy: %v/%v", w, th)
	}
}

// fakeSystemd runs the real liveness script under sh with stand-in systemctl and
// logger commands, so the freshness decision is tested as it runs in production.
type fakeSystemd struct {
	t   *testing.T
	bin string
	log string
}

func newFakeSystemd(t *testing.T) *fakeSystemd {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	d := t.TempDir()
	bin := filepath.Join(d, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logf := filepath.Join(d, "calls.log")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("systemctl", `case "$1" in
is-active) [ "$FAKE_ACTIVE" = yes ];;
show) echo "$FAKE_ENTER";;
*) echo "systemctl $*" >>"$FAKE_LOG";;
esac`)
	write("logger", `echo "logger $*" >>"$FAKE_LOG"`)
	return &fakeSystemd{t: t, bin: bin, log: logf}
}

// run executes the script; active says whether the unit is active, since is when
// it became active, hb the heartbeat file content ("-" = no file). It returns
// the systemctl/logger calls made.
func (f *fakeSystemd) run(active bool, since time.Time, hb string) string {
	f.t.Helper()
	_ = os.Remove(f.log)
	hbFile := filepath.Join(f.t.TempDir(), "heartbeat")
	if hb != "-" {
		if err := os.WriteFile(hbFile, []byte(hb), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	cmd := exec.Command("sh", "-c", LivenessScript("fleet-test.service", hbFile))
	yes := "no"
	if active {
		yes = "yes"
	}
	cmd.Env = append(os.Environ(),
		"PATH="+f.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FAKE_ACTIVE="+yes, "FAKE_LOG="+f.log, "TZ=UTC",
		"FAKE_ENTER="+since.UTC().Format("Mon 2006-01-02 15:04:05")+" UTC")
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("liveness script failed: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(f.log)
	return string(b)
}

func hbAt(age int, threshold int) string {
	return fmt.Sprintf("%010d %05d\n", time.Now().Add(-secs(age)).Unix(), threshold)
}

func TestLivenessScriptFreshness(t *testing.T) {
	f := newFakeSystemd(t)
	longAgo := time.Now().Add(-time.Hour)

	if got := f.run(true, longAgo, hbAt(10, 90)); got != "" {
		t.Errorf("fresh heartbeat must not restart: %q", got)
	}
	got := f.run(true, longAgo, hbAt(200, 90))
	if !strings.Contains(got, "systemctl restart fleet-test.service") || !strings.Contains(got, "hung") {
		t.Errorf("stale heartbeat must report hung and restart: %q", got)
	}
	// The threshold is the one in the file, so a pushed change applies live.
	if got := f.run(true, longAgo, hbAt(200, 300)); got != "" {
		t.Errorf("200 s old with a 300 s threshold is fresh: %q", got)
	}
	if got := f.run(true, longAgo, hbAt(60, 45)); !strings.Contains(got, "systemctl restart") {
		t.Errorf("60 s old with a 45 s threshold is stale: %q", got)
	}
	// An unreadable threshold falls back to 90 s.
	if got := f.run(true, longAgo, fmt.Sprintf("%010d xx\n", time.Now().Add(-secs(100)).Unix())); !strings.Contains(got, "systemctl restart") {
		t.Errorf("100 s old with the default 90 s threshold is stale: %q", got)
	}
	// A heartbeat from before the unit's last restart does not count: the service
	// has only just started and is given the threshold from now.
	if got := f.run(true, time.Now().Add(-secs(5)), hbAt(500, 90)); got != "" {
		t.Errorf("a freshly started unit must not be restarted for an old file: %q", got)
	}
	// ... but one that started long ago and never wrote anything new is hung.
	if got := f.run(true, time.Now().Add(-secs(500)), hbAt(1000, 90)); !strings.Contains(got, "systemctl restart") {
		t.Errorf("a unit that never wrote since starting is hung: %q", got)
	}
	// No heartbeat file at all: nothing to judge by, so leave it alone.
	if got := f.run(true, longAgo, "-"); got != "" {
		t.Errorf("missing heartbeat file must not restart: %q", got)
	}
	// The original is-active behaviour is kept.
	got = f.run(false, longAgo, hbAt(1, 90))
	if !strings.Contains(got, "systemctl start fleet-test.service") || strings.Contains(got, "restart") {
		t.Errorf("an inactive unit is started: %q", got)
	}
}

// The deploy/ unit files carry exactly the script the Go code defines, on the
// fixed 45 s cadence.
func TestDeployUnitsMatchTheScript(t *testing.T) {
	root := filepath.Join("..", "..", "deploy", "systemd")
	for unit, who := range map[string]string{"fleet-watchdog": HeartbeatWatchdog, "fleet-agent": HeartbeatAgent} {
		svc, err := os.ReadFile(filepath.Join(root, unit+"-liveness.service"))
		if err != nil {
			t.Fatal(err)
		}
		want := "ExecStart=" + LivenessExecStart(unit+".service", HeartbeatPath("/var/lib/fleet-agent", who)) + "\n"
		if !strings.Contains(string(svc), want) {
			t.Errorf("%s-liveness.service ExecStart is out of step with LivenessExecStart; want\n%s", unit, want)
		}
		tim, err := os.ReadFile(filepath.Join(root, unit+"-liveness.timer"))
		if err != nil {
			t.Fatal(err)
		}
		// fleet-watchdog-liveness.timer (1 min -> 45 s) is outside T-0027's touches
		// and ships as a patch, so only the agent timer is checked here.
		if unit == "fleet-agent" && !strings.Contains(string(tim), "OnUnitActiveSec=45s\n") {
			t.Errorf("%s-liveness.timer must fire every 45 s:\n%s", unit, tim)
		}
	}
}
