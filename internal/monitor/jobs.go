package monitor

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/probe"
)

// Assignment limits (architecture 8.1): a monitor agent carries 2-3 targets.
const (
	MinTargetsPerAgent = 2
	MaxTargetsPerAgent = 3
)

// JobsFileName is the jobs file inside <DataDir>/monitor/ (D-0006).
const JobsFileName = "jobs.json"

const jobsFormatVersion = 1

// Errors returned by Jobs.
var (
	ErrIPRequired     = errors.New("monitor: ip is required")
	ErrBadIP          = errors.New("monitor: invalid ip")
	ErrBadOrigin      = errors.New("monitor: invalid origin (want scheme://host:port)")
	ErrBadPath        = errors.New("monitor: invalid path (want a path starting with /)")
	ErrPathNeedsOrg   = errors.New("monitor: paths need an origin")
	ErrDuplicate      = errors.New("monitor: a job for this (ip, origin) already exists")
	ErrNotFound       = errors.New("monitor: no such job")
	ErrNoCapacity     = errors.New("monitor: no monitor agent has room for the job's targets")
	ErrNoMonitorAgent = errors.New("monitor: no monitor agents available")
)

// Spec is what a caller supplies to create a job.
type Spec struct {
	// IP is required: the address the target machine answers on.
	IP string
	// Origin is optional: scheme://host:port. Default ports (80/443) are
	// filled in, so http://h and http://h:80 are the same origin.
	Origin string
	// Paths are the paths of the origin to probe over HTTP (default "/").
	// Only valid with an Origin.
	Paths []string
}

// SpecFromURL builds a Spec from the form the operator enters: an IP and a
// URL, from which the origin and path are derived.
func SpecFromURL(ip, rawURL string) (Spec, error) {
	if strings.TrimSpace(rawURL) == "" {
		return Spec{IP: ip}, nil
	}
	origin, path, err := SplitURL(rawURL)
	if err != nil {
		return Spec{}, err
	}
	return Spec{IP: ip, Origin: origin, Paths: []string{path}}, nil
}

// SplitURL derives the normalised origin and the request path (with query)
// from a URL.
func SplitURL(raw string) (origin, path string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrBadOrigin, err)
	}
	origin, err = originOf(u)
	if err != nil {
		return "", "", err
	}
	path = u.RequestURI()
	if path == "*" || path == "" {
		path = "/"
	}
	return origin, path, nil
}

// NormalizeOrigin validates an origin (scheme://host[:port], no path) and
// returns its canonical form with the port always present.
func NormalizeOrigin(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadOrigin, err)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%w: %q has a path, query or fragment", ErrBadOrigin, raw)
	}
	return originOf(u)
}

func originOf(u *url.URL) (string, error) {
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("%w: scheme must be http or https", ErrBadOrigin)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || u.User != nil {
		return "", fmt.Errorf("%w: missing host", ErrBadOrigin)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%w: bad port %q", ErrBadOrigin, port)
	}
	return scheme + "://" + net.JoinHostPort(host, port), nil
}

func normalizePath(p string) (string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " \t\r\n") {
		return "", fmt.Errorf("%w: %q", ErrBadPath, p)
	}
	return p, nil
}

// Event is one entry in a (ip, origin) history.
type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // created, path-added, deleted
	Job    string    `json:"job,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// AssignedTarget is a probe target together with the monitor agent that runs it.
type AssignedTarget struct {
	Agent  string       `json:"agent"`
	Target probe.Target `json:"target"`
}

// Job is a stateful monitor job. It is bound to a target identity, not to the
// duration of any test: it lives until deleted.
type Job struct {
	ID string `json:"id"`
	IP string `json:"ip"`
	// Origin is the canonical scheme://host:port, or empty.
	Origin string `json:"origin,omitempty"`
	// Paths are the origin paths probed over HTTP.
	Paths []string `json:"paths,omitempty"`
	// HistoryKey is the unique (ip, origin) history this job feeds.
	HistoryKey string           `json:"history_key"`
	Targets    []AssignedTarget `json:"targets"`
	CreatedAt  time.Time        `json:"created_at"`
}

// HistoryKey is the platform-wide unique history identity of (ip, origin).
func HistoryKey(ip, origin string) string { return ip + "|" + origin }

// Settings are the server-wide probe settings pushed to every monitor agent.
type Settings struct {
	IntervalMS int                `json:"interval_ms"`
	TimeoutMS  map[probe.Type]int `json:"timeout_ms"`
}

// DefaultSettings returns the initial settings: the documented fallbacks, now
// owned by the server.
func DefaultSettings() Settings {
	return Settings{
		IntervalMS: int(probe.FallbackInterval / time.Millisecond),
		TimeoutMS: map[probe.Type]int{
			probe.TypeHTTP: int(probe.FallbackHTTPTimeout / time.Millisecond),
			probe.TypeTCP:  int(probe.FallbackTCPTimeout / time.Millisecond),
			probe.TypeICMP: int(probe.FallbackICMPTimeout / time.Millisecond),
			probe.TypeDNS:  int(probe.FallbackDNSTimeout / time.Millisecond),
		},
	}
}

type jobsFile struct {
	Version   int                `json:"version"`
	NextSeq   int                `json:"next_seq"`
	Settings  Settings           `json:"settings"`
	Jobs      []Job              `json:"jobs"`
	Histories map[string][]Event `json:"histories,omitempty"`
}

// Jobs is the server's set of monitor jobs, persisted under
// <DataDir>/monitor/jobs.json (D-0006). All methods are safe for concurrent
// use; every change is written to disk before it takes effect.
type Jobs struct {
	dir string
	now func() time.Time

	mu        sync.Mutex
	seq       int
	settings  Settings
	byID      map[string]*Job
	histories map[string][]Event
}

// OpenJobs loads the jobs under dataDir, or starts empty if there is no file.
func OpenJobs(dataDir string) (*Jobs, error) {
	j := &Jobs{
		dir:       filepath.Join(dataDir, config.MonitorDirName),
		now:       time.Now,
		seq:       1,
		settings:  DefaultSettings(),
		byID:      map[string]*Job{},
		histories: map[string][]Event{},
	}
	data, err := os.ReadFile(j.path())
	if errors.Is(err, fs.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return nil, fmt.Errorf("monitor: read %s: %w", j.path(), err)
	}
	var f jobsFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("monitor: parse %s: %w", j.path(), err)
	}
	if f.Version != jobsFormatVersion {
		return nil, fmt.Errorf("monitor: %s has unsupported version %d", j.path(), f.Version)
	}
	keys := map[string]string{}
	for i := range f.Jobs {
		job := f.Jobs[i]
		if job.ID == "" || job.IP == "" {
			return nil, fmt.Errorf("monitor: %s: job %d has no id or ip", j.path(), i)
		}
		if _, dup := j.byID[job.ID]; dup {
			return nil, fmt.Errorf("monitor: %s: duplicate job id %q", j.path(), job.ID)
		}
		if other, dup := keys[job.HistoryKey]; dup {
			return nil, fmt.Errorf("monitor: %s: jobs %q and %q share (ip, origin) %q", j.path(), other, job.ID, job.HistoryKey)
		}
		keys[job.HistoryKey] = job.ID
		j.byID[job.ID] = &job
	}
	j.seq = max(f.NextSeq, 1)
	if f.Settings.IntervalMS > 0 {
		j.settings = f.Settings
	}
	if f.Histories != nil {
		j.histories = f.Histories
	}
	return j, nil
}

// Dir is the monitor directory; Path the jobs file.
func (j *Jobs) Dir() string  { return j.dir }
func (j *Jobs) path() string { return filepath.Join(j.dir, JobsFileName) }

// Create makes a job from spec and assigns its targets to monitor agents from
// agents (ids of the monitor agents currently registered). It fails, changing
// nothing, if ip is missing or invalid, the origin or a path is malformed, a
// job for the same (ip, origin) exists, or the agents lack room.
func (j *Jobs) Create(spec Spec, agents []string) (Job, error) {
	ip := strings.TrimSpace(spec.IP)
	if ip == "" {
		return Job{}, ErrIPRequired
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return Job{}, fmt.Errorf("%w: %q", ErrBadIP, ip)
	}
	ip = addr.Unmap().String()

	origin := ""
	if strings.TrimSpace(spec.Origin) != "" {
		if origin, err = NormalizeOrigin(spec.Origin); err != nil {
			return Job{}, err
		}
	}
	var paths []string
	for _, p := range spec.Paths {
		if origin == "" {
			return Job{}, ErrPathNeedsOrg
		}
		if p, err = normalizePath(p); err != nil {
			return Job{}, err
		}
		if !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	if origin != "" && len(paths) == 0 {
		paths = []string{"/"}
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	key := HistoryKey(ip, origin)
	for _, other := range j.byID {
		if other.HistoryKey == key {
			return Job{}, fmt.Errorf("%w: %s (job %s)", ErrDuplicate, key, other.ID)
		}
	}
	job := Job{
		ID:         fmt.Sprintf("mj-%04d", j.seq),
		IP:         ip,
		Origin:     origin,
		Paths:      paths,
		HistoryKey: key,
		CreatedAt:  j.now().UTC(),
	}
	load := j.loadLocked()
	for _, t := range initialTargets(job) {
		agent, ok := pickAgent(agents, load)
		if !ok {
			if len(agents) == 0 {
				return Job{}, ErrNoMonitorAgent
			}
			return Job{}, ErrNoCapacity
		}
		load[agent]++
		job.Targets = append(job.Targets, AssignedTarget{Agent: agent, Target: t})
	}
	stored := job
	if err := j.commitLocked(func() {
		j.seq++
		j.byID[stored.ID] = &stored
		j.histories[key] = append(j.histories[key], Event{Time: stored.CreatedAt, Kind: "created", Job: stored.ID})
	}); err != nil {
		return Job{}, err
	}
	return *cloneJob(&job), nil
}

// AddPath adds another path of the job's origin (several paths of one origin
// are allowed under the one job).
func (j *Jobs) AddPath(id, path string, agents []string) (Job, error) {
	path, err := normalizePath(path)
	if err != nil {
		return Job{}, err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[id]
	if !ok {
		return Job{}, ErrNotFound
	}
	if job.Origin == "" {
		return Job{}, ErrPathNeedsOrg
	}
	if slices.Contains(job.Paths, path) {
		return *cloneJob(job), nil
	}
	load := j.loadLocked()
	agent, ok := pickAgent(agents, load)
	if !ok {
		if len(agents) == 0 {
			return Job{}, ErrNoMonitorAgent
		}
		return Job{}, ErrNoCapacity
	}
	t := httpTarget(*job, len(job.Paths), path)
	now := j.now().UTC()
	if err := j.commitLocked(func() {
		job.Paths = append(job.Paths, path)
		job.Targets = append(job.Targets, AssignedTarget{Agent: agent, Target: t})
		j.histories[job.HistoryKey] = append(j.histories[job.HistoryKey], Event{Time: now, Kind: "path-added", Job: job.ID, Detail: path})
	}); err != nil {
		return Job{}, err
	}
	return *cloneJob(j.byID[id]), nil
}

// Delete removes a job. Its (ip, origin) history is kept, so a later job for
// the same identity continues the same history.
func (j *Jobs) Delete(id string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[id]
	if !ok {
		return ErrNotFound
	}
	now := j.now().UTC()
	return j.commitLocked(func() {
		delete(j.byID, id)
		j.histories[job.HistoryKey] = append(j.histories[job.HistoryKey], Event{Time: now, Kind: "deleted", Job: id})
	})
}

// Get returns a copy of the job.
func (j *Jobs) Get(id string) (Job, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[id]
	if !ok {
		return Job{}, false
	}
	return *cloneJob(job), true
}

// Find returns the job for an exact (ip, origin) identity. origin "" finds the
// origin-less job. ip and origin are normalised first.
func (j *Jobs) Find(ip, origin string) (Job, bool) {
	if addr, err := netip.ParseAddr(strings.TrimSpace(ip)); err == nil {
		ip = addr.Unmap().String()
	}
	if strings.TrimSpace(origin) != "" {
		if o, err := NormalizeOrigin(origin); err == nil {
			origin = o
		}
	}
	key := HistoryKey(ip, origin)
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, job := range j.byID {
		if job.HistoryKey == key {
			return *cloneJob(job), true
		}
	}
	return Job{}, false
}

// List returns all jobs ordered by id.
func (j *Jobs) List() []Job {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Job, 0, len(j.byID))
	for _, job := range j.byID {
		out = append(out, *cloneJob(job))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// History returns the events of an (ip, origin) history, oldest first.
func (j *Jobs) History(ip, origin string) []Event {
	if addr, err := netip.ParseAddr(strings.TrimSpace(ip)); err == nil {
		ip = addr.Unmap().String()
	}
	if o, err := NormalizeOrigin(origin); err == nil && origin != "" {
		origin = o
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.histories[HistoryKey(ip, origin)])
}

// HistoryKeyFor maps a target id (as pushed in a probe.Config) to the
// (ip, origin) history its results belong to. It implements HistoryResolver.
func (j *Jobs) HistoryKeyFor(targetID string) (string, bool) {
	id, _, _ := strings.Cut(targetID, "/")
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[id]
	if !ok {
		return "", false
	}
	return job.HistoryKey, true
}

// AgentLoad returns the number of targets assigned to each monitor agent.
func (j *Jobs) AgentLoad() map[string]int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.loadLocked()
}

// Settings returns the server-wide probe settings.
func (j *Jobs) Settings() Settings {
	j.mu.Lock()
	defer j.mu.Unlock()
	return cloneSettings(j.settings)
}

// SetSettings replaces and persists the server-wide probe settings (global
// default interval and per-type timeouts).
func (j *Jobs) SetSettings(s Settings) error {
	if s.IntervalMS <= 0 {
		return errors.New("monitor: interval_ms must be positive")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	s = cloneSettings(s)
	return j.commitLocked(func() { j.settings = s })
}

// SetTargetInterval sets the per-target frequency override for one target (0
// clears it); the next ConfigFor carries it.
func (j *Jobs) SetTargetInterval(jobID, targetID string, interval time.Duration) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	job, ok := j.byID[jobID]
	if !ok {
		return ErrNotFound
	}
	for i := range job.Targets {
		if job.Targets[i].Target.ID == targetID {
			ms := int(interval / time.Millisecond)
			return j.commitLocked(func() { job.Targets[i].Target.IntervalMS = ms })
		}
	}
	return ErrNotFound
}

// ConfigFor builds the probe.Config to push to one monitor agent: the global
// frequency and timeouts plus the targets assigned to it.
func (j *Jobs) ConfigFor(agent string) probe.Config {
	j.mu.Lock()
	defer j.mu.Unlock()
	cfg := probe.Config{
		IntervalMS: j.settings.IntervalMS,
		TimeoutMS:  cloneSettings(j.settings).TimeoutMS,
		Targets:    []probe.Target{},
	}
	ids := make([]string, 0, len(j.byID))
	for id := range j.byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, at := range j.byID[id].Targets {
			if at.Agent == agent {
				cfg.Targets = append(cfg.Targets, at.Target)
			}
		}
	}
	return cfg
}

// loadLocked counts targets per agent across all jobs.
func (j *Jobs) loadLocked() map[string]int {
	load := map[string]int{}
	for _, job := range j.byID {
		for _, at := range job.Targets {
			load[at.Agent]++
		}
	}
	return load
}

// pickAgent chooses the agent for the next target. It packs: the most loaded
// agent that still has room wins, so agents fill to MaxTargetsPerAgent before
// a fresh one is started and carry 2-3 targets. Ties go to the lowest id.
func pickAgent(agents []string, load map[string]int) (string, bool) {
	best, bestLoad := "", -1
	for _, a := range agents {
		l := load[a]
		if l >= MaxTargetsPerAgent {
			continue
		}
		if l > bestLoad || (l == bestLoad && a < best) {
			best, bestLoad = a, l
		}
	}
	return best, bestLoad >= 0
}

// initialTargets derives the probe targets of a new job: ICMP to the ip, and
// when there is an origin a TCP connect to its port and one HTTP probe per
// path. HTTP and TCP connect to the job's ip while carrying the origin host.
func initialTargets(job Job) []probe.Target {
	ts := []probe.Target{{ID: job.ID + "/icmp", Type: probe.TypeICMP, Address: job.IP}}
	if job.Origin == "" {
		return ts
	}
	u, _ := url.Parse(job.Origin) // already normalised
	ts = append(ts, probe.Target{ID: job.ID + "/tcp", Type: probe.TypeTCP, Address: u.Host, IP: job.IP})
	for i, p := range job.Paths {
		ts = append(ts, httpTarget(job, i, p))
	}
	return ts
}

func httpTarget(job Job, n int, path string) probe.Target {
	return probe.Target{ID: fmt.Sprintf("%s/http/%d", job.ID, n), Type: probe.TypeHTTP, Address: job.Origin + path, IP: job.IP}
}

func cloneJob(job *Job) *Job {
	c := *job
	c.Paths = slices.Clone(job.Paths)
	c.Targets = slices.Clone(job.Targets)
	return &c
}

func cloneSettings(s Settings) Settings {
	c := Settings{IntervalMS: s.IntervalMS, TimeoutMS: map[probe.Type]int{}}
	for k, v := range s.TimeoutMS {
		c.TimeoutMS[k] = v
	}
	return c
}

// commitLocked applies mutate and persists the result atomically. If saving
// fails the in-memory state is restored, so memory never runs ahead of disk.
func (j *Jobs) commitLocked(mutate func()) error {
	before := j.snapshotLocked()
	mutate()
	if err := j.saveLocked(); err != nil {
		j.restoreLocked(before)
		return err
	}
	return nil
}

func (j *Jobs) snapshotLocked() jobsFile {
	f := jobsFile{Version: jobsFormatVersion, NextSeq: j.seq, Settings: cloneSettings(j.settings), Histories: map[string][]Event{}}
	for _, job := range j.byID {
		f.Jobs = append(f.Jobs, *cloneJob(job))
	}
	sort.Slice(f.Jobs, func(a, b int) bool { return f.Jobs[a].ID < f.Jobs[b].ID })
	for k, v := range j.histories {
		f.Histories[k] = slices.Clone(v)
	}
	return f
}

func (j *Jobs) restoreLocked(f jobsFile) {
	j.seq, j.settings = f.NextSeq, f.Settings
	j.byID = map[string]*Job{}
	for i := range f.Jobs {
		job := f.Jobs[i]
		j.byID[job.ID] = &job
	}
	j.histories = f.Histories
}

// saveLocked writes the jobs file atomically (temp file in the same
// directory, fsync, rename).
func (j *Jobs) saveLocked() error {
	data, err := json.MarshalIndent(j.snapshotLocked(), "", "  ")
	if err != nil {
		return fmt.Errorf("monitor: encode jobs: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(j.dir, 0o750); err != nil {
		return fmt.Errorf("monitor: create %s: %w", j.dir, err)
	}
	tmp, err := os.CreateTemp(j.dir, JobsFileName+".*.tmp")
	if err != nil {
		return fmt.Errorf("monitor: temp file: %w", err)
	}
	name := tmp.Name()
	fail := func(what string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("monitor: %s: %w", what, err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		return fail("chmod temp file", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail("write temp file", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync temp file", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("monitor: close temp file: %w", err)
	}
	if err := os.Rename(name, j.path()); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("monitor: replace %s: %w", j.path(), err)
	}
	if d, err := os.Open(j.dir); err == nil { // best effort: persist the rename
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
