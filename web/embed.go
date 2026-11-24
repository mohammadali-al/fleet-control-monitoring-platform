// Package web embeds the static operator-panel front-end (the *.html pages and
// assets/) so fleet-server can serve it from a single binary. go:embed cannot
// reach outside its package directory, so the embed lives next to the files.
package web

import "embed"

// FS holds the panel's pages at its root (index.html, machines.html, ...) and
// the shared styles and scripts under assets/.
//
//go:embed *.html assets
var FS embed.FS
