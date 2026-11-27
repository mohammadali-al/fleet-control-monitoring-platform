package panel

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/registry"
)

// Evidence page API (internal/evidence, T-0009): the session browser, packet
// records of one session, pull + sync status, the segment manifest and
// exports.
//
//	GET  /api/evidence/sessions?machine=&kind=&q=            sessions in the archive
//	GET  /api/evidence/sessions/{machine}/{session}/records  first packet records of a session
//	GET  /api/evidence/sync                                  per worker agent sync status + archive disk
//	POST /api/evidence/pull {machine}                        start a pull (machine empty = every online worker)
//	GET  /api/evidence/manifest/{machine}                    archived segments of a machine
//	GET  /api/evidence/export?format=&machine=&session=&from=&to=   download pcapng, csv or json
func (s *server) routesEvidence(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/evidence/sessions", s.observed(s.evSessions))
	mux.HandleFunc("GET /api/evidence/sessions/{machine}/{session}/records", s.observed(s.evRecords))
	mux.HandleFunc("GET /api/evidence/sync", s.observed(s.evSync))
	mux.HandleFunc("POST /api/evidence/pull", s.observed(s.evPull))
	mux.HandleFunc("GET /api/evidence/manifest/{machine}", s.observed(s.evManifest))
	mux.HandleFunc("GET /api/evidence/export", s.observed(s.evExport))
}

type evSession struct {
	Session  string    `json:"session"`
	Machine  string    `json:"machine"`
	Kind     string    `json:"kind"` // oneshot | pty | "" (not in the command history)
	Command  string    `json:"command,omitempty"`
	User     string    `json:"user,omitempty"`
	Started  time.Time `json:"started"`
	Ended    time.Time `json:"ended"`
	Segments int       `json:"segments"`
	// Packets is the number of captured records belonging to this session
	// across its segments (exact even when Shared is true).
	Packets int    `json:"packets"`
	Shared  bool   `json:"shared"`
	Dropped uint64 `json:"dropped"`
	// Online is whether the worker agent is connected now.
	Online bool `json:"online"`
}

func (s *server) evSessions(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe) {
	if o.Archive == nil {
		notConfigured(w, "the evidence archive")
		return
	}
	q := r.URL.Query()
	want := strings.ToLower(strings.TrimSpace(q.Get("q")))
	machines, err := o.Archive.Machines()
	if err != nil {
		writeErr(w, err)
		return
	}
	cmds := map[string]fleet.Command{}
	for _, c := range s.be.Commands() {
		cmds[c.Session] = c
	}
	online := map[string]bool{}
	for _, m := range s.be.Machines() {
		online[m.ID] = m.State == fleet.StateOnline
	}
	agg := map[string]*evSession{}
	for _, machine := range machines {
		if f := q.Get("machine"); f != "" && f != machine {
			continue
		}
		segs, err := o.Archive.Segments(machine)
		if err != nil {
			s.log.Warn("evidence: list segments", "component", "panel", "machine", machine, "err", err)
			continue
		}
		for _, seg := range segs {
			ids := seg.Sessions
			if len(ids) == 0 {
				ids = []string{seg.Session}
			}
			var counts map[string]int
			if len(ids) > 1 {
				var cerr error
				counts, cerr = sessionPacketCounts(o.Archive, machine, seg)
				if cerr != nil {
					s.log.Warn("evidence: count shared segment records", "component", "panel", "machine", machine, "file", seg.File, "err", cerr)
					counts = nil
				}
			}
			for _, id := range ids {
				key := machine + "/" + id
				e := agg[key]
				if e == nil {
					e = &evSession{Session: id, Machine: machine, Started: seg.StartTime, Ended: seg.EndTime, Online: online[machine]}
					if c, ok := cmds[id]; ok {
						e.Kind, e.Command, e.User = c.Mode, c.Command, c.User
					}
					agg[key] = e
				}
				if seg.StartTime.Before(e.Started) {
					e.Started = seg.StartTime
				}
				if seg.EndTime.After(e.Ended) {
					e.Ended = seg.EndTime
				}
				e.Segments++
				if counts != nil {
					e.Packets += counts[id]
				} else {
					e.Packets += seg.Records
				}
				e.Dropped += seg.Dropped
				if len(ids) > 1 {
					e.Shared = true
				}
			}
		}
	}
	out := make([]evSession, 0, len(agg))
	for _, e := range agg {
		if k := q.Get("kind"); k != "" && e.Kind != k {
			continue
		}
		if want != "" && !strings.Contains(strings.ToLower(e.Session+" "+e.Machine+" "+e.Command), want) {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Started.Equal(out[j].Started) {
			return out[i].Started.After(out[j].Started)
		}
		return out[i].Session < out[j].Session
	})
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

type evRecord struct {
	Time          time.Time `json:"time"`
	Protocol      string    `json:"protocol"`
	Src           string    `json:"src"`
	Dst           string    `json:"dst"`
	Flags         string    `json:"flags,omitempty"`
	Seq           uint32    `json:"seq,omitempty"`
	Ack           uint32    `json:"ack,omitempty"`
	PayloadStatus string    `json:"payload_status"`
	PayloadBytes  int       `json:"payload_bytes"`
	PayloadSHA256 string    `json:"payload_sha256"`
	PayloadHead   []byte    `json:"payload_head,omitempty"`
	SrcIPUsed     string    `json:"src_ip_used,omitempty"`
}

func sessionPacketCounts(arch *evidence.Archive, machine string, seg evidence.ArchivedSegment) (map[string]int, error) {
	f, err := arch.OpenSegment(machine, seg)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	counts := map[string]int{}
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			var row struct {
				SessionID string `json:"session_id"`
			}
			if jerr := json.Unmarshal(line, &row); jerr != nil {
				return nil, jerr
			}
			counts[row.SessionID]++
		}
		if err == io.EOF {
			return counts, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

const (
	defaultRecordLimit = 200
	maxRecordLimit     = 2000
)

var errEnough = errors.New("enough records")

// evRecords returns the first records of one session. The archive only offers
// a streaming export, so the JSON export is read through a pipe and closed once
// enough records have been decoded.
func (s *server) evRecords(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Archive == nil {
		notConfigured(w, "the evidence archive")
		return
	}
	machine, session := r.PathValue("machine"), r.PathValue("session")
	limit := defaultRecordLimit
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = min(v, maxRecordLimit)
	}
	segs, err := o.Archive.Segments(machine)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("unknown machine in the archive"))
		return
	}
	sel := evidence.Selector{Machine: machine, Session: session}
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := evidence.ExportJSON(pw, o.Archive, sel)
		pw.CloseWithError(err) // nil closes with EOF
	}()
	recs, total, derr := decodeRecords(pr, limit)
	_ = pr.CloseWithError(errEnough)
	<-done
	if derr != nil && !errors.Is(derr, errEnough) {
		s.log.Warn("evidence: read records", "component", "panel", "machine", machine, "session", session, "err", derr)
		writeJSON(w, http.StatusBadGateway, errBody("the archive could not be read: "+derr.Error()))
		return
	}
	// A segment is filed in the archive only after its SHA-256 matched, so
	// every archived segment of the session counts as verified.
	count := 0
	for _, sg := range segs {
		for _, id := range append([]string{sg.Session}, sg.Sessions...) {
			if id == session {
				count++
				break
			}
		}
	}
	if count == 0 {
		writeJSON(w, http.StatusNotFound, errBody("no such session in the archive"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine": machine, "session": session, "records": recs, "returned": len(recs), "more": total > len(recs),
		"segments": count,
	})
}

// decodeRecords reads up to limit records from the JSON array stream and
// reports how many it saw (stopping one past the limit to know there is more).
func decodeRecords(rd io.Reader, limit int) ([]evRecord, int, error) {
	dec := json.NewDecoder(rd)
	if _, err := dec.Token(); err != nil { // '['
		return nil, 0, err
	}
	out := []evRecord{}
	total := 0
	for dec.More() {
		var row struct {
			Machine string `json:"machine"`
			capture.Record
		}
		if err := dec.Decode(&row); err != nil {
			return out, total, err
		}
		total++
		if len(out) >= limit {
			return out, total, nil
		}
		rec := row.Record
		out = append(out, evRecord{
			Time: rec.Timestamp, Protocol: rec.Protocol, Src: rec.Src, Dst: rec.Dst, Flags: rec.Flags,
			Seq: rec.Seq, Ack: rec.Ack, PayloadStatus: rec.PayloadStatus, PayloadBytes: rec.PayloadBytesFull,
			PayloadSHA256: rec.PayloadSHA256, PayloadHead: rec.PayloadHead, SrcIPUsed: rec.SrcIPUsed,
		})
	}
	return out, total, nil
}

type evSyncRow struct {
	Machine  string           `json:"machine"`
	State    string           `json:"state"` // the machine's connection state
	Archived int              `json:"archived_segments"`
	Bytes    int64            `json:"archived_bytes"`
	Sync     fleet.SyncStatus `json:"sync"`
}

func (s *server) evSync(w http.ResponseWriter, _ *http.Request, ob observer, o *fleet.Observe) {
	if o.Archive == nil {
		notConfigured(w, "the evidence archive")
		return
	}
	statuses := ob.SyncStatuses()
	rows := []evSyncRow{}
	for _, m := range s.be.Machines() {
		if m.Role != registry.RoleWorker || m.Removed {
			continue
		}
		row := evSyncRow{Machine: m.ID, State: m.State, Sync: fleet.SyncStatus{Machine: m.ID, State: "idle"}}
		if st, ok := statuses[m.ID]; ok {
			row.Sync = st
		}
		if segs, err := o.Archive.Segments(m.ID); err == nil {
			row.Archived = len(segs)
			for _, sg := range segs {
				row.Bytes += sg.Bytes
			}
		}
		rows = append(rows, row)
	}
	st, use := o.Archive.Guard()
	writeJSON(w, http.StatusOK, map[string]any{
		"workers": rows,
		"disk":    map[string]any{"state": st.String(), "used_percent": use, "warn_at": evidence.DefaultWarnPercent, "pause_at": evidence.DefaultPausePercent},
	})
}

type pullBody struct {
	Machine string `json:"machine"`
}

type pullResult struct {
	Machine string `json:"machine"`
	Started bool   `json:"started"`
	Error   string `json:"error,omitempty"`
}

func (s *server) evPull(w http.ResponseWriter, r *http.Request, ob observer, o *fleet.Observe) {
	if o.Archive == nil {
		notConfigured(w, "the evidence archive")
		return
	}
	var b pullBody
	if !decode(w, r, &b) {
		return
	}
	targets := []string{b.Machine}
	if b.Machine == "" {
		targets = nil
		for _, m := range s.be.Machines() {
			if m.Role == registry.RoleWorker && !m.Removed && m.State == fleet.StateOnline {
				targets = append(targets, m.ID)
			}
		}
	}
	s.log.Info("evidence pull", "component", "panel", "operator", operatorFrom(r), "machines", targets)
	results := make([]pullResult, 0, len(targets))
	started := 0
	var firstErr error
	for _, id := range targets {
		if err := ob.PullEvidence(id); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			results = append(results, pullResult{Machine: id, Error: err.Error()})
			continue
		}
		started++
		results = append(results, pullResult{Machine: id, Started: true})
	}
	status := http.StatusAccepted
	if started == 0 && firstErr != nil {
		status = statusFor(firstErr)
	}
	writeJSON(w, status, map[string]any{"results": results})
}

func (s *server) evManifest(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Archive == nil {
		notConfigured(w, "the evidence archive")
		return
	}
	machine := r.PathValue("machine")
	segs, err := o.Archive.Segments(machine)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errBody("unknown machine in the archive"))
		return
	}
	type seg struct {
		File    string    `json:"file"`
		Session string    `json:"session"`
		Start   time.Time `json:"start"`
		End     time.Time `json:"end"`
		Bytes   int64     `json:"bytes"`
		Records int       `json:"records"`
		Dropped uint64    `json:"dropped"`
		SHA256  string    `json:"sha256"`
	}
	out := make([]seg, 0, len(segs))
	for _, sg := range segs {
		out = append(out, seg{File: sg.File, Session: sg.Session, Start: sg.StartTime, End: sg.EndTime,
			Bytes: sg.Bytes, Records: sg.Records, Dropped: sg.Dropped, SHA256: sg.SHA256})
	}
	writeJSON(w, http.StatusOK, map[string]any{"machine": machine, "segments": out})
}

// evExport streams an export as a download. The format and selector come from
// the query; from and to are RFC 3339 (the page converts its local pickers).
func (s *server) evExport(w http.ResponseWriter, r *http.Request, _ observer, o *fleet.Observe) {
	if o.Archive == nil {
		notConfigured(w, "the evidence archive")
		return
	}
	q := r.URL.Query()
	sel := evidence.Selector{Machine: q.Get("machine"), Session: q.Get("session")}
	for name, dst := range map[string]*time.Time{"from": &sel.From, "to": &sel.To} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, errBody(name+" must be an RFC 3339 time"))
				return
			}
			*dst = t
		}
	}
	format := q.Get("format")
	var (
		ctype, ext string
		export     func(io.Writer, *evidence.Archive, evidence.Selector) (int, error)
	)
	switch format {
	case "pcapng":
		ctype, ext, export = "application/vnd.tcpdump.pcap", "pcapng", evidence.ExportPCAPNG
	case "csv":
		ctype, ext, export = "text/csv; charset=utf-8", "csv", evidence.ExportCSV
	case "json":
		ctype, ext, export = "application/json", "json", evidence.ExportJSON
	default:
		writeJSON(w, http.StatusBadRequest, errBody(`format must be "pcapng", "csv" or "json"`))
		return
	}
	name := "evidence"
	for _, part := range []string{sel.Machine, sel.Session} {
		if part != "" {
			name += "-" + safeFilePart(part)
		}
	}
	s.log.Info("evidence export", "component", "panel", "operator", operatorFrom(r), "format", format,
		"machine", sel.Machine, "session", sel.Session)
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.%s"`, name, ext))
	n, err := export(w, o.Archive, sel)
	if err != nil {
		// The status line is already sent; the truncated body is the signal.
		s.log.Warn("evidence export failed", "component", "panel", "format", format, "records", n, "err", err)
	}
}

func safeFilePart(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, s)
}
