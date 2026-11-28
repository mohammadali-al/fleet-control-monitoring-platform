package panel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/maneuver"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/monitor"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/project"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/report"
)

// Test report API (internal/report, T-0062):
//
//	GET /api/projects/{id}/tests/{testId}/report           render interactive HTML report inline
//	GET /api/projects/{id}/tests/{testId}/report?download=1 download HTML report as an attachment
func (s *server) routesReport(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects/{id}/tests/{testId}/report", s.live(s.getProjectTestReport))
}

func (s *server) getProjectTestReport(w http.ResponseWriter, r *http.Request) {
	st, err := s.projectStore()
	if err != nil {
		s.writeProjectErr(w, err)
		return
	}
	projID := r.PathValue("id")
	testID := r.PathValue("testId")
	proj, ok := st.Get(projID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("project not found"))
		return
	}
	test, ok := st.GetTest(projID, testID)
	if !ok {
		writeJSON(w, http.StatusNotFound, errBody("test not found"))
		return
	}
	if !project.IsFinishedStatus(test.Status) {
		writeJSON(w, http.StatusNotFound, errBody("report is only available for completed tests"))
		return
	}

	var ob observer
	var obs *fleet.Observe
	if o, ok := s.be.(observer); ok {
		ob = o
		obs = o.Observe()
	}

	evFrom, evTo, evMachines, evRunID := evidenceWindowForTest(test)
	monFrom, monTo, monKey, monJobID := monitorWindowForTest(test)

	var paths []string
	if obs != nil && obs.Jobs != nil && monJobID != "" {
		if job, ok := obs.Jobs.Get(monJobID); ok {
			if monKey == "" {
				monKey = job.HistoryKey
			}
			paths = append(paths, job.Paths...)
		}
	}

	var runPtr *fleet.ManeuverRun
	if evRunID != "" {
		if mr, ok := s.be.(maneuverRunner); ok {
			if run, ok := mr.ManeuverRun(evRunID); ok {
				runPtr = &run
			}
		}
	}

	plan, matPlan := s.loadReportPlan(test)
	workers := collectReportWorkers(evMachines, test.Agents, runPtr)

	var archive *evidence.Archive
	if obs != nil {
		archive = obs.Archive
	}

	workerIDs := make([]string, len(workers))
	for i, wk := range workers {
		workerIDs[i] = wk.Machine
	}
	cmds := s.collectReportCommands(workerIDs, evFrom, evTo, evRunID, test, runPtr, matPlan, archive)
	alignment := alignmentFromManeuverRun(runPtr)
	watchdog := collectReportWatchdog(ob, workerIDs)

	var notes []string
	targetIP := strings.TrimSpace(test.Target.IP)
	if targetIP == "" {
		targetIP = strings.TrimSpace(test.Monitor.IP)
	}
	targetOrigin := strings.TrimSpace(test.Target.Origin)
	if targetOrigin == "" {
		targetOrigin = strings.TrimSpace(test.Monitor.Origin)
	}
	targetHistoryKey := monitor.HistoryKey(targetIP, targetOrigin)
	if monKey == "" {
		monKey = targetHistoryKey
	}

	samples, monNote := s.loadReportMonitorSamples(r.Context(), obs, monKey, targetHistoryKey, monFrom, monTo)
	if monNote != "" {
		notes = append(notes, monNote)
	}

	spec := report.SpecFromProjectTest(proj, test, report.ProjectTestOptions{
		Plan:      plan,
		Paths:     paths,
		Workers:   workers,
		Commands:  cmds,
		Alignment: alignment,
		Watchdog:  watchdog,
	})
	src := report.Sources{
		Archive: archive,
		Samples: samples,
	}

	data, err := report.Gather(spec, src)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, errBody("could not gather report data: "+err.Error()))
		return
	}
	if len(notes) > 0 {
		data.Notes = append(data.Notes, notes...)
	}

	var buf bytes.Buffer
	if err := report.Render(&buf, data); err != nil {
		s.log.Warn("render project test report", "component", "panel", "project", projID, "test", testID, "err", err)
		writeJSON(w, http.StatusInternalServerError, errBody("could not render report: "+err.Error()))
		return
	}

	filename := fmt.Sprintf("report-%s-%s.html", safeFilePart(proj.ID), safeFilePart(test.ID))
	disposition := "inline"
	if q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("download"))); q == "1" || q == "true" {
		disposition = "attachment"
	}

	s.log.Info("project test report generated", "component", "panel", "operator", operatorFrom(r),
		"project", projID, "test", testID, "disposition", disposition)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func evidenceWindowForTest(t project.Test) (from, to time.Time, machines []string, runID string) {
	if t.StartedAt != nil {
		from = t.StartedAt.UTC()
	}
	if t.EndedAt != nil {
		to = t.EndedAt.UTC()
	}
	runID = t.ManeuverRunID
	machines = append(machines, t.Agents...)
	if t.ResultPointers != nil && t.ResultPointers.Evidence != nil {
		ev := t.ResultPointers.Evidence
		if !ev.From.IsZero() {
			from = ev.From.UTC()
		}
		if ev.To != nil && !ev.To.IsZero() {
			to = ev.To.UTC()
		}
		if len(ev.Machines) > 0 {
			machines = append(machines, ev.Machines...)
		}
		if ev.ManeuverRunID != "" {
			runID = ev.ManeuverRunID
		}
	}
	if !from.IsZero() && (to.IsZero() || to.Before(from)) {
		to = from
	}
	return from, to, machines, runID
}

func monitorWindowForTest(t project.Test) (from, to time.Time, historyKey, jobID string) {
	if t.StartedAt != nil {
		from = t.StartedAt.UTC()
	}
	if t.EndedAt != nil {
		to = t.EndedAt.UTC()
	}
	historyKey = t.Monitor.HistoryKey
	jobID = t.Monitor.JobID
	if t.ResultPointers != nil && t.ResultPointers.Monitor != nil {
		mw := t.ResultPointers.Monitor
		if !mw.From.IsZero() {
			from = mw.From.UTC()
		}
		if mw.To != nil && !mw.To.IsZero() {
			to = mw.To.UTC()
		}
		if mw.HistoryKey != "" {
			historyKey = mw.HistoryKey
		}
		if mw.JobID != "" {
			jobID = mw.JobID
		}
	}
	if !from.IsZero() && (to.IsZero() || to.Before(from)) {
		to = from
	}
	return from, to, historyKey, jobID
}

func (s *server) loadReportPlan(t project.Test) (*report.Plan, *maneuver.MaterializedPlan) {
	if t.ManeuverID == "" {
		return nil, nil
	}
	ms, err := s.maneuverStore()
	if err != nil {
		return nil, nil
	}
	mv, ok := ms.Get(t.ManeuverID)
	if !ok {
		return nil, nil
	}
	mat, err := s.validateAndMaterializeSpec(maneuver.Spec{
		ID:          mv.ID,
		Name:        mv.Name,
		Description: mv.Description,
		Target:      mv.Target,
		TotalSec:    mv.TotalSec,
		Pattern:     mv.Pattern,
		Timeline:    mv.Timeline,
		Agents:      mv.Agents,
	})
	if err != nil {
		return report.PlanFromManeuver(mv, nil), nil
	}
	return report.PlanFromManeuver(mv, &mat), &mat
}

func collectReportWorkers(evMachines, agents []string, run *fleet.ManeuverRun) []report.Worker {
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, id := range evMachines {
		add(id)
	}
	for _, id := range agents {
		add(id)
	}
	if run != nil {
		for _, id := range run.SortedAgents() {
			add(id)
		}
	}
	sort.Strings(ids)
	out := make([]report.Worker, len(ids))
	for i, id := range ids {
		out[i] = report.Worker{Machine: id}
	}
	return out
}

func (s *server) collectReportCommands(
	workerIDs []string,
	from, to time.Time,
	runID string,
	test project.Test,
	run *fleet.ManeuverRun,
	mat *maneuver.MaterializedPlan,
	archive *evidence.Archive,
) []report.Command {
	workerSet := map[string]bool{}
	for _, id := range workerIDs {
		workerSet[id] = true
	}

	seen := map[string]bool{}
	var cmds []report.Command

	if s.be != nil {
		for _, c := range s.be.Commands() {
			if len(workerSet) > 0 && !workerSet[c.Machine] {
				continue
			}
			cStart := c.Queued
			if c.Started != nil && !c.Started.IsZero() {
				cStart = *c.Started
			}
			cEnd := to
			if c.Finished != nil && !c.Finished.IsZero() {
				cEnd = *c.Finished
			}
			if !from.IsZero() && !to.IsZero() {
				if cEnd.Before(from) || cStart.After(to) {
					continue
				}
			}
			exitCode := 0
			if c.Exit != nil {
				exitCode = c.Exit.ExitCode
			}
			seen[c.Session] = true
			cmds = append(cmds, report.Command{
				SessionID: c.Session,
				Machine:   c.Machine,
				Command:   c.Command,
				User:      c.User,
				Mode:      c.Mode,
				Start:     cStart.UTC(),
				End:       cEnd.UTC(),
				ExitCode:  exitCode,
			})
		}
	}

	itemCmds := map[string]string{}
	if mat != nil {
		for _, it := range mat.Items {
			itemCmds[it.ItemID] = it.Command
		}
	}

	if archive != nil && len(workerIDs) > 0 {
		discovered := discoverArchiveSessions(archive, workerIDs, from, to, runID, seen)
		for _, ds := range discovered {
			cmdText := itemCmds[ds.itemID]
			if cmdText == "" {
				cmdText = test.ManeuverName
			}
			if cmdText == "" {
				cmdText = test.ManeuverID
			}
			if cmdText == "" {
				cmdText = "maneuver replica"
			}
			user := test.StartedBy
			if user == "" {
				user = "maneuver"
			}
			mode := "replica"
			if ds.runID == "" {
				mode = "captured"
			}
			seen[ds.sessionID] = true
			cmds = append(cmds, report.Command{
				SessionID: ds.sessionID,
				Machine:   ds.machine,
				Command:   cmdText,
				User:      user,
				Mode:      mode,
				Start:     ds.start,
				End:       ds.end,
				ExitCode:  0,
			})
		}
	}

	if run != nil {
		for _, machine := range run.SortedAgents() {
			ag := run.Agents[machine]
			for _, sid := range ag.SessionIDs {
				if sid == "" || seen[sid] {
					continue
				}
				seen[sid] = true
				cmdText := test.ManeuverName
				if cmdText == "" && mat != nil && len(mat.Items) > 0 {
					cmdText = mat.Items[0].Command
				}
				cmds = append(cmds, report.Command{
					SessionID: sid,
					Machine:   machine,
					Command:   cmdText,
					User:      run.User,
					Mode:      "replica",
					Start:     from,
					End:       to,
				})
			}
		}
	}

	return cmds
}

type discoveredSession struct {
	sessionID string
	machine   string
	itemID    string
	runID     string
	start     time.Time
	end       time.Time
}

func discoverArchiveSessions(
	archive *evidence.Archive,
	workerIDs []string,
	from, to time.Time,
	runID string,
	alreadySeen map[string]bool,
) []discoveredSession {
	held, err := archive.Machines()
	if err != nil {
		return nil
	}
	inArchive := map[string]bool{}
	for _, m := range held {
		inArchive[m] = true
	}

	type key struct{ machine, session string }
	found := map[key]*discoveredSession{}
	var order []key

	for _, machine := range workerIDs {
		if !inArchive[machine] {
			continue
		}
		segs, err := archive.Segments(machine)
		if err != nil {
			continue
		}
		for _, seg := range segs {
			if !from.IsZero() && !to.IsZero() && (seg.EndTime.Before(from) || seg.StartTime.After(to)) {
				continue
			}
			f, err := archive.OpenSegment(machine, seg)
			if err != nil {
				continue
			}
			_ = scanSegmentRecords(f, func(rec capture.Record) {
				if rec.SessionID == "" || alreadySeen[rec.SessionID] {
					return
				}
				if runID != "" && rec.RunID != "" {
					if rec.RunID != runID {
						return
					}
				} else if !from.IsZero() && !to.IsZero() {
					if rec.Timestamp.Before(from) || rec.Timestamp.After(to) {
						return
					}
				}
				k := key{machine, rec.SessionID}
				ds := found[k]
				if ds == nil {
					ds = &discoveredSession{
						sessionID: rec.SessionID,
						machine:   machine,
						itemID:    rec.ItemID,
						runID:     rec.RunID,
						start:     rec.Timestamp.UTC(),
						end:       rec.Timestamp.UTC(),
					}
					found[k] = ds
					order = append(order, k)
					return
				}
				ts := rec.Timestamp.UTC()
				if ts.Before(ds.start) {
					ds.start = ts
				}
				if ts.After(ds.end) {
					ds.end = ts
				}
				if ds.itemID == "" && rec.ItemID != "" {
					ds.itemID = rec.ItemID
				}
				if ds.runID == "" && rec.RunID != "" {
					ds.runID = rec.RunID
				}
			})
			_ = f.Close()
		}
	}

	out := make([]discoveredSession, 0, len(order))
	for _, k := range order {
		out = append(out, *found[k])
	}
	return out
}

func scanSegmentRecords(r io.Reader, fn func(capture.Record)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var rec capture.Record
			if jerr := json.Unmarshal(line, &rec); jerr == nil {
				fn(rec)
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func alignmentFromManeuverRun(run *fleet.ManeuverRun) *report.Alignment {
	if run == nil || run.StartAt.IsZero() {
		return nil
	}
	al := &report.Alignment{
		Intended: run.StartAt.UTC(),
		Agents:   make([]report.AgentStart, 0, len(run.Agents)),
	}
	for _, machine := range run.SortedAgents() {
		st := run.Agents[machine]
		switch st.State {
		case channel.ManeuverAckMissed:
			al.Agents = append(al.Agents, report.AgentStart{
				Machine: machine,
				Missed:  true,
				Note:    st.Reason,
			})
		case channel.ManeuverAckRejected, "offline", "pending":
			note := st.Reason
			if note == "" {
				note = st.State
			}
			al.Agents = append(al.Agents, report.AgentStart{
				Machine: machine,
				Note:    note,
			})
		default:
			al.Agents = append(al.Agents, report.AgentStart{
				Machine:   machine,
				StartedAt: run.StartAt.UTC(),
				Note:      st.Reason,
			})
		}
	}
	return al
}

func collectReportWatchdog(ob observer, workerIDs []string) []report.WatchdogEvent {
	if ob == nil {
		return nil
	}
	var out []report.WatchdogEvent
	for _, machine := range workerIDs {
		for _, ev := range ob.WatchdogEvents(machine) {
			t := ev.Time
			if t.IsZero() {
				t = ev.Received
			}
			action := ev.Step
			if action == "" {
				action = ev.Kind
			}
			sessionID := ""
			if ev.Scope == "session" || strings.HasPrefix(ev.Target, "s-") {
				sessionID = ev.Target
			}
			out = append(out, report.WatchdogEvent{
				Time:      t.UTC(),
				Machine:   machine,
				Action:    action,
				Detail:    ev.Detail,
				SessionID: sessionID,
			})
		}
	}
	return out
}

func (s *server) loadReportMonitorSamples(
	ctx context.Context,
	obs *fleet.Observe,
	queryKey, targetKey string,
	from, to time.Time,
) ([]monitor.Sample, string) {
	if obs == nil || from.IsZero() || to.IsZero() || to.Before(from) {
		return nil, ""
	}
	if obs.Store != nil && queryKey != "" {
		qFrom := from.Truncate(time.Millisecond)
		qTo := to
		if qTo.Nanosecond()%int(time.Millisecond) != 0 {
			qTo = qTo.Truncate(time.Millisecond).Add(time.Millisecond)
		}
		okSeries, okErr := obs.Store.QueryRange(ctx, monitor.HistoryQuery{
			Metric:     monitor.MetricOK,
			HistoryKey: queryKey,
			Start:      qFrom,
			End:        qTo,
		})
		latSeries, latErr := obs.Store.QueryRange(ctx, monitor.HistoryQuery{
			Metric:     monitor.MetricLatencyMs,
			HistoryKey: queryKey,
			Start:      qFrom,
			End:        qTo,
		})
		if okErr != nil && latErr != nil {
			s.log.Warn("report: monitor history query failed", "component", "panel", "history_key", queryKey, "err", okErr)
			return nil, fmt.Sprintf("Monitor history could not be read: %v", okErr)
		}
		var combined []monitor.HistorySeries
		if okErr == nil {
			combined = append(combined, okSeries...)
		}
		if latErr == nil {
			combined = append(combined, latSeries...)
		}
		return report.SamplesFromHistory(combined, targetKey), ""
	}

	if obs.Series != nil {
		snap := obs.Series.Snapshot(from)
		var samples []monitor.Sample
		for _, sd := range snap {
			if sd.HistoryKey != queryKey && sd.HistoryKey != targetKey {
				continue
			}
			for _, pt := range sd.Points {
				if pt.T.After(to) {
					continue
				}
				samples = append(samples, monitor.Sample{
					Agent:      sd.Agent,
					HistoryKey: targetKey,
					Result: probe.Result{
						TargetID: sd.Target,
						Type:     sd.Type,
						Time:     pt.T.UTC(),
						OK:       pt.OK,
						Latency:  time.Duration(pt.Ms * float64(time.Millisecond)),
						Status:   pt.Status,
						Error:    pt.Error,
						Loss:     pt.Loss,
					},
				})
			}
		}
		return samples, ""
	}
	return nil, ""
}
