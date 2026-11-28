package agent

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/channel"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/exec"
)

const (
	defaultReconnectTimeout = 60 * time.Second
	defaultUpdateGrace      = 120 * time.Second
	updateResultSendTimeout = 10 * time.Second
	updateChunkTimeout      = 30 * time.Second
	updateHTTPTimeout       = 10 * time.Minute
	maxUpdateBinaryBytes    = 256 << 20 // 256 MiB safety cap
)

// UpdateOptions configures the agent's push-triggered self-updater. Most
// fields default to production behavior and exist for test injection.
type UpdateOptions struct {
	// BinaryPath overrides os.Executable() as the binary replaced on update.
	BinaryPath string
	// HTTPClient fetches HTTPS update URLs (defaults to an mTLS client loaded
	// from the agent's cert/key/CA when configured, else http.DefaultClient).
	HTTPClient *http.Client
	// Restart overrides how the agent restarts its service after swapping the
	// binary (default: ServiceRestarter on ServiceController, else systemctl/launchctl).
	Restart func(ctx context.Context) error
	// VerifyBinary runs an optional check on the downloaded temp binary before
	// atomic swap (after SHA-256 verification).
	VerifyBinary func(tmpPath string) error
	// ReconnectTimeout bounds how long the updated agent has to reconnect
	// before rolling back to the previous binary (default 60s).
	ReconnectTimeout time.Duration
	// GraceWindow bounds how long the watchdog suppresses escalation around an
	// update restart (default 120s).
	GraceWindow time.Duration
	// Now overrides time.Now (tests).
	Now func() time.Time
}

type updatePendingState struct {
	RequestID         string    `json:"request_id"`
	TargetVersion     string    `json:"target_version"`
	PreviousVersion   string    `json:"previous_version"`
	BinPath           string    `json:"bin_path"`
	BackupPath        string    `json:"backup_path"`
	StartedAt         time.Time `json:"started_at"`
	GraceUntil        time.Time `json:"grace_until"`
	ReconnectDeadline time.Time `json:"reconnect_deadline"`
	RolledBack        bool      `json:"rolled_back,omitempty"`
}

type updateDeferredResult struct {
	RequestID string                           `json:"request_id"`
	Payload   channel.AgentUpdateResultPayload `json:"payload"`
}

type agentUpdater struct {
	dataDir    string
	ver        string
	log        *slog.Logger
	sender     exec.Sender
	base       func() context.Context
	ctrl       func() ServiceController
	opts       UpdateOptions
	connected  atomic.Bool
	updating   bool
	mu         sync.Mutex
	commitCh   chan struct{}
	chunkMu    sync.Mutex
	chunkWait  map[string]chan channel.AgentUpdateChunkPayload
	httpClient *http.Client
}

func newAgentUpdater(dataDir, verOverride string, log *slog.Logger, sender exec.Sender, base func() context.Context, ctrl func() ServiceController, opts UpdateOptions) *agentUpdater {
	if log == nil {
		log = slog.Default()
	}
	if opts.ReconnectTimeout <= 0 {
		opts.ReconnectTimeout = defaultReconnectTimeout
	}
	if opts.GraceWindow <= 0 {
		opts.GraceWindow = defaultUpdateGrace
	}
	if opts.GraceWindow < opts.ReconnectTimeout {
		opts.GraceWindow = opts.ReconnectTimeout
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: updateHTTPTimeout}
	}
	return &agentUpdater{
		dataDir:    dataDir,
		ver:        verOverride,
		log:        log,
		sender:     sender,
		base:       base,
		ctrl:       ctrl,
		opts:       opts,
		commitCh:   make(chan struct{}),
		chunkWait:  map[string]chan channel.AgentUpdateChunkPayload{},
		httpClient: hc,
	}
}

func (u *agentUpdater) currentVersion() string {
	return effectiveAgentVersion(u.ver)
}

func (u *agentUpdater) now() time.Time {
	return u.opts.Now()
}

func (u *agentUpdater) resolveBinPath() (string, error) {
	if p := strings.TrimSpace(u.opts.BinaryPath); p != "" {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return exe, nil
}

func updateStatePaths(dataDir, binPath string) (pendingPath, resultPath string) {
	if dataDir != "" {
		dir := watchdogDir(dataDir)
		return filepath.Join(dir, "update-pending.json"), filepath.Join(dir, "update-result.json")
	}
	return binPath + ".update-pending.json", binPath + ".update-result.json"
}

func (u *agentUpdater) statePaths() (pendingPath, resultPath string, err error) {
	if u.dataDir != "" {
		p, r := updateStatePaths(u.dataDir, "")
		return p, r, nil
	}
	binPath, err := u.resolveBinPath()
	if err != nil {
		return "", "", err
	}
	p, r := updateStatePaths("", binPath)
	return p, r, nil
}

// start checks on agent startup whether an update is pending verification
// (waiting for onConnect before ReconnectDeadline, else rolling back).
func (u *agentUpdater) start(ctx context.Context) {
	pendingPath, resultPath, err := u.statePaths()
	if err != nil {
		return
	}
	pending, err := readPendingState(pendingPath)
	if err != nil || pending == nil {
		return
	}
	if u.currentVersion() != pending.TargetVersion {
		// Not running the target binary (e.g. already rolled back or stale marker).
		return
	}
	wait := pending.ReconnectDeadline.Sub(u.now())
	if wait < 0 {
		wait = 0
	}
	go func() {
		t := time.NewTimer(wait)
		defer t.Stop()
		u.mu.Lock()
		ch := u.commitCh
		u.mu.Unlock()
		select {
		case <-ch:
			return
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if u.connected.Load() {
			return
		}
		u.log.Error("updated agent failed to reconnect within timeout; rolling back",
			"component", "fleet-agent", "target", pending.TargetVersion, "previous", pending.PreviousVersion)
		if rerr := restoreBackup(pending.BackupPath, pending.BinPath); rerr != nil {
			u.log.Error("rollback restore failed", "component", "fleet-agent", "err", rerr)
			return
		}
		_ = writeJSONAtomic(resultPath, updateDeferredResult{
			RequestID: pending.RequestID,
			Payload: channel.AgentUpdateResultPayload{
				TargetVersion:  pending.TargetVersion,
				CurrentVersion: pending.PreviousVersion,
				Status:         channel.AgentUpdateFailed,
				Error:          "rolled back: updated agent failed to reconnect within timeout",
				At:             u.now().UTC(),
			},
		})
		_ = os.Remove(pendingPath)
		_ = u.restart(ctx)
	}()
}

// onConnect commits a pending update if running the target version, or flushes
// any deferred rollback failure report from an earlier failed update.
func (u *agentUpdater) onConnect() {
	u.connected.Store(true)
	u.signalCommit()

	pendingPath, resultPath, err := u.statePaths()
	if err != nil {
		return
	}
	if pending, err := readPendingState(pendingPath); err == nil && pending != nil {
		_ = os.Remove(pendingPath)
		cur := u.currentVersion()
		if cur == pending.TargetVersion {
			u.log.Info("agent self-update committed on reconnect", "component", "fleet-agent", "version", cur)
			u.sendResult(pending.RequestID, channel.AgentUpdateResultPayload{
				TargetVersion:  pending.TargetVersion,
				CurrentVersion: cur,
				Status:         channel.AgentUpdateOK,
				At:             u.now().UTC(),
			})
		} else {
			u.log.Warn("agent reconnected on previous version after update", "component", "fleet-agent", "current", cur, "target", pending.TargetVersion)
			u.sendResult(pending.RequestID, channel.AgentUpdateResultPayload{
				TargetVersion:  pending.TargetVersion,
				CurrentVersion: cur,
				Status:         channel.AgentUpdateFailed,
				Error:          "rolled back to previous version " + cur,
				At:             u.now().UTC(),
			})
		}
	}
	if res, err := readDeferredResult(resultPath); err == nil && res != nil {
		_ = os.Remove(resultPath)
		res.Payload.CurrentVersion = u.currentVersion()
		if res.Payload.At.IsZero() {
			res.Payload.At = u.now().UTC()
		}
		u.sendResult(res.RequestID, res.Payload)
	}
}

func (u *agentUpdater) signalCommit() {
	u.mu.Lock()
	select {
	case <-u.commitCh:
	default:
		close(u.commitCh)
	}
	u.mu.Unlock()
}

func (u *agentUpdater) resetCommitWait() chan struct{} {
	u.mu.Lock()
	ch := make(chan struct{})
	u.commitCh = ch
	u.mu.Unlock()
	return ch
}

func (u *agentUpdater) handleEnvelope(ctx context.Context, env channel.Envelope) {
	switch env.Type {
	case channel.TypeAgentUpdate:
		u.handleUpdate(ctx, env)
	case channel.TypeAgentUpdateChunk:
		u.handleChunk(env)
	}
}

func (u *agentUpdater) handleChunk(env channel.Envelope) {
	var chunk channel.AgentUpdateChunkPayload
	if err := json.Unmarshal(env.Payload, &chunk); err != nil {
		u.log.Warn("bad agent-update-chunk payload", "component", "fleet-agent", "err", err)
		return
	}
	u.chunkMu.Lock()
	ch := u.chunkWait[env.ID]
	u.chunkMu.Unlock()
	if ch != nil {
		select {
		case ch <- chunk:
		default:
		}
	}
}

func (u *agentUpdater) handleUpdate(_ context.Context, env channel.Envelope) {
	var req channel.AgentUpdatePayload
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		u.log.Warn("bad agent-update payload", "component", "fleet-agent", "err", err)
		u.sendResult(env.ID, channel.AgentUpdateResultPayload{
			CurrentVersion: u.currentVersion(),
			Status:         channel.AgentUpdateFailed,
			Error:          "bad agent-update payload: " + err.Error(),
			At:             u.now().UTC(),
		})
		return
	}
	req.TargetVersion = strings.TrimSpace(req.TargetVersion)
	if req.TargetVersion == "" {
		u.sendResult(env.ID, channel.AgentUpdateResultPayload{
			CurrentVersion: u.currentVersion(),
			Status:         channel.AgentUpdateFailed,
			Error:          "target_version is required",
			At:             u.now().UTC(),
		})
		return
	}
	cur := u.currentVersion()
	if req.TargetVersion == cur {
		u.log.Info("agent already on target version; self-update is a no-op", "component", "fleet-agent", "version", cur)
		u.sendResult(env.ID, channel.AgentUpdateResultPayload{
			TargetVersion:  req.TargetVersion,
			CurrentVersion: cur,
			Status:         channel.AgentUpdateOK,
			At:             u.now().UTC(),
		})
		return
	}

	shaHex, err := normalizeSHA256(req.SHA256)
	if err != nil {
		u.sendResult(env.ID, channel.AgentUpdateResultPayload{
			TargetVersion:  req.TargetVersion,
			CurrentVersion: cur,
			Status:         channel.AgentUpdateFailed,
			Error:          err.Error(),
			At:             u.now().UTC(),
		})
		return
	}
	req.SHA256 = shaHex

	u.mu.Lock()
	if u.updating {
		u.mu.Unlock()
		u.sendResult(env.ID, channel.AgentUpdateResultPayload{
			TargetVersion:  req.TargetVersion,
			CurrentVersion: cur,
			Status:         channel.AgentUpdateFailed,
			Error:          "an agent update is already in progress",
			At:             u.now().UTC(),
		})
		return
	}
	u.updating = true
	u.mu.Unlock()

	go u.runUpdate(u.baseCtx(), env.ID, req)
}

func (u *agentUpdater) baseCtx() context.Context {
	if u.base != nil {
		if ctx := u.base(); ctx != nil {
			return ctx
		}
	}
	return context.Background()
}

func (u *agentUpdater) runUpdate(ctx context.Context, reqID string, req channel.AgentUpdatePayload) {
	defer func() {
		u.mu.Lock()
		u.updating = false
		u.mu.Unlock()
	}()

	cur := u.currentVersion()
	fail := func(msg string) {
		u.log.Warn("agent self-update failed", "component", "fleet-agent", "target", req.TargetVersion, "err", msg)
		u.sendResult(reqID, channel.AgentUpdateResultPayload{
			TargetVersion:  req.TargetVersion,
			CurrentVersion: cur,
			Status:         channel.AgentUpdateFailed,
			Error:          msg,
			At:             u.now().UTC(),
		})
	}

	u.sendResult(reqID, channel.AgentUpdateResultPayload{
		TargetVersion:  req.TargetVersion,
		CurrentVersion: cur,
		Status:         channel.AgentUpdateDownloading,
		At:             u.now().UTC(),
	})

	binPath, err := u.resolveBinPath()
	if err != nil {
		fail(err.Error())
		return
	}
	dir := filepath.Dir(binPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(fmt.Sprintf("prepare binary dir: %v", err))
		return
	}

	mode := os.FileMode(0o755)
	if st, err := os.Stat(binPath); err == nil {
		mode = st.Mode().Perm() | 0o111
	}

	tmp, err := os.CreateTemp(dir, ".fleet-agent-update-*.tmp")
	if err != nil {
		fail(fmt.Sprintf("create staging file: %v", err))
		return
	}
	tmpPath := tmp.Name()
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	gotSHA, gotSize, err := u.downloadBinary(ctx, reqID, req, tmp)
	if cerr := tmp.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		cleanupTmp()
		fail(err.Error())
		return
	}
	if gotSize == 0 {
		cleanupTmp()
		fail("downloaded binary is empty")
		return
	}
	if req.Size > 0 && gotSize != req.Size {
		cleanupTmp()
		fail(fmt.Sprintf("downloaded size %d does not match expected %d", gotSize, req.Size))
		return
	}
	if gotSHA != req.SHA256 {
		cleanupTmp()
		fail(fmt.Sprintf("checksum mismatch: got %s, want %s", gotSHA, req.SHA256))
		return
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		cleanupTmp()
		fail(fmt.Sprintf("chmod staging binary: %v", err))
		return
	}
	if u.opts.VerifyBinary != nil {
		if err := u.opts.VerifyBinary(tmpPath); err != nil {
			cleanupTmp()
			fail(fmt.Sprintf("verify binary: %v", err))
			return
		}
	}

	u.sendResult(reqID, channel.AgentUpdateResultPayload{
		TargetVersion:  req.TargetVersion,
		CurrentVersion: cur,
		Status:         channel.AgentUpdateApplying,
		At:             u.now().UTC(),
	})

	bakPath := binPath + ".bak"
	if _, err := os.Stat(binPath); err == nil {
		if err := copyFileAtomic(binPath, bakPath, mode); err != nil {
			cleanupTmp()
			fail(fmt.Sprintf("backup current binary: %v", err))
			return
		}
	}

	pendingPath, _ := updateStatePaths(u.dataDir, binPath)
	now := u.now().UTC()
	pending := updatePendingState{
		RequestID:         reqID,
		TargetVersion:     req.TargetVersion,
		PreviousVersion:   cur,
		BinPath:           binPath,
		BackupPath:        bakPath,
		StartedAt:         now,
		GraceUntil:        now.Add(u.opts.GraceWindow),
		ReconnectDeadline: now.Add(u.opts.ReconnectTimeout),
	}
	if err := writeJSONAtomic(pendingPath, pending); err != nil {
		cleanupTmp()
		fail(fmt.Sprintf("write pending update state: %v", err))
		return
	}

	if err := os.Rename(tmpPath, binPath); err != nil {
		cleanupTmp()
		_ = os.Remove(pendingPath)
		fail(fmt.Sprintf("atomic binary replace: %v", err))
		return
	}
	syncDir(dir)

	commitWait := u.resetCommitWait()
	if err := u.restart(ctx); err != nil {
		_ = restoreBackup(bakPath, binPath)
		_ = os.Remove(pendingPath)
		fail(fmt.Sprintf("service restart failed (rolled back): %v", err))
		return
	}

	// Under real systemd/launchd, the service restart sends SIGTERM so ctx is
	// cancelled immediately and the new process takes over commit/rollback. If
	// this process remains alive past ReconnectTimeout without committing, roll back.
	timer := time.NewTimer(u.opts.ReconnectTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return
	case <-commitWait:
		_ = os.Remove(pendingPath)
		u.sendResult(reqID, channel.AgentUpdateResultPayload{
			TargetVersion:  req.TargetVersion,
			CurrentVersion: req.TargetVersion,
			Status:         channel.AgentUpdateOK,
			At:             u.now().UTC(),
		})
		return
	case <-timer.C:
		_ = restoreBackup(bakPath, binPath)
		_ = os.Remove(pendingPath)
		fail("rolled back: agent did not restart or reconnect within timeout")
	}
}

func (u *agentUpdater) downloadBinary(ctx context.Context, reqID string, req channel.AgentUpdatePayload, dst *os.File) (string, int64, error) {
	if req.URL != "" && req.Method != "channel" {
		shaHex, n, err := u.downloadHTTP(ctx, req.URL, req.SHA256, dst)
		if err == nil || req.Method == "http" {
			return shaHex, n, err
		}
		// Only fall back to the channel when Method was unspecified and HTTP failed
		// before writing anything (e.g. network/firewall unreachable), never on checksum errors.
		if errors.Is(err, errHTTPTransport) {
			if _, serr := dst.Seek(0, io.SeekStart); serr == nil {
				if terr := dst.Truncate(0); terr == nil {
					return u.downloadChannel(ctx, reqID, req, dst)
				}
			}
		}
		return "", 0, err
	}
	return u.downloadChannel(ctx, reqID, req, dst)
}

var errHTTPTransport = errors.New("http transport error")

func (u *agentUpdater) downloadHTTP(ctx context.Context, rawURL, wantSHA string, dst *os.File) (string, int64, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("build download request: %w", err)
	}
	resp, err := u.httpClient.Do(hreq)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %v", errHTTPTransport, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("download binary: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if hdr := strings.TrimSpace(resp.Header.Get("X-Checksum-Sha256")); hdr != "" {
		if norm, err := normalizeSHA256(hdr); err == nil && norm != wantSHA {
			return "", 0, fmt.Errorf("checksum mismatch in header: got %s, want %s", norm, wantSHA)
		}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(dst, h), io.LimitReader(resp.Body, maxUpdateBinaryBytes+1))
	if err != nil {
		return "", 0, fmt.Errorf("read binary response: %w", err)
	}
	if n > maxUpdateBinaryBytes {
		return "", 0, fmt.Errorf("binary exceeds max allowed size (%d bytes)", maxUpdateBinaryBytes)
	}
	if err := dst.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync staging binary: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func (u *agentUpdater) downloadChannel(ctx context.Context, reqID string, req channel.AgentUpdatePayload, dst *os.File) (string, int64, error) {
	if u.sender == nil {
		return "", 0, errors.New("no control-channel sender available for update download")
	}
	goos, arch := req.OS, req.Arch
	if goos == "" {
		goos = runtime.GOOS
	}
	if arch == "" {
		arch = runtime.GOARCH
	}

	ch := make(chan channel.AgentUpdateChunkPayload, 4)
	u.chunkMu.Lock()
	u.chunkWait[reqID] = ch
	u.chunkMu.Unlock()
	defer func() {
		u.chunkMu.Lock()
		delete(u.chunkWait, reqID)
		u.chunkMu.Unlock()
	}()

	h := sha256.New()
	var offset int64
	for {
		creq, err := channel.NewEnvelope(channel.TypeAgentUpdateChunkReq, reqID, channel.AgentUpdateChunkReqPayload{
			OS:     goos,
			Arch:   arch,
			Offset: offset,
			Length: 512 << 10,
		})
		if err != nil {
			return "", 0, fmt.Errorf("build chunk request: %w", err)
		}
		sctx, cancel := context.WithTimeout(ctx, updateChunkTimeout)
		err = u.sender.Send(sctx, creq)
		cancel()
		if err != nil {
			return "", 0, fmt.Errorf("send chunk request: %w", err)
		}

		t := time.NewTimer(updateChunkTimeout)
		var chunk channel.AgentUpdateChunkPayload
		select {
		case chunk = <-ch:
			t.Stop()
		case <-ctx.Done():
			t.Stop()
			return "", 0, ctx.Err()
		case <-t.C:
			return "", 0, errors.New("timed out waiting for update binary chunk from server")
		}
		if chunk.Error != "" {
			return "", 0, fmt.Errorf("server chunk error: %s", chunk.Error)
		}
		if chunk.Offset != offset {
			return "", 0, fmt.Errorf("unexpected chunk offset %d, want %d", chunk.Offset, offset)
		}
		if len(chunk.Data) > 0 {
			if offset+int64(len(chunk.Data)) > maxUpdateBinaryBytes {
				return "", 0, fmt.Errorf("binary exceeds max allowed size (%d bytes)", maxUpdateBinaryBytes)
			}
			if _, err := dst.Write(chunk.Data); err != nil {
				return "", 0, fmt.Errorf("write staging binary: %w", err)
			}
			_, _ = h.Write(chunk.Data)
			offset += int64(len(chunk.Data))
		}
		if chunk.EOF || (chunk.Total > 0 && offset >= chunk.Total) {
			break
		}
		if len(chunk.Data) == 0 {
			return "", 0, errors.New("received empty non-EOF update chunk")
		}
	}
	if err := dst.Sync(); err != nil {
		return "", 0, fmt.Errorf("sync staging binary: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), offset, nil
}

func (u *agentUpdater) restart(ctx context.Context) error {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceControlTimeout)
	defer cancel()
	if u.opts.Restart != nil {
		return u.opts.Restart(rctx)
	}
	if u.ctrl != nil {
		if sc := u.ctrl(); sc != nil {
			if sr, ok := sc.(ServiceRestarter); ok {
				return sr.Restart(rctx)
			}
		}
	}
	return defaultServiceController().(*osServiceController).Restart(rctx)
}

func (u *agentUpdater) sendResult(reqID string, res channel.AgentUpdateResultPayload) {
	if u.sender == nil {
		return
	}
	env, err := channel.NewEnvelope(channel.TypeAgentUpdateResult, reqID, res)
	if err != nil {
		u.log.Warn("build agent-update-result envelope", "component", "fleet-agent", "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(u.baseCtx(), updateResultSendTimeout)
	defer cancel()
	if err := u.sender.Send(ctx, env); err != nil {
		u.log.Debug("send agent-update-result", "component", "fleet-agent", "status", res.Status, "err", err)
	}
}

func normalizeSHA256(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 64 {
		return "", fmt.Errorf("invalid sha256 checksum length (%d)", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("invalid sha256 checksum: %w", err)
	}
	return s, nil
}

func readPendingState(path string) (*updatePendingState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var st updatePendingState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func readDeferredResult(path string) (*updateDeferredResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var res updateDeferredResult
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	syncDir(dir)
	return nil
}

func copyFileAtomic(srcPath, dstPath string, mode os.FileMode) error {
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()
	dir := filepath.Dir(dstPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dstPath)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, src); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		cleanup()
		return err
	}
	syncDir(dir)
	return nil
}

func restoreBackup(bakPath, binPath string) error {
	st, err := os.Stat(bakPath)
	if err != nil {
		return fmt.Errorf("backup binary %s unavailable: %w", bakPath, err)
	}
	if !st.Mode().IsRegular() || st.Size() == 0 {
		return fmt.Errorf("backup binary %s is invalid", bakPath)
	}
	return copyFileAtomic(bakPath, binPath, st.Mode().Perm()|0o111)
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

func loadUpdateHTTPClient(caFile, certFile, keyFile string) *http.Client {
	if caFile == "" || certFile == "" || keyFile == "" {
		return nil
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil
	}
	return &http.Client{
		Timeout: updateHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				MinVersion:   tls.VersionTLS12,
				RootCAs:      pool,
				Certificates: []tls.Certificate{cert},
			},
		},
	}
}
