package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Throwaway credentials for the in-process SSH target (D-0002).
const (
	testUser = "backup"
	testPass = "throwaway-test-password"
)

// ---- in-process SSH target: runs the real POSIX shell commands in a temp home ----

type sshTarget struct {
	home       string
	host       string
	port       int
	hostFP     string
	rejectKeys bool // refuse public-key auth (forces the password fallback)
	ln         net.Listener
	wg         sync.WaitGroup
	mu         sync.Mutex
	cmds       []string
}

func startTarget(t *testing.T, rejectKeys bool) *sshTarget {
	t.Helper()
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("sha256sum not available")
	}
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	tg := &sshTarget{home: home, rejectKeys: rejectKeys, hostFP: ssh.FingerprintSHA256(hostSigner.PublicKey())}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == testUser && string(pw) == testPass {
				return nil, nil
			}
			return nil, errors.New("bad password")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if tg.rejectKeys || c.User() != testUser {
				return nil, errors.New("keys refused")
			}
			data, err := os.ReadFile(filepath.Join(home, ".ssh", "authorized_keys"))
			if err != nil {
				return nil, err
			}
			for len(data) > 0 {
				ak, _, _, rest, err := ssh.ParseAuthorizedKey(data)
				if err != nil {
					break
				}
				if bytes.Equal(ak.Marshal(), k.Marshal()) {
					return nil, nil
				}
				data = rest
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	tg.host, tg.ln = h, ln
	tg.port, _ = strconv.Atoi(p)
	tg.wg.Add(1)
	go func() {
		defer tg.wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tg.wg.Add(1)
			go func() { defer tg.wg.Done(); tg.serve(c, cfg) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); tg.wg.Wait() })
	return tg
}

func (tg *sshTarget) serve(c net.Conn, cfg *ssh.ServerConfig) {
	defer c.Close()
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			return
		}
		go tg.session(ch, creqs)
	}
}

func (tg *sshTarget) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if ssh.Unmarshal(req.Payload, &payload) != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)
		tg.mu.Lock()
		tg.cmds = append(tg.cmds, payload.Command)
		tg.mu.Unlock()
		cmd := exec.Command("sh", "-c", payload.Command)
		cmd.Dir = tg.home
		cmd.Env = []string{"HOME=" + tg.home, "PATH=" + os.Getenv("PATH")}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
		status := uint32(0)
		if err := cmd.Run(); err != nil {
			status = 1
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				status = uint32(ee.ExitCode())
			}
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

func (tg *sshTarget) server() Server { return Server{Host: tg.host, Port: tg.port, User: testUser} }

func (tg *sshTarget) remoteFile(rel string) string { return filepath.Join(tg.home, RemoteDataDir, rel) }

func (tg *sshTarget) commands() []string {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return append([]string(nil), tg.cmds...)
}

// ---- fixtures ----

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func write(t *testing.T, base, rel, content string) {
	t.Helper()
	p := filepath.Join(base, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func sum(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// populate writes a representative backup set, including an archive segment
// with a real archive manifest, and a CA key.
func populate(t *testing.T, dataDir string) {
	t.Helper()
	seg := "segment-one-bytes"
	write(t, dataDir, "archive/m1/s1/seg-0001.fev", seg)
	write(t, dataDir, "archive/m1/manifest.json", `{"schema_version":1,"machine":"m1","segments":[{"file":"seg-0001.fev","sha256":"`+
		sum(seg)+`","bytes":`+strconv.Itoa(len(seg))+`,"session":"s1","path":"s1/seg-0001.fev"}]}`)
	write(t, dataDir, "archive/m1/.partial/inflight.part", "must not be backed up")
	write(t, dataDir, "monitor/jobs.json", `{"jobs":[]}`)
	write(t, dataDir, "projects/p1/maneuver.json", `{"name":"ramp"}`)
	write(t, dataDir, "registry/machines.json", `{"machines":[]}`)
	write(t, dataDir, "ca/ca.key", "-----BEGIN PRIVATE KEY-----\nSECRET-CA-KEY\n-----END PRIVATE KEY-----\n")
	write(t, dataDir, "ca/ca.crt", "CERT")
	write(t, dataDir, "config/gate.key", "gate secret, not in the set")
	write(t, dataDir, "logs/server.log", "not in the set")
}

type env struct {
	t   *testing.T
	dir string
	tg  *sshTarget
	svc *Service
	now time.Time
	mu  sync.Mutex
}

func (e *env) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func newEnv(t *testing.T, rejectKeys bool) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), tg: startTarget(t, rejectKeys),
		now: time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)}
	populate(t, e.dir)
	svc, err := Open(e.dir, Options{Now: e.clock, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	e.svc = svc
	return e
}

func (e *env) add() *AddResult {
	e.t.Helper()
	res, err := e.svc.AddServer(context.Background(), e.tg.server(), []byte(testPass))
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

func (e *env) backup() *RunResult {
	e.t.Helper()
	res, err := e.svc.BackupNow(context.Background())
	if err != nil {
		e.t.Fatalf("backup: %v (%+v)", err, res)
	}
	return res
}

// ---- tests ----

func TestAddServerRecordsItAndSuggestsASchedule(t *testing.T) {
	e := newEnv(t, false)
	res := e.add()

	srv, ok := e.svc.Server()
	if !ok || srv.Host != e.tg.host || srv.User != testUser || srv.Port != e.tg.port {
		t.Fatalf("server not recorded: %+v ok=%v", srv, ok)
	}
	if srv.HostKeyFP != e.tg.hostFP {
		t.Errorf("host key not pinned on first use: %q", srv.HostKeyFP)
	}
	if srv.Auth != AuthKey {
		t.Errorf("auth = %q, want key", srv.Auth)
	}
	if srv.RemotePath() != "fleet-control-backup/data" {
		t.Errorf("remote path = %q", srv.RemotePath())
	}
	// The constant path now exists on the backup server.
	if st, err := os.Stat(filepath.Join(e.tg.home, RemoteDataDir)); err != nil || !st.IsDir() {
		t.Errorf("constant path not created: %v", err)
	}
	// The diff after adding: everything is missing; a nightly schedule is suggested.
	if res.DiffErr != nil || res.Diff == nil || len(res.Diff.Missing) == 0 || res.Diff.Present != 0 {
		t.Errorf("diff after add: %+v err=%v", res.Diff, res.DiffErr)
	}
	if res.Suggested.Hour < 1 || res.Suggested.Hour > 4 || res.Suggested.Enabled {
		t.Errorf("suggested schedule not a middle-of-the-night suggestion: %+v", res.Suggested)
	}
	// It survives a restart.
	again, err := Open(e.dir, Options{Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if s2, ok := again.Server(); !ok || s2.Host != srv.Host {
		t.Errorf("server not persisted: %+v", s2)
	}
}

func TestPasswordIsNotStoredWhenAKeyCanBeInstalled(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	// Nothing under the data dir contains the password.
	_ = filepath.WalkDir(e.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.Contains(read(t, p), testPass) {
			t.Errorf("password found in %s", p)
		}
		return nil
	})
	st, err := os.Stat(filepath.Join(e.dir, "config", "backup-credential"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("credential mode = %v, want 0600", st.Mode().Perm())
	}
	// Backups work with the key alone.
	if res := e.backup(); len(res.Uploaded) == 0 {
		t.Error("nothing uploaded")
	}
}

func TestPasswordFallbackWhenKeyAuthIsRefused(t *testing.T) {
	e := newEnv(t, true)
	res := e.add()
	if res.Server.Auth != AuthPassword {
		t.Fatalf("auth = %q, want password fallback", res.Server.Auth)
	}
	cred := read(t, filepath.Join(e.dir, "config", "backup-credential"))
	if !strings.Contains(cred, testPass) {
		t.Error("fallback should keep the password in the credential file")
	}
	e.backup() // still works
	// The credential is not part of the backup set.
	for _, c := range e.tg.commands() {
		if strings.Contains(c, testPass) {
			t.Fatalf("password appeared in a remote command: %s", c)
		}
	}
}

func TestBackupIsAdditiveWithSHA256Verification(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	res := e.backup()

	// The set is on the backup under the constant path, byte-identical.
	want := map[string]string{
		"archive/m1/s1/seg-0001.fev": "segment-one-bytes",
		"archive/m1/manifest.json":   read(t, filepath.Join(e.dir, "archive/m1/manifest.json")),
		"monitor/jobs.json":          `{"jobs":[]}`,
		"projects/p1/maneuver.json":  `{"name":"ramp"}`,
		"registry/machines.json":     `{"machines":[]}`,
		"ca/ca.key":                  read(t, filepath.Join(e.dir, "ca/ca.key")),
		"ca/ca.crt":                  "CERT",
	}
	for rel, content := range want {
		if got := read(t, e.tg.remoteFile(rel)); got != content {
			t.Errorf("%s on backup = %q, want %q", rel, got, content)
		}
	}
	if len(res.Uploaded) != len(want) {
		t.Errorf("uploaded %d files, want %d", len(res.Uploaded), len(want))
	}
	// Not in the set.
	for _, rel := range []string{"config/gate.key", "logs/server.log", "archive/m1/.partial/inflight.part"} {
		if _, err := os.Stat(e.tg.remoteFile(rel)); err == nil {
			t.Errorf("%s must not be backed up", rel)
		}
	}
	// No temp left behind.
	leftovers, _ := filepath.Glob(filepath.Join(e.tg.home, RemoteBase, "*", "*"+partSuffix))
	if len(leftovers) != 0 {
		t.Errorf("leftover temps: %v", leftovers)
	}

	// A second run sends nothing.
	if again := e.backup(); len(again.Uploaded) != 0 || again.Unchanged != len(want) {
		t.Errorf("second run uploaded %d, unchanged %d", len(again.Uploaded), again.Unchanged)
	}
	if last, ok := e.svc.LastBackup(); !ok || !last.OK {
		t.Errorf("last backup not recorded: %+v", last)
	}
	// The backup never issues a delete of backed-up data: the only rm in any
	// command is the upload's own temp file.
	for _, c := range e.tg.commands() {
		if strings.Contains(c, "rm ") && !strings.Contains(c, `rm -f "$t"`) {
			t.Errorf("unexpected rm in remote command: %s", c)
		}
		if strings.Contains(c, "rm -r") || strings.Contains(c, "--delete") {
			t.Errorf("destructive command: %s", c)
		}
	}
}

func TestChangedFileKeepsPreviousVersionAndNothingIsDeleted(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	e.backup()

	oldRegistry := `{"machines":[]}`
	write(t, e.dir, "registry/machines.json", `{"machines":["m1"]}`)
	if err := os.Remove(filepath.Join(e.dir, "projects/p1/maneuver.json")); err != nil { // vanishes on main
		t.Fatal(err)
	}
	res := e.backup()
	if len(res.Uploaded) != 1 || res.Uploaded[0].Path != "registry/machines.json" || len(res.Replaced) != 1 {
		t.Fatalf("run result: %+v", res)
	}
	if got := read(t, e.tg.remoteFile("registry/machines.json")); got != `{"machines":["m1"]}` {
		t.Errorf("current = %q", got)
	}
	hist := filepath.Join(e.tg.home, RemoteHistoryDir, "registry/machines.json."+sum(oldRegistry))
	if got := read(t, hist); got != oldRegistry {
		t.Errorf("previous version not kept in history: %q", got)
	}
	// Deleted on main, still on the backup.
	if got := read(t, e.tg.remoteFile("projects/p1/maneuver.json")); got != `{"name":"ramp"}` {
		t.Errorf("file deleted on main must remain on backup, got %q", got)
	}
	d, err := e.svc.Diff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.InSync() || len(d.BackupOnly) != 1 || d.BackupOnly[0] != "projects/p1/maneuver.json" {
		t.Errorf("diff: %+v", d)
	}
}

func TestPreExistingBackupFileIsPreservedNotClobbered(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	// Something already sits at a path main is about to write.
	p := e.tg.remoteFile("monitor/jobs.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("older backup content"), 0o600); err != nil {
		t.Fatal(err)
	}
	e.backup()
	hist := filepath.Join(e.tg.home, RemoteHistoryDir, "monitor/jobs.json."+sum("older backup content"))
	if read(t, hist) != "older backup content" {
		t.Error("existing backup file must be preserved in history")
	}
}

// corruptingDial wraps the real dialer and flips one byte of every upload.
func corruptingDial(timeout time.Duration) DialFunc {
	return func(ctx context.Context, srv Server, cred Credential) (Remote, string, error) {
		r, fp, err := dialSSH(ctx, srv, cred, timeout)
		if err != nil {
			return nil, "", err
		}
		return &corruptRemote{Remote: r}, fp, nil
	}
}

type corruptRemote struct{ Remote }

func (c *corruptRemote) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	if stdin != nil && strings.Contains(cmd, "$t") && !strings.Contains(cmd, "index.json") {
		b, _ := io.ReadAll(stdin)
		if len(b) > 0 {
			b[0] ^= 0xff
		}
		stdin = bytes.NewReader(b)
	}
	return c.Remote.Exec(ctx, cmd, stdin, stdout)
}

func TestTransferCorruptionIsDetectedAndNotCommitted(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	svc, err := Open(e.dir, Options{Dial: corruptingDial(5 * time.Second), Now: e.clock, Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	res, err := svc.BackupNow(context.Background())
	if err == nil || res.OK() {
		t.Fatalf("expected a checksum failure, got err=%v res=%+v", err, res)
	}
	if len(res.Uploaded) != 0 {
		t.Errorf("corrupt uploads must not count: %+v", res.Uploaded)
	}
	for _, f := range res.Errors {
		if !strings.Contains(f.Err, "checksum mismatch") {
			t.Errorf("%s: %s", f.Path, f.Err)
		}
	}
	if _, err := os.Stat(e.tg.remoteFile("ca/ca.key")); err == nil {
		t.Error("a file that failed verification must not be committed")
	}
	if rec, _ := svc.LastBackup(); rec.OK {
		t.Error("a failed run must not be recorded as OK")
	}
}

func TestSourceDifferingFromManifestDigestIsReported(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	// The archive file no longer matches its manifest digest (same size).
	write(t, e.dir, "archive/m1/s1/seg-0001.fev", "SEGMENT-ONE-BYTES")
	res, err := e.svc.BackupNow(context.Background())
	if err == nil {
		t.Fatal("expected failure")
	}
	var found bool
	for _, f := range res.Errors {
		if f.Path == "archive/m1/s1/seg-0001.fev" && strings.Contains(f.Err, "manifest digest") {
			found = true
		}
	}
	if !found {
		t.Errorf("errors: %+v", res.Errors)
	}
	// Everything else still went through.
	if read(t, e.tg.remoteFile("ca/ca.key")) == "" {
		t.Error("other files should still back up")
	}
}

func TestDiffReportsWhatMainHasThatBackupLacks(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	e.backup()

	write(t, e.dir, "archive/m1/s1/seg-0002.fev", "new segment")
	write(t, e.dir, "monitor/jobs.json", `{"jobs":["a"]}`)
	d, err := e.svc.Diff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Missing) != 1 || d.Missing[0].Path != "archive/m1/s1/seg-0002.fev" {
		t.Errorf("missing: %+v", d.Missing)
	}
	if len(d.Changed) != 1 || d.Changed[0].Path != "monitor/jobs.json" {
		t.Errorf("changed: %+v", d.Changed)
	}
	if d.InSync() || d.Pending != int64(len("new segment")+len(`{"jobs":["a"]}`)) || d.Present == 0 {
		t.Errorf("diff: %+v", d)
	}
	// Diff sends nothing.
	if _, err := os.Stat(e.tg.remoteFile("archive/m1/s1/seg-0002.fev")); err == nil {
		t.Error("Diff must not transfer")
	}
	e.backup()
	if d2, _ := e.svc.Diff(context.Background()); !d2.InSync() {
		t.Errorf("after backup: %+v", d2)
	}
}

func TestScheduleSuggestionAndNext(t *testing.T) {
	s := SuggestSchedule()
	if s.Hour != 2 || s.Minute != 30 {
		t.Fatalf("suggestion: %+v", s)
	}
	loc := time.UTC
	if got := s.Next(time.Date(2026, 11, 24, 1, 0, 0, 0, loc)); !got.Equal(time.Date(2026, 11, 24, 2, 30, 0, 0, loc)) {
		t.Errorf("next before: %v", got)
	}
	if got := s.Next(time.Date(2026, 11, 24, 2, 30, 0, 0, loc)); !got.Equal(time.Date(2026, 11, 25, 2, 30, 0, 0, loc)) {
		t.Errorf("next at: %v", got)
	}
	e := newEnv(t, false)
	if err := e.svc.SetSchedule(Schedule{Enabled: true, Hour: 25}); err == nil {
		t.Error("invalid hour accepted")
	}
	if err := e.svc.SetSchedule(Schedule{Enabled: true, Hour: 3, Minute: 5}); err != nil {
		t.Fatal(err)
	}
	again, _ := Open(e.dir, Options{Log: quietLog()})
	if sc := again.Schedule(); !sc.Enabled || sc.Hour != 3 || sc.Minute != 5 {
		t.Errorf("schedule not persisted: %+v", sc)
	}
}

func TestSchedulerRunsBackupAtTheScheduledTime(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	// A clock that starts one second before the scheduled 02:30 and runs in real time.
	start := time.Now()
	base := time.Date(2026, 11, 24, 2, 29, 59, 0, time.UTC)
	clock := func() time.Time { return base.Add(time.Since(start)) }
	if err := e.svc.SetSchedule(Schedule{Enabled: true, Hour: 2, Minute: 30}); err != nil {
		t.Fatal(err)
	}
	svc, _ := Open(e.dir, Options{Log: quietLog(), Now: clock})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := svc.LastBackup(); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	if last, ok := svc.LastBackup(); !ok || !last.OK {
		t.Fatalf("scheduled backup did not run: %+v", last)
	}
	if read(t, e.tg.remoteFile("ca/ca.crt")) != "CERT" {
		t.Error("scheduled backup did not transfer")
	}
}

// blockingRemote stalls the first upload until released.
type blockingRemote struct {
	Remote
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingRemote) Exec(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {
	if stdin != nil && strings.Contains(cmd, "$t") {
		b.once.Do(func() { close(b.started) })
		<-b.release
	}
	return b.Remote.Exec(ctx, cmd, stdin, stdout)
}

func TestBackupNowDoesNotStackAndReminderNeverBlocks(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	b := &blockingRemote{started: make(chan struct{}), release: make(chan struct{})}
	svc, err := Open(e.dir, Options{Now: e.clock, Log: quietLog(), Dial: func(ctx context.Context, srv Server, c Credential) (Remote, string, error) {
		r, fp, err := dialSSH(ctx, srv, c, 5*time.Second)
		b.Remote = r
		return b, fp, err
	}})
	if err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { _, err := svc.BackupNow(context.Background()); errc <- err }()
	<-b.started

	if _, err := svc.BackupNow(context.Background()); !errors.Is(err, ErrBackupRunning) {
		t.Errorf("second backup: %v, want ErrBackupRunning", err)
	}
	// The reminder returns while the backup is stuck mid-transfer.
	got := make(chan bool, 1)
	go func() { _, ok := svc.Reminder(e.clock()); got <- ok }()
	select {
	case ok := <-got:
		if !ok {
			t.Error("a server with no backup yet should get a stale reminder")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Reminder blocked while a backup was running")
	}
	close(b.release)
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
}

func TestReminderAtMostOncePerDay(t *testing.T) {
	dir := t.TempDir()
	svc, err := Open(dir, Options{Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 11, 24, 9, 0, 0, 0, time.UTC)
	r, ok := svc.Reminder(day1)
	if !ok || r.Kind != ReminderNoServer || r.Message == "" {
		t.Fatalf("first reminder: %+v %v", r, ok)
	}
	if _, ok := svc.Reminder(day1.Add(5 * time.Hour)); ok {
		t.Error("second reminder the same day")
	}
	// It survives a restart: still once per day.
	svc2, _ := Open(dir, Options{Log: quietLog()})
	if _, ok := svc2.Reminder(day1.Add(8 * time.Hour)); ok {
		t.Error("reminder repeated after restart on the same day")
	}
	if _, ok := svc2.Reminder(day1.Add(24 * time.Hour)); !ok {
		t.Error("next day should remind again")
	}
}

func TestNoReminderAfterAFreshBackupThenStale(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	e.backup()
	if _, ok := e.svc.Reminder(e.clock()); ok {
		t.Error("no reminder expected right after a successful backup")
	}
	e.advance(36 * time.Hour)
	r, ok := e.svc.Reminder(e.clock())
	if !ok || r.Kind != ReminderStale {
		t.Errorf("stale reminder: %+v %v", r, ok)
	}
	if _, ok := e.svc.Reminder(e.clock().Add(time.Hour)); ok {
		t.Error("second reminder the same day")
	}
}

func TestRoundTripBackupWipeImportReproducesTheData(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	e.backup()
	snapshot := snapshotSet(t, e.dir)
	caKey := read(t, filepath.Join(e.dir, "ca/ca.key"))

	// Wipe the local copy of the set.
	for _, d := range SetDirs() {
		if err := os.RemoveAll(filepath.Join(e.dir, d)); err != nil {
			t.Fatal(err)
		}
	}
	res, err := e.svc.Restore(context.Background(), RestoreOptions{})
	if err != nil {
		t.Fatalf("restore: %v (%+v)", err, res)
	}
	if len(res.Restored) != len(snapshot) || !res.OK() {
		t.Errorf("restored %d of %d: %+v", len(res.Restored), len(snapshot), res)
	}
	got := snapshotSet(t, e.dir)
	if len(got) != len(snapshot) {
		t.Fatalf("files after restore: %v, want %v", keys(got), keys(snapshot))
	}
	for p, h := range snapshot {
		if got[p] != h {
			t.Errorf("%s differs after round trip", p)
		}
	}
	if read(t, filepath.Join(e.dir, "ca/ca.key")) != caKey {
		t.Error("CA key not restored")
	}
	if st, _ := os.Stat(filepath.Join(e.dir, "ca/ca.key")); st.Mode().Perm() != 0o600 {
		t.Errorf("restored CA key mode = %v", st.Mode().Perm())
	}
	// A restored tree is in sync with the backup.
	if d, _ := e.svc.Diff(context.Background()); !d.InSync() {
		t.Errorf("diff after restore: %+v", d)
	}
	// Re-running the import changes nothing.
	again, err := e.svc.Restore(context.Background(), RestoreOptions{})
	if err != nil || len(again.Restored) != 0 || again.Unchanged != len(snapshot) {
		t.Errorf("idempotent restore: %+v %v", again, err)
	}
}

func TestRestoreKeepsDifferingLocalFilesUnlessOverwrite(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	e.backup()
	write(t, e.dir, "monitor/jobs.json", "newer local data")
	res, err := e.svc.Restore(context.Background(), RestoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0] != "monitor/jobs.json" ||
		read(t, filepath.Join(e.dir, "monitor/jobs.json")) != "newer local data" {
		t.Errorf("conflict handling: %+v", res)
	}
	if _, err := e.svc.Restore(context.Background(), RestoreOptions{Overwrite: true}); err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(e.dir, "monitor/jobs.json")) != `{"jobs":[]}` {
		t.Error("overwrite did not restore")
	}
}

func TestRestoreIntoAFreshServerWithoutItsOwnState(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	e.backup()
	snapshot := snapshotSet(t, e.dir)

	fresh := t.TempDir() // a rebuilt main server: no config/, no credential
	svc, err := Open(fresh, Options{Log: quietLog()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Restore(context.Background(), RestoreOptions{}); !errors.Is(err, ErrNoServer) {
		t.Fatalf("restore without a server: %v", err)
	}
	if _, err := svc.RestoreWithPassword(context.Background(), e.tg.server(), []byte(testPass), RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	got := snapshotSet(t, fresh)
	for p, h := range snapshot {
		if got[p] != h {
			t.Errorf("%s differs on the fresh server", p)
		}
	}
	if read(t, filepath.Join(fresh, "ca/ca.key")) == "" {
		t.Error("CA key missing")
	}
}

func TestHostKeyIsPinned(t *testing.T) {
	e := newEnv(t, false)
	e.add()
	srv, _ := e.svc.Server()
	srv.HostKeyFP = "SHA256:not-the-real-one"
	e.svc.mu.Lock()
	e.svc.st.Server = &srv
	e.svc.mu.Unlock()
	if _, err := e.svc.BackupNow(context.Background()); err == nil || !strings.Contains(err.Error(), "host key mismatch") {
		t.Errorf("err = %v, want host key mismatch", err)
	}
}

func TestBackupWithoutServerAndBadInput(t *testing.T) {
	svc, _ := Open(t.TempDir(), Options{Log: quietLog()})
	if _, err := svc.BackupNow(context.Background()); !errors.Is(err, ErrNoServer) {
		t.Errorf("BackupNow: %v", err)
	}
	if _, err := svc.Diff(context.Background()); !errors.Is(err, ErrNoServer) {
		t.Errorf("Diff: %v", err)
	}
	for _, s := range []Server{{User: "u"}, {Host: "1.2.3.4"}, {Host: "a b", User: "u"}, {Host: "h", User: "u", Port: 70000}} {
		if _, err := svc.AddServer(context.Background(), s, []byte("x")); err == nil {
			t.Errorf("AddServer(%+v) accepted", s)
		}
	}
	if _, err := svc.AddServer(context.Background(), Server{Host: "127.0.0.1", User: "u"}, nil); err == nil {
		t.Error("empty password accepted")
	}
	pw := []byte("secret")
	_, _ = svc.AddServer(context.Background(), Server{Host: "127.0.0.1", Port: 1, User: "u"}, pw)
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Error("password buffer not zeroed")
	}
}

func TestInSetRejectsUnsafePaths(t *testing.T) {
	for _, ok := range []string{"ca/ca.key", "archive/m/s/f", "projects/x"} {
		if !inSet(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "ca/../../x", "config/gate.key", "logs/a", "ca", "ca//x", "./ca/x", "a/ca/x"} {
		if inSet(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestShQuote(t *testing.T) {
	if got := shQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shQuote = %s", got)
	}
}

func TestConstantPathIsFixed(t *testing.T) {
	if RemoteDataDir != "fleet-control-backup/data" || RemoteHistoryDir != "fleet-control-backup/history" ||
		RemoteMetaDir != "fleet-control-backup/meta" {
		t.Errorf("constant path changed: %s %s %s", RemoteDataDir, RemoteHistoryDir, RemoteMetaDir)
	}
}

// ---- helpers ----

func snapshotSet(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, d := range SetDirs() {
		_ = filepath.WalkDir(filepath.Join(dir, d), func(p string, de fs.DirEntry, err error) error {
			if err != nil || de.IsDir() || strings.Contains(p, ".partial") {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			out[filepath.ToSlash(rel)] = sum(read(t, p))
			return nil
		})
	}
	return out
}

func keys(m map[string]string) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}
