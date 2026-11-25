package panel

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

// The Watchdog page's "Reset counters" (T-0028): the endpoint pushes a
// watchdog-reset to the named worker, or to every connected worker, and says
// plainly when it could not.
func TestWatchdogResetCounters(t *testing.T) {
	e := newObsEnv(t, nil)
	sent := func(id string) int { return len(e.link(id).sentOfType(channel.TypeWatchdogReset)) }

	// One host.
	var one struct {
		Reset  []string `json:"reset"`
		Failed []string `json:"failed"`
	}
	if code, _ := e.req("POST", "/api/watchdog/reset", map[string]string{"machine": "w1"}, &one); code != 200 || len(one.Reset) != 1 || one.Reset[0] != "w1" {
		t.Fatalf("reset w1 = %d %+v", code, one)
	}
	if sent("w1") != 1 {
		t.Fatalf("w1 was sent %d watchdog-reset envelopes, want 1", sent("w1"))
	}

	// An offline worker is refused (not queued), an unknown one and a monitor agent are not found.
	var bad struct {
		Error string `json:"error"`
	}
	if code, _ := e.req("POST", "/api/watchdog/reset", map[string]string{"machine": "w2"}, &bad); code != 409 || !strings.Contains(bad.Error, "not connected") {
		t.Errorf("reset offline w2 = %d %+v, want 409", code, bad)
	}
	if sent("w2") != 0 {
		t.Errorf("an offline agent was sent a reset")
	}
	if code, _ := e.req("POST", "/api/watchdog/reset", map[string]string{"machine": "nope"}, nil); code != 404 {
		t.Errorf("reset unknown = %d, want 404", code)
	}
	if code, _ := e.req("POST", "/api/watchdog/reset", map[string]string{"machine": "mon1"}, nil); code != 404 {
		t.Errorf("reset monitor agent = %d, want 404", code)
	}
	if code, _ := e.req("POST", "/api/watchdog/reset", map[string]string{"machine": "w1", "bogus": "x"}, nil); code != 400 {
		t.Errorf("unknown field = %d, want 400", code)
	}

	// Every connected worker: only w1 is online.
	var all struct {
		Reset  []string `json:"reset"`
		Failed []string `json:"failed"`
	}
	if code, _ := e.req("POST", "/api/watchdog/reset", map[string]string{}, &all); code != 200 || len(all.Reset) != 1 || all.Reset[0] != "w1" || len(all.Failed) != 0 {
		t.Fatalf("reset all = %d %+v", code, all)
	}
	if sent("w1") != 2 || sent("w2") != 0 {
		t.Errorf("sent w1=%d w2=%d, want 2 and 0", sent("w1"), sent("w2"))
	}
	if n := len(e.link("mon1").sentOfType(channel.TypeWatchdogReset)); n != 0 {
		t.Errorf("the monitor agent was sent %d resets", n)
	}

	// The page no longer carries the stub for it.
	resp, err := http.Get(e.srv.URL + "/watchdog.html")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(page), `id="wd-reset-all"`) || strings.Contains(string(page), "does not exist yet") {
		t.Error("the Watchdog page still stubs the counter reset")
	}
}
