package panel

import (
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

// The Watchdog policy editor exposes heartbeat_write and heartbeat_threshold,
// validated by the one shared check (T-0027); the 45 s check cadence is not a key.
func TestWatchdogPolicyHeartbeatFields(t *testing.T) {
	e := newObsEnv(t, nil)

	var pol struct {
		YAML      string          `json:"yaml"`
		Effective watchdog.Policy `json:"effective"`
	}
	if code, _ := e.req("GET", "/api/watchdog/policy", nil, &pol); code != 200 {
		t.Fatalf("policy = %d", code)
	}
	for _, want := range []string{"heartbeat_write: 5s\n", "heartbeat_threshold: 1m30s\n"} {
		if !strings.Contains(pol.YAML, want) {
			t.Errorf("template lacks %q:\n%s", want, pol.YAML)
		}
	}
	if strings.Contains(pol.YAML, "check_interval") || strings.Contains(pol.YAML, "heartbeat_check") {
		t.Errorf("the fixed 45 s check cadence must not be a policy key:\n%s", pol.YAML)
	}
	if pol.Effective.HeartbeatWrite.D() != watchdog.DefaultHeartbeatWrite || pol.Effective.HeartbeatThreshold.D() != watchdog.DefaultHeartbeatThreshold {
		t.Errorf("effective = %v / %v", pol.Effective.HeartbeatWrite.D(), pol.Effective.HeartbeatThreshold.D())
	}

	save := func(w, th string) int {
		code, _ := e.req("POST", "/api/watchdog/policy", map[string]string{"yaml": "version: 1\nheartbeat_write: " + w + "\nheartbeat_threshold: " + th + "\n"}, nil)
		return code
	}
	for _, bad := range [][2]string{{"50s", "75s"}, {"1s", "75s"}, {"10s", "44s"}, {"10s", "301s"}, {"15s", "40s"}} {
		if code := save(bad[0], bad[1]); code != 400 {
			t.Errorf("write %s threshold %s = %d, want 400", bad[0], bad[1], code)
		}
	}
	if code := save("10s", "75s"); code != 200 {
		t.Errorf("10s/75s = %d, want 200", code)
	}
}
