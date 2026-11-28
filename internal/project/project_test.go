package project

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestStorePersistenceAndCompletedByDate(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	wantPath := filepath.Join(dir, "projects", "projects.json")
	if st.Path() != wantPath {
		t.Fatalf("Path() = %q, want %q", st.Path(), wantPath)
	}

	cur := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	st.SetNowForTest(func() time.Time { return cur })

	p, err := st.CreateProject(ProjectSpec{
		Name:        "Acme Engagement",
		Description: "Load & failover validation",
		Metadata:    map[string]string{"customer": "Acme Corp", "env": "staging"},
	}, "alice")
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	if p.ID != "acme-engagement" || p.Metadata["customer"] != "Acme Corp" {
		t.Fatalf("unexpected project: %+v", p)
	}

	// Create two tests across two days.
	_, t1, err := st.CreateTest(p.ID, TestSpec{
		Name:         "Spike run 1",
		ManeuverID:   "mv-web",
		ManeuverName: "Web spike",
		Target:       TargetRef{IP: "198.51.100.10", Origin: "https://shop.example.test"},
		Agents:       []string{"w1", "w2"},
	}, "alice")
	if err != nil {
		t.Fatalf("CreateTest t1: %v", err)
	}
	if t1.Status != StatusPlanned {
		t.Fatalf("t1 status = %q, want planned", t1.Status)
	}

	_, _, err = st.BeginStart(p.ID, t1.ID, StartParams{
		Monitor: MonitorRef{JobID: "mj-0001", HistoryKey: "198.51.100.10|https://shop.example.test:443", Mode: MonitorModeAttached},
		Actor:   "alice",
	})
	if err != nil {
		t.Fatalf("BeginStart t1: %v", err)
	}
	_, _, err = st.AttachRun(p.ID, t1.ID, "m-run-1", []string{"w1", "w2"})
	if err != nil {
		t.Fatalf("AttachRun t1: %v", err)
	}
	cur = cur.Add(5 * time.Minute)
	_, t1Done, err := st.FinishTest(p.ID, t1.ID, StatusCompleted, OutcomeCompleted, "", "alice", cur)
	if err != nil {
		t.Fatalf("FinishTest t1: %v", err)
	}
	if t1Done.ResultPointers == nil || t1Done.ResultPointers.Evidence == nil || t1Done.ResultPointers.Monitor == nil {
		t.Fatalf("missing result pointers: %+v", t1Done)
	}
	if t1Done.ResultPointers.Evidence.ManeuverRunID != "m-run-1" || t1Done.ResultPointers.Monitor.JobID != "mj-0001" {
		t.Fatalf("unexpected result pointers: %+v", t1Done.ResultPointers)
	}

	// Day 2 failed test.
	cur = cur.Add(24 * time.Hour)
	_, t2, err := st.CreateTest(p.ID, TestSpec{
		Name:       "Spike run 2",
		ManeuverID: "mv-web",
	}, "bob")
	if err != nil {
		t.Fatalf("CreateTest t2: %v", err)
	}
	_, _, err = st.BeginStart(p.ID, t2.ID, StartParams{
		Monitor: MonitorRef{JobID: "mj-0001", HistoryKey: "198.51.100.10|https://shop.example.test:443", Mode: MonitorModeAttached},
		Actor:   "bob",
	})
	if err != nil {
		t.Fatalf("BeginStart t2: %v", err)
	}
	cur = cur.Add(30 * time.Second)
	if _, _, err := st.FinishTest(p.ID, t2.ID, StatusFailed, OutcomeAborted, "w2 missed T", "", cur); err != nil {
		t.Fatalf("FinishTest t2: %v", err)
	}

	// Re-open from disk and verify persistence + CompletedByDate ordering.
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}
	gotProj, ok := st2.Get(p.ID)
	if !ok {
		t.Fatalf("project missing after reopen")
	}
	if len(gotProj.Tests) != 2 {
		t.Fatalf("got %d tests after reopen, want 2", len(gotProj.Tests))
	}
	if _, active := st2.ActiveLock(); active {
		t.Fatalf("expected no active lock after both tests finished")
	}

	groups := CompletedByDate(gotProj.Tests)
	if len(groups) != 2 {
		t.Fatalf("CompletedByDate groups = %d, want 2", len(groups))
	}
	if groups[0].Date != "2026-11-21" || groups[0].Tests[0].ID != t2.ID {
		t.Fatalf("newest group = %+v, want 2026-11-21 / %s", groups[0], t2.ID)
	}
	if groups[1].Date != "2026-11-20" || groups[1].Tests[0].ID != t1.ID {
		t.Fatalf("older group = %+v, want 2026-11-20 / %s", groups[1], t1.ID)
	}
}

func TestSingleActiveTestLockAndConcurrency(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	p1, _ := st.CreateProject(ProjectSpec{Name: "Project A"}, "op")
	p2, _ := st.CreateProject(ProjectSpec{Name: "Project B"}, "op")
	_, t1, _ := st.CreateTest(p1.ID, TestSpec{Name: "Test A1", ManeuverID: "mv1"}, "op")
	_, t2, _ := st.CreateTest(p2.ID, TestSpec{Name: "Test B1", ManeuverID: "mv2"}, "op")

	// Race 8 concurrent BeginStart calls across p1/t1 and p2/t2: exactly one must win.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	conflicts := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pid, tid := p1.ID, t1.ID
			if i%2 == 1 {
				pid, tid = p2.ID, t2.ID
			}
			_, _, err := st.BeginStart(pid, tid, StartParams{
				Monitor: MonitorRef{JobID: "mj-0001", Mode: MonitorModeAttached},
				Actor:   "op",
			})
			mu.Lock()
			if err == nil {
				wins++
			} else if errors.Is(err, ErrActiveTestConflict) {
				conflicts++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if wins != 1 || conflicts != 7 {
		t.Fatalf("concurrent BeginStart: wins=%d conflicts=%d, want 1 win and 7 conflicts", wins, conflicts)
	}

	lock, ok := st.ActiveLock()
	if !ok {
		t.Fatal("expected active lock")
	}

	// Reopening from disk preserves the active lock.
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-open with lock: %v", err)
	}
	if _, ok := st2.ActiveLock(); !ok {
		t.Fatal("active lock lost across reopen")
	}
	// Deleting the project holding the lock is refused.
	if err := st2.DeleteProject(lock.ProjectID); !errors.Is(err, ErrActiveTestConflict) {
		t.Fatalf("DeleteProject while active = %v, want ErrActiveTestConflict", err)
	}
	// Finishing releases the lock.
	if _, _, err := st2.FinishTest(lock.ProjectID, lock.TestID, StatusCompleted, OutcomeStopped, "", "op", time.Now()); err != nil {
		t.Fatalf("FinishTest: %v", err)
	}
	if _, ok := st2.ActiveLock(); ok {
		t.Fatal("expected lock released after FinishTest")
	}
}
