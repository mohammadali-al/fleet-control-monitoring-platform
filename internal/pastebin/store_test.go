package pastebin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRoundTripAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if text, v := s.Get(); text != "" || v != 0 {
		t.Fatalf("fresh pad = %q v%d, want empty v0", text, v)
	}
	v, err := s.Set("hello\nworld", "c1")
	if err != nil || v != 1 {
		t.Fatalf("Set = v%d err=%v, want v1", v, err)
	}
	if v, _ = s.Set("hello again", "c1"); v != 2 {
		t.Fatalf("second Set version = %d, want 2", v)
	}

	got, err := os.ReadFile(filepath.Join(dir, "pastebin", "paste.txt"))
	if err != nil || string(got) != "hello again" {
		t.Fatalf("paste.txt = %q err=%v", got, err)
	}
	left, _ := filepath.Glob(filepath.Join(dir, "pastebin", "*.tmp"))
	if len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := s2.Get(); text != "hello again" {
		t.Fatalf("reopened pad = %q", text)
	}
}

func TestStoreSizeCap(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(strings.Repeat("x", MaxBytes+1), ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize Set err = %v, want ErrTooLarge", err)
	}
	if _, err := s.Set(strings.Repeat("x", MaxBytes), ""); err != nil {
		t.Fatalf("Set at the cap: %v", err)
	}
}

func TestStoreSaveFailureKeepsPad(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set("keep", ""); err != nil {
		t.Fatal(err)
	}
	s.rename = func(string, string) error { return errors.New("boom") }
	if _, err := s.Set("lost", ""); err == nil {
		t.Fatal("Set succeeded despite rename failure")
	}
	if text, v := s.Get(); text != "keep" || v != 1 {
		t.Fatalf("pad after failed save = %q v%d, want keep v1", text, v)
	}
}

func recv(t *testing.T, ch <-chan Frame) Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a frame")
	}
	return Frame{}
}

func TestSubscribeSnapshotAndBroadcast(t *testing.T) {
	s, _ := Open(t.TempDir())
	_, _ = s.Set("one", "")
	ch, cancel := s.Subscribe()
	defer cancel()
	if f := recv(t, ch); f.Text != "one" || f.Version != 1 || f.Client != "" {
		t.Fatalf("snapshot = %+v", f)
	}
	_, _ = s.Set("two", "tab-a")
	if f := recv(t, ch); f.Text != "two" || f.Version != 2 || f.Client != "tab-a" {
		t.Fatalf("broadcast = %+v", f)
	}
	cancel()
	cancel() // idempotent
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after cancel")
	}
}

func TestSlowSubscriberDropped(t *testing.T) {
	s, _ := Open(t.TempDir())
	ch, cancel := s.Subscribe()
	defer cancel()
	for i := 0; i < subscriberBuffer+2; i++ {
		if _, err := s.Set("x", ""); err != nil {
			t.Fatal(err)
		}
	}
	closed := false
	for i := 0; i < subscriberBuffer+4; i++ {
		if _, ok := <-ch; !ok {
			closed = true
			break
		}
	}
	if !closed {
		t.Fatal("lagging subscriber was not dropped")
	}
}
