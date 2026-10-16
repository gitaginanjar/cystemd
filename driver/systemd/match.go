// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"path"
	"strings"
)

// matchAny reports whether name matches one of the path.Match patterns. An
// empty pattern list matches nothing: every capability is opt-in.
func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// matchAnyPath is matchAny for file paths, where a pattern "dir/**" also
// matches anything below dir at any depth.
func matchAnyPath(patterns []string, p string) bool {
	for _, pat := range patterns {
		if dir, ok := strings.CutSuffix(pat, "/**"); ok {
			if strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/") {
				return true
			}
			continue
		}
		if ok, err := path.Match(pat, p); err == nil && ok {
			return true
		}
	}
	return false
}
