package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRPMVersions swaps the rpm-truth seam, so the decision logic runs without rpm.
func stubRPMVersions(t *testing.T, versions []string, ok bool) *int {
	t.Helper()
	calls := 0
	prev := rpmVersionsOf
	rpmVersionsOf = func(string) ([]string, bool) {
		calls++
		return versions, ok
	}
	t.Cleanup(func() { rpmVersionsOf = prev })
	return &calls
}

func writeTracking(t *testing.T, version string) string {
	t.Helper()
	dir := mkTempDir(t)
	if version != "" {
		if err := os.WriteFile(filepath.Join(dir, installedVersionFilename), []byte(version), 0o644); err != nil {
			t.Fatalf("write tracking file: %v", err)
		}
	}
	return dir
}

// THE REGRESSION (ADR-0011): a blanket `dnf update` moved the package while
// .installed_version still named the pin, and the install was skipped. What rpm
// reports is the answer.
func TestCurrentServiceVersion_ReportsWhatRPMSaysNotWhatWasRecorded(t *testing.T) {
	const wanted = "4b319079d26728bc331f499bf951581e065fda23"
	const actuallyInstalled = "ffffffffffffffffffffffffffffffffffffffff"

	stubRPMVersions(t, []string{actuallyInstalled}, true)

	current, atWanted, known := currentServiceVersion("svc", wanted)

	if atWanted {
		t.Fatal("atWanted = true — this is the bug: cystemd would skip the install and the host would never converge")
	}
	if !known {
		t.Error("known = false, want true (rpm answered)")
	}
	if current != actuallyInstalled {
		t.Errorf("current = %q, want the rpm truth %q", current, actuallyInstalled)
	}
}

func TestCurrentServiceVersion_RPMConfirmsWantedVersion(t *testing.T) {
	const wanted = "abc123"
	stubRPMVersions(t, []string{wanted}, true)

	current, atWanted, known := currentServiceVersion("svc", wanted)
	if !atWanted || !known || current != wanted {
		t.Errorf("got (%q, %v, %v), want (%q, true, true)", current, atWanted, known, wanted)
	}
}

// Package absent is an empty current, which callers and /diff render as "no current version".
func TestCurrentServiceVersion_NotInstalledYieldsEmptyCurrent(t *testing.T) {
	stubRPMVersions(t, nil, true)

	current, atWanted, known := currentServiceVersion("svc", "wanted")
	if current != "" {
		t.Errorf("current = %q, want \"\" when rpm says the package is not installed", current)
	}
	if atWanted {
		t.Error("atWanted = true for a package rpm says is absent")
	}
	if !known {
		t.Error("known = false, want true — rpm answered definitively")
	}
}

// Inconclusive rpm yields known=false and nothing else: no .installed_version fallback,
// the caller declines to decide.
func TestCurrentServiceVersion_InconclusiveRPMNeverFallsBackToFile(t *testing.T) {
	stubRPMVersions(t, nil, false)

	current, atWanted, known := currentServiceVersion("svc", "v9")
	if known {
		t.Fatal("known = true although rpm could not be consulted")
	}
	if atWanted {
		t.Error("atWanted = true from an inconclusive rpm — no decision is possible here")
	}
	if current != "" {
		t.Errorf("current = %q, want \"\" — nothing may be reported when rpm cannot answer", current)
	}
}

// A duplicate-package host already carrying the wanted version is not reinstalled
// (that would restart a healthy service).
func TestCurrentServiceVersion_DuplicatePackagesIncludingWanted(t *testing.T) {
	const wanted = "v2"
	stubRPMVersions(t, []string{"v1", wanted}, true)

	current, atWanted, known := currentServiceVersion("svc", wanted)
	if !atWanted {
		t.Error("atWanted = false although one installed version IS the wanted one")
	}
	if !known || current != wanted {
		t.Errorf("got (%q, %v), want (%q, true)", current, known, wanted)
	}
}

// Duplicates without the wanted version are drift; current lists every version.
func TestCurrentServiceVersion_DuplicatePackagesWithoutWanted(t *testing.T) {
	stubRPMVersions(t, []string{"v1", "v3"}, true)

	current, atWanted, _ := currentServiceVersion("svc", "v2")
	if atWanted {
		t.Error("atWanted = true although no installed version matches")
	}
	if current != "v1,v3" {
		t.Errorf("current = %q, want \"v1,v3\" (all installed versions reported)", current)
	}
}

// currentServiceVersion takes no destDir, so a tracking file cannot sway it; a
// stale file on disk changes nothing.
func TestRunServiceVersionInstall_InconclusiveRPMSkipsCycleAndIgnoresFile(t *testing.T) {
	destDir := writeTracking(t, "a-lie-that-must-not-be-believed")
	stubRPMVersions(t, nil, false)

	entry := &ServiceEntry{
		Service:             "cystemd-test-no-such-pkg",
		Selector:            "cystemd-test-no-such-pkg",
		GitValues:           "values.yaml",
		GitValuesVersionKey: ".${selector}.global.image.tag",
		AutoSync:            true,
	}
	raw := []byte("cystemd-test-no-such-pkg:\n  global:\n    image:\n      tag: wanted-version\n")

	buf := captureLogs(t, LevelDebug)
	got := runServiceVersionInstall("cystemd-test-no-such-pkg", entry, raw, destDir, true)
	out := buf.String()

	if got {
		t.Error("expected false — no install may be attempted when rpm cannot be consulted")
	}
	if !strings.Contains(out, "version check skipped") {
		t.Errorf("expected the skip warning naming the unavailable rpm database; got:\n%s", out)
	}
	// Must not have reached dnf, and must not have decided from the file.
	if strings.Contains(out, "installing RPM") || strings.Contains(out, "version change detected") {
		t.Errorf("declined cycle still made an install decision; got:\n%s", out)
	}
	if got := readInstalledVersion(destDir); got != "a-lie-that-must-not-be-believed" {
		t.Errorf("tracking file was modified: %q", got)
	}
}
