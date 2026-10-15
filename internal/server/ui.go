// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"path"
	"strings"
)

// uiCSP is the standalone server's Content-Security-Policy for the UI: no
// inline script, same-origin API calls and assets only.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'self'"

// uiHandler serves a single-page application: unknown paths get index.html
// so client-side routes survive a reload. Hashed assets are cached forever.
func uiHandler(fsys http.FileSystem) http.Handler {
	files := http.FileServer(fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", uiCSP)
		p := "/" + strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if f, err := fsys.Open(p); err == nil {
			st, err := f.Stat()
			f.Close()
			if err == nil && !st.IsDir() {
				if strings.HasPrefix(p, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("Cache-Control", "no-cache")
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		files.ServeHTTP(w, r2)
	})
}
