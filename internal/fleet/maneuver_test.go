package fleet_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

type scriptedLink struct {
	id      string
	h       channel.Handler
	mu      sync.Mutex
	online  bool
	sent    []channel.Envelope
	onSend  func(channel.Envelope)
	started chan struct{}
}

func (l *scriptedLink) Send(_ context.Context, env channel.Envelope) error {
	l.mu.Lock()
	online := l.online
	if online {
		l.sent = append(l.sent, env)
	}
	cb := l.onSend
	l.mu.Unlock()
	if !online {
		return channel.ErrNotConnected
	}
	if cb != nil {
		cb(env)
	}
	return nil
}

func (l *scriptedLink) Run(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (l *scriptedLink) Online() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.online
}

func (l *scriptedLink) SentTypes() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.sent))
	for i, e := range l.sent {
		out[i] = e.Type
	}
	return out
}

func TestManeuverSyncStartHandshake(t *testing.T) {
	reg := registry.New()
	for _, id := range []string{"w1", "w2"} {
		if err := reg.Add(registry.Machine{ID: id, Role: registry.RoleWorker, ConnectMode: registry.ConnectModeServerDial, PrivateIPs: []string{"10.0.0.1"}, Provisioned: true}); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := exec.NewGate([]byte("secret"), nil)
	if err != nil {
		t.Fatal(err)
	}

	var linksMu sync.Mutex
	links := map[string]*scriptedLink{}
	m, err := fleet.New(fleet.Config{
		Registry:     reg,
		Gate:         gate,
		PollInterval: 10 * time.Millisecond,
		NewLink: func(mc registry.Machine, h channel.Handler) (fleet.Link, error) {
			l := &scriptedLink{id: mc.ID, h: h, online: true}
			linksMu.Lock()
			links[mc.ID] = l
			linksMu.Unlock()
			return l, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.Start(ctx)
	t.Cleanup(func() { cancel(); m.Close() })

	eventually(t, "workers online", func() bool {
		m1, ok1 := m.Machine("w1")
		m2, ok2 := m.Machine("w2")
		return ok1 && ok2 && m1.State == fleet.StateOnline && m2.State == fleet.StateOnline
	})

	plan := &maneuver.MaterializedPlan{
		ManeuverID: "mv-1",
		Name:       "Sync test",
		Target:     maneuver.Target{IP: "198.51.100.10"},
		TotalSec:   10,
		Pattern:    maneuver.PatternConfig{Type: maneuver.PatternAllInOne},
		Items: []maneuver.MaterializedItem{
			{ItemID: "i1", BaselineID: "b1", Name: "Echo", Command: "echo hi", StartSec: 0, DurationSec: 2, Replicas: 1},
		},
		Agents: []string{"w1", "w2"},
	}

	t.Run("commits only after all agents ACK ready", func(t *testing.T) {
		linksMu.Lock()
		w1, w2 := links["w1"], links["w2"]
		linksMu.Unlock()

		w1.mu.Lock()
		w1.sent = nil
		w1.onSend = func(env channel.Envelope) {
			if env.Type == channel.TypeManeuverPrepare {
				ack, _ := channel.NewEnvelope(channel.TypeManeuverAck, env.ID, channel.ManeuverAckPayload{
					RunID: env.ID, Status: channel.ManeuverAckReady,
				})
				go w1.h(ctx, ack)
			}
		}
		w1.mu.Unlock()

		// w2 delays its ACK briefly so we can verify commit is NOT sent after w1's ACK alone.
		w2PrepCh := make(chan channel.Envelope, 1)
		w2.mu.Lock()
		w2.sent = nil
		w2.onSend = func(env channel.Envelope) {
			if env.Type == channel.TypeManeuverPrepare {
				w2PrepCh <- env
			}
		}
		w2.mu.Unlock()

		run, err := m.StartManeuver(ctx, fleet.StartManeuverRequest{
			Plan:     plan,
			LeadTime: 500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("StartManeuver: %v", err)
		}

		prepEnv := <-w2PrepCh
		eventually(t, "w1 ack ready recorded", func() bool {
			r, ok := m.ManeuverRun(run.RunID)
			return ok && r.Agents["w1"].State == channel.ManeuverAckReady
		})

		// Commit must NOT have been sent yet while w2 is still pending.
		for _, typ := range w1.SentTypes() {
			if typ == channel.TypeManeuverCommit {
				t.Fatalf("commit sent before all ACKs: w1 sent=%v", w1.SentTypes())
			}
		}

		// Now w2 ACKs ready -> server commits T.
		ack2, _ := channel.NewEnvelope(channel.TypeManeuverAck, prepEnv.ID, channel.ManeuverAckPayload{
			RunID: prepEnv.ID, Status: channel.ManeuverAckReady,
		})
		w2.h(ctx, ack2)

		eventually(t, "run committed or running and commit sent to both", func() bool {
			r, ok := m.ManeuverRun(run.RunID)
			if !ok || (r.State != fleet.ManeuverStateCommitted && r.State != fleet.ManeuverStateRunning) {
				return false
			}
			hasCommit := func(l *scriptedLink) bool {
				for _, typ := range l.SentTypes() {
					if typ == channel.TypeManeuverCommit {
						return true
					}
				}
				return false
			}
			return hasCommit(w1) && hasCommit(w2)
		})
	})

	t.Run("aborts without commit if any agent reports missed", func(t *testing.T) {
		linksMu.Lock()
		w1, w2 := links["w1"], links["w2"]
		linksMu.Unlock()

		for _, tc := range []struct {
			l      *scriptedLink
			status string
		}{
			{w1, channel.ManeuverAckReady},
			{w2, channel.ManeuverAckMissed},
		} {
			l := tc.l
			st := tc.status
			l.mu.Lock()
			l.sent = nil
			l.onSend = func(env channel.Envelope) {
				if env.Type == channel.TypeManeuverPrepare {
					ack, _ := channel.NewEnvelope(channel.TypeManeuverAck, env.ID, channel.ManeuverAckPayload{
						RunID: env.ID, Status: st, Reason: "test " + st,
					})
					go l.h(ctx, ack)
				}
			}
			l.mu.Unlock()
		}

		run, err := m.StartManeuver(ctx, fleet.StartManeuverRequest{
			Plan:     plan,
			LeadTime: 500 * time.Millisecond,
		})
		if err != nil {
			t.Fatalf("StartManeuver: %v", err)
		}

		eventually(t, "run aborted on missed", func() bool {
			r, ok := m.ManeuverRun(run.RunID)
			return ok && r.State == fleet.ManeuverStateAborted && r.Agents["w2"].State == channel.ManeuverAckMissed
		})

		for _, l := range []*scriptedLink{w1, w2} {
			var gotAbort bool
			for _, typ := range l.SentTypes() {
				if typ == channel.TypeManeuverCommit {
					t.Fatalf("%s received commit despite missed ACK", l.id)
				}
				if typ == channel.TypeManeuverAbort {
					gotAbort = true
				}
			}
			if !gotAbort {
				t.Errorf("%s did not receive maneuver-abort", l.id)
			}
		}
		_ = json.Marshal
	})
}
