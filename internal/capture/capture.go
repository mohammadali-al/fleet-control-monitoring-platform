package capture

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// EvidenceDirName is the evidence subtree of the base data dir (D-0006):
// DataDir/evidence/<machine>/.
const EvidenceDirName = "evidence"

// DefaultNFLOGGroupBase is the starting nftables NFLOG group when none is
// configured. Each concurrent session gets a distinct group above the base so
// their captures do not cross-talk.
const DefaultNFLOGGroupBase = 100

// maxNFLOGGroups bounds the per-session group range (NFLOG group is 16-bit).
const maxNFLOGGroups = 1024

// captureBuffer is the packet-source channel depth. A generous buffer keeps the
// capture best-effort and non-blocking: if it fills under burst the source
// drops and counts (§6.3b) rather than back-pressuring the command.
const captureBuffer = 4096

// capturedPacket is one raw outbound IP packet handed from a source to the
// record loop.
type capturedPacket struct {
	data []byte
	ts   time.Time
}

// packetSource yields a session cgroup's outbound packets. The live
// implementation (newLiveSource, Linux + root) binds nftables meta cgroupv2 to
// an NFLOG group; tests inject a fake source. Close stops capture, tears down
// any kernel state, and closes the Packets channel.
type packetSource interface {
	Packets() <-chan capturedPacket
	Drops() uint64
	Close() error
}

// sourceFactory builds a packet source bound to cgroupPath. It is a field on
// the Controller so tests can substitute a synthetic source with no kernel.
type sourceFactory func(cgroupPath string, group int, log *slog.Logger) (packetSource, error)

// Config configures the capture subsystem for one agent.
type Config struct {
	DataDir       string        // base data dir (D-0006); evidence/<machine> lives under it
	Machine       string        // machine id, the <machine> path element
	TrimThreshold int           // payload trim threshold (0 -> DefaultTrimThreshold)
	Segment       SegmentConfig // segment rolling + disk guard
	Reflectors    []string      // public-IP reflector URLs (empty -> DefaultReflectors)
	NFLOGGroup    int           // nftables NFLOG group for the live source
	Log           *slog.Logger
}

// Summary reports what a session capture produced.
type Summary struct {
	Records  int
	Dropped  uint64
	Segments int
}

// Controller owns the agent-wide capture state: the public-IP cache (refreshed
// on a timer) and the source-IP resolver. Per session it spins up a
// SessionCapture bound to that session's cgroup.
type Controller struct {
	cfg       Config
	log       *slog.Logger
	cache     *PublicIPCache
	resolver  *SourceIPResolver
	newSource sourceFactory

	groupSeq  atomic.Uint32
	startOnce sync.Once
	stopOnce  sync.Once
}

// NewController builds a Controller. It does not start the public-IP refresh
// loop; call Start for that. DataDir and Machine are required.
func NewController(cfg Config) (*Controller, error) {
	if cfg.DataDir == "" {
		return nil, errors.New("capture: DataDir is required")
	}
	if cfg.Machine == "" {
		return nil, errors.New("capture: Machine is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	cache := NewPublicIPCache(NewReflector(cfg.Reflectors), 0)
	return &Controller{
		cfg:       cfg,
		log:       cfg.Log,
		cache:     cache,
		resolver:  NewSourceIPResolver(cache),
		newSource: newLiveSource,
	}, nil
}

// Start begins the public-IP refresh loop (boot + every 15 min + on demand).
func (c *Controller) Start(ctx context.Context) {
	c.startOnce.Do(func() { c.cache.Start(ctx) })
}

// RefreshPublicIP forces an immediate reflector refresh (the panel's manual
// button / a network-change signal).
func (c *Controller) RefreshPublicIP() { c.cache.TriggerRefresh() }

// Close stops the refresh loop. Active session captures must be stopped first.
func (c *Controller) Close() {
	c.stopOnce.Do(func() { c.cache.Stop() })
}

// PublicIP returns the latest cached reflector public-IP result.
func (c *Controller) PublicIP() PublicIPResult { return c.cache.Result() }

// PublicIPCache returns the controller's public-IP reflector cache.
func (c *Controller) PublicIPCache() *PublicIPCache { return c.cache }

// RefreshPublicIPNow resolves the public IP synchronously and updates the cache.
func (c *Controller) RefreshPublicIPNow(ctx context.Context) (PublicIPResult, error) {
	return c.cache.RefreshNow(ctx)
}

// machineDir is DataDir/evidence/<machine>.
func (c *Controller) machineDir() string {
	return filepath.Join(c.cfg.DataDir, EvidenceDirName, c.cfg.Machine)
}

// Capture starts capturing a session's outbound traffic, bound to its cgroup
// (from internal/exec: Session.ID and Session.CgroupPath). It is wired into the
// executor's OnSessionStart hook. Capture is best-effort and must never block
// the command: a source that cannot be set up (e.g. missing privilege) returns
// an error the caller should log and ignore, letting the command run uncaptured.
func (c *Controller) Capture(sessionID, cgroupPath string) (*SessionCapture, error) {
	sw, err := NewSegmentWriter(c.machineDir(), c.cfg.Machine, c.cfg.Segment, c.log)
	if err != nil {
		return nil, err
	}
	src, err := c.newSource(cgroupPath, c.nextGroup(), c.log)
	if err != nil {
		_ = sw.Close()
		return nil, fmt.Errorf("capture: start source for %s: %w", sessionID, err)
	}
	return c.startSession(sessionID, sw, src), nil
}

// nextGroup allocates a distinct NFLOG group for a session.
func (c *Controller) nextGroup() int {
	base := c.cfg.NFLOGGroup
	if base <= 0 {
		base = DefaultNFLOGGroupBase
	}
	n := c.groupSeq.Add(1) - 1
	return base + int(n%maxNFLOGGroups)
}

// startSession wires a writer and source into a running SessionCapture. It is
// the seam tests use with a synthetic source.
func (c *Controller) startSession(sessionID string, sw *SegmentWriter, src packetSource) *SessionCapture {
	rollEvery := c.cfg.Segment.MaxAge
	if rollEvery <= 0 || rollEvery > DefaultSegmentMaxAge {
		rollEvery = DefaultSegmentMaxAge
	}
	if rollEvery > 30*time.Second {
		rollEvery = 30 * time.Second // check staleness often enough to bound ranges
	}
	sc := &SessionCapture{
		SessionID: sessionID,
		sw:        sw,
		src:       src,
		resolver:  c.resolver,
		trim:      c.cfg.TrimThreshold,
		log:       c.log,
		tick:      rollEvery,
		done:      make(chan struct{}),
	}
	go sc.run()
	return sc
}

// SessionCapture is one session's running capture: it reads packets from the
// source, turns each into a record (resolving the source IP from the packet's
// destination), and appends them to rolling segments.
type SessionCapture struct {
	SessionID string

	sw       *SegmentWriter
	src      packetSource
	resolver *SourceIPResolver
	trim     int
	log      *slog.Logger
	tick     time.Duration

	done      chan struct{}
	stopOnce  sync.Once
	records   atomic.Int64
	lastDrops uint64
}

// Records returns the number of packets recorded so far (safe to call while the
// capture is running).
func (sc *SessionCapture) Records() int { return int(sc.records.Load()) }

// run is the per-session record loop. It exits when the source closes its
// channel (which Stop triggers via src.Close).
func (sc *SessionCapture) run() {
	defer close(sc.done)
	t := time.NewTicker(sc.tick)
	defer t.Stop()
	pkts := sc.src.Packets()
	for {
		select {
		case <-t.C:
			_ = sc.sw.RollIfStale()
			sc.reconcileDrops()
		case p, ok := <-pkts:
			if !ok {
				return
			}
			sc.handle(p)
		}
	}
}

// handle records one packet. Non-IP or unsupported packets are ignored; a
// disk-paused write is skipped (§6.5), never fatal.
func (sc *SessionCapture) handle(p capturedPacket) {
	d, err := decode(p.data)
	if err != nil {
		return // non-IP / unsupported L4: not recorded
	}
	src := sc.resolver.For(d.dstIP)
	rec := buildFromDecoded(sc.SessionID, p.ts, d, src, sc.trim)
	switch err := sc.sw.Write(rec); {
	case err == nil:
		sc.records.Add(1)
	case errors.Is(err, ErrDiskPaused):
		// paused: skip silently (guard already logged once)
	default:
		sc.log.Error("capture: write record", "session", sc.SessionID, "err", err)
	}
}

// reconcileDrops folds the source's cumulative drop count into the current
// segment's accounting as a delta.
func (sc *SessionCapture) reconcileDrops() {
	total := sc.src.Drops()
	if total > sc.lastDrops {
		sc.sw.AddDropped(total - sc.lastDrops)
		sc.lastDrops = total
	}
}

// Stop ends capture: it closes the source (stopping kernel capture and tearing
// down any nftables/NFLOG state), waits for the loop to drain, finalizes the
// last segment, and returns a Summary.
func (sc *SessionCapture) Stop() (Summary, error) {
	var closeErr error
	sc.stopOnce.Do(func() {
		closeErr = sc.src.Close()
		<-sc.done
		sc.reconcileDrops()
	})
	if err := sc.sw.Close(); err != nil && closeErr == nil {
		closeErr = err
	}
	return Summary{
		Records:  int(sc.records.Load()),
		Dropped:  sc.lastDrops,
		Segments: len(sc.sw.Manifest().Entries()),
	}, closeErr
}

// resolveDataDirMachineDir is a convenience for callers that want the evidence
// dir without a Controller (e.g. verification tooling in later tasks).
func EvidenceMachineDir(dataDir, machine string) string {
	return filepath.Join(dataDir, EvidenceDirName, machine)
}

// ensure the evidence dir exists early (used by the agent at boot).
func EnsureEvidenceDir(dataDir, machine string) (string, error) {
	dir := EvidenceMachineDir(dataDir, machine)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("capture: evidence dir: %w", err)
	}
	return dir, nil
}
