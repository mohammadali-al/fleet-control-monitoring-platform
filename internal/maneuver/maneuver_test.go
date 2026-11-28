package maneuver

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/baseline"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
)

func TestValidateTargetAndPattern(t *testing.T) {
	t.Run("target validation", func(t *testing.T) {
		got, err := ValidateTarget(Target{IP: " 198.51.100.10 ", Origin: "HTTPS://Shop.Example.Test:8443/"})
		if err != nil {
			t.Fatalf("ValidateTarget unexpected err: %v", err)
		}
		if got.IP != "198.51.100.10" || got.Origin != "https://Shop.Example.Test:8443" {
			t.Fatalf("got %+v", got)
		}

		// IP without origin is valid (origin is optional).
		got2, err := ValidateTarget(Target{IP: "2001:db8::1"})
		if err != nil || got2.IP != "2001:db8::1" || got2.Origin != "" {
			t.Fatalf("ipv6 no origin = %+v, err=%v", got2, err)
		}

		bad := []Target{
			{IP: ""},
			{IP: "not-an-ip"},
			{IP: "198.51.100.1,198.51.100.2"},
			{IP: "198.51.100.10", Origin: "ftp://example.test"},
			{IP: "198.51.100.10", Origin: "https://example.test/path"},
			{IP: "198.51.100.10", Origin: "https://example.test?a=1"},
			{IP: "198.51.100.10", Origin: "https://a.test,https://b.test"},
		}
		for _, tc := range bad {
			if _, err := ValidateTarget(tc); !errors.Is(err, ErrInvalidManeuver) {
				t.Errorf("ValidateTarget(%+v) = %v, want ErrInvalidManeuver", tc, err)
			}
		}
	})

	t.Run("pattern validation defers linear and spikes", func(t *testing.T) {
		p, err := ValidatePattern(PatternConfig{})
		if err != nil || p.Type != PatternAllInOne {
			t.Fatalf("empty pattern = %+v, err=%v", p, err)
		}
		for _, deferred := range []string{PatternLinear, PatternSpikes} {
			if _, err := ValidatePattern(PatternConfig{Type: deferred}); !errors.Is(err, ErrPatternDeferred) {
				t.Errorf("ValidatePattern(%q) = %v, want ErrPatternDeferred", deferred, err)
			}
		}
		if _, err := ValidatePattern(PatternConfig{Type: "burst"}); !errors.Is(err, ErrInvalidManeuver) {
			t.Errorf("unknown pattern should return ErrInvalidManeuver")
		}
	})
}

func TestStoreCRUDAndPersistence(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	created, err := st.Create(Spec{
		Name:     "Web tier probe",
		Target:   Target{IP: "198.51.100.10", Origin: "https://shop.example.test"},
		TotalSec: 300,
		Pattern:  PatternConfig{Type: PatternAllInOne, MaxCPUCores: 4, MaxRAMGB: 6},
		Timeline: []TimelineItem{
			{BaselineID: "http-get", StartSec: 0, DurationSec: 60, Replicas: 2, Inputs: map[string]string{"rate": "200"}},
		},
		Agents: []string{"w1", "w2", "w1"},
	}, "alice")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "web-tier-probe" || !reflect.DeepEqual(created.Agents, []string{"w1", "w2"}) {
		t.Fatalf("created = %+v", created)
	}
	if st.Path() != filepath.Join(dir, config.ProjectsDirName, config.ManeuversFileName) {
		t.Fatalf("unexpected path %s", st.Path())
	}

	reloaded, err := Open(dir)
	if err != nil {
		t.Fatalf("reload Open: %v", err)
	}
	got, ok := reloaded.Get(created.ID)
	if !ok || got.Name != "Web tier probe" || len(got.Timeline) != 1 || got.Timeline[0].Replicas != 2 {
		t.Fatalf("reloaded = %+v, ok=%v", got, ok)
	}

	dup, err := reloaded.Duplicate(created.ID, "", "bob")
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if dup.ID == created.ID || dup.Name != "Web tier probe (copy)" {
		t.Fatalf("dup = %+v", dup)
	}

	updated, err := reloaded.Update(created.ID, Spec{
		Name:     "Web tier probe v2",
		Target:   Target{IP: "198.51.100.20"},
		TotalSec: 120,
		Agents:   []string{"w1"},
	}, "bob")
	if err != nil || updated.TotalSec != 120 || updated.Target.IP != "198.51.100.20" {
		t.Fatalf("Update = %+v, err=%v", updated, err)
	}

	if err := reloaded.Delete(dup.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(reloaded.List()) != 1 {
		t.Fatalf("List len = %d, want 1", len(reloaded.List()))
	}
}

func TestMaterializeWithBaseline(t *testing.T) {
	bStore, err := baseline.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	item, err := bStore.Create(baseline.ItemSpec{
		ID:              "http-get",
		Name:            "HTTP GET",
		CommandTemplate: "hey -z {duration}s -q {rate} -H 'Host: {ip}' {url}",
		Defaults:        map[string]string{"rate": "100"},
	}, "alice")
	if err != nil {
		t.Fatal(err)
	}

	plan, err := Materialize(Spec{
		Name:     "Probe",
		Target:   Target{IP: "198.51.100.10", Origin: "https://shop.example.test"},
		TotalSec: 120,
		Timeline: []TimelineItem{
			{ID: "step-1", BaselineID: item.ID, StartSec: 10, DurationSec: 45, Replicas: 3, Inputs: map[string]string{"path": "/cart", "rate": "250"}},
		},
		Agents: []string{"w1"},
	}, bStore)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	wantCmd := "hey -z 45s -q 250 -H 'Host: 198.51.100.10' https://shop.example.test/cart"
	if len(plan.Items) != 1 || plan.Items[0].Command != wantCmd || plan.Items[0].Replicas != 3 {
		t.Fatalf("plan = %+v, want cmd %q", plan, wantCmd)
	}

	// Reject overriding target ip per item.
	_, err = Materialize(Spec{
		Name:     "Override",
		Target:   Target{IP: "198.51.100.10"},
		TotalSec: 60,
		Timeline: []TimelineItem{
			{BaselineID: item.ID, StartSec: 0, DurationSec: 30, Inputs: map[string]string{"ip": "10.0.0.1", "url": "http://10.0.0.1"}},
		},
	}, bStore)
	if !errors.Is(err, ErrInvalidManeuver) {
		t.Fatalf("override ip err = %v, want ErrInvalidManeuver", err)
	}

	// Unresolved {url} when origin is omitted and url input is missing.
	_, err = Materialize(Spec{
		Name:     "Missing URL",
		Target:   Target{IP: "198.51.100.10"},
		TotalSec: 60,
		Timeline: []TimelineItem{
			{BaselineID: item.ID, StartSec: 0, DurationSec: 30},
		},
	}, bStore)
	if !errors.Is(err, baseline.ErrUnresolvedPlaceholder) {
		t.Fatalf("missing url err = %v, want ErrUnresolvedPlaceholder", err)
	}
}
