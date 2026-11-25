package registry

import "testing"

func TestSpecsArePersistedAndCopied(t *testing.T) {
	r := New()
	err := r.Add(Machine{
		ID: "e1", Role: RoleMonitor, ConnectMode: ConnectModeAgentDial, Provisioned: true,
		EnrolledVia: EnrolledToken, Stopped: true,
		Specs: &Specs{Hostname: "h", OS: "linux", Arch: "amd64", CPUs: 4, MemBytes: 1 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A returned copy does not alias the stored specs.
	got, _ := r.Get("e1")
	got.Specs.CPUs = 99
	if again, _ := r.Get("e1"); again.Specs.CPUs != 4 {
		t.Error("Get returned the stored Specs, not a copy")
	}

	dir := t.TempDir()
	st := NewStore(dir)
	if err := st.Save(r); err != nil {
		t.Fatal(err)
	}
	loaded, err := st.Load()
	if err != nil {
		t.Fatal(err)
	}
	m, ok := loaded.Get("e1")
	if !ok || m.Specs == nil || m.Specs.CPUs != 4 || m.Specs.MemBytes != 1<<30 || m.EnrolledVia != EnrolledToken || !m.Stopped {
		t.Errorf("reloaded machine = %+v specs %+v", m, m.Specs)
	}
	// An SSH-onboarded machine has no specs and still round-trips.
	if err := r.Add(Machine{ID: "s1", Role: RoleWorker, PrivateIPs: []string{"10.0.0.2"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(r); err != nil {
		t.Fatal(err)
	}
	if loaded, err = st.Load(); err != nil {
		t.Fatal(err)
	}
	if m, _ := loaded.Get("s1"); m.Specs != nil || m.EnrolledVia != "" {
		t.Errorf("ssh machine = %+v", m)
	}
}
