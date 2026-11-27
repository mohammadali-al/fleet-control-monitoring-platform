// Package web embeds the operator-panel front-end so fleet-server can serve it
// from a single binary. go:embed cannot reach outside its package directory, so
// the embed lives next to the files. FS holds the built React SPA shell
// (dist/index.html), the static assets (including assets/dist/app.{js,css}),
// and the hand-written styleguide.html component reference.
package web

import "embed"

// FS holds the static tree served by fleet-server.
//
//go:embed styleguide.html assets dist/index.html
var FS embed.FS
