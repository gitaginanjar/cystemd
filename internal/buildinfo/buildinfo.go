// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package buildinfo carries build metadata injected with -ldflags -X (see
// the Makefile): the version is the exact Git tag of HEAD (empty when
// untagged), the commit is the full 40-character hash, and the build time is
// ISO 8601 with an explicit offset — the v4 build metadata contract.
package buildinfo

import (
	"log/slog"
	"os"
	"strings"
)

var (
	version   = ""
	commit    = "unknown"
	buildTime = "unknown"
)

// Version returns the release version, "dev" for local builds.
func Version() string {
	if version == "" {
		if commit != "unknown" && len(commit) >= 12 {
			return "dev+" + commit[:12]
		}
		return "dev"
	}
	return version
}

// Commit is the full Git commit hash.
func Commit() string { return commit }

// BuildTime is the build timestamp.
func BuildTime() string { return buildTime }

// Logger returns a slog logger at level ("trace"/"debug"/"info"/"warn"/
// "error"), JSON when asJSON.
func Logger(level string, asJSON bool) *slog.Logger {
	var l slog.Level
	switch strings.ToLower(level) {
	case "trace", "debug":
		l = slog.LevelDebug
	case "warn", "warning":
		l = slog.LevelWarn
	case "error", "critical", "emergency":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: l}
	if asJSON {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}
