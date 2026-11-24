// Package web embeds the operator-panel front-end so fleet-server can serve it
// from a single binary. go:embed cannot reach outside its package directory, so
// the embed lives next to the files. The pages themselves are Go templates (see
// templates.go); FS holds only the static files served as-is.
package web

import "embed"

// FS holds the static tree: the shared styles, scripts and vendored xterm.js
// under assets/, plus the hand-written styleguide.html component reference.
//
//go:embed styleguide.html assets
var FS embed.FS
