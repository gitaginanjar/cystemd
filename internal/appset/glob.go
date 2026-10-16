// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package appset

import (
	"path"
	"strings"
)

// globMatch matches a slash-separated path against a pattern: * ? and [...]
// within a segment (path.Match), ** across any number of segments.
func globMatch(pattern, name string) bool {
	return matchSegments(strings.Split(strings.Trim(pattern, "/"), "/"), strings.Split(strings.Trim(name, "/"), "/"))
}

func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(name); i++ {
				if matchSegments(pattern[1:], name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], name[0]); err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// maxDepth is how deep a pattern can match (-1: any depth).
func maxDepth(pattern string) int {
	segs := strings.Split(strings.Trim(pattern, "/"), "/")
	for _, s := range segs {
		if s == "**" {
			return -1
		}
	}
	return len(segs)
}
