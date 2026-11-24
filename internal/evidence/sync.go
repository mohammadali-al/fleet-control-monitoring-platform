package evidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/capture"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
)

// The delta-sync protocol (§7.2), one for both directions of initiative:
//
//	pull:  server -> ev-manifest-req
//	       agent  -> ev-manifest            (reply, same id)
//	push:  agent  -> ev-manifest            (unprompted offer, id "push-N")
//
//	then, identically for both, for each segment the server lacks or holds with
//	a different SHA-256:
//	       server -> ev-range  {file, sha256, offset}
//	       agent  -> ev-chunk* {file, offset, data}, the last flagged eof
//	       server -> ev-verify {file, ok}   after hashing the whole segment
//
// Bytes always flow agent -> server. The server owns the archive, so it alone
// decides what is missing and where to resume; an idle push is just the agent
// volunteering its manifest, after which the server runs the very same
// diff-and-fetch routine as a pull.

// Sender writes an envelope to the peer. *channel.Dialer and *channel.Listener
// both satisfy it.
type Sender interface {
	Send(ctx context.Context, env channel.Envelope) error
}

// DefaultChunkSize is the slice of a segment carried by one ev-chunk. It stays
// far below the channel's 4 MiB envelope cap even after base64 encoding.
const DefaultChunkSize = 256 << 10

// DefaultTimeout bounds how long the server waits for the next message of a
// transfer before treating the channel as lost.
const DefaultTimeout = 30 * time.Second

// ErrTimeout means the peer went quiet mid-exchange. Partial segment bytes are
// kept so the next sync resumes where this one stopped.
var ErrTimeout = errors.New("evidence: timed out waiting for the peer")

func send(ctx context.Context, s Sender, typ, id string, payload any) error {
	env, err := channel.NewEnvelope(typ, id, payload)
	if err != nil {
		return err
	}
	return s.Send(ctx, env)
}

// =========================== agent side ===========================

// AgentConfig configures the agent-side sync endpoint.
type AgentConfig struct {
	Machine   string
	Dir       string      // DataDir/evidence/<machine>: segments + manifest.json
	Sender    Sender      // the channel back to the main server
	ChunkSize int         // default DefaultChunkSize
	Idle      func() bool // true when no session is running and the queue is empty; nil never pushes
	Log       *slog.Logger
}

// Agent serves the sync protocol from a worker agent's local evidence.
type Agent struct {
	cfg AgentConfig
	log *slog.Logger

	pushSeq     atomic.Uint64
	bytesServed atomic.Int64

	mu     sync.Mutex
	synced map[string]bool // file+"@"+sha the server confirmed in ev-verify
}

// NewAgent validates cfg.
func NewAgent(cfg AgentConfig) (*Agent, error) {
	if cfg.Machine == "" || cfg.Dir == "" || cfg.Sender == nil {
		return nil, errors.New("evidence: agent needs a machine, a dir and a sender")
	}
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = DefaultChunkSize
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Agent{cfg: cfg, log: log, synced: map[string]bool{}}, nil
}

// BytesServed is the total segment bytes sent in chunks so far.
func (a *Agent) BytesServed() int64 { return a.bytesServed.Load() }

// Handle processes one envelope from the main server. It reports whether the
// envelope was an evidence-sync message; install it (alongside the other
// handlers) as the channel Handler. It never blocks the read loop on bulk work.
func (a *Agent) Handle(ctx context.Context, env channel.Envelope) bool {
	switch env.Type {
	case channel.TypeEvManifestReq:
		if err := a.sendManifest(ctx, env.ID, a.allEntries); err != nil {
			a.log.Warn("evidence: manifest reply failed", "err", err)
		}
	case channel.TypeEvRange:
		var r RangeMsg
		if err := json.Unmarshal(env.Payload, &r); err != nil {
			a.log.Warn("evidence: bad ev-range", "err", err)
			return true
		}
		go func() {
			if err := a.serveRange(ctx, env.ID, r); err != nil {
				a.log.Warn("evidence: serve range failed", "file", r.File, "err", err)
			}
		}()
	case channel.TypeEvVerify:
		var v VerifyMsg
		if err := json.Unmarshal(env.Payload, &v); err != nil {
			return true
		}
		if v.OK {
			a.markSynced(v.File)
		} else {
			a.log.Warn("evidence: server rejected segment", "file", v.File, "err", v.Err)
		}
	default:
		return false
	}
	return true
}

func syncKey(e capture.SegmentEntry) string { return e.File + "@" + e.SHA256 }

// markSynced remembers a confirmed segment (looked up by file in the current
// manifest so the digest is the one that was verified).
func (a *Agent) markSynced(file string) {
	m, err := capture.LoadManifest(a.cfg.Dir, a.cfg.Machine)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range m.Entries() {
		if e.File == file {
			a.synced[syncKey(e)] = true
		}
	}
}

func (a *Agent) allEntries() ([]capture.SegmentEntry, error) {
	m, err := capture.LoadManifest(a.cfg.Dir, a.cfg.Machine)
	if err != nil {
		return nil, err
	}
	return m.Entries(), nil
}

func (a *Agent) pendingEntries() ([]capture.SegmentEntry, error) {
	all, err := a.allEntries()
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []capture.SegmentEntry
	for _, e := range all {
		if !a.synced[syncKey(e)] {
			out = append(out, e)
		}
	}
	return out, nil
}

func (a *Agent) sendManifest(ctx context.Context, id string, entries func() ([]capture.SegmentEntry, error)) error {
	segs, err := entries()
	if err != nil {
		return err
	}
	return send(ctx, a.cfg.Sender, channel.TypeEvManifest, id, ManifestMsg{Machine: a.cfg.Machine, Segments: segs})
}

// PushIfIdle is the idle push: when the agent is idle and holds segments the
// server has not confirmed, it offers its manifest unprompted; the server then
// fetches what it lacks over the same range/chunk/verify exchange as a pull. A
// busy (or Idle-less) agent sends nothing. It reports whether an offer was sent.
func (a *Agent) PushIfIdle(ctx context.Context) (bool, error) {
	if a.cfg.Idle == nil || !a.cfg.Idle() {
		return false, nil
	}
	pending, err := a.pendingEntries()
	if err != nil || len(pending) == 0 {
		return false, err
	}
	id := fmt.Sprintf("push-%d", a.pushSeq.Add(1))
	err = send(ctx, a.cfg.Sender, channel.TypeEvManifest, id, ManifestMsg{Machine: a.cfg.Machine, Segments: pending})
	return err == nil, err
}

// Run calls PushIfIdle every interval until ctx ends.
func (a *Agent) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := a.PushIfIdle(ctx); err != nil && !errors.Is(err, channel.ErrNotConnected) {
				a.log.Warn("evidence: idle push failed", "err", err)
			}
		}
	}
}

// serveRange streams one finalized segment from req.Offset, in chunks, the last
// flagged eof. Only segments listed in the manifest are served, and only up to
// the manifest's byte count.
func (a *Agent) serveRange(ctx context.Context, id string, req RangeMsg) error {
	fail := func(msg string) error {
		_ = send(ctx, a.cfg.Sender, channel.TypeEvChunk, id, ChunkMsg{File: req.File, Offset: req.Offset, Err: msg})
		return errors.New(msg)
	}
	entries, err := a.allEntries()
	if err != nil {
		return fail("manifest unreadable")
	}
	var entry *capture.SegmentEntry
	for i := range entries {
		if entries[i].File == req.File {
			entry = &entries[i]
		}
	}
	if entry == nil || !safeComponent(entry.File) {
		return fail("unknown segment")
	}
	if req.SHA256 != "" && req.SHA256 != entry.SHA256 {
		return fail("segment changed")
	}
	if req.Offset < 0 || req.Offset > entry.Bytes {
		return fail("offset out of range")
	}
	f, err := os.Open(filepath.Join(a.cfg.Dir, entry.File))
	if err != nil {
		return fail("segment unreadable")
	}
	defer f.Close()
	if _, err := f.Seek(req.Offset, io.SeekStart); err != nil {
		return fail("seek failed")
	}

	buf := make([]byte, a.cfg.ChunkSize)
	pos := req.Offset
	for {
		want := int64(len(buf))
		if rem := entry.Bytes - pos; rem < want {
			want = rem
		}
		n, rerr := io.ReadFull(f, buf[:want])
		if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
			return fail("read failed")
		}
		pos += int64(n)
		last := pos >= entry.Bytes || n == 0
		chunk := ChunkMsg{File: entry.File, Offset: pos - int64(n), Data: buf[:n], EOF: last}
		if err := send(ctx, a.cfg.Sender, channel.TypeEvChunk, id, chunk); err != nil {
			return err
		}
		a.bytesServed.Add(int64(n))
		if last {
			return nil
		}
	}
}

// =========================== server side ===========================

// SyncerConfig configures the main-server sync endpoint for one machine.
type SyncerConfig struct {
	Machine string // the connected agent's verified id
	Archive *Archive
	Sender  Sender        // the channel to that agent
	Timeout time.Duration // per-message wait, default DefaultTimeout
	// OnPush, if set, receives the outcome of every sync started by an agent's
	// idle push.
	OnPush func(Result, error)
	Log    *slog.Logger
}

// Result summarizes one sync.
type Result struct {
	Fetched  []string // segments transferred, verified and archived
	Resumed  []string // the subset of Fetched that continued a partial transfer
	Rejected []string // segments whose SHA-256 did not verify (discarded)
	Skipped  int      // segments already archived, not transferred
	Bytes    int64    // segment bytes received during this sync
}

// Syncer is the main-server side of the sync protocol for one machine.
type Syncer struct {
	cfg SyncerConfig
	log *slog.Logger
	seq atomic.Uint64

	syncMu sync.Mutex // one sync at a time per machine

	mu sync.Mutex
	mb map[string]*mailbox
}

type mailbox struct {
	ch   chan channel.Envelope
	done chan struct{}
}

// NewSyncer validates cfg.
func NewSyncer(cfg SyncerConfig) (*Syncer, error) {
	if cfg.Machine == "" || cfg.Archive == nil || cfg.Sender == nil {
		return nil, errors.New("evidence: syncer needs a machine, an archive and a sender")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Syncer{cfg: cfg, log: log, mb: map[string]*mailbox{}}, nil
}

func (s *Syncer) register(id string) *mailbox {
	m := &mailbox{ch: make(chan channel.Envelope, 256), done: make(chan struct{})}
	s.mu.Lock()
	s.mb[id] = m
	s.mu.Unlock()
	return m
}

func (s *Syncer) unregister(id string) {
	s.mu.Lock()
	m := s.mb[id]
	delete(s.mb, id)
	s.mu.Unlock()
	if m != nil {
		close(m.done)
	}
}

// next waits for the mailbox's next envelope.
func (s *Syncer) next(ctx context.Context, m *mailbox) (channel.Envelope, error) {
	t := time.NewTimer(s.cfg.Timeout)
	defer t.Stop()
	select {
	case env := <-m.ch:
		return env, nil
	case <-t.C:
		return channel.Envelope{}, ErrTimeout
	case <-ctx.Done():
		return channel.Envelope{}, ctx.Err()
	}
}

// Handle processes one envelope from the agent. It reports whether the
// envelope was an evidence-sync message; install it as the channel Handler.
func (s *Syncer) Handle(ctx context.Context, env channel.Envelope) bool {
	switch env.Type {
	case channel.TypeEvManifest, channel.TypeEvChunk:
	default:
		return false
	}
	s.mu.Lock()
	m := s.mb[env.ID]
	s.mu.Unlock()
	if m != nil {
		select {
		case m.ch <- env:
		case <-m.done:
		case <-ctx.Done():
		}
		return true
	}
	if env.Type == channel.TypeEvManifest {
		// No request is waiting for it: an idle-push offer.
		var msg ManifestMsg
		if err := json.Unmarshal(env.Payload, &msg); err != nil || msg.Machine != s.cfg.Machine {
			s.log.Warn("evidence: ignoring push offer", "machine", s.cfg.Machine, "claimed", msg.Machine, "err", err)
			return true
		}
		go func() {
			res, err := s.syncFrom(ctx, msg.Segments)
			if s.cfg.OnPush != nil {
				s.cfg.OnPush(res, err)
			}
		}()
	}
	// A stray chunk for an abandoned request is dropped.
	return true
}

// Pull syncs the machine: request its manifest, then fetch what the archive
// lacks or holds with a different digest.
func (s *Syncer) Pull(ctx context.Context) (Result, error) {
	id := fmt.Sprintf("pull-%d", s.seq.Add(1))
	m := s.register(id)
	defer s.unregister(id)
	if err := send(ctx, s.cfg.Sender, channel.TypeEvManifestReq, id, nil); err != nil {
		return Result{}, fmt.Errorf("evidence: request manifest: %w", err)
	}
	env, err := s.next(ctx, m)
	if err != nil {
		return Result{}, fmt.Errorf("evidence: await manifest: %w", err)
	}
	var msg ManifestMsg
	if err := json.Unmarshal(env.Payload, &msg); err != nil {
		return Result{}, fmt.Errorf("evidence: bad manifest: %w", err)
	}
	if msg.Machine != s.cfg.Machine {
		return Result{}, fmt.Errorf("evidence: manifest is for %q, expected %q", msg.Machine, s.cfg.Machine)
	}
	return s.syncFrom(ctx, msg.Segments)
}

// syncFrom is the single diff-and-fetch routine behind both pull and push.
func (s *Syncer) syncFrom(ctx context.Context, remote []capture.SegmentEntry) (Result, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	var res Result
	need, err := s.cfg.Archive.Need(s.cfg.Machine, remote)
	if err != nil {
		return res, err
	}
	res.Skipped = len(remote) - len(need)
	for _, e := range need {
		if err := s.cfg.Archive.CheckIntake(); err != nil {
			return res, err
		}
		if err := s.fetch(ctx, e, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// fetch brings one segment into the archive: resume from whatever partial
// bytes exist, hash the whole file, and commit only on a match. A mismatch
// discards the bytes; if they had been resumed (they may be the culprit) it
// retries once from the start, otherwise the segment is rejected.
func (s *Syncer) fetch(ctx context.Context, e capture.SegmentEntry, res *Result) error {
	arch := s.cfg.Archive
	part, err := arch.partialPath(s.cfg.Machine, e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(part), 0o750); err != nil {
		return fmt.Errorf("evidence: partial dir: %w", err)
	}
	retried := false
	for {
		var offset int64
		if st, err := os.Stat(part); err == nil {
			offset = st.Size()
			if offset > e.Bytes {
				_ = os.Remove(part)
				offset = 0
			}
		}
		resumed := offset > 0
		if offset < e.Bytes {
			if err := s.transfer(ctx, e, part, offset, res); err != nil {
				return err
			}
		}
		why := verifyFile(part, e)
		if why == "" {
			if _, err := arch.Commit(s.cfg.Machine, e, part); err != nil {
				return err
			}
			res.Fetched = append(res.Fetched, e.File)
			if resumed {
				res.Resumed = append(res.Resumed, e.File)
			}
			_ = send(ctx, s.cfg.Sender, channel.TypeEvVerify, "", VerifyMsg{File: e.File, OK: true})
			return nil
		}
		_ = os.Remove(part)
		s.log.Warn("evidence: segment failed verification", "machine", s.cfg.Machine, "file", e.File, "why", why, "resumed", resumed)
		if resumed && !retried {
			retried = true
			continue
		}
		res.Rejected = append(res.Rejected, e.File)
		_ = send(ctx, s.cfg.Sender, channel.TypeEvVerify, "", VerifyMsg{File: e.File, OK: false, Err: why})
		return nil
	}
}

// transfer requests e from offset and appends the chunks to part. On any
// error the bytes received so far stay on disk for the next attempt to resume.
func (s *Syncer) transfer(ctx context.Context, e capture.SegmentEntry, part string, offset int64, res *Result) error {
	id := fmt.Sprintf("rng-%d", s.seq.Add(1))
	m := s.register(id)
	defer s.unregister(id)

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("evidence: open partial: %w", err)
	}
	defer f.Close()

	if err := send(ctx, s.cfg.Sender, channel.TypeEvRange, id, RangeMsg{File: e.File, SHA256: e.SHA256, Offset: offset}); err != nil {
		return fmt.Errorf("evidence: request %s: %w", e.File, err)
	}
	pos := offset
	for {
		env, err := s.next(ctx, m)
		if err != nil {
			return fmt.Errorf("evidence: receive %s at offset %d: %w", e.File, pos, err)
		}
		var c ChunkMsg
		if err := json.Unmarshal(env.Payload, &c); err != nil {
			return fmt.Errorf("evidence: bad chunk for %s: %w", e.File, err)
		}
		switch {
		case c.Err != "":
			return fmt.Errorf("evidence: agent refused %s: %s", e.File, c.Err)
		case c.File != e.File || c.Offset != pos:
			return fmt.Errorf("evidence: out-of-order chunk for %s: got %s@%d, want @%d", e.File, c.File, c.Offset, pos)
		case pos+int64(len(c.Data)) > e.Bytes:
			return fmt.Errorf("evidence: chunk overruns %s (%d > %d bytes)", e.File, pos+int64(len(c.Data)), e.Bytes)
		}
		if _, err := f.Write(c.Data); err != nil {
			return fmt.Errorf("evidence: write partial: %w", err)
		}
		pos += int64(len(c.Data))
		res.Bytes += int64(len(c.Data))
		if c.EOF {
			if pos != e.Bytes {
				return fmt.Errorf("evidence: %s ended at %d bytes, manifest says %d", e.File, pos, e.Bytes)
			}
			return nil
		}
	}
}

// verifyFile checks a file against its manifest entry (size and SHA-256). It
// returns "" on a match, else the reason.
func verifyFile(path string, e capture.SegmentEntry) string {
	f, err := os.Open(path)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "unreadable: " + err.Error()
	}
	if n != e.Bytes {
		return fmt.Sprintf("size %d != manifest %d", n, e.Bytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Sprintf("sha256 %s != manifest %s", got, e.SHA256)
	}
	return ""
}
