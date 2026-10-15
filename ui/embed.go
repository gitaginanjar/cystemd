// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package ui holds the web UI's static assets: one stylesheet, one small
// script, images and the vendored htmx files (see scripts/vendor-htmx.sh).
// Pages are rendered on the server by the templ components in ui/views;
// there is no JavaScript build step.
package ui

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"sync"
)

//go:embed static
var static embed.FS

// Prefix is the URL path the assets are served under.
const Prefix = "/ui/static/"

// Static returns the asset files rooted at static/.
func Static() fs.FS {
	sub, err := fs.Sub(static, "static")
	if err != nil {
		panic(err)
	}
	return sub
}

var (
	hashOnce sync.Once
	hashes   map[string]string
)

// Asset returns the URL of an asset with a content hash, so browsers can
// cache it forever and still pick up a new build at once.
func Asset(name string) string {
	hashOnce.Do(func() {
		hashes = map[string]string{}
		_ = fs.WalkDir(Static(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(Static(), p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			hashes[p] = hex.EncodeToString(sum[:])[:12]
			return nil
		})
	})
	if h, ok := hashes[name]; ok {
		return Prefix + name + "?v=" + h
	}
	return Prefix + name
}

// Handler serves the assets under Prefix. Versioned URLs are immutable.
func Handler() http.Handler {
	files := http.FileServer(http.FS(Static()))
	return http.StripPrefix(strings.TrimSuffix(Prefix, "/"), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") || path.Base(r.URL.Path) == "SHA256SUMS" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("v") != "" || strings.HasPrefix(r.URL.Path, "/vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	}))
}
