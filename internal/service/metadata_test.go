package service

import (
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestVersion_ReturnsNonEmpty(t *testing.T) {
	v := Version()
	if v == "" {
		t.Error("Version() returned empty string")
	}
}

func TestVersion_DefaultIsDev(t *testing.T) {
	// Without -ldflags injection the default is "dev".
	v := Version()
	if v != "dev" {
		// An injected build overrides the default; log, don't fail.
		t.Logf("Version() = %q (non-default, possibly injected by build)", v)
	}
}

func TestIsDevVersion(t *testing.T) {
	// Pins the exact, case-sensitive sentinels (the metadata.go defaults); a real
	// tag and "" must not trip the self-update guard.
	cases := []struct {
		in   string
		want bool
	}{
		{"dev", true},
		{"unknown", true},
		{"v5.0.24", false},
		{"v1.0.0", false},
		{"", false},
		{"Dev", false},     // case-sensitive
		{"UNKNOWN", false}, // case-sensitive
	}
	for _, tc := range cases {
		if got := isDevVersion(tc.in); got != tc.want {
			t.Errorf("isDevVersion(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestCommit_ReturnsNonEmpty(t *testing.T) {
	c := Commit()
	if c == "" {
		t.Error("Commit() returned empty string")
	}
}

func TestCommit_DefaultIsUnknown(t *testing.T) {
	c := Commit()
	if c != "unknown" {
		t.Logf("Commit() = %q (non-default, possibly injected by build)", c)
	}
}

func TestBuildTime_ReturnsNonEmpty(t *testing.T) {
	b := BuildTime()
	if b == "" {
		t.Error("BuildTime() returned empty string")
	}
}

func TestBuildTime_DefaultIsUnknown(t *testing.T) {
	b := BuildTime()
	if b != "unknown" {
		t.Logf("BuildTime() = %q (non-default, possibly injected by build)", b)
	}
}

func TestGoVersion_MatchesRuntime(t *testing.T) {
	got := GoVersion()
	want := runtime.Version()
	if got != want {
		t.Errorf("GoVersion() = %q, want %q (runtime.Version())", got, want)
	}
}

func TestGoVersion_StartsWithGo(t *testing.T) {
	v := GoVersion()
	if !strings.HasPrefix(v, "go") {
		t.Errorf("GoVersion() = %q; expected it to start with 'go'", v)
	}
}

// ─── Build-injected metadata shape guards (skip under go test defaults) ───

// TestCommit_WhenInjected_IsFullGitHash pins an injected commit as the full
// 40-char hash (git rev-parse HEAD, never --short), so --version names an exact
// revision. See DOCS/CLAUDE.md § Build & Run.
func TestCommit_WhenInjected_IsFullGitHash(t *testing.T) {
	c := Commit()
	if c == "unknown" || c == "" {
		t.Skip("commit is default (unknown); not built via make build — skipping shape assertion")
	}
	fullHash := regexp.MustCompile(`^[0-9a-f]{40}$`)
	if !fullHash.MatchString(c) {
		t.Errorf("Commit() = %q; expected a 40-char lowercase hex git hash. "+
			"The Makefile must use `git rev-parse HEAD`, not `--short`.", c)
	}
}

// TestBuildTime_WhenInjected_IsJakartaISO8601 pins an injected build time as
// WIB ISO 8601 with a colon-less offset (2026-05-18T19:50:47+0700, date +%z);
// UTC "Z" or any other offset fails, catching a Makefile drift to date -u.
func TestBuildTime_WhenInjected_IsJakartaISO8601(t *testing.T) {
	b := BuildTime()
	if b == "unknown" || b == "" {
		t.Skip("build_time is default (unknown); not built via make build — skipping shape assertion")
	}
	jakartaISO := regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\+0700$`)
	if !jakartaISO.MatchString(b) {
		t.Errorf("BuildTime() = %q; expected `YYYY-MM-DDTHH:MM:SS+0700` "+
			"(24-hour Asia/Jakarta / WIB). The Makefile must use "+
			"`TZ=Asia/Jakarta date +%%Y-%%m-%%dT%%H:%%M:%%S%%z`, not `date -u`.", b)
	}
}
