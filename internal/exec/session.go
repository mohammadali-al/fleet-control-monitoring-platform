package exec

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var sessionIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidSessionID reports whether id is usable as a session id. Session ids
// become cgroup directory names, so they are restricted to a safe charset.
func ValidSessionID(id string) bool { return sessionIDRe.MatchString(id) }

// Session is one command execution on a worker agent. Its metadata is held
// once here, not repeated per packet; evidence capture (T-0008) binds to ID
// and CgroupPath.
type Session struct {
	ID      string
	Command string
	User    string
	Host    string
	Mode    string

	cgroup *Cgroup

	mu       sync.Mutex
	start    time.Time
	end      time.Time
	exitCode int
	finished bool
}

// SessionInfo is an immutable snapshot of a Session.
type SessionInfo struct {
	ID, Command, User, Host, Mode string
	CgroupPath                    string // "" when cgroups are disabled
	Start, End                    time.Time
	ExitCode                      int
	Finished                      bool
}

// CgroupPath is the session's tracked cgroup v2 directory ("" if none). It is
// valid from when the session starts until it finishes.
func (s *Session) CgroupPath() string {
	if s.cgroup == nil {
		return ""
	}
	return s.cgroup.Path
}

// Info returns a snapshot.
func (s *Session) Info() SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SessionInfo{
		ID: s.ID, Command: s.Command, User: s.User, Host: s.Host, Mode: s.Mode,
		CgroupPath: s.CgroupPath(), Start: s.start, End: s.end,
		ExitCode: s.exitCode, Finished: s.finished,
	}
}

func (s *Session) markStarted() {
	s.mu.Lock()
	s.start = time.Now()
	s.mu.Unlock()
}

func (s *Session) markFinished(code int) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.end, s.exitCode, s.finished = time.Now(), code, true
	return s.end.Sub(s.start)
}

// Cgroup is a per-session cgroup v2 directory. The command is placed in it at
// fork time (CLONE_INTO_CGROUP via SysProcAttr.CgroupFD), so there is no window
// in which it runs outside the group.
type Cgroup struct {
	Path string
	dir  *os.File
}

// newCgroup creates <root>/<id> and opens it.
func newCgroup(root, id string) (*Cgroup, error) {
	if !ValidSessionID(id) {
		return nil, fmt.Errorf("exec: invalid session id %q", id)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("exec: cgroup root: %w", err)
	}
	path := filepath.Join(root, id)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, fmt.Errorf("exec: create cgroup: %w", err)
	}
	dir, err := os.Open(path)
	if err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("exec: open cgroup: %w", err)
	}
	return &Cgroup{Path: path, dir: dir}, nil
}

// attach makes attrs place the child in this cgroup.
func (c *Cgroup) attach(attrs *syscall.SysProcAttr) {
	attrs.UseCgroupFD = true
	attrs.CgroupFD = int(c.dir.Fd())
}

// Kill SIGKILLs every process in the cgroup (cgroup.kill, kernel 5.14+).
func (c *Cgroup) Kill() error {
	return os.WriteFile(filepath.Join(c.Path, "cgroup.kill"), []byte("1"), 0o644)
}

// Procs lists the pids currently in the cgroup.
func (c *Cgroup) Procs() []int {
	b, err := os.ReadFile(filepath.Join(c.Path, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			pids = append(pids, n)
		}
	}
	return pids
}

// Remove kills anything left in the cgroup and removes the directory.
func (c *Cgroup) Remove() error {
	defer c.dir.Close()
	_ = c.Kill()
	var err error
	for i := 0; i < 100; i++ { // rmdir fails until killed tasks are reaped
		if err = os.Remove(c.Path); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return err
}
