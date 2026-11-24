// Package panel is the operator panel: fleet-server's web front-end on :6789.
//
// Pages are Go html/template (the layout in web/_src/shell.html, one file per
// page under web/pages/), served together with the static assets (CSS, JS,
// the vendored xterm.js). The Operate pages (Machines, Command runner,
// Terminal) are live: a JSON API (api.go) and two WebSockets (ws.go) sit on a
// Backend, which is the fleet manager (internal/fleet) in production.
//
// There is no authentication here: the panel sits behind the auth proxy and
// trusts the headers it forwards (D-0004); see operatorFrom.
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

	"github.com/mohammadali-al/fleet-control-monitoring-platform/internal/fleet"
	"github.com/mohammadali-al/fleet-control-monitoring-platform/web"
)

// shutdownTimeout bounds how long graceful shutdown waits for in-flight requests.
const shutdownTimeout = 5 * time.Second

// Options configures the panel.
type Options struct {
	// Assets is the static tree: assets/... and styleguide.html at its root
	// (web.FS in production).
	Assets fs.FS
	// Templates holds _src/shell.html and pages/ (web.Templates in production).
	Templates fs.FS
	// Backend feeds the live pages. Nil renders them empty and answers the
	// API with 503.
	Backend Backend
	// Logger receives request errors (default slog.Default()).
	Logger *slog.Logger
}

// New builds the panel's http.Handler.
func New(o Options) (http.Handler, error) {
	if o.Assets == nil || o.Templates == nil {
		return nil, errors.New("panel: assets and templates are required")
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	tpl, err := loadTemplates(o.Templates)
	if err != nil {
		return nil, err
	}
	s := &server{assets: o.Assets, tpl: tpl, be: o.Backend, log: o.Logger}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.page(pages[0].name))
	mux.HandleFunc("GET /index.html", s.page(pages[0].name))
	for _, p := range pages {
		mux.HandleFunc("GET /"+p.name+".html", s.page(p.name))
	}
	mux.HandleFunc("GET /fragments/{name}", s.fragment)
	mux.HandleFunc("GET /assets/", s.static)
	mux.HandleFunc("GET /styleguide.html", s.static)
	s.routesAPI(mux)
	s.routesWS(mux)
	return secure(mux), nil
}

// Handler returns the panel with no backend, serving the embedded web tree.
// Production code uses New with a Backend; this form is kept for callers that
// only need the pages. It panics only if the embedded templates are broken,
// which the package tests rule out.
func Handler(assets fs.FS) http.Handler {
	h, err := New(Options{Assets: assets, Templates: web.Templates})
	if err != nil {
		panic(err)
	}
	return h
}

// secure adds the headers every response carries.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

type server struct {
	assets fs.FS
	tpl    *templates
	be     Backend
	log    *slog.Logger
}

// data gathers what the templates need for a page.
func (s *server) data(page string) pageData {
	d := pageData{Page: page, Backend: s.be != nil}
	for _, p := range pages {
		if p.name == page {
			d.Mock = p.mock
		}
	}
	if s.be == nil {
		return d
	}
	d.Machines = s.be.Machines()
	d.Summary = fleet.Summarize(d.Machines)
	for _, m := range d.Machines {
		if m.Role == "worker" && !m.Removed {
			d.Workers = append(d.Workers, m)
		}
	}
	return d
}

func (s *server) page(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d := s.data(name)
		d.Preselect = r.URL.Query().Get("machine")
		body, err := s.tpl.render(name, d)
		if err != nil {
			s.log.Error("render page", "component", "panel", "page", name, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}

func (s *server) fragment(w http.ResponseWriter, r *http.Request) {
	d := s.data("")
	d.Preselect = r.URL.Query().Get("machine")
	body, err := s.tpl.renderFragment(r.PathValue("name"), d)
	if err != nil {
		if strings.Contains(err.Error(), "unknown fragment") {
			http.NotFound(w, r)
			return
		}
		s.log.Error("render fragment", "component", "panel", "name", r.PathValue("name"), "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(body)
}

// static serves files as-is from the assets tree. Only /assets/... and
// /styleguide.html reach it; anything that is not a regular file (unknown
// paths, directories) is a 404 and there is never a directory listing.
func (s *server) static(w http.ResponseWriter, r *http.Request) {
	// Treat the URL path as an absolute, cleaned path so ".." cannot escape.
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	f, err := s.assets.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
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
// clean shutdown. WebSockets are hijacked connections: they are not waited
// for, and end when their handlers see ctx.Done through the request context
// (BaseContext below).
func Serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger) error {
	srvCtx, cancelConns := context.WithCancel(context.Background())
	defer cancelConns()
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
		BaseContext:       func(net.Listener) context.Context { return srvCtx },
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

	cancelConns() // ends WebSocket handlers, which Shutdown does not track
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		_ = srv.Close()
		return err
	}
	<-errc
	return nil
}
