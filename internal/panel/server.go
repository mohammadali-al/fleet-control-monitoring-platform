// Package panel serves the static operator panel over net/http.
//
// It serves files as-is from an fs.FS (the embedded web/ tree in production):
// "/" is index.html, every other path maps to the file of the same name, and
// anything that is not a regular file (unknown paths, directories) is a 404.
// There is no directory listing, no authentication and no backend API; the
// panel sits behind the auth proxy (D-0004).
package panel

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight requests.
const shutdownTimeout = 5 * time.Second

// Handler returns an http.Handler serving the files in fsys.
func Handler(fsys fs.FS) http.Handler {
	return &handler{fsys: fsys}
}

type handler struct{ fsys fs.FS }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Treat the URL path as an absolute, cleaned path so ".." cannot escape.
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}

	f, err := h.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r) // directories are never listed
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// ModTime is zero for embedded files, so no Last-Modified is sent.
	http.ServeContent(w, r, name, info.ModTime(), rs)
}

// URL returns a browsable http URL for a listen address such as ":6789".
func URL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// Listen opens the TCP listener for addr. It is separate from Serve so that a
// bad or busy address fails before anything is logged as started.
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", addr)
}

// Serve serves h on ln until ctx is cancelled, then shuts down gracefully,
// waiting up to shutdownTimeout for in-flight requests. It returns nil on a
// clean shutdown.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
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

	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
		return err
	}
	<-errc
	return nil
}
