package web

import "embed"

// Templates holds the panel's Go html/template sources: _src/shell.html (the
// layout, {{define "layout"}}) and pages/ (one file per page defining "title"
// and "content", plus fragments.html with the pieces the browser re-fetches).
// internal/panel parses and serves them; they supersede the pre-rendered
// *.html pages embedded in FS.
//
//go:embed _src/shell.html pages
var Templates embed.FS
