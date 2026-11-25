package enroll

// The token-gated enrollment endpoint. It is served on its own TLS listener,
// not through the panel, so it is reachable without the operator auth proxy:
// the token is the only authentication here. Nothing on it is logged with a
// credential in it.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxFormBytes = 32 << 10
	// failLimit failed authentications from one address within failWindow
	// block that address for the rest of the window (a short code is guessable
	// in principle; this makes it not so in practice).
	failLimit  = 10
	failWindow = time.Minute
)

// Handler is the enrollment endpoint:
//
//	GET  /enroll/e/{code}        the bootstrap script, carrying the token (code or token)
//	GET  /enroll/ca.crt          the pinned CA certificate     (Authorization: Bearer <token>)
//	GET  /enroll/agent?os&arch   the agent binary              (Authorization: Bearer <token>)
//	POST /enroll/csr             CSR + specs -> machine id + signed certificate (Bearer <token>)
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /enroll/e/{code}", s.serveScript)
	mux.HandleFunc("GET /enroll/ca.crt", s.serveCA)
	mux.HandleFunc("GET /enroll/agent", s.serveAgent)
	mux.HandleFunc("POST /enroll/csr", s.serveCSR)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

// TLSConfig is the endpoint's TLS configuration: the certificate the CA issued
// for it, whose public key the one-liner pins.
func (s *Service) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{s.tlsCert}}
}

// Listen binds the enrollment endpoint's TLS listener.
func (s *Service) Listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	return tls.NewListener(ln, s.TLSConfig()), nil
}

// Serve serves the endpoint on ln until ctx is cancelled.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      10 * time.Minute, // the agent binary is tens of MB
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shut); err != nil {
		_ = srv.Close()
		return err
	}
	<-errc
	return nil
}

// ---- failed-attempt limiter ----------------------------------------------------

type limiter struct {
	mu    sync.Mutex
	now   func() time.Time
	fails map[string][]time.Time
}

func newLimiter(now func() time.Time) *limiter {
	return &limiter{now: now, fails: map[string][]time.Time{}}
}

func (l *limiter) blocked(addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(addr)) >= failLimit
}

func (l *limiter) fail(addr string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.fails[addr] = append(l.recent(addr), l.now())
}

// recent drops the attempts older than the window and returns the rest.
func (l *limiter) recent(addr string) []time.Time {
	cut := l.now().Add(-failWindow)
	kept := l.fails[addr][:0]
	for _, t := range l.fails[addr] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.fails, addr)
	}
	return kept
}

func remoteHost(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// ---- handlers ------------------------------------------------------------------

func bearer(r *http.Request) string {
	const p = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// deny answers every token failure the same way, whatever the reason.
func (s *Service) deny(w http.ResponseWriter, r *http.Request) {
	s.lim.fail(remoteHost(r))
	http.Error(w, "enrollment token is invalid, expired or already used", http.StatusUnauthorized)
}

// authorize checks the token of a download (it is not consumed here).
func (s *Service) authorize(w http.ResponseWriter, r *http.Request, cred string, viaCode bool) (Token, bool) {
	if s.lim.blocked(remoteHost(r)) {
		http.Error(w, "too many failed attempts; wait a minute", http.StatusTooManyRequests)
		return Token{}, false
	}
	t, err := s.tokens.check(cred, viaCode)
	if err != nil {
		s.deny(w, r)
		return Token{}, false
	}
	return t, true
}

func (s *Service) serveScript(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authorize(w, r, r.PathValue("code"), true)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	_, _ = io.WriteString(w, RenderScript(s.scriptParams(t.Token, t.Role)))
}

func (s *Service) serveCA(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authorize(w, r, bearer(r), false); !ok {
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(s.cfg.CA.CertPEM())
}

func (s *Service) serveAgent(w http.ResponseWriter, r *http.Request) {
	t, ok := s.authorize(w, r, bearer(r), false)
	if !ok {
		return
	}
	goos, arch := r.URL.Query().Get("os"), r.URL.Query().Get("arch")
	if err := CheckRoleOS(t.Role, goos); err != nil || (goos != OSLinux && goos != OSDarwin) {
		http.Error(w, "this token cannot enroll that operating system", http.StatusBadRequest)
		return
	}
	if s.cfg.Binary == nil {
		http.Error(w, "no agent binary is configured on the server", http.StatusServiceUnavailable)
		return
	}
	path, err := s.cfg.Binary(goos, arch)
	if err != nil {
		http.Error(w, "no agent binary for "+goos+"/"+arch+" on the server", http.StatusNotFound)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.log.Error("open agent binary", "component", "enroll", "err", err)
		http.Error(w, "the agent binary is unavailable", http.StatusServiceUnavailable)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.Error(w, "the agent binary is unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "fleet-agent", st.ModTime(), f)
}

func (s *Service) serveCSR(w http.ResponseWriter, r *http.Request) {
	tok := bearer(r)
	if s.lim.blocked(remoteHost(r)) {
		http.Error(w, "too many failed attempts; wait a minute", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cpus, _ := strconv.Atoi(r.PostFormValue("cpus"))
	mem, _ := strconv.ParseUint(r.PostFormValue("mem_bytes"), 10, 64)
	res, err := s.Enroll(Request{
		Token: tok, CSRPEM: []byte(r.PostFormValue("csr")),
		Hostname: r.PostFormValue("hostname"), OS: r.PostFormValue("os"), OSVersion: r.PostFormValue("os_version"),
		Kernel: r.PostFormValue("kernel"), Arch: r.PostFormValue("arch"), CPUs: cpus, MemBytes: mem,
		RemoteIP: remoteHost(r),
	})
	switch {
	case err == nil:
	case errors.Is(err, ErrTokenInvalid), errors.Is(err, ErrTokenExpired), errors.Is(err, ErrTokenUsed):
		s.deny(w, r)
		return
	case errors.Is(err, ErrMacWorker), errors.Is(err, ErrBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	default:
		s.log.Error("enroll", "component", "enroll", "err", err)
		http.Error(w, "enrollment failed on the server", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintf(w, "%s\n%s", res.MachineID, res.CertPEM)
}

// BinaryDir returns a Config.Binary that serves <dir>/fleet-agent-<os>-<arch>,
// falling back to fallback for the running server's own OS and architecture
// (config.AgentBinaryPath). Only the known OS and architecture names reach the
// file system, so the query cannot name another path.
func BinaryDir(dir, fallback, hostOS, hostArch string) func(goos, arch string) (string, error) {
	return func(goos, arch string) (string, error) {
		if (goos != OSLinux && goos != OSDarwin) || (arch != "amd64" && arch != "arm64") {
			return "", errors.New("enroll: unsupported platform")
		}
		if dir != "" {
			p := filepath.Join(dir, "fleet-agent-"+goos+"-"+arch)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return p, nil
			}
		}
		if fallback != "" && goos == hostOS && arch == hostArch {
			return fallback, nil
		}
		return "", os.ErrNotExist
	}
}
