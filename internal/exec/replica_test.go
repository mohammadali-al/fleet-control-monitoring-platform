package exec

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestReplicaSessionAttribution(t *testing.T) {
	var (
		mu       sync.Mutex
		started  []SessionInfo
		finished []SessionInfo
	)
	te := newEnv(t, func(c *Config) {
		c.OnSessionStart = func(s *Session) {
			mu.Lock()
			started = append(started, s.Info())
			mu.Unlock()
		}
		c.OnSessionEnd = func(info SessionInfo) {
			mu.Lock()
			finished = append(finished, info)
			mu.Unlock()
		}
	})

	sessID := "mr-test-replica-1"
	te.ex.RunSync(RunPayload{
		SessionID:  sessID,
		Command:    "echo replica-ok",
		User:       "maneuver",
		Mode:       ModeReplica,
		DurationMS: 1500,
		ManeuverID: "mv-web",
		RunID:      "m-run1",
		ItemID:     "item-1",
		Replica:    2,
	})

	mu.Lock()
	defer mu.Unlock()
	if len(started) != 1 || len(finished) != 1 {
		t.Fatalf("started=%d finished=%d, want 1/1", len(started), len(finished))
	}
	got := finished[0]
	if got.ID != sessID || got.Mode != ModeReplica || got.ManeuverID != "mv-web" || got.RunID != "m-run1" || got.ItemID != "item-1" || got.Replica != 2 {
		t.Fatalf("session info = %+v", got)
	}
	if !got.Finished || got.ExitCode != 0 {
		t.Fatalf("expected clean exit, got %+v", got)
	}
}

func TestReplicaRunsInSessionCgroup(t *testing.T) {
	root := testCgroupRoot(t)
	var hookPath string
	var hookSessID string
	te := newEnv(t, func(c *Config) {
		c.CgroupRoot = root
		c.OnSessionStart = func(s *Session) {
			hookPath = s.CgroupPath()
			hookSessID = s.ID
		}
	})

	sessID := "mr-cg-replica-1"
	cgFile := filepath.Join(te.dir, "cgroup.txt")
	te.ex.RunSync(RunPayload{
		SessionID:  sessID,
		Command:    "cat /proc/self/cgroup > " + cgFile,
		User:       "maneuver",
		Mode:       ModeReplica,
		DurationMS: 2000,
		ManeuverID: "mv-cg",
		RunID:      "m-run-cg",
		ItemID:     "item-1",
		Replica:    1,
	})

	want := filepath.Join(root, sessID)
	if hookSessID != sessID || hookPath != want {
		t.Fatalf("hook=(%q, %q), want (%q, %q)", hookSessID, hookPath, sessID, want)
	}
	b, err := os.ReadFile(cgFile)
	if err != nil {
		t.Fatalf("read cgroup output: %v", err)
	}
	if !strings.Contains(string(b), "/"+filepath.Base(root)+"/"+sessID) {
		t.Errorf("replica did not run inside tracked cgroup; got=%q", string(b))
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("replica cgroup %s not cleaned up (err=%v)", want, err)
	}
}
