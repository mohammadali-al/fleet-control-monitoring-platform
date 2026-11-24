// Package report builds the per-test, self-contained interactive HTML report
// (architecture chapter 13): the human-readable summary of what one test did,
// drawn from the main server's evidence archive (internal/evidence, T-0009)
// and the monitor data (internal/monitor, T-0010) after the fact.
//
// The package has two halves. report.go (this file) gathers everything into a
// plain Data value; html.go and template.gohtml render that value as ONE
// standalone HTML file with inline CSS/JS and embedded data, so the file can
// be kept, mailed or opened from file:// with no external request.
//
// Not everything in a report lives in the archive or the monitor store.
// Identity, the Maneuver plan, command metadata, resource-usage samples,
// start-alignment and watchdog events are session / control-plane facts that
// the (later) panel wiring hands in through Spec; the archive supplies the
// packet and flow counts, the manifest hashes and the drop counts, and the
// monitor sink supplies the availability / response-time series. Gather never
// writes to either store.
package report

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
)

// SchemaVersion is the version of the embedded report data.
const SchemaVersion = 1

// maxChartPoints bounds how many points one embedded series carries; longer
// windows are bucketed.
const maxChartPoints = 240

// ---- inputs ----

// Target is the test's target identity: one (ip, origin) pair (architecture
// 9.3). Origin may be empty for an IP-only target.
type Target struct {
	IP     string   `json:"ip"`
	Origin string   `json:"origin,omitempty"`
	Paths  []string `json:"paths,omitempty"`
}

// PlanItem is one baseline item placed on the Maneuver timeline.
type PlanItem struct {
	Name     string        `json:"name"`
	Command  string        `json:"command,omitempty"`
	Offset   time.Duration `json:"offset_ns"`   // from the test start
	Duration time.Duration `json:"duration_ns"` // 0 = until the end
}

// Load pattern names (architecture 9.4).
const (
	PatternAllInOne = "all-in-one"
	PatternLinear   = "linear"
	PatternSpikes   = "spikes"
)

// Plan is the Maneuver plan and load pattern of a test. A test that ran only
// manual commands has no plan (Spec.Plan == nil) and the report omits the
// whole section.
type Plan struct {
	Name         string        `json:"name"`
	Pattern      string        `json:"pattern"`                     // all-in-one | linear | spikes
	TotalTime    time.Duration `json:"total_time_ns,omitempty"`     // 0 = the test window
	WindowLength time.Duration `json:"window_length_ns,omitempty"`  // linear
	PatternLen   time.Duration `json:"pattern_length_ns,omitempty"` // spikes
	SpikeLen     time.Duration `json:"spike_duration_ns,omitempty"` // spikes
	MaxCores     float64       `json:"max_cores,omitempty"`         // per-worker ceiling at pattern max
	MaxRAMGB     float64       `json:"max_ram_gb,omitempty"`
	Items        []PlanItem    `json:"items,omitempty"`
}

// Worker is a participating worker agent.
type Worker struct {
	Machine string `json:"machine"`
	Label   string `json:"label,omitempty"`
}

// UsageSample is one live resource reading of one worker agent: absolute
// units, CPU as cores used and RAM as GB (10^9 bytes), per architecture 10.
type UsageSample struct {
	Machine   string    `json:"machine"`
	Time      time.Time `json:"time"`
	CoresUsed float64   `json:"cores_used"`
	RAMGB     float64   `json:"ram_gb"`
}

// GBFromBytes converts a byte count to GB (10^9 bytes), the report's RAM unit.
func GBFromBytes(b uint64) float64 { return float64(b) / 1e9 }

// Command is one command execution (a session) of the test.
type Command struct {
	SessionID string    `json:"session_id"`
	Machine   string    `json:"machine"`
	Command   string    `json:"command"`
	User      string    `json:"user,omitempty"`
	Mode      string    `json:"mode,omitempty"`
	Start     time.Time `json:"start"`
	End       time.Time `json:"end"`
	ExitCode  int       `json:"exit_code"`
}

// AgentStart is one worker agent's outcome in the synchronized start (4.9).
type AgentStart struct {
	Machine   string    `json:"machine"`
	StartedAt time.Time `json:"started_at,omitempty"` // zero if it never started
	Missed    bool      `json:"missed,omitempty"`     // order arrived after T; never started late
	Note      string    `json:"note,omitempty"`
}

// Alignment is the synchronized-start record: the committed start T and what
// each agent did with it.
type Alignment struct {
	Intended time.Time    `json:"intended"`
	Agents   []AgentStart `json:"agents"`
}

// WatchdogEvent is one watchdog action on a worker agent (chapter 11).
type WatchdogEvent struct {
	Time      time.Time `json:"time"`
	Machine   string    `json:"machine"`
	Action    string    `json:"action"` // throttle | kill-session | restart-service | reboot | ...
	Detail    string    `json:"detail,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
}

// Spec describes one test: the facts the report needs that the archive and the
// monitor store do not hold.
type Spec struct {
	Project  string    `json:"project"`
	TestID   string    `json:"test_id"`
	TestName string    `json:"test_name,omitempty"`
	Target   Target    `json:"target"`
	Start    time.Time `json:"start"` // test window; zero = derived from the data
	End      time.Time `json:"end"`

	Plan      *Plan           `json:"plan,omitempty"`
	Workers   []Worker        `json:"workers,omitempty"` // empty = derived from commands/usage
	Usage     []UsageSample   `json:"usage,omitempty"`
	Commands  []Command       `json:"commands,omitempty"`
	Alignment *Alignment      `json:"alignment,omitempty"`
	Watchdog  []WatchdogEvent `json:"watchdog,omitempty"`

	// Now stamps GeneratedAt; zero means time.Now (a test hook).
	Now func() time.Time `json:"-"`
}

// Sources are the stores a report reads. Either may be nil: the matching
// sections then say that no data was available.
type Sources struct {
	Archive *evidence.Archive
	Samples []monitor.Sample
}

// ---- gathered data (also embedded in the HTML as JSON) ----

// Data is everything the renderer shows.
type Data struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`

	Project  string    `json:"project"`
	TestID   string    `json:"test_id"`
	TestName string    `json:"test_name,omitempty"`
	Target   Target    `json:"target"`
	History  string    `json:"history_key"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
	Duration string    `json:"duration"`

	Plan     *Plan           `json:"plan,omitempty"`
	Workers  []WorkerRow     `json:"workers"`
	Usage    UsageData       `json:"usage"`
	Commands []CommandRow    `json:"commands"`
	Monitor  MonitorData     `json:"monitor"`
	Evidence EvidenceData    `json:"evidence"`
	Align    *AlignData      `json:"alignment,omitempty"`
	Watchdog []WatchdogEvent `json:"watchdog"`
	// WatchdogOutside counts supplied events that fell outside the window and
	// were left out.
	WatchdogOutside int `json:"watchdog_outside_window,omitempty"`

	Totals Totals `json:"totals"`

	Notes []string `json:"notes,omitempty"`
}

// Totals are the headline numbers.
type Totals struct {
	Workers      int     `json:"workers"`
	Commands     int     `json:"commands"`
	Packets      int     `json:"packets"`
	Flows        int     `json:"flows"`
	PayloadBytes int64   `json:"payload_bytes"`
	Dropped      uint64  `json:"dropped"`
	PeakCores    float64 `json:"peak_cores"`
	PeakRAMGB    float64 `json:"peak_ram_gb"`
	Missed       int     `json:"missed"`
	Watchdog     int     `json:"watchdog_events"`
	Availability float64 `json:"availability_percent"` // -1 when no monitor data
}

// WorkerRow is one participating worker agent.
type WorkerRow struct {
	Machine   string  `json:"machine"`
	Label     string  `json:"label,omitempty"`
	Commands  int     `json:"commands"`
	Packets   int     `json:"packets"`
	Flows     int     `json:"flows"`
	Dropped   uint64  `json:"dropped"`
	Samples   int     `json:"usage_samples"`
	MeanCores float64 `json:"mean_cores"`
	PeakCores float64 `json:"peak_cores"`
	MeanRAMGB float64 `json:"mean_ram_gb"`
	PeakRAMGB float64 `json:"peak_ram_gb"`
	Missed    bool    `json:"missed,omitempty"`
}

// Point is one chart point: epoch milliseconds and a value.
type Point struct {
	T int64   `json:"t"`
	V float64 `json:"v"`
}

// Series is a named, bucketed time series.
type Series struct {
	Name   string  `json:"name"`
	Points []Point `json:"points"`
}

// UsageData is resource usage over the window. Aggregate sums the workers'
// bucketed readings.
type UsageData struct {
	BucketSeconds float64  `json:"bucket_seconds"`
	AggCores      Series   `json:"aggregate_cores"`
	AggRAMGB      Series   `json:"aggregate_ram_gb"`
	MeanCores     float64  `json:"aggregate_mean_cores"`
	PeakCores     float64  `json:"aggregate_peak_cores"`
	MeanRAMGB     float64  `json:"aggregate_mean_ram_gb"`
	PeakRAMGB     float64  `json:"aggregate_peak_ram_gb"`
	WorkerCores   []Series `json:"worker_cores,omitempty"`
	WorkerRAMGB   []Series `json:"worker_ram_gb,omitempty"`
}

// CommandRow is one command with its packet / flow counts from the archive.
type CommandRow struct {
	SessionID    string    `json:"session_id"`
	Machine      string    `json:"machine"`
	Command      string    `json:"command"`
	User         string    `json:"user,omitempty"`
	Mode         string    `json:"mode,omitempty"`
	Start        time.Time `json:"start"`
	End          time.Time `json:"end"`
	ExitCode     int       `json:"exit_code"`
	Packets      int       `json:"packets"`
	Flows        int       `json:"flows"`
	PayloadBytes int64     `json:"payload_bytes"`
	Duration     string    `json:"duration"`
}

// MonitorData is the availability and response-time view of the target's
// (ip, origin) history over the window.
type MonitorData struct {
	Available     bool     `json:"available"`
	BucketSeconds float64  `json:"bucket_seconds"`
	Probes        int      `json:"probes"`
	OK            int      `json:"ok"`
	Availability  float64  `json:"availability_percent"`
	MeanMS        float64  `json:"mean_latency_ms"`
	P95MS         float64  `json:"p95_latency_ms"`
	MaxMS         float64  `json:"max_latency_ms"`
	Agents        []string `json:"agents,omitempty"`
	// Availability and ResponseMS carry one series per probe type plus "all"
	// (availability only).
	AvailabilitySeries []Series `json:"availability_series,omitempty"`
	ResponseSeries     []Series `json:"response_series,omitempty"`
}

// SegmentRef is one archive segment the report relies on: the manifest entry
// that pins its content hash.
type SegmentRef struct {
	Machine  string    `json:"machine"`
	File     string    `json:"file"`
	Path     string    `json:"path"` // <session>/<file> under archive/<machine>/
	SHA256   string    `json:"sha256"`
	Bytes    int64     `json:"bytes"`
	Records  int       `json:"records"`
	Dropped  uint64    `json:"dropped"`
	Start    time.Time `json:"start_time"`
	End      time.Time `json:"end_time"`
	Sessions []string  `json:"sessions,omitempty"`
}

// MachineEvidence is one machine's evidence references.
type MachineEvidence struct {
	Machine  string       `json:"machine"`
	Manifest string       `json:"manifest"` // archive-relative manifest path
	Digest   string       `json:"manifest_digest"`
	Dropped  uint64       `json:"dropped"`
	Packets  int          `json:"packets"` // this test's packets on the machine
	Flows    int          `json:"flows"`   // distinct flows among them
	Segments []SegmentRef `json:"segments"`
}

// EvidenceData is the hash / manifest-reference section plus drop counts.
type EvidenceData struct {
	Available bool              `json:"available"`
	Root      string            `json:"archive_root,omitempty"`
	Digest    string            `json:"digest,omitempty"` // over every machine digest
	Machines  []MachineEvidence `json:"machines,omitempty"`
	Segments  int               `json:"segments"`
	Dropped   uint64            `json:"dropped"`
	Packets   int               `json:"packets"`
	Flows     int               `json:"flows"` // distinct (protocol, src, dst) across the test
}

// AgentAlign is one agent's start offset relative to the committed T.
type AgentAlign struct {
	AgentStart
	OffsetMS float64 `json:"offset_ms"` // StartedAt - T; meaningful when it started
	Started  bool    `json:"started"`
}

// AlignData is the start-alignment summary.
type AlignData struct {
	Intended time.Time    `json:"intended"`
	Agents   []AgentAlign `json:"agents"`
	Missed   []string     `json:"missed,omitempty"`
	SpreadMS float64      `json:"spread_ms"` // max - min offset among started agents
}

// ---- gather ----

// Gather collects one test's report data. It reads the archive and the monitor
// samples and never modifies either.
func Gather(spec Spec, src Sources) (*Data, error) {
	if spec.TestID == "" {
		return nil, errors.New("report: spec needs a test id")
	}
	now := time.Now
	if spec.Now != nil {
		now = spec.Now
	}
	d := &Data{
		SchemaVersion: SchemaVersion,
		GeneratedAt:   now().UTC(),
		Project:       spec.Project,
		TestID:        spec.TestID,
		TestName:      spec.TestName,
		Target:        spec.Target,
		History:       monitor.HistoryKey(spec.Target.IP, spec.Target.Origin),
		Plan:          spec.Plan,
	}
	d.Start, d.End = window(spec)
	if d.Start.IsZero() || d.End.IsZero() || d.End.Before(d.Start) {
		return nil, errors.New("report: the test window is empty and cannot be derived (give Start/End or data)")
	}
	d.Duration = d.End.Sub(d.Start).Round(time.Millisecond).String()

	bucket := bucketFor(d.End.Sub(d.Start))

	// Commands and workers first: the archive scan needs the session ids.
	cmds := append([]Command(nil), spec.Commands...)
	sort.SliceStable(cmds, func(i, j int) bool { return cmds[i].Start.Before(cmds[j].Start) })
	workers := workerList(spec, cmds)

	d.Commands = make([]CommandRow, len(cmds))
	for i, c := range cmds {
		d.Commands[i] = CommandRow{
			SessionID: c.SessionID, Machine: c.Machine, Command: c.Command, User: c.User, Mode: c.Mode,
			Start: c.Start.UTC(), End: c.End.UTC(), ExitCode: c.ExitCode,
			Duration: c.End.Sub(c.Start).Round(time.Millisecond).String(),
		}
	}

	d.Evidence = gatherEvidence(src.Archive, d, workers, &d.Notes)
	d.Usage = gatherUsage(spec.Usage, d.Start, d.End, bucket, workers)
	d.Monitor = gatherMonitor(src.Samples, d.History, d.Start, d.End, bucket)
	d.Align = gatherAlignment(spec.Alignment)
	d.Watchdog, d.WatchdogOutside = windowWatchdog(spec.Watchdog, d.Start, d.End)

	if src.Archive == nil {
		d.Notes = append(d.Notes, "No evidence archive was available: packet and flow counts, hashes and drop counts are not shown.")
	}
	if !d.Monitor.Available {
		d.Notes = append(d.Notes, "No monitor samples for this target in the window.")
	}

	d.Workers = workerRows(d, workers)
	d.Totals = totals(d)
	return d, nil
}

// window is the test window: the spec's, else the span of the supplied data.
func window(spec Spec) (start, end time.Time) {
	start, end = spec.Start, spec.End
	if !start.IsZero() && !end.IsZero() {
		return start.UTC(), end.UTC()
	}
	var lo, hi time.Time
	see := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if lo.IsZero() || t.Before(lo) {
			lo = t
		}
		if hi.IsZero() || t.After(hi) {
			hi = t
		}
	}
	for _, c := range spec.Commands {
		see(c.Start)
		see(c.End)
	}
	for _, u := range spec.Usage {
		see(u.Time)
	}
	if spec.Alignment != nil {
		see(spec.Alignment.Intended)
	}
	if start.IsZero() {
		start = lo
	}
	if end.IsZero() {
		end = hi
	}
	return start.UTC(), end.UTC()
}

func bucketFor(span time.Duration) time.Duration {
	b := span / maxChartPoints
	if b < time.Second {
		b = time.Second
	}
	// Round up to a whole second so axis labels stay tidy.
	return ((b + time.Second - 1) / time.Second) * time.Second
}

func workerList(spec Spec, cmds []Command) []Worker {
	seen := map[string]bool{}
	var out []Worker
	add := func(w Worker) {
		if w.Machine == "" || seen[w.Machine] {
			return
		}
		seen[w.Machine] = true
		out = append(out, w)
	}
	for _, w := range spec.Workers {
		add(w)
	}
	for _, c := range cmds {
		add(Worker{Machine: c.Machine})
	}
	for _, u := range spec.Usage {
		add(Worker{Machine: u.Machine})
	}
	if spec.Alignment != nil {
		for _, a := range spec.Alignment.Agents {
			add(Worker{Machine: a.Machine})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out
}

// ---- evidence ----

// flowKey identifies a flow: the same tuple on two workers is two flows.
type flowKey struct{ machine, proto, src, dst string }

func gatherEvidence(a *evidence.Archive, d *Data, workers []Worker, notes *[]string) EvidenceData {
	ev := EvidenceData{}
	if a == nil {
		return ev
	}
	ev.Available = true
	ev.Root = evidence.ArchiveDir

	// session id -> command row index
	bySession := map[string]int{}
	for i := range d.Commands {
		if id := d.Commands[i].SessionID; id != "" {
			bySession[id] = i
		}
	}
	cmdFlows := make([]map[flowKey]bool, len(d.Commands))
	allFlows := map[flowKey]bool{}

	// Archive.Segments creates the machine's directory on first use; only ask
	// for machines the archive already holds so a report never writes to it.
	held, err := a.Machines()
	if err != nil {
		*notes = append(*notes, fmt.Sprintf("The evidence archive could not be listed: %v", err))
		return ev
	}
	inArchive := map[string]bool{}
	for _, m := range held {
		inArchive[m] = true
	}

	var machineDigests []string
	for _, w := range workers {
		if !inArchive[w.Machine] {
			continue
		}
		segs, err := a.Segments(w.Machine)
		if err != nil {
			*notes = append(*notes, fmt.Sprintf("Evidence for %s could not be read: %v", w.Machine, err))
			continue
		}
		me := MachineEvidence{Machine: w.Machine, Manifest: w.Machine + "/" + evidence.ArchiveManifestName}
		machFlows := map[flowKey]bool{}
		for _, s := range segs {
			if !wantSegment(s, bySession, d.Start, d.End, len(d.Commands) == 0) {
				continue
			}
			ref := SegmentRef{
				Machine: w.Machine, File: s.File, Path: s.Path, SHA256: s.SHA256, Bytes: s.Bytes,
				Records: s.Records, Dropped: s.Dropped,
				Start: s.StartTime.UTC(), End: s.EndTime.UTC(), Sessions: s.Sessions,
			}
			me.Segments = append(me.Segments, ref)
			me.Dropped += s.Dropped

			if len(bySession) == 0 {
				continue
			}
			f, err := a.OpenSegment(w.Machine, s)
			if err != nil {
				*notes = append(*notes, fmt.Sprintf("Segment %s/%s could not be opened: %v", w.Machine, s.File, err))
				continue
			}
			err = scanRecords(f, func(r capture.Record) {
				i, ok := bySession[r.SessionID]
				if !ok {
					return
				}
				d.Commands[i].Packets++
				me.Packets++
				d.Commands[i].PayloadBytes += int64(r.PayloadBytesFull)
				k := flowKey{w.Machine, r.Protocol, r.Src, r.Dst}
				if cmdFlows[i] == nil {
					cmdFlows[i] = map[flowKey]bool{}
				}
				cmdFlows[i][k] = true
				allFlows[k] = true
				machFlows[k] = true
			})
			f.Close()
			if err != nil {
				*notes = append(*notes, fmt.Sprintf("Segment %s/%s could not be fully read: %v", w.Machine, s.File, err))
			}
		}
		me.Flows = len(machFlows)
		ev.Packets += me.Packets
		if len(me.Segments) == 0 {
			continue
		}
		sort.SliceStable(me.Segments, func(i, j int) bool {
			if !me.Segments[i].Start.Equal(me.Segments[j].Start) {
				return me.Segments[i].Start.Before(me.Segments[j].Start)
			}
			return me.Segments[i].File < me.Segments[j].File
		})
		me.Digest = manifestDigest(me.Segments)
		machineDigests = append(machineDigests, me.Machine+":"+me.Digest)
		ev.Segments += len(me.Segments)
		ev.Dropped += me.Dropped
		ev.Machines = append(ev.Machines, me)
	}
	for i := range d.Commands {
		d.Commands[i].Flows = len(cmdFlows[i])
	}
	ev.Flows = len(allFlows)
	if len(machineDigests) > 0 {
		sum := sha256.Sum256([]byte(strings.Join(machineDigests, "\n")))
		ev.Digest = hex.EncodeToString(sum[:])
	}
	return ev
}

// wantSegment selects the segments that belong to this test: those holding a
// record of one of its sessions, or - when the test supplied no commands -
// those overlapping the window.
func wantSegment(s evidence.ArchivedSegment, sessions map[string]int, from, to time.Time, byWindow bool) bool {
	if !byWindow {
		ids := s.Sessions
		if len(ids) == 0 {
			ids = []string{s.Session}
		}
		for _, id := range ids {
			if _, ok := sessions[id]; ok {
				return true
			}
		}
		return false
	}
	return !s.EndTime.Before(from) && !s.StartTime.After(to)
}

func scanRecords(r io.Reader, fn func(capture.Record)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var rec capture.Record
			if jerr := json.Unmarshal(line, &rec); jerr != nil {
				return fmt.Errorf("bad record: %w", jerr)
			}
			fn(rec)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// manifestDigest is the SHA-256 over the sorted "file sha256" lines of the
// segments the report relies on: one value that pins them all.
func manifestDigest(segs []SegmentRef) string {
	lines := make([]string, 0, len(segs))
	for _, s := range segs {
		lines = append(lines, s.File+" "+s.SHA256)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// ---- usage ----

type acc struct {
	sum float64
	n   int
}

func (a acc) mean() float64 {
	if a.n == 0 {
		return 0
	}
	return a.sum / float64(a.n)
}

func gatherUsage(samples []UsageSample, from, to time.Time, bucket time.Duration, workers []Worker) UsageData {
	u := UsageData{BucketSeconds: bucket.Seconds(), AggCores: Series{Name: "All workers"}, AggRAMGB: Series{Name: "All workers"}}
	type key struct {
		m string
		i int64
	}
	cores := map[key]*acc{}
	ram := map[key]*acc{}
	nb := int64(to.Sub(from)/bucket) + 1
	for _, s := range samples {
		if s.Time.Before(from) || s.Time.After(to) {
			continue
		}
		k := key{s.Machine, int64(s.Time.Sub(from) / bucket)}
		if cores[k] == nil {
			cores[k], ram[k] = &acc{}, &acc{}
		}
		cores[k].sum += s.CoresUsed
		cores[k].n++
		ram[k].sum += s.RAMGB
		ram[k].n++
	}
	aggC := make([]float64, nb)
	aggR := make([]float64, nb)
	has := make([]bool, nb)
	for _, w := range workers {
		sc := Series{Name: w.Machine}
		sr := Series{Name: w.Machine}
		for i := int64(0); i < nb; i++ {
			k := key{w.Machine, i}
			c, ok := cores[k]
			if !ok {
				continue
			}
			t := from.Add(time.Duration(i) * bucket).UnixMilli()
			sc.Points = append(sc.Points, Point{t, round2(c.mean())})
			sr.Points = append(sr.Points, Point{t, round3(ram[k].mean())})
			aggC[i] += c.mean()
			aggR[i] += ram[k].mean()
			has[i] = true
		}
		if len(sc.Points) > 0 {
			u.WorkerCores = append(u.WorkerCores, sc)
			u.WorkerRAMGB = append(u.WorkerRAMGB, sr)
		}
	}
	var sumC, sumR float64
	var n int
	for i := int64(0); i < nb; i++ {
		if !has[i] {
			continue
		}
		t := from.Add(time.Duration(i) * bucket).UnixMilli()
		u.AggCores.Points = append(u.AggCores.Points, Point{t, round2(aggC[i])})
		u.AggRAMGB.Points = append(u.AggRAMGB.Points, Point{t, round3(aggR[i])})
		sumC += aggC[i]
		sumR += aggR[i]
		n++
		u.PeakCores = math.Max(u.PeakCores, aggC[i])
		u.PeakRAMGB = math.Max(u.PeakRAMGB, aggR[i])
	}
	if n > 0 {
		u.MeanCores, u.MeanRAMGB = sumC/float64(n), sumR/float64(n)
	}
	u.PeakCores, u.PeakRAMGB = round2(u.PeakCores), round3(u.PeakRAMGB)
	u.MeanCores, u.MeanRAMGB = round2(u.MeanCores), round3(u.MeanRAMGB)
	return u
}

// ---- monitor ----

func gatherMonitor(samples []monitor.Sample, key string, from, to time.Time, bucket time.Duration) MonitorData {
	m := MonitorData{BucketSeconds: bucket.Seconds()}
	type bk struct {
		typ string
		i   int64
	}
	type cell struct {
		n, ok  int
		latSum float64
		latMax float64
		latN   int
	}
	cells := map[bk]*cell{}
	all := map[int64]*cell{}
	types := map[string]bool{}
	agents := map[string]bool{}
	var lats []float64
	var latSum float64

	bump := func(c *cell, ok bool, ms float64) {
		c.n++
		if ok {
			c.ok++
			c.latSum += ms
			c.latN++
			c.latMax = math.Max(c.latMax, ms)
		}
	}
	for _, s := range samples {
		if s.HistoryKey != key {
			continue
		}
		t := s.Result.Time
		if t.Before(from) || t.After(to) {
			continue
		}
		i := int64(t.Sub(from) / bucket)
		typ := string(s.Result.Type)
		types[typ] = true
		agents[s.Agent] = true
		ms := float64(s.Result.Latency) / float64(time.Millisecond)
		c := cells[bk{typ, i}]
		if c == nil {
			c = &cell{}
			cells[bk{typ, i}] = c
		}
		bump(c, s.Result.OK, ms)
		a := all[i]
		if a == nil {
			a = &cell{}
			all[i] = a
		}
		bump(a, s.Result.OK, ms)
		m.Probes++
		if s.Result.OK {
			m.OK++
			lats = append(lats, ms)
			latSum += ms
			m.MaxMS = math.Max(m.MaxMS, ms)
		}
	}
	if m.Probes == 0 {
		return m
	}
	m.Available = true
	m.Availability = round2(100 * float64(m.OK) / float64(m.Probes))
	if len(lats) > 0 {
		sort.Float64s(lats)
		m.MeanMS = round2(latSum / float64(len(lats)))
		m.P95MS = round2(lats[int(math.Ceil(0.95*float64(len(lats))))-1])
	}
	m.MaxMS = round2(m.MaxMS)
	for a := range agents {
		m.Agents = append(m.Agents, a)
	}
	sort.Strings(m.Agents)

	nb := int64(to.Sub(from)/bucket) + 1
	at := func(i int64) int64 { return from.Add(time.Duration(i) * bucket).UnixMilli() }

	allAvail := Series{Name: "all"}
	for i := int64(0); i < nb; i++ {
		if c := all[i]; c != nil {
			allAvail.Points = append(allAvail.Points, Point{at(i), round2(100 * float64(c.ok) / float64(c.n))})
		}
	}
	m.AvailabilitySeries = append(m.AvailabilitySeries, allAvail)

	var tnames []string
	for t := range types {
		tnames = append(tnames, t)
	}
	sort.Strings(tnames)
	for _, t := range tnames {
		sa := Series{Name: t}
		sr := Series{Name: t}
		for i := int64(0); i < nb; i++ {
			c := cells[bk{t, i}]
			if c == nil {
				continue
			}
			sa.Points = append(sa.Points, Point{at(i), round2(100 * float64(c.ok) / float64(c.n))})
			if c.latN > 0 {
				sr.Points = append(sr.Points, Point{at(i), round2(c.latSum / float64(c.latN))})
			}
		}
		if len(tnames) > 1 {
			m.AvailabilitySeries = append(m.AvailabilitySeries, sa)
		}
		if len(sr.Points) > 0 {
			m.ResponseSeries = append(m.ResponseSeries, sr)
		}
	}
	return m
}

// ---- alignment, watchdog ----

func gatherAlignment(a *Alignment) *AlignData {
	if a == nil {
		return nil
	}
	out := &AlignData{Intended: a.Intended.UTC()}
	var lo, hi float64
	first := true
	for _, ag := range a.Agents {
		row := AgentAlign{AgentStart: ag}
		row.StartedAt = ag.StartedAt.UTC()
		if ag.StartedAt.IsZero() {
			row.StartedAt = time.Time{}
		}
		if !ag.StartedAt.IsZero() && !ag.Missed {
			row.Started = true
			row.OffsetMS = round2(float64(ag.StartedAt.Sub(a.Intended)) / float64(time.Millisecond))
			if first || row.OffsetMS < lo {
				lo = row.OffsetMS
			}
			if first || row.OffsetMS > hi {
				hi = row.OffsetMS
			}
			first = false
		}
		if ag.Missed {
			out.Missed = append(out.Missed, ag.Machine)
		}
		out.Agents = append(out.Agents, row)
	}
	sort.SliceStable(out.Agents, func(i, j int) bool { return out.Agents[i].Machine < out.Agents[j].Machine })
	sort.Strings(out.Missed)
	out.SpreadMS = round2(hi - lo)
	return out
}

func windowWatchdog(ev []WatchdogEvent, from, to time.Time) (in []WatchdogEvent, outside int) {
	in = []WatchdogEvent{}
	for _, e := range ev {
		if e.Time.Before(from) || e.Time.After(to) {
			outside++
			continue
		}
		e.Time = e.Time.UTC()
		in = append(in, e)
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Time.Before(in[j].Time) })
	return in, outside
}

// ---- rollups ----

func workerRows(d *Data, workers []Worker) []WorkerRow {
	rows := make([]WorkerRow, 0, len(workers))
	idx := map[string]int{}
	for _, w := range workers {
		idx[w.Machine] = len(rows)
		rows = append(rows, WorkerRow{Machine: w.Machine, Label: w.Label})
	}
	for _, c := range d.Commands {
		if i, ok := idx[c.Machine]; ok {
			rows[i].Commands++
		}
	}
	for _, me := range d.Evidence.Machines {
		if i, ok := idx[me.Machine]; ok {
			rows[i].Dropped = me.Dropped
			rows[i].Packets = me.Packets
			rows[i].Flows = me.Flows
		}
	}
	for _, s := range d.Usage.WorkerCores {
		wi, ok := idx[s.Name]
		if !ok {
			continue
		}
		var sum, peak float64
		for _, p := range s.Points {
			sum += p.V
			peak = math.Max(peak, p.V)
		}
		rows[wi].Samples = len(s.Points)
		rows[wi].PeakCores = peak
		if len(s.Points) > 0 {
			rows[wi].MeanCores = round2(sum / float64(len(s.Points)))
		}
	}
	for _, s := range d.Usage.WorkerRAMGB {
		wi, ok := idx[s.Name]
		if !ok {
			continue
		}
		var sum, peak float64
		for _, p := range s.Points {
			sum += p.V
			peak = math.Max(peak, p.V)
		}
		rows[wi].PeakRAMGB = peak
		if len(s.Points) > 0 {
			rows[wi].MeanRAMGB = round3(sum / float64(len(s.Points)))
		}
	}
	if d.Align != nil {
		for _, m := range d.Align.Missed {
			if i, ok := idx[m]; ok {
				rows[i].Missed = true
			}
		}
	}
	return rows
}

func totals(d *Data) Totals {
	t := Totals{
		Workers:      len(d.Workers),
		Commands:     len(d.Commands),
		Dropped:      d.Evidence.Dropped,
		PeakCores:    d.Usage.PeakCores,
		PeakRAMGB:    d.Usage.PeakRAMGB,
		Watchdog:     len(d.Watchdog),
		Availability: -1,
	}
	t.Packets, t.Flows = d.Evidence.Packets, d.Evidence.Flows
	for _, c := range d.Commands {
		t.PayloadBytes += c.PayloadBytes
	}
	if d.Align != nil {
		t.Missed = len(d.Align.Missed)
	}
	if d.Monitor.Available {
		t.Availability = d.Monitor.Availability
	}
	return t
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
