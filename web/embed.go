// Package web embeds the built console UI (web/dist) into the Go binary.
//
// Run `npm run build` in this directory (or `make web`) before `go build`.
// `make dist-stub` creates a placeholder so Go builds and tests work without
// a Node toolchain.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Assets returns the built UI with the dist/ prefix stripped.
func Assets() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // dist is embedded at compile time; this cannot fail at runtime
	}
	return sub
}
