package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
)

type captureSender struct {
	mu   sync.Mutex
	envs []channel.Envelope
}

func (s *captureSender) Send(_ context.Context, env channel.Envelope) error {
	s.mu.Lock()
	s.envs = append(s.envs, env)
	s.mu.Unlock()
	return nil
}

func (s *captureSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envs)
}

// GAP-18: idle() must require both no running sessions (len(w.live) == 0) AND
// an empty server FIFO queue before PushIfIdle volunteers a manifest.
func TestIdleChecksServerQueueEmpty(t *testing.T) {
	dir := t.TempDir()
	w := newWorker(Config{MachineID: "w1", DataDir: dir})
	defer w.setServerQueueEmpty(true)

	// Write one finalized segment so PushIfIdle has pending work to offer.
	evDir := capture.EvidenceMachineDir(dir, "w1")
	sw, err := capture.NewSegmentWriter(evDir, "w1", capture.SegmentConfig{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Write(capture.Record{
		SessionID: "s-1", Timestamp: time.Now().UTC(), Protocol: "UDP",
		Src: "10.0.0.5:4000", Dst: "192.0.2.1:53", PayloadStatus: "full",
		PayloadBytesFull: 2, PayloadSHA256: "00", PayloadHead: []byte{1, 2}, SrcIPUsed: "10.0.0.5",
	}); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}

	snd := &captureSender{}
	ev, err := evidence.NewAgent(evidence.AgentConfig{
		Machine: "w1",
		Dir:     filepath.Join(dir, "evidence", "w1"),
		Sender:  snd,
		Idle:    w.idle,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.ev = ev

	if !w.idle() {
		t.Fatal("expected idle() == true initially")
	}

	// Server signals FIFO queue is non-empty.
	busyEnv, err := channel.NewEnvelope(typeQueueState, "", queueStatePayload{Empty: false})
	if err != nil {
		t.Fatal(err)
	}
	if !w.handleEvidence(context.Background(), busyEnv) {
		t.Fatal("handleEvidence did not consume queue-state")
	}
	if w.idle() {
		t.Fatal("idle() must be false when server FIFO queue is non-empty, even with len(w.live) == 0")
	}
	pushed, err := w.ev.PushIfIdle(context.Background())
	if err != nil || pushed {
		t.Fatalf("PushIfIdle = (%v, %v), want (false, nil) while server queue non-empty", pushed, err)
	}
	if snd.count() != 0 {
		t.Fatalf("sent %d envelopes while server queue non-empty", snd.count())
	}

	// Server signals FIFO queue drained.
	var emptyMsg struct {
		Empty bool `json:"empty"`
	}
	emptyMsg.Empty = true
	raw, _ := json.Marshal(emptyMsg)
	if !w.handleEvidence(context.Background(), channel.Envelope{Type: typeQueueState, Payload: raw}) {
		t.Fatal("handleEvidence did not consume queue-state empty=true")
	}
	if !w.idle() {
		t.Fatal("idle() must be true once len(w.live) == 0 and server queue is empty")
	}
	pushed, err = w.ev.PushIfIdle(context.Background())
	if err != nil || !pushed {
		t.Fatalf("PushIfIdle = (%v, %v), want (true, nil) after server queue emptied", pushed, err)
	}
	if snd.count() != 1 {
		t.Fatalf("sent %d envelopes, want 1", snd.count())
	}
}
