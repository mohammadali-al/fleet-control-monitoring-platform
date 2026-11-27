package panel

import (
	"bytes"
	"fmt"
	"html/template"
	"io/fs"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// pageSpec describes one panel page. Mock pages still show their static
// mock data and the "Mock data" chip; live pages are wired to the backend.
// Pages with spa: true are served by the React SPA shell; un-ported pages keep
// their legacy server-rendered .html handler during the incremental migration.
type pageSpec struct {
	name string // URL stem and nav key: /<name> (SPA) or /<name>.html (legacy)
	mock bool
	spa  bool
}

// pages is every panel page, in nav order. The default page is the first.
var pages = []pageSpec{
	{"machines", false, true},
	{"projects", true, false},
	{"commands", false, false},
	{"terminal", false, false},
	{"baseline", true, false},
	{"maneuver", true, false},
	{"monitor", false, false},
	{"watchdog", false, false},
	{"evidence", false, false},
	{"backup", false, false},
}

// fragments are the pieces of live pages that the browser re-fetches as data
// changes (GET /fragments/<name>). Each is a named template in
// web/pages/fragments.html, rendered against the same pageData as its page.
var fragments = map[string]string{
	"machines-tiles":  "machines-tiles",
	"machines-rows":   "machines-rows",
	"command-targets": "command-targets",
	"shell-hosts":     "shell-hosts",
}

// pageData is what every template receives.
type pageData struct {
	Page    string // active nav key
	Mock    bool   // the page still shows mock data
	Backend bool   // a live backend is attached

	Machines  []fleet.MachineInfo
	Summary   fleet.Summary
	Workers   []fleet.MachineInfo // registered worker agents (never monitors, never decommissioned)
	Preselect string              // worker to preselect in the shell form (?machine=)
}

// templates is the parsed template set: one *template.Template per page, each
// holding the layout, the shared fragments and that page's "title" and
// "content".
type templates struct {
	byPage map[string]*template.Template
}

func loadTemplates(fsys fs.FS) (*templates, error) {
	t := &templates{byPage: map[string]*template.Template{}}
	for _, p := range pages {
		set, err := template.New(p.name).Funcs(funcs).ParseFS(fsys, "_src/shell.html", "pages/fragments.html", "pages/"+p.name+".html")
		if err != nil {
			return nil, fmt.Errorf("panel: parse templates for %s: %w", p.name, err)
		}
		t.byPage[p.name] = set
	}
	return t, nil
}

// render executes the layout for a page. It renders to a buffer first so a
// template error is a clean 500, never half a page.
func (t *templates) render(page string, data pageData) ([]byte, error) {
	set := t.byPage[page]
	if set == nil {
		return nil, fmt.Errorf("panel: unknown page %q", page)
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "layout", data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// renderFragment executes a named fragment template.
func (t *templates) renderFragment(name string, data pageData) ([]byte, error) {
	tmpl, ok := fragments[name]
	if !ok {
		return nil, fmt.Errorf("panel: unknown fragment %q", name)
	}
	set := t.byPage[pages[0].name]
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, tmpl, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- template functions ----

var funcs = template.FuncMap{
	"add":  func(a, b int) int { return a + b },
	"sub":  func(a, b int) int { return a - b },
	"join": strings.Join,
	"ago":  ago,
	"disk": diskView,
}

// ago renders a time (time.Time or *time.Time) relative to now.
func ago(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x == nil {
			return "never"
		}
		t = *x
	default:
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%d s ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d ago", int(d.Hours()/24))
	}
}

// DiskView is a disk meter ready to render.
type DiskView struct {
	Pct         int
	Used, Total string
	Class       string // "", "warn" or "danger"
	Word        string // text that carries the status without colour
}

// Disk guard thresholds (architecture 6.5, 7.3).
const (
	diskWarnPct     = 80
	diskCriticalPct = 90
)

// diskView returns nil when no disk figures have been reported.
func diskView(d registry.DiskState) *DiskView {
	if d.TotalBytes == 0 || d.FreeBytes > d.TotalBytes {
		return nil
	}
	used := d.TotalBytes - d.FreeBytes
	v := &DiskView{Pct: int(used * 100 / d.TotalBytes), Used: humanBytes(used), Total: humanBytes(d.TotalBytes)}
	switch {
	case v.Pct >= diskCriticalPct:
		v.Class, v.Word = "danger", "intake paused"
	case v.Pct >= diskWarnPct:
		v.Class, v.Word = "warn", "warning"
	}
	return v
}

func humanBytes(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}
