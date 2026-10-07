package service

import "runtime"

// version, commit and buildTime are injected at build time via -ldflags (Makefile).
var (
	version   = "dev"
	commit    = "unknown"
	buildTime = "unknown"
)

// Version returns the binary version string.
func Version() string {
	return version
}

// isDevVersion reports whether v is a no-version-injected sentinel ("dev" or
// "unknown": a plain go build/test binary). RunSelfVersionInstall and
// removeDuplicateRPMPackages use it to never auto-install over a local build.
// "" (untagged make build, CI per-commit build) is deliberately not a sentinel.
// Why: DOCS/CLAUDE.md § Build & Run
func isDevVersion(v string) bool {
	return v == "dev" || v == "unknown"
}

// Commit returns the git commit hash.
func Commit() string {
	return commit
}

// BuildTime returns the build timestamp.
func BuildTime() string {
	return buildTime
}

// GoVersion returns the Go toolchain version.
func GoVersion() string {
	return runtime.Version()
}
