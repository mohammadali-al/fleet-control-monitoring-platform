package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/watchdog"
)

type recSender struct {
	mu   sync.Mutex
	got  []channel.WatchdogEventPayload
	fail error
}

func (r *recSender) Send(_ context.Context, env channel.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	var p channel.WatchdogEventPayload
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return err
	}
	if env.Type != channel.TypeWatchdogEv {
		return errors.New("unexpected envelope type " + env.Type)
	}
	r.got = append(r.got, p)
	return nil
}

func (r *recSender) details() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, p := range r.got {
		out = append(out, p.Detail)
	}
	return out
}

func fwdLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newForwarder(dir string) *eventForwarder {
	return &eventForwarder{dir: watchdogDir(dir), log: fwdLog()}
}

func emit(w *EventWriter, detail string) {
	w.Write(watchdog.Event{Time: time.Now(), Kind: watchdog.EventBreach, Detail: detail})
}

func wantDetails(t *testing.T, s *recSender, want ...string) {
	t.Helper()
	got := s.details()
	if len(got) != len(want) {
		t.Fatalf("forwarded %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("forwarded %v, want %v", got, want)
		}
	}
}

func TestForwarderSendsEachEventOnceAndKeepsBacklogWhileOffline(t *testing.T) {
	dir := t.TempDir()
	w := NewEventWriter(dir, fwdLog())
	f := newForwarder(dir)
	s := &recSender{}
	ctx := context.Background()

	if n, err := f.forward(ctx, s); n != 0 || err != nil {
		t.Fatalf("no file yet: %d, %v", n, err)
	}
	emit(w, "a")
	emit(w, "b")

	s.fail = channel.ErrNotConnected // the server is away
	if _, err := f.forward(ctx, s); !errors.Is(err, channel.ErrNotConnected) {
		t.Fatalf("offline forward: %v", err)
	}
	wantDetails(t, s)

	s.fail = nil
	if n, err := f.forward(ctx, s); n != 2 || err != nil {
		t.Fatalf("after reconnect: %d, %v", n, err)
	}
	wantDetails(t, s, "a", "b")
	if n, _ := f.forward(ctx, s); n != 0 {
		t.Fatalf("resent %d events", n)
	}
	emit(w, "c")
	if n, _ := f.forward(ctx, s); n != 1 {
		t.Fatalf("new event: sent %d", n)
	}
	wantDetails(t, s, "a", "b", "c")
}

func TestForwarderResumesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	w := NewEventWriter(dir, fwdLog())
	s := &recSender{}
	ctx := context.Background()
	emit(w, "a")
	if _, err := newForwarder(dir).forward(ctx, s); err != nil {
		t.Fatal(err)
	}
	emit(w, "b")
	// A new forwarder (agent restart) starts from the persisted cursor.
	if _, err := newForwarder(dir).forward(ctx, s); err != nil {
		t.Fatal(err)
	}
	wantDetails(t, s, "a", "b")
}

func TestForwarderStopsAtFirstFailureWithoutLosingTheRest(t *testing.T) {
	dir := t.TempDir()
	w := NewEventWriter(dir, fwdLog())
	f := newForwarder(dir)
	for _, d := range []string{"a", "b", "c"} {
		emit(w, d)
	}
	flaky := &flakySender{okAfter: 1}
	if n, err := f.forward(context.Background(), flaky); n != 1 || err == nil {
		t.Fatalf("flaky forward: %d, %v; want 1 sent then an error", n, err)
	}
	s := &recSender{}
	if n, err := f.forward(context.Background(), s); n != 2 || err != nil {
		t.Fatalf("retry: %d, %v", n, err)
	}
	wantDetails(t, s, "b", "c")
}

type flakySender struct{ okAfter, n int }

func (f *flakySender) Send(context.Context, channel.Envelope) error {
	f.n++
	if f.n > f.okAfter {
		return channel.ErrNotConnected
	}
	return nil
}

func TestForwarderWaitsForTheNewlineAndSkipsGarbage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(watchdogDir(dir), 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(watchdogDir(dir), eventsFileName)
	good := `{"time":"2026-11-25T09:00:00Z","kind":"breach","detail":"ok"}`
	if err := os.WriteFile(path, []byte("not json\n"+good+"\n"+`{"kind":"unfinis`), 0o640); err != nil {
		t.Fatal(err)
	}
	f := newForwarder(dir)
	s := &recSender{}
	if n, err := f.forward(context.Background(), s); n != 1 || err != nil {
		t.Fatalf("%d, %v", n, err)
	}
	wantDetails(t, s, "ok")
	// The writer finishes the line.
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fh.WriteString(`hed","detail":"late"}` + "\n")
	fh.Close()
	if n, _ := f.forward(context.Background(), s); n != 1 {
		t.Fatalf("finished line: sent %d", n)
	}
	wantDetails(t, s, "ok", "late")
}

func TestForwarderFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	w := NewEventWriter(dir, fwdLog())
	f := newForwarder(dir)
	s := &recSender{}
	ctx := context.Background()
	emit(w, "a")
	if _, err := f.forward(ctx, s); err != nil {
		t.Fatal(err)
	}
	emit(w, "b") // written but not forwarded yet

	// Make the writer rotate on its next event: pad the file past the limit.
	path := filepath.Join(watchdogDir(dir), eventsFileName)
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	pad := make([]byte, eventsMaxBytes)
	for i := range pad {
		pad[i] = ' '
	}
	pad = append(pad, '\n')
	_, _ = fh.Write(pad)
	fh.Close()
	emit(w, "c") // rotates events.jsonl to events.jsonl.1, then starts a fresh file

	if n, err := f.forward(ctx, s); err != nil || n != 2 {
		t.Fatalf("after rotation: %d, %v", n, err)
	}
	wantDetails(t, s, "a", "b", "c")
	emit(w, "d")
	if n, _ := f.forward(ctx, s); n != 1 {
		t.Fatalf("after rotation settled: sent %d", n)
	}
	wantDetails(t, s, "a", "b", "c", "d")
}
