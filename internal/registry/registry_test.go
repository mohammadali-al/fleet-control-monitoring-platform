package registry

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAddDefaultsConnectMode(t *testing.T) {
	r := New()
	if err := r.Add(Machine{ID: "m1", Role: RoleWorker, PrivateIPs: []string{"10.0.0.5"}}); err != nil {
		t.Fatal(err)
	}
	m, ok := r.Get("m1")
	if !ok {
		t.Fatal("m1 missing")
	}
	if m.ConnectMode != ConnectModeServerDial {
		t.Fatalf("connect_mode = %q, want server_dial", m.ConnectMode)
	}
	if m.CreatedAt.IsZero() || m.UpdatedAt.IsZero() {
		t.Fatal("timestamps not set")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]Machine{
		"bad role":        {ID: "a", Role: "boss", ConnectMode: ConnectModeServerDial},
		"empty role":      {ID: "a", ConnectMode: ConnectModeServerDial},
		"bad mode":        {ID: "a", Role: RoleWorker, ConnectMode: "carrier_pigeon"},
		"empty id":        {Role: RoleWorker, ConnectMode: ConnectModeServerDial},
		"id with space":   {ID: "a b", Role: RoleWorker, ConnectMode: ConnectModeServerDial},
		"id with slash":   {ID: "../x", Role: RoleWorker, ConnectMode: ConnectModeServerDial},
		"bad ip":          {ID: "a", Role: RoleWorker, ConnectMode: ConnectModeServerDial, PrivateIPs: []string{"nope"}},
		"bad wd stage":    {ID: "a", Role: RoleWorker, ConnectMode: ConnectModeServerDial, WatchdogState: &WatchdogState{Stage: "explode"}},
		"neg wd count":    {ID: "a", Role: RoleWorker, ConnectMode: ConnectModeServerDial, WatchdogState: &WatchdogState{RestartsToday: -1}},
		"empty group":     {ID: "a", Role: RoleWorker, ConnectMode: ConnectModeServerDial, Groups: []string{""}},
		"bad group slash": {ID: "a", Role: RoleWorker, ConnectMode: ConnectModeServerDial, Groups: []string{"prod/edge"}},
		"bad group space": {ID: "a", Role: RoleWorker, ConnectMode: ConnectModeServerDial, Groups: []string{"my group"}},
		"dup group":       {ID: "a", Role: RoleWorker, ConnectMode: ConnectModeServerDial, Groups: []string{"edge", "edge"}},
	}
	for name, m := range cases {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, m)
		}
	}
	good := Machine{ID: "a-1.x_y", Role: RoleMonitor, ConnectMode: ConnectModeAgentDial, PrivateIPs: []string{"10.0.0.1", "fe80::1"}}
	if err := good.Validate(); err != nil {
		t.Fatalf("good machine rejected: %v", err)
	}
}

func TestDuplicateID(t *testing.T) {
	r := New()
	if err := r.Add(Machine{ID: "m1", Role: RoleWorker}); err != nil {
		t.Fatal(err)
	}
	err := r.Add(Machine{ID: "m1", Role: RoleMonitor})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate add: err = %v", err)
	}
	err = Validate([]Machine{
		{ID: "x", Role: RoleWorker, ConnectMode: ConnectModeServerDial},
		{ID: "x", Role: RoleWorker, ConnectMode: ConnectModeServerDial},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("Validate duplicate: err = %v", err)
	}
	if err := r.Add(Machine{ID: "bad", Role: "boss"}); err == nil {
		t.Fatal("Add accepted bad role")
	}
}

func TestUpdateRemoveSetOnline(t *testing.T) {
	r := New()
	_ = r.Add(Machine{ID: "m1", Role: RoleWorker, PrivateIPs: []string{"10.0.0.5"}})
	before, _ := r.Get("m1")
	time.Sleep(2 * time.Millisecond)

	if err := r.Update("m1", func(m *Machine) { m.Role = RoleMonitor }); err != nil {
		t.Fatal(err)
	}
	m, _ := r.Get("m1")
	if m.Role != RoleMonitor || !m.UpdatedAt.After(before.UpdatedAt) {
		t.Fatalf("update not applied: %+v", m)
	}
	if err := r.Update("m1", func(m *Machine) { m.Role = "bad" }); err == nil {
		t.Fatal("update to bad role accepted")
	}
	if err := r.Update("m1", func(m *Machine) { m.ID = "other" }); err == nil {
		t.Fatal("id change accepted")
	}
	if got, _ := r.Get("m1"); got.Role != RoleMonitor {
		t.Fatal("failed update modified the machine")
	}
	if err := r.Update("nope", func(*Machine) {}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	if err := r.SetOnline("m1", true); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("m1")
	if !got.Online {
		t.Fatal("online not set")
	}
	if !got.UpdatedAt.Equal(m.UpdatedAt) {
		t.Fatal("SetOnline must not bump UpdatedAt")
	}
	if err := r.SetOnline("nope", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	if err := r.Remove("m1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get("m1"); ok {
		t.Fatal("still present")
	}
	if err := r.Remove("m1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestGetReturnsCopy(t *testing.T) {
	r := New()
	_ = r.Add(Machine{ID: "m1", Role: RoleWorker, PrivateIPs: []string{"10.0.0.5"}})
	m, _ := r.Get("m1")
	m.PrivateIPs[0] = "99.99.99.99"
	again, _ := r.Get("m1")
	if again.PrivateIPs[0] != "10.0.0.5" {
		t.Fatal("Get leaked internal slice")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := New()
	_ = r.Add(Machine{ID: "m0", Role: RoleWorker})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.SetOnline("m0", j%2 == 0)
				_ = r.Update("m0", func(m *Machine) { m.PublicIP = "1.2.3.4" })
				_ = r.List()
			}
		}(i)
	}
	wg.Wait()
}

func sample() *Registry {
	r := New()
	now := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)
	_ = r.Add(Machine{
		ID: "w-1", Role: RoleWorker, PrivateIPs: []string{"10.0.0.5", "10.0.1.5"},
		PublicIP: "203.0.113.9", PublicIPAt: &now, CertFingerprint: "abc123", SSHHostKey: "SHA256:xyz",
		Disk:   DiskState{Status: "ok", TotalBytes: 100, FreeBytes: 40, UpdatedAt: &now},
		Groups: []string{"edge", "gpu-1.a_b"},
		WatchdogState: &WatchdogState{
			Day: "2026-11-24", Stage: "restart", RestartsToday: 2, RebootsToday: 1, AlertOnly: true,
			Breaches: []string{"machine/cpu_percent"}, Throttled: []string{"session s-1"}, Events: 4,
			LastEvent: &WatchdogEventRecord{Machine: "w-1", Time: now, Kind: "alert-only", Step: "reboot", Received: now},
		},
		Provisioned: true, CreatedAt: now, UpdatedAt: now,
	})
	_ = r.Add(Machine{ID: "mon-1", Role: RoleMonitor, ConnectMode: ConnectModeAgentDial, CreatedAt: now, UpdatedAt: now})
	return r
}

func TestStoreRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	st := NewStore(dataDir)
	r := sample()
	_ = r.SetOnline("w-1", true) // derived, must not persist
	if err := st.Save(r); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dataDir, "registry", "machines.json"); st.Path() != want {
		t.Fatalf("path = %s, want %s", st.Path(), want)
	}
	got, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	want := r.List()
	for i := range want {
		want[i].Online = false
	}
	if !reflect.DeepEqual(got.List(), want) {
		t.Fatalf("round trip differs:\n got %+v\nwant %+v", got.List(), want)
	}
	raw, _ := os.ReadFile(st.Path())
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("file is not JSON: %v", err)
	}
	if strings.Contains(string(raw), "online") {
		t.Fatal("online state was persisted")
	}
	info, _ := os.Stat(st.Path())
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode().Perm())
	}
	assertNoTemp(t, st.Dir())
}

func TestLoadMissingIsEmpty(t *testing.T) {
	r, err := NewStore(t.TempDir()).Load()
	if err != nil || len(r.List()) != 0 {
		t.Fatalf("r=%v err=%v", r, err)
	}
}

func TestLoadRejectsCorruptAndInvalid(t *testing.T) {
	dir := t.TempDir()
	st := NewStore(dir)
	_ = os.MkdirAll(st.Dir(), 0o750)
	for name, body := range map[string]string{
		"corrupt":   `{"version":1,"machines":[`,
		"dup":       `{"version":1,"machines":[{"id":"a","role":"worker","connect_mode":"server_dial"},{"id":"a","role":"worker","connect_mode":"server_dial"}]}`,
		"bad role":  `{"version":1,"machines":[{"id":"a","role":"x","connect_mode":"server_dial"}]}`,
		"bad ver":   `{"version":9,"machines":[]}`,
		"no config": `{"version":1,"machines":[{"id":"a","role":"worker"}]}`,
	} {
		_ = os.WriteFile(st.Path(), []byte(body), 0o600)
		if _, err := st.Load(); err == nil {
			t.Errorf("%s: Load accepted", name)
		}
	}
}

func TestAtomicWriteLeavesNoPartialOnFailure(t *testing.T) {
	st := NewStore(t.TempDir())
	r := sample()
	if err := st.Save(r); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(st.Path())

	// Fail at the last step on an existing file: the old content must survive.
	st.rename = func(string, string) error { return errors.New("simulated crash") }
	_ = r.Add(Machine{ID: "extra", Role: RoleWorker})
	if err := st.Save(r); err == nil {
		t.Fatal("Save should have failed")
	}
	after, _ := os.ReadFile(st.Path())
	if string(after) != string(original) {
		t.Fatal("failed save changed the existing file")
	}
	assertNoTemp(t, st.Dir())

	// Fail on a first-ever save: no file at all, no temp leftovers.
	fresh := NewStore(t.TempDir())
	fresh.rename = st.rename
	if err := fresh.Save(r); err == nil {
		t.Fatal("Save should have failed")
	}
	if _, err := os.Stat(fresh.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial file exists: %v", err)
	}
	assertNoTemp(t, fresh.Dir())
}

func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}
}

func TestGroupsCloneUpdateAndOmitEmpty(t *testing.T) {
	r := New()
	if err := r.Add(Machine{ID: "m1", Role: RoleWorker, Groups: []string{"edge"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := r.Get("m1")
	got.Groups[0] = "mutated"
	again, _ := r.Get("m1")
	if len(again.Groups) != 1 || again.Groups[0] != "edge" {
		t.Fatalf("Get leaked Groups slice: %+v", again.Groups)
	}
	if err := r.Update("m1", func(m *Machine) { m.Groups = []string{"bad/group"} }); err == nil {
		t.Fatal("Update accepted invalid group")
	}
	if err := r.Update("m1", func(m *Machine) { m.Groups = []string{} }); err != nil {
		t.Fatal(err)
	}
	st := NewStore(t.TempDir())
	if err := st.Save(r); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(st.Path())
	if strings.Contains(string(raw), `"groups"`) {
		t.Fatalf("empty groups should be omitted from JSON:\n%s", raw)
	}
}

func TestAgentVersionPersistence(t *testing.T) {
	r := New()
	if err := r.Add(Machine{ID: "m1", Role: RoleWorker, AgentVersion: "1.2.3"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Update("m1", func(m *Machine) { m.AgentVersion = "1.2.4" }); err != nil {
		t.Fatal(err)
	}
	st := NewStore(t.TempDir())
	if err := st.Save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.Get("m1")
	if !ok || got.AgentVersion != "1.2.4" {
		t.Fatalf("loaded AgentVersion = %q (ok=%v), want 1.2.4", got.AgentVersion, ok)
	}
}
