// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package ui embeds the built web UI (ui/dist, produced by `make ui`). The
// repository keeps a placeholder index.html so Go builds work without Node.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:dist
var dist embed.FS

// FS returns the UI files rooted at dist/.
func FS() http.FileSystem {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return http.FS(sub)
}

// Built reports whether a real UI build (not the placeholder) is embedded.
func Built() bool {
	_, err := fs.Stat(dist, "dist/assets")
	return err == nil
}
