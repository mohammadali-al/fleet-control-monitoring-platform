// Package backup replicates the main server's data to a storage-only backup
// server (architecture 7.5, decision #37). The backup server is a plain SSH
// target: it runs no fleet software, only a POSIX shell with sha256sum, cat,
// cp, mv, mkdir, find, cut and dirname. The main server owns the connection.
//
// What is backed up (the "set", all under the base data dir, D-0006):
//
//	archive/   evidence archive (internal/evidence) - segments + manifests
//	monitor/   monitor data
//	projects/  Maneuver/project metadata
//	registry/  machine registry (so a restore keeps machine identities)
//	ca/        internal CA key + cert + issued certs (else every machine must
//	           be re-enrolled)
//
// config/ is not in the set: it holds the backup credential and the gate
// secret, which never leave the main server.
//
// Constant path. The destination on the backup server is fixed by the main
// server, not chosen by the operator or by agents: RemoteBase (relative to the
// SSH login directory), which mirrors the base data dir:
//
//	fleet-control-backup/data/      the set, same layout as the base data dir
//	fleet-control-backup/history/   earlier versions of files that changed
//	fleet-control-backup/meta/      index.json - what the backup holds (path, size, SHA-256)
//
// Additive only. Nothing on the backup is ever deleted and no backed-up file is
// destroyed: a file that changed on main is first copied to history/<path>.<sha256 of the old
// content>, then replaced atomically; a file that vanished from main stays on
// the backup. The only thing the main server removes is its own half-written
// "<file>.fleet-part" upload temp, and only when the checksum did not match.
//
// Verification. The sync reuses the evidence manifest + SHA-256: the digests of
// archive segments come from the archive manifests (internal/evidence), every
// other file is hashed. Each upload is streamed to a temp file on the backup,
// the backup computes SHA-256 of what it received, and the file is committed
// only if that equals the digest main expected.
//
// Credentials (D-0002). The password is used once, to open SSH and install a
// freshly generated ed25519 key in the backup user's authorized_keys; after
// that only the key is kept (<data>/config/backup-credential, mode 0600). Only
// if the key cannot be installed is the password kept in that same file. It is
// never logged and never part of the backup set.
package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/config"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/evidence"
)

// The constant, unique destination on the backup server (relative to the SSH
// login directory). Owned by the main server; there is no setting for it.
const (
	RemoteBase       = "fleet-control-backup"
	RemoteDataDir    = RemoteBase + "/data"
	RemoteHistoryDir = RemoteBase + "/history"
	RemoteMetaDir    = RemoteBase + "/meta"
	remoteIndexFile  = RemoteMetaDir + "/index.json"
)

// partSuffix marks an in-flight upload temp on the backup (and is skipped when
// scanning either side).
const partSuffix = ".fleet-part"

// IndexSchemaVersion is the version of the backup index file.
const IndexSchemaVersion = 1

// Authentication methods recorded for a backup server.
const (
	AuthKey      = "key"      // key installed with the password; password discarded
	AuthPassword = "password" // key install failed; password kept in the 0600 credential file
)

// Errors.
var (
	ErrNoServer      = errors.New("backup: no backup server configured")
	ErrBackupRunning = errors.New("backup: a backup is already running")
	ErrChecksum      = errors.New("backup: checksum mismatch")
)

// SetDirs returns the top-level directories of the backup set, relative to the
// base data dir.
func SetDirs() []string {
	return []string{config.CADirName, evidence.ArchiveDir, config.MonitorDirName,
		config.ProjectsDirName, config.RegistryDirName}
}

// Server is a backup server record. It holds no secret.
type Server struct {
	Host      string    `json:"host"` // IP address (or host name)
	Port      int       `json:"port,omitempty"`
	User      string    `json:"user"`
	HostKeyFP string    `json:"host_key_fp,omitempty"` // pinned SSH host key, "SHA256:..." (trust on first use)
	Auth      string    `json:"auth,omitempty"`        // AuthKey | AuthPassword
	AddedAt   time.Time `json:"added_at"`
}

func (s Server) addr() string {
	p := s.Port
	if p == 0 {
		p = 22
	}
	return net.JoinHostPort(s.Host, strconv.Itoa(p))
}

// RemotePath is the constant destination on the backup server (for display).
func (Server) RemotePath() string { return RemoteDataDir }

func (s Server) validate() error {
	if strings.TrimSpace(s.Host) == "" || strings.ContainsAny(s.Host, " \t/\\@") {
		return errors.New("backup: host must be an IP address or host name")
	}
	if strings.TrimSpace(s.User) == "" {
		return errors.New("backup: user is required")
	}
	if s.Port < 0 || s.Port > 65535 {
		return errors.New("backup: invalid port")
	}
	return nil
}

// Credential is what SSH needs to authenticate. Exactly one field is set.
type Credential struct {
	Password      []byte // used once at setup, or kept only as the fallback
	PrivateKeyPEM []byte // PKCS#8 ed25519
}

// Remote runs commands on the backup server.
type Remote interface {
	// Exec runs cmd with stdin (may be nil) and writes its stdout to stdout
	// (may be nil). A non-zero exit is a *RemoteError.
	Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error
	Close() error
}

// RemoteError is a command that exited non-zero.
type RemoteError struct {
	Code   int
	Stderr string
}

func (e *RemoteError) Error() string {
	return fmt.Sprintf("backup: remote command exited %d: %s", e.Code, strings.TrimSpace(e.Stderr))
}

// DialFunc opens a Remote to the backup server and reports the SSH host key
// fingerprint it saw. A non-empty srv.HostKeyFP must match.
type DialFunc func(ctx context.Context, srv Server, cred Credential) (Remote, string, error)

// Options configure a Service. The zero value selects production behavior.
type Options struct {
	Dial        DialFunc         // default: real SSH
	Now         func() time.Time // default: time.Now
	Log         *slog.Logger
	DialTimeout time.Duration // default 15s
}

// Service is the main-server side of the backup: the server record, schedule,
// replication, diff, restore and reminder. Methods are safe for concurrent use.
type Service struct {
	dataDir string
	dial    DialFunc
	now     func() time.Time
	log     *slog.Logger

	runMu sync.Mutex // held for the duration of a backup or restore

	mu sync.Mutex // guards st; never held across network or disk-scan work
	st state
}

type state struct {
	Server       *Server    `json:"server,omitempty"`
	Schedule     *Schedule  `json:"schedule,omitempty"`
	LastBackup   *RunRecord `json:"last_backup,omitempty"`
	LastReminder string     `json:"last_reminder,omitempty"` // local calendar date "2006-01-02"
}

// RunRecord summarizes a finished backup, kept in the state file.
type RunRecord struct {
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	OK       bool      `json:"ok"`
	Uploaded int       `json:"uploaded"`
	Failed   int       `json:"failed"`
	Bytes    int64     `json:"bytes"`
}

// Open loads the backup state under dataDir/config (absent = no server yet).
func Open(dataDir string, opts Options) (*Service, error) {
	if dataDir == "" {
		return nil, errors.New("backup: needs a data dir")
	}
	s := &Service{dataDir: dataDir, dial: opts.Dial, now: opts.Now, log: opts.Log}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	if s.dial == nil {
		timeout := opts.DialTimeout
		if timeout == 0 {
			timeout = 15 * time.Second
		}
		s.dial = func(ctx context.Context, srv Server, cred Credential) (Remote, string, error) {
			return dialSSH(ctx, srv, cred, timeout)
		}
	}
	data, err := os.ReadFile(s.statePath())
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &s.st); err != nil {
			return nil, fmt.Errorf("backup: parse %s: %w", s.statePath(), err)
		}
	case errors.Is(err, fs.ErrNotExist):
	default:
		return nil, fmt.Errorf("backup: read state: %w", err)
	}
	return s, nil
}

func (s *Service) statePath() string {
	return filepath.Join(s.dataDir, config.ConfigDirName, config.BackupStateFile)
}

func (s *Service) credPath() string {
	return filepath.Join(s.dataDir, config.ConfigDirName, config.BackupCredentialFile)
}

// saveLocked persists the state; the caller holds s.mu.
func (s *Service) saveLocked() error {
	return writeJSONAtomic(s.statePath(), 0o600, s.st)
}

func writeJSONAtomic(p string, mode os.FileMode, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(p, data, mode)
}

func writeFileAtomic(p string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".backup-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(err error) error { tmp.Close(); os.Remove(name); return err }
	if err := tmp.Chmod(mode); err != nil {
		return fail(err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, p); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Server returns the configured backup server.
func (s *Service) Server() (Server, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Server == nil {
		return Server{}, false
	}
	return *s.st.Server, true
}

// LastBackup returns the summary of the most recent backup, if any.
func (s *Service) LastBackup() (RunRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.LastBackup == nil {
		return RunRecord{}, false
	}
	return *s.st.LastBackup, true
}

// AddResult is what adding a backup server reports: the record, the data diff
// between main and the (so far empty or existing) backup, and the suggested
// nightly schedule (not yet enabled; call SetSchedule to accept it).
type AddResult struct {
	Server    Server
	Diff      *DiffResult // nil if DiffErr is set
	DiffErr   error
	Suggested Schedule
}

// AddServer adds the backup server by username + password + IP and records it
// (replacing any earlier record). The password is used here to connect, to
// create the constant backup path, and to install an ed25519 key; it is zeroed
// before returning and, unless key installation fails, never stored.
func (s *Service) AddServer(ctx context.Context, srv Server, password []byte) (*AddResult, error) {
	defer zero(password)
	if err := srv.validate(); err != nil {
		return nil, err
	}
	if len(password) == 0 {
		return nil, errors.New("backup: password is required")
	}
	srv.AddedAt = s.now()
	srv.Auth = ""

	rem, seen, err := s.dial(ctx, srv, Credential{Password: password})
	if err != nil {
		return nil, fmt.Errorf("backup: connect to %s: %w", srv.addr(), err)
	}
	defer rem.Close()
	if srv.HostKeyFP == "" {
		srv.HostKeyFP = seen // trust on first use; pinned from now on
	}
	if _, err := runOut(ctx, rem, "set -e; umask 077; mkdir -p "+shQuote(RemoteDataDir)+" "+
		shQuote(RemoteHistoryDir)+" "+shQuote(RemoteMetaDir), nil); err != nil {
		return nil, fmt.Errorf("backup: prepare %s on the backup server: %w", RemoteBase, err)
	}

	// Prefer a key over keeping the password.
	var cred []byte
	var credErr error
	keyPEM, line, kerr := newKey()
	if kerr == nil {
		kerr = installKey(ctx, rem, line)
	}
	if kerr == nil {
		if r2, _, derr := s.dial(ctx, srv, Credential{PrivateKeyPEM: keyPEM}); derr == nil {
			r2.Close()
			srv.Auth = AuthKey
			cred, credErr = json.Marshal(credentialFile{Auth: AuthKey, PrivateKeyPEM: string(keyPEM)})
		} else {
			kerr = derr
		}
	}
	if srv.Auth == "" {
		s.log.Warn("backup: could not install a key on the backup server; keeping the password in the credential file",
			"host", srv.Host, "err", kerr)
		srv.Auth = AuthPassword
		cred, credErr = json.Marshal(credentialFile{Auth: AuthPassword, Password: string(password)})
	}
	if credErr != nil {
		return nil, credErr
	}
	if err := writeFileAtomic(s.credPath(), cred, 0o600); err != nil {
		return nil, fmt.Errorf("backup: store credential: %w", err)
	}
	zero(cred)

	s.mu.Lock()
	s.st.Server = &srv
	err = s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("backup: record server: %w", err)
	}
	s.log.Info("backup: server added", "host", srv.Host, "user", srv.User, "auth", srv.Auth, "path", RemoteDataDir)

	res := &AddResult{Server: srv, Suggested: SuggestSchedule()}
	res.Diff, res.DiffErr = s.Diff(ctx)
	return res, nil
}

type credentialFile struct {
	Auth          string `json:"auth"`
	PrivateKeyPEM string `json:"private_key_pem,omitempty"`
	Password      string `json:"password,omitempty"`
}

func (s *Service) loadCredential() (Credential, error) {
	data, err := os.ReadFile(s.credPath())
	if err != nil {
		return Credential{}, fmt.Errorf("backup: read credential: %w", err)
	}
	var f credentialFile
	if err := json.Unmarshal(data, &f); err != nil {
		return Credential{}, fmt.Errorf("backup: parse credential: %w", err)
	}
	zero(data)
	if f.Auth == AuthKey {
		return Credential{PrivateKeyPEM: []byte(f.PrivateKeyPEM)}, nil
	}
	return Credential{Password: []byte(f.Password)}, nil
}

func newKey() (pemBytes []byte, authorizedLine string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " fleet-main-server-backup"
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), line, nil
}

func installKey(ctx context.Context, rem Remote, line string) error {
	cmd := "set -e; umask 077; mkdir -p \"$HOME/.ssh\"; touch \"$HOME/.ssh/authorized_keys\"; " +
		"grep -qxF " + shQuote(line) + " \"$HOME/.ssh/authorized_keys\" || printf '%s\\n' " +
		shQuote(line) + " >> \"$HOME/.ssh/authorized_keys\""
	_, err := runOut(ctx, rem, cmd, nil)
	return err
}

// ---- local scan: the set as FileEntry values ----

// FileEntry is one file of the backup set: path relative to the base data dir
// (slash-separated), size and SHA-256.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// scan lists the regular files of the backup set under dataDir. Archive
// segments take their digest from the archive manifest when the manifest entry
// matches the file's size (the evidence manifest + SHA-256 reuse); everything
// else is hashed. Files that cannot be read are returned as errs.
func (s *Service) scan() (entries []FileEntry, errs []FileError) {
	for _, top := range SetDirs() {
		root := filepath.Join(s.dataDir, top)
		cache := map[string]evidence.ArchivedSegment{} // slash path -> manifest entry
		if top == evidence.ArchiveDir {
			cache = loadArchiveDigests(root)
		}
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && p == root {
					return nil
				}
				errs = append(errs, FileError{Path: relSlash(s.dataDir, p), Err: err.Error()})
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if d.IsDir() {
				if d.Name() == ".partial" { // in-flight, not yet verified evidence
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() || skipName(d.Name()) {
				return nil
			}
			rel := relSlash(s.dataDir, p)
			if strings.Contains(rel, "\n") {
				errs = append(errs, FileError{Path: rel, Err: "file name contains a newline"})
				return nil
			}
			info, err := d.Info()
			if err != nil {
				errs = append(errs, FileError{Path: rel, Err: err.Error()})
				return nil
			}
			e := FileEntry{Path: rel, Size: info.Size()}
			if seg, ok := cache[rel]; ok && seg.Bytes == e.Size && seg.SHA256 != "" {
				e.SHA256 = seg.SHA256
			} else if e.SHA256, err = hashFile(p); err != nil {
				errs = append(errs, FileError{Path: rel, Err: err.Error()})
				return nil
			}
			entries = append(entries, e)
			return nil
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, errs
}

func skipName(n string) bool {
	return strings.HasSuffix(n, ".tmp") || strings.HasSuffix(n, partSuffix)
}

func relSlash(base, p string) string {
	r, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// loadArchiveDigests reads every archive/<machine>/manifest.json and maps the
// slash path (relative to the base data dir) of each filed segment to its entry.
func loadArchiveDigests(archiveRoot string) map[string]evidence.ArchivedSegment {
	out := map[string]evidence.ArchivedSegment{}
	machines, err := os.ReadDir(archiveRoot)
	if err != nil {
		return out
	}
	for _, m := range machines {
		if !m.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(archiveRoot, m.Name(), evidence.ArchiveManifestName))
		if err != nil {
			continue
		}
		var am evidence.ArchiveManifest
		if json.Unmarshal(data, &am) != nil {
			continue
		}
		for _, seg := range am.Segments {
			out[path.Join(evidence.ArchiveDir, m.Name(), seg.Path)] = seg
		}
	}
	return out
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---- remote index ----

type remoteIndex struct {
	SchemaVersion int         `json:"schema_version"`
	Updated       time.Time   `json:"updated"`
	Files         []FileEntry `json:"files"`
}

func (ix *remoteIndex) byPath() map[string]FileEntry {
	m := make(map[string]FileEntry, len(ix.Files))
	for _, f := range ix.Files {
		m[f.Path] = f
	}
	return m
}

func readIndex(ctx context.Context, rem Remote) (*remoteIndex, error) {
	out, err := runOut(ctx, rem, "cat "+shQuote(remoteIndexFile)+" 2>/dev/null || true", nil)
	if err != nil {
		return nil, err
	}
	ix := &remoteIndex{SchemaVersion: IndexSchemaVersion}
	if len(bytes.TrimSpace(out)) == 0 {
		return ix, nil
	}
	if err := json.Unmarshal(out, ix); err != nil {
		return nil, fmt.Errorf("backup: parse remote index: %w", err)
	}
	return ix, nil
}

// writeIndex merges add into the remote index (additive: entries are updated or
// added, never dropped) and stores it atomically.
func writeIndex(ctx context.Context, rem Remote, ix *remoteIndex, add []FileEntry, now time.Time) error {
	m := ix.byPath()
	for _, e := range add {
		m[e.Path] = e
	}
	ix.Files = ix.Files[:0]
	for _, e := range m {
		ix.Files = append(ix.Files, e)
	}
	sort.Slice(ix.Files, func(i, j int) bool { return ix.Files[i].Path < ix.Files[j].Path })
	ix.SchemaVersion = IndexSchemaVersion
	ix.Updated = now.UTC()
	data, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	f := shQuote(remoteIndexFile)
	_, err = runOut(ctx, rem, "set -e; umask 077; mkdir -p "+shQuote(RemoteMetaDir)+"; cat > "+f+partSuffix+"; mv -f "+
		f+partSuffix+" "+f, bytes.NewReader(data))
	return err
}

// ---- backup ----

// FileError is a file that could not be backed up (or restored).
type FileError struct {
	Path string `json:"path"`
	Err  string `json:"err"`
}

// RunResult reports one backup run.
type RunResult struct {
	Started, Finished time.Time
	Uploaded          []FileEntry // new files and new versions
	Replaced          []string    // uploaded paths whose previous version went to history/
	Unchanged         int         // already on the backup with the same SHA-256
	Bytes             int64       // bytes sent
	Errors            []FileError
}

// OK reports whether every file was backed up or already present.
func (r *RunResult) OK() bool { return len(r.Errors) == 0 }

// BackupNow replicates the set to the backup server, additively, with SHA-256
// verification of every transferred file. It is the manual "back up now" and
// what the nightly schedule calls. It does not wait: if a backup or restore is
// already running it returns ErrBackupRunning.
func (s *Service) BackupNow(ctx context.Context) (*RunResult, error) {
	if !s.runMu.TryLock() {
		return nil, ErrBackupRunning
	}
	defer s.runMu.Unlock()

	srv, ok := s.Server()
	if !ok {
		return nil, ErrNoServer
	}
	res := &RunResult{Started: s.now()}
	err := s.backupLocked(ctx, srv, res)
	res.Finished = s.now()

	rec := RunRecord{Started: res.Started, Finished: res.Finished, OK: err == nil && res.OK(),
		Uploaded: len(res.Uploaded), Failed: len(res.Errors), Bytes: res.Bytes}
	s.mu.Lock()
	s.st.LastBackup = &rec
	if serr := s.saveLocked(); serr != nil {
		s.log.Warn("backup: could not record the run", "err", serr)
	}
	s.mu.Unlock()
	s.log.Info("backup: run finished", "ok", rec.OK, "uploaded", rec.Uploaded, "unchanged", res.Unchanged,
		"failed", rec.Failed, "bytes", rec.Bytes)

	if err != nil {
		return res, err
	}
	if !res.OK() {
		return res, fmt.Errorf("backup: %d file(s) failed, first: %s: %s", len(res.Errors), res.Errors[0].Path, res.Errors[0].Err)
	}
	return res, nil
}

func (s *Service) backupLocked(ctx context.Context, srv Server, res *RunResult) error {
	rem, err := s.connect(ctx, srv)
	if err != nil {
		return err
	}
	defer rem.Close()
	ix, err := readIndex(ctx, rem)
	if err != nil {
		return err
	}
	local, scanErrs := s.scan()
	res.Errors = append(res.Errors, scanErrs...)
	known := ix.byPath()

	var done []FileEntry // verified on the backup (uploaded or already identical)
	for _, e := range local {
		if err := ctx.Err(); err != nil {
			return err
		}
		if k, ok := known[e.Path]; ok && k.SHA256 == e.SHA256 {
			res.Unchanged++
			continue
		}
		replaced, skipped, err := s.upload(ctx, rem, e)
		switch {
		case err != nil:
			res.Errors = append(res.Errors, FileError{Path: e.Path, Err: err.Error()})
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
		case skipped: // present on the backup with this digest, index was behind
			res.Unchanged++
			done = append(done, e)
		default:
			res.Uploaded = append(res.Uploaded, e)
			res.Bytes += e.Size
			if replaced {
				res.Replaced = append(res.Replaced, e.Path)
			}
			done = append(done, e)
		}
	}
	if len(done) > 0 || len(ix.Files) == 0 {
		if err := writeIndex(ctx, rem, ix, done, s.now()); err != nil {
			return fmt.Errorf("backup: update index: %w", err)
		}
	}
	return nil
}

// upload sends one file. skipped: the backup already holds exactly this
// content. replaced: a different version was there and was kept in history/.
func (s *Service) upload(ctx context.Context, rem Remote, e FileEntry) (replaced, skipped bool, err error) {
	dest := path.Join(RemoteDataDir, e.Path)
	cur, err := runOut(ctx, rem, "if [ -f "+shQuote(dest)+" ]; then sha256sum "+shQuote(dest)+" | cut -d' ' -f1; fi", nil)
	if err != nil {
		return false, false, err
	}
	have := strings.TrimSpace(string(cur))
	if have == e.SHA256 {
		return false, true, nil
	}
	replaced = have != ""

	f, err := os.Open(filepath.Join(s.dataDir, filepath.FromSlash(e.Path)))
	if err != nil {
		return false, false, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Size() < e.Size {
		return false, false, errors.New("file changed since it was scanned")
	}
	// Send exactly the scanned length: an append-only file that grew since the
	// scan still hashes to the scanned digest; the next run picks up the rest.
	h := sha256.New()
	body := io.TeeReader(io.LimitReader(f, e.Size), h)

	tmp := shQuote(dest + partSuffix)
	hist := shQuote(path.Join(RemoteHistoryDir, e.Path) + ".")
	script := "set -e; umask 077; f=" + shQuote(dest) + "; t=" + tmp + "; mkdir -p \"$(dirname \"$f\")\"; cat > \"$t\"; " +
		"sum=$(sha256sum \"$t\" | cut -d' ' -f1); " +
		"if [ \"$sum\" != " + shQuote(e.SHA256) + " ]; then rm -f \"$t\"; exit 3; fi; " +
		"if [ -f \"$f\" ]; then old=$(sha256sum \"$f\" | cut -d' ' -f1); h=" + hist + "\"$old\"; " +
		"mkdir -p \"$(dirname \"$h\")\"; [ -e \"$h\" ] || cp -p \"$f\" \"$h\"; fi; " +
		"mv -f \"$t\" \"$f\""
	if err := rem.Exec(ctx, script, body, nil); err != nil {
		var re *RemoteError
		if errors.As(err, &re) && re.Code == 3 {
			if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
				return false, false, fmt.Errorf("%w: the file no longer matches its manifest digest (changed or corrupt locally)", ErrChecksum)
			}
			return false, false, fmt.Errorf("%w: backup server received different bytes", ErrChecksum)
		}
		return false, false, err
	}
	return replaced, false, nil
}

func (s *Service) connect(ctx context.Context, srv Server) (Remote, error) {
	cred, err := s.loadCredential()
	if err != nil {
		return nil, err
	}
	rem, _, err := s.dial(ctx, srv, cred)
	zero(cred.Password)
	if err != nil {
		return nil, fmt.Errorf("backup: connect to %s: %w", srv.addr(), err)
	}
	return rem, nil
}

// ---- helpers ----

func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// shQuote single-quotes s for a POSIX shell.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// runOut runs cmd and returns its stdout.
func runOut(ctx context.Context, rem Remote, cmd string, stdin io.Reader) ([]byte, error) {
	var out bytes.Buffer
	if err := rem.Exec(ctx, cmd, stdin, &out); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ---- real SSH ----

func dialSSH(ctx context.Context, srv Server, cred Credential, timeout time.Duration) (Remote, string, error) {
	var methods []ssh.AuthMethod
	if len(cred.PrivateKeyPEM) > 0 {
		signer, err := ssh.ParsePrivateKey(cred.PrivateKeyPEM)
		if err != nil {
			return nil, "", fmt.Errorf("backup: parse key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if len(cred.Password) > 0 {
		methods = append(methods, ssh.Password(string(cred.Password)))
	}
	if len(methods) == 0 {
		return nil, "", errors.New("backup: no credential")
	}
	var seen string
	cfg := &ssh.ClientConfig{
		User: srv.User,
		Auth: methods,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			seen = ssh.FingerprintSHA256(key)
			if srv.HostKeyFP != "" && srv.HostKeyFP != seen {
				return fmt.Errorf("SSH host key mismatch: got %s, pinned %s", seen, srv.HostKeyFP)
			}
			return nil
		},
		Timeout: timeout,
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", srv.addr())
	if err != nil {
		return nil, "", err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout)) // bound the handshake
	sc, chans, reqs, err := ssh.NewClientConn(conn, srv.addr(), cfg)
	if err != nil {
		_ = conn.Close()
		return nil, "", err
	}
	_ = conn.SetDeadline(time.Time{})
	return &sshRemote{client: ssh.NewClient(sc, chans, reqs)}, seen, nil
}

type sshRemote struct{ client *ssh.Client }

func (r *sshRemote) Close() error { return r.client.Close() }

func (r *sshRemote) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	sess, err := r.client.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close()
	var stderr limitedBuffer
	if stdin != nil {
		sess.Stdin = stdin
	}
	if stdout != nil {
		sess.Stdout = stdout
	}
	sess.Stderr = &stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Close()
		return ctx.Err()
	case err := <-done:
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			return &RemoteError{Code: ee.ExitStatus(), Stderr: stderr.String()}
		}
		return err
	}
}

// limitedBuffer keeps the first 4 KiB written to it (stderr for error messages).
type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := 4096 - b.buf.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		b.buf.Write(p[:room])
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
