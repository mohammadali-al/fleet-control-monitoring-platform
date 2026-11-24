package watchdog

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseYAMLSubset(t *testing.T) {
	src := `---
# a comment
version: 3          # trailing comment
name: "quoted # not a comment"
single: 'it''s'
ratio: 0.5
neg: -4
on: true
off: False
nothing: ~
dur: 20s
list:
  - a.service
  - "b c"
same_indent_list:
- x
- y
flow: [1, two, "three, 3"]
pair: {trigger: 97, recover: 92.5}
nested:
  deeper:
    key: value
  other: [ ]
empty:
`
	got, err := parseYAML([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": int64(3), "name": "quoted # not a comment", "single": "it's",
		"ratio": 0.5, "neg": int64(-4), "on": true, "off": false, "nothing": nil, "dur": "20s",
		"list":             []any{"a.service", "b c"},
		"same_indent_list": []any{"x", "y"},
		"flow":             []any{int64(1), "two", "three, 3"},
		"pair":             map[string]any{"trigger": int64(97), "recover": 92.5},
		"nested":           map[string]any{"deeper": map[string]any{"key": "value"}, "other": []any{}},
		"empty":            nil,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed:\n got  %#v\n want %#v", got, want)
	}
}

func TestParseYAMLRejectsWhatItDoesNotSupport(t *testing.T) {
	for name, src := range map[string]string{
		"empty":             "",
		"only comments":     "# nothing\n",
		"tab indent":        "a:\n\tb: 1\n",
		"duplicate key":     "a: 1\na: 2\n",
		"bad indent":        "a: 1\n  b: 2\n",
		"top-level list":    "- a\n- b\n",
		"no colon":          "just text\n",
		"anchor":            "a: &x 1\n",
		"alias":             "a: *x\n",
		"tag":               "a: !!str 1\n",
		"block scalar":      "a: |\n  text\n",
		"multi doc":         "a: 1\n---\nb: 2\n",
		"list of maps":      "a:\n  - k: v\n",
		"unterminated flow": "a: [1, 2\n",
		"unterminated map":  "a: {x: 1\n",
		"dup flow key":      "a: {x: 1, x: 2}\n",
		"bad quote":         "a: \"open\n",
		"trailing flow":     "a: [1] junk\n",
		"indented top":      "  a: 1\n",
	} {
		if _, err := parseYAML([]byte(src)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestDefaultPolicyIsValidWithFourSampleSustain(t *testing.T) {
	p := DefaultPolicy()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if p.SampleInterval.D() != 5*time.Second || p.Sustain.D() != 20*time.Second || p.SustainSamples() != 4 {
		t.Fatalf("want 5 s / 20 s / 4 samples, got %v / %v / %d", p.SampleInterval.D(), p.Sustain.D(), p.SustainSamples())
	}
	if p.Throttle.Duration.D() != 120*time.Second || p.Caps.RestartsPerDay != 3 || p.Caps.RebootsPerDay != 1 || p.BootWindow.D() != 5*time.Minute {
		t.Fatalf("defaults differ from the design: %+v", p)
	}
	c := p.Cooldowns
	if c.Throttle.D() != 10*time.Minute || c.Restart.D() != 30*time.Minute || c.Reboot.D() != 60*time.Minute {
		t.Fatalf("cooldowns: %+v", c)
	}
	// The baked-in profile resolves for any host.
	if got, err := DefaultProfile().Effective("any", nil); err != nil || got.Version != 0 {
		t.Fatalf("default profile: %+v %v", got, err)
	}
}

func TestParseProfileValidation(t *testing.T) {
	ok := "version: 1\n"
	if _, err := ParseProfile([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for name, src := range map[string]string{
		"zero version":        "version: 0\n",
		"float version":       "version: 1.5\n",
		"unknown override":    "version: 1\noverrides:\n  zones:\n    a: {}\n",
		"override not map":    "version: 1\noverrides: 3\n",
		"bad unit name":       "version: 1\nservices:\n  \"-rf\":\n    cpu_percent: {trigger: 50, recover: 40}\n",
		"recover zero":        "version: 1\nservices:\n  a.service:\n    cpu_percent: {trigger: 50, recover: 0}\n",
		"percent above 100":   "version: 1\nservices:\n  a.service:\n    cpu_percent: {trigger: 150, recover: 40}\n",
		"memory below ceil":   "version: 1\nmachine:\n  memory_percent: {trigger: 90, recover: 80}\n",
		"recover below ceil":  "version: 1\nmachine:\n  cpu_percent: {trigger: 99, recover: 85}\n",
		"zero throttle quota": "version: 1\nthrottle: {cpu_percent: 0}\n",
		"negative cap":        "version: 1\ncaps: {restarts_per_day: -1}\n",
		"bad share":           "version: 1\nattribution_share: 2\n",
		"relative disk path":  "version: 1\ndisk_path: var\n",
		"agent unit is self":  "version: 1\nagent_unit: fleet-watchdog.service\n",
		"agent unit flag":     "version: 1\nagent_unit: --now\n",
		"tiny interval":       "version: 1\nsample_interval: 10ms\nsustain: 20ms\n",
		"override bad value":  "version: 1\noverrides:\n  groups:\n    g:\n      caps: {restarts_per_day: -2}\n",
		"host sets overrides": "version: 1\noverrides:\n  hosts:\n    h:\n      overrides: {}\n",
	} {
		if _, err := ParseProfile([]byte(src)); err == nil {
			t.Errorf("%s: want a validation error", name)
		}
	}
	// Raising the ceiling lets a higher trigger be configured, and lowering the
	// trigger below it is refused: thresholds are tied to the Load Controller.
	if _, err := ParseProfile([]byte("version: 1\nload_controller_ceiling: {cpu_percent: 80}\nmachine:\n  cpu_percent: {trigger: 85, recover: 82}\n")); err != nil {
		t.Errorf("trigger above a lower ceiling must be accepted: %v", err)
	}
}

func TestEffectiveLayeringOrder(t *testing.T) {
	pf, err := ParseProfile([]byte(`version: 2
caps: {restarts_per_day: 3}
overrides:
  groups:
    a:
      caps: {restarts_per_day: 4}
    b:
      caps: {restarts_per_day: 5}
  hosts:
    h1:
      caps: {restarts_per_day: 6}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		host   string
		groups []string
		want   int
	}{
		{"zz", nil, 3},
		{"zz", []string{"a"}, 4},
		{"zz", []string{"a", "b"}, 5}, // later group wins
		{"zz", []string{"b", "a"}, 4},
		{"h1", []string{"a", "b"}, 6}, // host beats groups
		{"zz", []string{"unknown"}, 3},
	} {
		p, err := pf.Effective(c.host, c.groups)
		if err != nil || p.Caps.RestartsPerDay != c.want || p.Version != 2 {
			t.Errorf("%s %v: got %d (%v), want %d", c.host, c.groups, p.Caps.RestartsPerDay, err, c.want)
		}
	}
	hosts, groups := pf.Names()
	if strings.Join(hosts, ",") != "h1" || strings.Join(groups, ",") != "a,b" {
		t.Errorf("names: %v %v", hosts, groups)
	}
}

func TestDetectorHysteresisUnit(t *testing.T) {
	d := &detector{}
	th := Threshold{Trigger: 80, Recover: 60}
	seq := []struct {
		v    float64
		want string
	}{
		{90, ""}, {90, ""}, {90, ""}, {90, EventBreach}, // 4th consecutive sample confirms
		{70, ""}, {85, ""}, // band and above: still active, no new event
		{59, EventRecovered},                                                // below recovery clears
		{90, ""}, {70, ""}, {90, ""}, {90, ""}, {90, ""}, {90, EventBreach}, // run restarted from the dip
	}
	for i, s := range seq {
		if got := d.update(s.v, th, 4); got != s.want {
			t.Fatalf("step %d (v=%v): got %q want %q", i, s.v, got, s.want)
		}
	}
}
