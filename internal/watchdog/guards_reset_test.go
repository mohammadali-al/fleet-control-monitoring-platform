package watchdog

import (
	"testing"
	"time"
)

// A reset request left in the data dir (by the main agent) is applied once, by
// the next Roll of the watchdog's guards: counters, alert-only latch and stage
// cleared, cooldown clocks kept, OnReset called.
func TestRollAppliesResetRequestOnce(t *testing.T) {
	dir := t.TempDir()
	g, err := LoadGuards(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	g.Roll(now, false) // the first sample of the day
	if err := g.Record(StepRestart, now); err != nil {
		t.Fatal(err)
	}
	if err := g.SetAlertOnly(true); err != nil {
		t.Fatal(err)
	}
	resets := 0
	g.OnReset = func(time.Time) { resets++ }

	g.Roll(now, false)
	if st := g.Snapshot(); st.Restarts != 1 || !st.AlertOnly || resets != 0 {
		t.Fatalf("no request: state %+v resets %d, want untouched", st, resets)
	}

	if err := RequestReset(dir, now); err != nil {
		t.Fatal(err)
	}
	if !ResetRequested(dir) {
		t.Fatal("request not visible")
	}
	g.Roll(now, false)
	st := g.Snapshot()
	if st.Restarts != 0 || st.AlertOnly || st.Stage != StepNone || st.LastRestart.IsZero() || resets != 1 {
		t.Fatalf("after the request: state %+v resets %d", st, resets)
	}
	if ResetRequested(dir) {
		t.Error("request still pending")
	}
	if err := g.Record(StepRestart, now); err != nil {
		t.Fatal(err)
	}
	g.Roll(now, false)
	if st := g.Snapshot(); st.Restarts != 1 || resets != 1 {
		t.Errorf("a taken request was applied again: %+v resets %d", st, resets)
	}
}
