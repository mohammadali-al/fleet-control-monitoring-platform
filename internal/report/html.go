package report

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed template.gohtml
var pageTemplate string

//go:embed report.css
var reportCSS string

//go:embed report.js
var reportJS string

var page = template.Must(template.New("report").Funcs(template.FuncMap{
	"stamp":  func(t time.Time) string { return fmtTime(t) },
	"cores":  func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) + " cores" },
	"gb":     func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) + " GB" },
	"pct":    func(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) + " %" },
	"ms":     func(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) + " ms" },
	"offset": func(v float64) string { return fmt.Sprintf("%+.2f ms", v) },
	"bytes":  humanBytes,
	"dur":    func(d time.Duration) string { return d.Round(time.Millisecond).String() },
	"join":   strings.Join,
}).Parse(pageTemplate))

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.UTC().Format("2006-01-02 15:04:05.000") + " UTC"
}

func humanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "kMGTPE"[exp])
}

// view is what the template receives.
type view struct {
	D      *Data
	CSS    template.CSS
	JS     template.JS
	JSON   template.JS
	Plan   *planView
	Charts map[string]template.HTML
}

type kv struct{ K, V string }

type ganttRow struct {
	Name, Command, Offset, Length string
	Left, Width                   string
}

type planView struct {
	*Plan
	Total  string
	Params []kv
	Gantt  []ganttRow
	HasGnt bool
}

// Render writes the report as one standalone HTML document to w.
func Render(w io.Writer, d *Data) error {
	js, err := json.Marshal(d) // escapes <, >, & so the data cannot close its script block
	if err != nil {
		return fmt.Errorf("report: encode data: %w", err)
	}
	v := view{
		D:      d,
		CSS:    template.CSS(reportCSS),
		JS:     template.JS(reportJS),
		JSON:   template.JS(js),
		Charts: buildCharts(d),
	}
	if d.Plan != nil {
		v.Plan = buildPlanView(d)
	}
	var buf bytes.Buffer
	if err := page.Execute(&buf, v); err != nil {
		return fmt.Errorf("report: render: %w", err)
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// Generate gathers and renders one test's report to w.
func Generate(w io.Writer, spec Spec, src Sources) error {
	d, err := Gather(spec, src)
	if err != nil {
		return err
	}
	return Render(w, d)
}

// WriteFile gathers and renders the report to path, replacing it atomically.
func WriteFile(path string, spec Spec, src Sources) error {
	var buf bytes.Buffer
	if err := Generate(&buf, spec, src); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".report-*.tmp")
	if err != nil {
		return fmt.Errorf("report: temp file: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("report: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("report: place report: %w", err)
	}
	return nil
}

// ---- plan ----

func buildPlanView(d *Data) *planView {
	p := d.Plan
	total := p.TotalTime
	if total <= 0 {
		total = d.End.Sub(d.Start)
	}
	pv := &planView{Plan: p, Total: total.Round(time.Millisecond).String()}
	switch p.Pattern {
	case PatternLinear:
		if p.WindowLength > 0 {
			pv.Params = append(pv.Params, kv{"Window length", p.WindowLength.String()})
		}
	case PatternSpikes:
		if p.PatternLen > 0 {
			pv.Params = append(pv.Params, kv{"pattern_length", p.PatternLen.String()})
		}
		if p.SpikeLen > 0 {
			pv.Params = append(pv.Params, kv{"spike_duration", p.SpikeLen.String()})
		}
	}
	if p.MaxCores > 0 {
		pv.Params = append(pv.Params, kv{"Ceiling max (per worker)", strconv.FormatFloat(p.MaxCores, 'f', 2, 64) + " cores"})
	}
	if p.MaxRAMGB > 0 {
		pv.Params = append(pv.Params, kv{"RAM ceiling max (per worker)", strconv.FormatFloat(p.MaxRAMGB, 'f', 2, 64) + " GB"})
	}
	for _, it := range p.Items {
		dur := it.Duration
		if dur <= 0 || it.Offset+dur > total {
			dur = total - it.Offset
		}
		if dur < 0 {
			dur = 0
		}
		row := ganttRow{
			Name: it.Name, Command: it.Command,
			Offset: it.Offset.String(), Length: dur.String(),
			Left:  strconv.FormatFloat(100*float64(it.Offset)/float64(total), 'f', 2, 64),
			Width: strconv.FormatFloat(math.Max(0.5, 100*float64(dur)/float64(total)), 'f', 2, 64),
		}
		pv.Gantt = append(pv.Gantt, row)
	}
	pv.HasGnt = len(pv.Gantt) > 0 && total > 0
	return pv
}

// patternShape is the ceiling (0..100 % of the pattern max) over the plan,
// as chart points starting at start.
func patternShape(p *Plan, start time.Time, total time.Duration) []Point {
	if p == nil || total <= 0 {
		return nil
	}
	at := func(off time.Duration) int64 { return start.Add(off).UnixMilli() }
	switch p.Pattern {
	case PatternAllInOne, "":
		return []Point{{at(0), 100}, {at(total), 100}}
	case PatternLinear:
		w := p.WindowLength
		if w <= 0 {
			return nil
		}
		var pts []Point
		for k := time.Duration(0); k*w < total && len(pts) < 3000; k++ {
			b, e := k*w, min((k+1)*w, total)
			pts = append(pts, Point{at(b), 0}, Point{at(e), 100 * float64(e-b) / float64(w)}, Point{at(e), 0})
		}
		return pts
	case PatternSpikes:
		l, s := p.PatternLen, p.SpikeLen
		if l <= 0 || s <= 0 || s >= l {
			return nil
		}
		var pts []Point
		gap := (l - s) / 2
		for k := time.Duration(0); k*l < total && len(pts) < 3000; k++ {
			b := k * l
			pts = append(pts,
				Point{at(b), 0},
				Point{at(min(b+gap, total)), 0}, Point{at(min(b+gap, total)), 100},
				Point{at(min(b+gap+s, total)), 100}, Point{at(min(b+gap+s, total)), 0},
				Point{at(min(b+l, total)), 0})
		}
		return pts
	}
	return nil
}

// ---- charts ----

type chartSeries struct {
	Name string
	Pts  []Point
}

type chartSpec struct {
	ID, Title, Unit string
	T0, T1          int64
	FixedMax        float64 // 0 = automatic
	Series          []chartSeries
	Overlays        bool
	Markers         bool
}

func buildCharts(d *Data) map[string]template.HTML {
	t0, t1 := d.Start.UnixMilli(), d.End.UnixMilli()
	out := map[string]template.HTML{}

	toS := func(ss []Series) []chartSeries {
		var o []chartSeries
		for _, s := range ss {
			o = append(o, chartSeries{s.Name, s.Points})
		}
		return o
	}
	out["availability"] = renderChart(d, chartSpec{ID: "availability", Title: "Availability over the test window", Unit: "%",
		T0: t0, T1: t1, FixedMax: 100, Series: toS(d.Monitor.AvailabilitySeries), Overlays: true, Markers: true})
	out["response"] = renderChart(d, chartSpec{ID: "response", Title: "Response time over the test window", Unit: "ms",
		T0: t0, T1: t1, Series: toS(d.Monitor.ResponseSeries), Overlays: true, Markers: true})

	coreSer := append([]chartSeries{{"All workers", d.Usage.AggCores.Points}}, toS(d.Usage.WorkerCores)...)
	ramSer := append([]chartSeries{{"All workers", d.Usage.AggRAMGB.Points}}, toS(d.Usage.WorkerRAMGB)...)
	out["cores"] = renderChart(d, chartSpec{ID: "cores", Title: "CPU used (cores)", Unit: "cores",
		T0: t0, T1: t1, Series: coreSer, Overlays: true, Markers: true})
	out["ram"] = renderChart(d, chartSpec{ID: "ram", Title: "RAM used (GB)", Unit: "GB",
		T0: t0, T1: t1, Series: ramSer, Overlays: true, Markers: true})

	if d.Plan != nil {
		total := d.Plan.TotalTime
		if total <= 0 {
			total = d.End.Sub(d.Start)
		}
		if pts := patternShape(d.Plan, d.Start, total); len(pts) > 0 {
			out["pattern"] = renderChart(d, chartSpec{ID: "pattern", Title: "Load pattern: resource ceiling", Unit: "% of max",
				T0: t0, T1: t0 + total.Milliseconds(), FixedMax: 100, Series: []chartSeries{{"Ceiling", pts}}})
		}
	}
	return out
}

const (
	cw, ch                 = 900.0, 260.0
	padL, padR, padT, padB = 56.0, 14.0, 12.0, 30.0
)

func niceMax(v float64) float64 {
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	f := v / exp
	switch {
	case f <= 1:
		f = 1
	case f <= 2:
		f = 2
	case f <= 5:
		f = 5
	default:
		f = 10
	}
	return f * exp
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// renderChart draws one chart as inline SVG: gridlines, axes, one path per
// series, the command overlay (a band per command) and watchdog markers. The
// raw points ride on data-pts attributes so the page script can add the hover
// readout; without JavaScript the chart is still fully drawn.
func renderChart(d *Data, c chartSpec) template.HTML {
	esc := template.HTMLEscapeString
	var b strings.Builder
	has := false
	for _, s := range c.Series {
		if len(s.Pts) > 0 {
			has = true
		}
	}
	if !has {
		return template.HTML(`<p class="empty" data-chart-empty="` + esc(c.ID) + `">No data in this window.</p>`)
	}
	ymax := c.FixedMax
	if ymax == 0 {
		for _, s := range c.Series {
			for _, p := range s.Pts {
				ymax = math.Max(ymax, p.V)
			}
		}
		ymax = niceMax(ymax * 1.05)
	}
	span := float64(c.T1 - c.T0)
	if span <= 0 {
		span = 1
	}
	pw, ph := cw-padL-padR, ch-padT-padB
	x := func(t int64) float64 { return padL + pw*math.Min(1, math.Max(0, float64(t-c.T0)/span)) }
	y := func(v float64) float64 { return padT + ph*(1-math.Min(1, math.Max(0, v/ymax))) }

	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %g %g" role="img" aria-label="%s" data-chart="%s" data-t0="%d" data-t1="%d" data-unit="%s" data-plot="%g,%g,%g,%g">`,
		cw, ch, esc(c.Title), esc(c.ID), c.T0, c.T1, esc(c.Unit), padL, padT, pw, ph)

	// y grid + labels
	for i := 0; i <= 4; i++ {
		v := ymax * float64(i) / 4
		fmt.Fprintf(&b, `<line class="grid" x1="%g" x2="%g" y1="%.1f" y2="%.1f"/><text class="tick" x="%g" y="%.1f" text-anchor="end">%s</text>`,
			padL, cw-padR, y(v), y(v), padL-6, y(v)+4, esc(trimFloat(round3(v))))
	}
	// x ticks
	for i := 0; i <= 5; i++ {
		t := c.T0 + int64(span*float64(i)/5)
		anchor := "middle"
		if i == 5 {
			anchor = "end" // keep the last label inside the viewBox
		}
		fmt.Fprintf(&b, `<line class="grid v" x1="%.1f" x2="%.1f" y1="%g" y2="%g"/><text class="tick" x="%.1f" y="%g" text-anchor="%s">%s</text>`,
			x(t), x(t), padT, padT+ph, x(t), ch-8, anchor, time.UnixMilli(t).UTC().Format("15:04:05"))
	}
	// command overlay
	if c.Overlays {
		for i, cmd := range d.Commands {
			t0, t1 := cmd.Start.UnixMilli(), cmd.End.UnixMilli()
			if t1 < c.T0 || t0 > c.T1 {
				continue
			}
			w := math.Max(2, x(t1)-x(t0))
			label := fmt.Sprintf("%s: %s", cmd.Machine, cmd.Command)
			fmt.Fprintf(&b, `<rect class="ov ov%d" x="%.1f" y="%g" width="%.1f" height="%g" data-t0="%d" data-t1="%d" data-label="%s"><title>%s</title></rect>`,
				i%3, x(t0), padT, w, ph, t0, t1, esc(label), esc(label))
		}
	}
	// watchdog markers
	if c.Markers {
		for _, e := range d.Watchdog {
			t := e.Time.UnixMilli()
			if t < c.T0 || t > c.T1 {
				continue
			}
			label := fmt.Sprintf("watchdog %s on %s", e.Action, e.Machine)
			fmt.Fprintf(&b, `<line class="mark" x1="%.1f" x2="%.1f" y1="%g" y2="%g" data-t="%d" data-label="%s"><title>%s</title></line>`,
				x(t), x(t), padT, padT+ph, t, esc(label), esc(label))
		}
	}
	// series
	for i, s := range c.Series {
		if len(s.Pts) == 0 {
			continue
		}
		var pb, data strings.Builder
		for j, p := range s.Pts {
			if j == 0 {
				pb.WriteString("M")
			} else {
				pb.WriteString("L")
			}
			fmt.Fprintf(&pb, "%.1f %.1f", x(p.T), y(p.V))
			if j > 0 {
				data.WriteByte(';')
			}
			fmt.Fprintf(&data, "%d:%s", p.T, trimFloat(p.V))
		}
		fmt.Fprintf(&b, `<path class="line s%d" d="%s" data-name="%s" data-pts="%s"/>`, i%6, pb.String(), esc(s.Name), data.String())
		if len(s.Pts) == 1 {
			fmt.Fprintf(&b, `<circle class="dot s%d" cx="%.1f" cy="%.1f" r="3"/>`, i%6, x(s.Pts[0].T), y(s.Pts[0].V))
		}
	}
	fmt.Fprintf(&b, `<line class="cursor" x1="0" x2="0" y1="%g" y2="%g" hidden/>`, padT, padT+ph)
	b.WriteString(`</svg>`)

	// legend
	b.WriteString(`<ul class="legend">`)
	for i, s := range c.Series {
		if len(s.Pts) == 0 {
			continue
		}
		fmt.Fprintf(&b, `<li><svg class="swatch s%d" viewBox="0 0 24 8" aria-hidden="true"><path class="line" d="M0 4H24"/></svg>%s</li>`, i%6, esc(s.Name))
	}
	if c.Overlays && len(d.Commands) > 0 {
		b.WriteString(`<li><span class="swatch-ov" aria-hidden="true"></span>Command (runs during the shaded band)</li>`)
	}
	if c.Markers && len(d.Watchdog) > 0 {
		b.WriteString(`<li><span class="swatch-mark" aria-hidden="true"></span>Watchdog event</li>`)
	}
	b.WriteString(`</ul><div class="tip" role="status" hidden></div>`)
	return template.HTML(b.String())
}
