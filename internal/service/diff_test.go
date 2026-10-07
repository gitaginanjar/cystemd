package service

// lastDiffCache is package-global: every test touching it resets it before and
// (t.Cleanup) after with resetLastDiffCacheForTest.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ─── flattenYAMLProperties ──────────────────────────────────────────────────

func TestFlattenYAMLProperties(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantErr bool
		want    map[string]string
	}{
		{"empty input", "", false, map[string]string{}},
		{"whitespace-only input", "   \n\t\n", false, map[string]string{}},
		{"scalar root string", "just a string\n", true, nil},
		{"scalar root number", "42\n", true, nil},
		{"null root", "null\n", true, nil},
		{"invalid yaml (tab)", ":\tbad:\tyaml", true, nil},
		{"invalid yaml (bad indent)", "a: 1\n  b: 2\n", true, nil},
		{"empty map", "{}\n", false, map[string]string{"": "{}"}},
		{"empty sequence", "[]\n", false, map[string]string{"": "[]"}},
		{"nested map", "a:\n  b: 1\n  c:\n    d: two\n", false, map[string]string{"a.b": "1", "a.c.d": "two"}},
		{"sequence root", "- x\n- y\n", false, map[string]string{"[0]": "x", "[1]": "y"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := flattenYAMLProperties([]byte(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("flattenYAMLProperties(%q): expected error, got nil (result: %v)", tc.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("flattenYAMLProperties(%q): unexpected error: %v", tc.input, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("flattenYAMLProperties(%q) = %v, want %v", tc.input, got, tc.want)
			}
			for k, wantV := range tc.want {
				if gotV, ok := got[k]; !ok || gotV != wantV {
					t.Errorf("flattenYAMLProperties(%q)[%q] = %q (ok=%v), want %q", tc.input, k, gotV, ok, wantV)
				}
			}
		})
	}
}

// ─── diffProperties ─────────────────────────────────────────────────────────

func TestDiffProperties(t *testing.T) {
	oldProps := map[string]string{
		"unchanged.key": "same",
		"changed.key":   "old-value",
		"removed.key":   "gone",
	}
	newProps := map[string]string{
		"unchanged.key": "same",
		"changed.key":   "new-value",
		"added.key":     "fresh",
	}
	got := diffProperties(oldProps, newProps)
	want := []string{
		"+ added.key: fresh",
		"~ changed.key: old-value -> new-value",
		"- removed.key: gone",
	}
	if len(got) != len(want) {
		t.Fatalf("diffProperties: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("diffProperties[%d]: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
	for _, line := range got {
		if strings.Contains(line, "unchanged.key") {
			t.Errorf("unchanged property must not appear in the diff: %v", got)
		}
	}
}

func TestDiffProperties_EmptyBothSides(t *testing.T) {
	got := diffProperties(map[string]string{}, map[string]string{})
	if len(got) != 0 {
		t.Errorf("diffProperties({}, {}) = %v, want empty", got)
	}
}

// ─── diffRawLines ───────────────────────────────────────────────────────────

func TestDiffRawLines(t *testing.T) {
	oldText := "kept line\nremoved line\n\n   \nold-only\n"
	newText := "kept line\nadded line\n\n\t\nold-only\n"
	got := diffRawLines(oldText, newText)

	if slices.Contains(got, "+ kept line") || slices.Contains(got, "- kept line") {
		t.Errorf("a line present on both sides must not appear in the diff: %v", got)
	}
	if !slices.Contains(got, "- removed line") {
		t.Errorf("expected '- removed line' in %v", got)
	}
	if !slices.Contains(got, "+ added line") {
		t.Errorf("expected '+ added line' in %v", got)
	}
	if slices.Contains(got, "- old-only") || slices.Contains(got, "+ old-only") {
		t.Errorf("a line present on both sides must not appear in the diff: %v", got)
	}
	for _, line := range got {
		if strings.HasPrefix(line, "~ ") {
			t.Errorf("diffRawLines must never emit a '~' (changed) line, got: %q", line)
		}
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "+ "), "- ")) == "" {
			t.Errorf("a blank line leaked into the diff: %q", line)
		}
	}
}

func TestDiffRawLines_EqualText_Empty(t *testing.T) {
	got := diffRawLines("same\nlines\n", "same\nlines\n")
	if len(got) != 0 {
		t.Errorf("diffRawLines with identical text: got %v, want empty", got)
	}
}

// ─── computeConfigDiff ──────────────────────────────────────────────────────

func TestComputeConfigDiff(t *testing.T) {
	t.Run("equal bytes returns empty non-nil slice", func(t *testing.T) {
		got := computeConfigDiff([]byte("a: 1\n"), []byte("a: 1\n"))
		if got == nil {
			t.Fatal("expected non-nil empty slice for equal bytes, got nil")
		}
		if len(got) != 0 {
			t.Errorf("expected empty slice, got %v", got)
		}
	})

	t.Run("both structured YAML produces a property diff", func(t *testing.T) {
		got := computeConfigDiff([]byte("a:\n  b: 1\n"), []byte("a:\n  b: 2\n  c: 3\n"))
		want := []string{"~ a.b: 1 -> 2", "+ a.c: 3"}
		if len(got) != len(want) {
			t.Fatalf("computeConfigDiff: got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("computeConfigDiff[%d]: got %q, want %q", i, got[i], want[i])
			}
		}
	})

	t.Run("scalar side falls back to raw line diff, no changed lines", func(t *testing.T) {
		got := computeConfigDiff([]byte("hello world\n"), []byte("hello world\nnew line\n"))
		want := []string{"+ new line"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("computeConfigDiff (raw fallback): got %v, want %v", got, want)
		}
	})

	t.Run("nil oldBytes vs structured YAML: everything added via property diff", func(t *testing.T) {
		got := computeConfigDiff(nil, []byte("a: 1\n"))
		want := []string{"+ a: 1"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("computeConfigDiff(nil, structured): got %v, want %v", got, want)
		}
	})

	t.Run("empty oldBytes vs non-YAML: everything added via raw fallback", func(t *testing.T) {
		got := computeConfigDiff([]byte(""), []byte("just text\n"))
		want := []string{"+ just text"}
		if len(got) != len(want) || got[0] != want[0] {
			t.Errorf("computeConfigDiff(\"\", scalar): got %v, want %v", got, want)
		}
	})

	t.Run("no diff ever produces a nil slice", func(t *testing.T) {
		if got := computeConfigDiff([]byte("x: 1\n"), []byte("x: 1\n")); got == nil {
			t.Error("computeConfigDiff must never return nil, even for no drift")
		}
	})
}

// ─── computeEnvDiff ─────────────────────────────────────────────────────────

func TestComputeEnvDiff(t *testing.T) {
	const secretOld = "old-secret-value-xyz123"
	const secretNew = "new-secret-value-abc789"

	t.Run("added/changed/removed mix, unchanged key excluded from all three", func(t *testing.T) {
		oldPairs := map[string]string{
			"UNCHANGED_KEY": "same-value",
			"CHANGED_KEY":   secretOld,
			"REMOVED_KEY":   "gone-value",
		}
		newPairs := map[string]string{
			"UNCHANGED_KEY": "same-value",
			"CHANGED_KEY":   secretNew,
			"ADDED_KEY":     "fresh-value",
		}
		got := computeEnvDiff(oldPairs, newPairs)

		if !slices.Equal(got.Added, []string{"ADDED_KEY"}) {
			t.Errorf("Added: got %v, want [ADDED_KEY]", got.Added)
		}
		if !slices.Equal(got.Changed, []string{"CHANGED_KEY"}) {
			t.Errorf("Changed: got %v, want [CHANGED_KEY]", got.Changed)
		}
		if !slices.Equal(got.Removed, []string{"REMOVED_KEY"}) {
			t.Errorf("Removed: got %v, want [REMOVED_KEY]", got.Removed)
		}
		for _, list := range [][]string{got.Added, got.Changed, got.Removed} {
			if slices.Contains(list, "UNCHANGED_KEY") {
				t.Errorf("an unchanged key must not appear in any facet: %+v", got)
			}
		}
	})

	t.Run("empty inputs produce all-empty non-nil slices", func(t *testing.T) {
		got := computeEnvDiff(map[string]string{}, map[string]string{})
		if got.Added == nil || got.Changed == nil || got.Removed == nil {
			t.Fatalf("expected non-nil empty slices, got %+v", got)
		}
		if len(got.Added) != 0 || len(got.Changed) != 0 || len(got.Removed) != 0 {
			t.Errorf("expected all-empty EnvDiffJSON, got %+v", got)
		}
	})

	t.Run("nil inputs also produce all-empty non-nil slices", func(t *testing.T) {
		got := computeEnvDiff(nil, nil)
		if got.Added == nil || got.Changed == nil || got.Removed == nil {
			t.Fatalf("expected non-nil empty slices for nil input maps, got %+v", got)
		}
	})

	// Security contract: only NAMES, never a secret VALUE (checked on the
	// struct here; the JSON is covered at the ComputeCurrentDiffs/HTTP layer).
	t.Run("secret values never appear in the result, only key names", func(t *testing.T) {
		got := computeEnvDiff(
			map[string]string{"CHANGED_KEY": secretOld},
			map[string]string{"CHANGED_KEY": secretNew, "ADDED_KEY": secretNew},
		)
		for _, list := range [][]string{got.Added, got.Changed, got.Removed} {
			for _, s := range list {
				if strings.Contains(s, secretOld) || strings.Contains(s, secretNew) {
					t.Fatalf("secret value leaked into EnvDiffJSON field: %q", s)
				}
				if s != "CHANGED_KEY" && s != "ADDED_KEY" {
					t.Fatalf("unexpected entry %q; only variable NAMES should appear", s)
				}
			}
		}
	})
}

// ─── lastDiffCache: recordConfigDiff / recordVersionDiff / recordEnvDiff ───

// TestRecordDiffFacets_NoClobberingWithinSingleOrder pins that each record*Diff
// touches only its own facet: unrecorded facets stay zero and a populated one
// survives another facet's recorder.
func TestRecordDiffFacets_NoClobberingWithinSingleOrder(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	const svc = "clobber-check-svc"

	recordConfigDiff(svc, nil, []byte("a: 1\n"))
	afterConfig := getLastDiffs([]string{svc})[0]
	if len(afterConfig.Config) == 0 {
		t.Fatal("expected recordConfigDiff to populate Config")
	}
	if afterConfig.Version != (VersionDiffJSON{}) {
		t.Errorf("Version must still be zero-value after only recordConfigDiff; got %+v", afterConfig.Version)
	}
	if len(afterConfig.Env.Added) != 0 || len(afterConfig.Env.Changed) != 0 || len(afterConfig.Env.Removed) != 0 {
		t.Errorf("Env must still be zero-value after only recordConfigDiff; got %+v", afterConfig.Env)
	}

	recordVersionDiff(svc, "1.0.0", "1.1.0", true)
	afterVersion := getLastDiffs([]string{svc})[0]
	if !slices.Equal(afterVersion.Config, afterConfig.Config) {
		t.Errorf("recordVersionDiff must not clobber the previously-recorded Config; got %v, want %v",
			afterVersion.Config, afterConfig.Config)
	}
	if !afterVersion.Version.Changed || afterVersion.Version.Current != "1.0.0" || afterVersion.Version.Wanted != "1.1.0" {
		t.Errorf("expected recordVersionDiff to populate Version; got %+v", afterVersion.Version)
	}

	recordEnvDiff(svc, map[string]string{"K": "old"}, map[string]string{"K": "new"})
	final := getLastDiffs([]string{svc})[0]
	if !slices.Equal(final.Config, afterConfig.Config) {
		t.Errorf("recordEnvDiff must not clobber the previously-recorded Config; got %v, want %v",
			final.Config, afterConfig.Config)
	}
	if final.Version != afterVersion.Version {
		t.Errorf("recordEnvDiff must not clobber the previously-recorded Version; got %+v, want %+v",
			final.Version, afterVersion.Version)
	}
	if !slices.Contains(final.Env.Changed, "K") {
		t.Errorf("expected recordEnvDiff to populate Env; got %+v", final.Env)
	}
	if final.Service != svc {
		t.Errorf("Service: got %q, want %q", final.Service, svc)
	}
}

// TestRecordDiffFacets_AllThreeCoexist_RegardlessOfOrder records all three
// facets of one service in three rotations of call order and pins that all
// three are always present.
func TestRecordDiffFacets_AllThreeCoexist_RegardlessOfOrder(t *testing.T) {
	t.Cleanup(resetLastDiffCacheForTest)

	type step func(svc string)
	config := func(svc string) { recordConfigDiff(svc, nil, []byte("a: 1\n")) }
	version := func(svc string) { recordVersionDiff(svc, "1.0.0", "2.0.0", true) }
	env := func(svc string) { recordEnvDiff(svc, map[string]string{}, map[string]string{"NEW_KEY": "v"}) }

	orders := []struct {
		name  string
		steps []step
	}{
		{"config-version-env", []step{config, version, env}},
		{"version-env-config", []step{version, env, config}},
		{"env-config-version", []step{env, config, version}},
	}

	for _, o := range orders {
		t.Run(o.name, func(t *testing.T) {
			resetLastDiffCacheForTest()
			svc := "coexist-" + o.name
			for _, s := range o.steps {
				s(svc)
			}
			got := getLastDiffs([]string{svc})[0]
			if len(got.Config) == 0 {
				t.Errorf("[%s] Config facet missing: %+v", o.name, got)
			}
			if !got.Version.Changed || got.Version.Current != "1.0.0" || got.Version.Wanted != "2.0.0" {
				t.Errorf("[%s] Version facet missing/wrong: %+v", o.name, got.Version)
			}
			if !slices.Contains(got.Env.Added, "NEW_KEY") {
				t.Errorf("[%s] Env facet missing: %+v", o.name, got.Env)
			}
			if got.Service != svc {
				t.Errorf("[%s] Service: got %q, want %q", o.name, got.Service, svc)
			}
			if got.ComputedAt == "" {
				t.Errorf("[%s] expected ComputedAt to be set", o.name)
			}
		})
	}
}

// TestRecordVersionDiff_ExplicitChangedParam pins that recordVersionDiff stores
// the caller's changed flag as given, even when current == wanted.
// See DOCS/MEMORY.md § Fixed bugs — `/diff`/`/lastdiff`
func TestRecordVersionDiff_ExplicitChangedParam(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	const svc = "test-svc"

	// Case 1: versions differ, changed=true (normal case)
	recordVersionDiff(svc, "1.0.0", "2.0.0", true)
	cached := getLastDiffs([]string{svc})[0]
	if !cached.Version.Changed {
		t.Errorf("case 1 (versions differ): expected Changed=true, got false")
	}

	resetLastDiffCacheForTest()

	// Case 2: versions are the same, changed=false (in sync)
	recordVersionDiff(svc, "1.0.0", "1.0.0", false)
	cached = getLastDiffs([]string{svc})[0]
	if cached.Version.Changed {
		t.Errorf("case 2 (versions same, in sync): expected Changed=false, got true")
	}

	resetLastDiffCacheForTest()

	// Case 3: current == wanted yet changed=true — the flag is stored as given.
	recordVersionDiff(svc, "1.0.0", "1.0.0", true)
	cached = getLastDiffs([]string{svc})[0]
	if !cached.Version.Changed {
		t.Errorf("case 3 (versions same but package missing): expected Changed=true, got false")
	}
	if cached.Version.Current != "1.0.0" || cached.Version.Wanted != "1.0.0" {
		t.Errorf("case 3: expected Current=Wanted='1.0.0', got Current=%q, Wanted=%q",
			cached.Version.Current, cached.Version.Wanted)
	}
}

func TestGetLastDiffs_ZeroValueForUnrecordedService(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	got := getLastDiffs([]string{"never-seen-svc"})
	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(got))
	}
	want := ServiceDiffJSON{
		Service: "never-seen-svc",
		Config:  []string{},
		Env:     EnvDiffJSON{Added: []string{}, Changed: []string{}, Removed: []string{}},
	}
	gotJSON, _ := json.Marshal(got[0])
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("getLastDiffs zero-value shape: got %s, want %s", gotJSON, wantJSON)
	}
	if got[0].ComputedAt != "" {
		t.Errorf("ComputedAt should be zero-value (empty) for an unrecorded service, got %q", got[0].ComputedAt)
	}
	if got[0].Version != (VersionDiffJSON{}) {
		t.Errorf("Version should be zero-value for an unrecorded service, got %+v", got[0].Version)
	}
}

func TestGetLastDiffs_PreservesRequestedOrder(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	recordConfigDiff("svc-b", nil, []byte("x: 1\n"))
	recordConfigDiff("svc-a", nil, []byte("y: 1\n"))

	got := getLastDiffs([]string{"svc-a", "svc-b", "svc-c"})
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(got))
	}
	wantOrder := []string{"svc-a", "svc-b", "svc-c"}
	for i, want := range wantOrder {
		if got[i].Service != want {
			t.Errorf("position %d: got Service %q, want %q", i, got[i].Service, want)
		}
	}
}

func TestResetLastDiffCacheForTest_ClearsCache(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	recordConfigDiff("svc-to-clear", nil, []byte("a: 1\n"))
	if got := getLastDiffs([]string{"svc-to-clear"}); len(got[0].Config) == 0 {
		t.Fatal("precondition failed: expected a recorded diff before reset")
	}

	resetLastDiffCacheForTest()

	got := getLastDiffs([]string{"svc-to-clear"})
	want := ServiceDiffJSON{
		Service: "svc-to-clear",
		Config:  []string{},
		Env:     EnvDiffJSON{Added: []string{}, Changed: []string{}, Removed: []string{}},
	}
	gotJSON, _ := json.Marshal(got[0])
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("expected zero-value entry after reset; got %s, want %s", gotJSON, wantJSON)
	}
}

// ─── ComputeCurrentDiffs — integration (git values) ────────────────────────

func TestComputeCurrentDiffs_NoManagedService_ReturnsError(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	results, err := ComputeCurrentDiffs(&Config{})
	if err == nil {
		t.Fatal("expected error when no managed service is configured for this host")
	}
	if !strings.Contains(err.Error(), "no managed service") {
		t.Errorf("expected 'no managed service' error; got: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results on error, got %+v", results)
	}
}

// TestComputeCurrentDiffs_Drift_ConfigAddedAndVersionChanged pins the no-state
// case: every config line is an addition ("+", never "~"); Version is
// Changed=true, Current empty (rpm: package absent — needs rpm), Wanted from
// the fixture.
func TestComputeCurrentDiffs_Drift_ConfigAddedAndVersionChanged(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	repoDir := setupLocalGitRepo(t, map[string]string{"values.yaml": versionYAMLFixture})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workdir,
		}},
	}

	results, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 service result, got %d: %+v", len(results), results)
	}
	sd := results[0]
	if sd.Service != "myapplication" {
		t.Errorf("Service: got %q, want %q", sd.Service, "myapplication")
	}
	if len(sd.Config) == 0 {
		t.Fatal("expected non-empty Config diff (no on-disk file yet)")
	}
	for _, line := range sd.Config {
		if !strings.HasPrefix(line, "+ ") {
			t.Errorf("with no prior file every line should be an addition ('+'), got: %q (full: %v)", line, sd.Config)
		}
	}
	found := false
	for _, line := range sd.Config {
		if strings.Contains(line, "app.name") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an 'app.name' property in the config diff; got %v", sd.Config)
	}

	if !sd.Version.Changed {
		t.Error("expected Version.Changed=true (no .installed_version tracked yet)")
	}
	if sd.Version.Current != "" {
		t.Errorf("Version.Current: got %q, want empty", sd.Version.Current)
	}
	const wantVersion = "abc123def456abc123def456abc123def456abc123"
	if sd.Version.Wanted != wantVersion {
		t.Errorf("Version.Wanted: got %q, want %q", sd.Version.Wanted, wantVersion)
	}
	if _, perr := time.Parse(time.RFC3339, sd.ComputedAt); perr != nil {
		t.Errorf("ComputedAt not RFC3339: %q (%v)", sd.ComputedAt, perr)
	}

	// Read-only: ComputeCurrentDiffs must never create a managed-service path.
	if _, statErr := os.Stat(filepath.Join(workdir, "myapplication")); statErr == nil {
		t.Error("ComputeCurrentDiffs must not create the service directory or any file in it (read-only)")
	}
}

// TestComputeCurrentDiffs_ConfigInSync_AfterSeedingMatchingFile seeds
// target.config.yml with exactly what extractYAMLKey produces (as CD writes it)
// and pins an empty config diff on the next call.
func TestComputeCurrentDiffs_ConfigInSync_AfterSeedingMatchingFile(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	repoDir := setupLocalGitRepo(t, map[string]string{"values.yaml": valuesYAMLFixture})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir := mkTempDir(t)
	entry := ServiceEntry{
		Service:            "myapplication",
		Selector:           "myapplication",
		GitValues:          "values.yaml",
		GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
		Workdir:            workdir,
	}
	appCfg := &Config{
		ServiceName:          "myapplication",
		HostSelector:         "myapplication",
		ContinuousDeployment: []ServiceEntry{entry},
	}

	first, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs (1st): %v", err)
	}
	if len(first) != 1 || len(first[0].Config) == 0 {
		t.Fatalf("expected drift on the first call (no on-disk file yet); got %+v", first)
	}

	rawContent, rerr := os.ReadFile(filepath.Join(repoDir, "values.yaml"))
	if rerr != nil {
		t.Fatalf("read fixture values.yaml: %v", rerr)
	}
	extracted, eerr := extractYAMLKey(rawContent, entry.GitValuesConfigKey, effectiveSelector(entry))
	if eerr != nil {
		t.Fatalf("extractYAMLKey: %v", eerr)
	}
	destDir := serviceDir(entry)
	if merr := os.MkdirAll(destDir, 0o755); merr != nil {
		t.Fatalf("MkdirAll: %v", merr)
	}
	if werr := os.WriteFile(filepath.Join(destDir, serviceServiceConfig(entry)), extracted, 0o644); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}

	second, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs (2nd): %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("expected 1 result, got %d", len(second))
	}
	if len(second[0].Config) != 0 {
		t.Errorf("expected empty Config diff after seeding the exact extracted content; got %v", second[0].Config)
	}
}

// TestComputeCurrentDiffs_PopulatesLastDiffCache pins that ComputeCurrentDiffs
// returns exactly what /lastdiff then serves (getLastDiffs).
func TestComputeCurrentDiffs_PopulatesLastDiffCache(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	repoDir := setupLocalGitRepo(t, map[string]string{"values.yaml": versionYAMLFixture})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workdir,
		}},
	}

	results, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs: %v", err)
	}

	cached := getLastDiffs([]string{"myapplication"})
	if len(cached) != 1 {
		t.Fatalf("expected 1 cached entry, got %d", len(cached))
	}

	wantJSON, _ := json.Marshal(results[0])
	gotJSON, _ := json.Marshal(cached[0])
	if string(wantJSON) != string(gotJSON) {
		t.Errorf("ComputeCurrentDiffs's returned entry and the subsequently-cached entry differ:\nreturned: %s\ncached:   %s",
			wantJSON, gotJSON)
	}
}

// ─── ComputeCurrentDiffs — integration (Vault env facet) ───────────────────

func TestComputeCurrentDiffs_EnvDiff_AddedKey_NoValueLeak(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "") // isolate the env facet from config/version

	const secretValue = "sUpEr-Distinctive-Secret-9f8e7d"
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"APP_KEY":%q},"metadata":{}},"request_id":"x"}`, secretValue)
	})

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			Service:  "myapplication",
			Selector: "myapplication",
			Secrets:  "kubernetes/myapp/dev",
			Workdir:  workdir,
		}},
	}

	results, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	sd := results[0]
	if !slices.Contains(sd.Env.Added, "APP_KEY") {
		t.Errorf("expected APP_KEY in Env.Added; got %+v", sd.Env)
	}

	data, merr := json.Marshal(results)
	if merr != nil {
		t.Fatalf("json.Marshal: %v", merr)
	}
	if strings.Contains(string(data), secretValue) {
		t.Errorf("secret value leaked into the marshaled /diff result: %s", data)
	}
}

func TestComputeCurrentDiffs_EnvDiff_ChangedKey(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "")

	workdir := mkTempDir(t)
	entry := ServiceEntry{
		Service:  "myapplication",
		Selector: "myapplication",
		Secrets:  "kubernetes/myapp/dev",
		Workdir:  workdir,
	}
	destDir := serviceDir(entry)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(destDir, ".env"), []byte("APP_KEY=old-value\n"), 0o600); err != nil {
		t.Fatalf("seed .env: %v", err)
	}

	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"APP_KEY":"new-value"},"metadata":{}},"request_id":"x"}`)
	})

	appCfg := &Config{
		ServiceName:          "myapplication",
		HostSelector:         "myapplication",
		ContinuousDeployment: []ServiceEntry{entry},
	}
	results, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs: %v", err)
	}
	sd := results[0]
	if !slices.Contains(sd.Env.Changed, "APP_KEY") {
		t.Errorf("expected APP_KEY in Env.Changed; got %+v", sd.Env)
	}
	if slices.Contains(sd.Env.Added, "APP_KEY") {
		t.Errorf("APP_KEY should be reported as Changed, not Added; got %+v", sd.Env)
	}

	// Read-only contract: the on-disk .env must be untouched.
	data, rerr := os.ReadFile(filepath.Join(destDir, ".env"))
	if rerr != nil {
		t.Fatalf("read .env: %v", rerr)
	}
	if !strings.Contains(string(data), "old-value") {
		t.Errorf("ComputeCurrentDiffs must not rewrite the on-disk .env; got %q", string(data))
	}
}

// TestComputeCurrentDiffs_TransientGitUnavailable_PreservesRicherCachedDiff
// pins that a /diff which cannot reach git keeps the cached config/version
// facets instead of blanking them ("couldn't check" is not "no drift").
// Why: DOCS/CLAUDE.md § `/diff` and `/lastdiff` — drift visibility
func TestComputeCurrentDiffs_TransientGitUnavailable_PreservesRicherCachedDiff(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	repoDir := setupLocalGitRepo(t, map[string]string{"values.yaml": versionYAMLFixture})
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workdir,
		}},
	}

	// First call: git reachable → a rich config/version diff is cached.
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	first, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs (1st, git reachable): %v", err)
	}
	if len(first[0].Config) == 0 || !first[0].Version.Changed {
		t.Fatalf("precondition failed: expected a rich cached diff on the first call; got %+v", first[0])
	}
	wantCurrent := first[0].Version.Current
	wantWanted := first[0].Version.Wanted

	// Second call: git transiently unavailable — fail-soft, no error; its own
	// result (getLastDiffs) must still show the first call's diff.
	t.Setenv("GIT_REPOSITORY", "")
	second, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs (2nd, git unreachable): %v", err)
	}
	if len(second[0].Config) == 0 || !second[0].Version.Changed {
		t.Fatalf("fix regressed: expected the 2nd call's own result to still show the richer diff from the 1st call "+
			"(git being transiently unreachable must not blank an unrelated cached facet); got %+v", second[0])
	}

	// The cache /lastdiff serves must still hold it too.
	cached := getLastDiffs([]string{"myapplication"})[0]
	if len(cached.Config) == 0 {
		t.Errorf("expected the cache to still hold the richer Config diff from the first call; got %v", cached.Config)
	}
	if !cached.Version.Changed || cached.Version.Current != wantCurrent || cached.Version.Wanted != wantWanted {
		t.Errorf("expected the cache to still hold the richer Version diff from the first call "+
			"(Changed=true, Current=%q, Wanted=%q); got %+v", wantCurrent, wantWanted, cached.Version)
	}
}

// ─── hook-wiring: proving cd.go / secrets.go actually call into diff.go ────

// TestRunContinuousDeployment_RecordsConfigAndVersionDiffInLastDiffCache drives
// a real CD cycle and pins that recordConfigDiff/recordVersionDiff fire; with
// autosync=false the drift is recorded while the write is not done.
func TestRunContinuousDeployment_RecordsConfigAndVersionDiffInLastDiffCache(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	combinedRepo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            false,
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workdir,
		}},
	}
	cdCfg := &CDConfig{
		GitRepository:    "file://" + combinedRepo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cdCfg, appCfg)

	cached := getLastDiffs([]string{"myapplication"})
	if len(cached) != 1 {
		t.Fatalf("expected 1 cached entry, got %d", len(cached))
	}
	sd := cached[0]
	if len(sd.Config) == 0 {
		t.Error("expected the CD cycle to have recorded a non-empty config diff (no on-disk file yet)")
	}
	if !sd.Version.Changed {
		t.Error("expected the CD cycle to have recorded Version.Changed=true")
	}
	const wantVersion = "abc123def456abc123def456abc123def456abc123"
	if sd.Version.Wanted != wantVersion {
		t.Errorf("Version.Wanted: got %q, want %q", sd.Version.Wanted, wantVersion)
	}

	if _, err := os.Stat(filepath.Join(workdir, "myapplication", "target.config.yml")); err == nil {
		t.Error("autosync=false must not write target.config.yml, even though the diff was recorded")
	}
}

// TestFetchServiceSecrets_RecordsEnvDiffInLastDiffCache drives the real
// FetchServiceSecrets and pins that recordEnvDiff fires on the refresh path.
func TestFetchServiceSecrets_RecordsEnvDiffInLastDiffCache(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync: true,
			Service:  "myapplication",
			Selector: "myapplication",
			Secrets:  "kubernetes/myapp/dev",
			Workdir:  workdir,
		}},
	}
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"APP_KEY":"secret123"},"metadata":{}},"request_id":"x"}`)
	})

	vaultCfg := &VaultConfig{Address: "http://fake-vault"}
	FetchServiceSecrets(appCfg, vaultCfg)

	cached := getLastDiffs([]string{"myapplication"})
	if len(cached) != 1 {
		t.Fatalf("expected 1 cached entry, got %d", len(cached))
	}
	if !slices.Contains(cached[0].Env.Added, "APP_KEY") {
		t.Errorf("expected APP_KEY in cached Env.Added after FetchServiceSecrets; got %+v", cached[0].Env)
	}
}

// ─── /diff and /lastdiff — HTTP layer ──────────────────────────────────────

func TestDiffEndpoint_RejectsNonGET(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodPost, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /diff: expected 405, got %d", w.Code)
	}
	msgs := decodeLogMsgs(t, buf)
	if n := strings.Count(msgs, "[AUDIT]"); n != 1 {
		t.Errorf("expected exactly 1 [AUDIT] line for a 405 /diff request, got %d; log:\n%s", n, msgs)
	}
	if !strings.Contains(msgs, "Result=DENIED") {
		t.Errorf("expected a DENIED audit entry for method-not-allowed on /diff; got:\n%s", msgs)
	}
}

func TestLastDiffEndpoint_RejectsNonGET(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodPost, "/lastdiff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /lastdiff: expected 405, got %d", w.Code)
	}
	msgs := decodeLogMsgs(t, buf)
	if n := strings.Count(msgs, "[AUDIT]"); n != 1 {
		t.Errorf("expected exactly 1 [AUDIT] line for a 405 /lastdiff request, got %d; log:\n%s", n, msgs)
	}
	if !strings.Contains(msgs, "Result=DENIED") {
		t.Errorf("expected a DENIED audit entry for method-not-allowed on /lastdiff; got:\n%s", msgs)
	}
}

func TestDiffEndpoint_NoManagedService_Returns500(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "")

	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: false}} // no ContinuousDeployment/selector
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("/diff with no managed service: expected 500, got %d (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("/diff 500 body should be JSON: %v (body: %s)", err, w.Body.String())
	}
	if body["error"] == "" {
		t.Error("expected a non-empty JSON error message")
	}
	msgs := decodeLogMsgs(t, buf)
	if n := strings.Count(msgs, "[AUDIT]"); n != 1 {
		t.Errorf("expected exactly 1 [AUDIT] line, got %d; log:\n%s", n, msgs)
	}
	if !strings.Contains(msgs, "Result=FAILED") || !strings.Contains(msgs, "Service=<none>") {
		t.Errorf("expected a FAILED audit entry for Service=<none>; got:\n%s", msgs)
	}
}

// TestLastDiffEndpoint_NoManagedService_Returns500 pins /lastdiff's
// no-managed-service 500 as a jsonError body (like /diff) plus one FAILED
// "<none>" audit line.
func TestLastDiffEndpoint_NoManagedService_Returns500(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: false}}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/lastdiff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("/lastdiff with no managed service: expected 500, got %d (body: %s)", w.Code, w.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("/lastdiff 500 body should be JSON: %v (body: %s)", err, w.Body.String())
	}
	if !strings.Contains(body["error"], "no managed service") {
		t.Errorf("expected 'no managed service' in JSON error; got: %+v", body)
	}
	msgs := decodeLogMsgs(t, buf)
	if n := strings.Count(msgs, "[AUDIT]"); n != 1 {
		t.Errorf("expected exactly 1 [AUDIT] line, got %d; log:\n%s", n, msgs)
	}
	if !strings.Contains(msgs, "Result=FAILED") || !strings.Contains(msgs, "Service=<none>") {
		t.Errorf("expected a FAILED audit entry for Service=<none>; got:\n%s", msgs)
	}
}

func TestDiffEndpoint_HappyPath_ReturnsJSON(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "") // no clone; still a valid (degraded) read-only run

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "testsvc", Selector: "testsvc"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("/diff: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/diff Content-Type: got %q, want application/json", ct)
	}
	var resp DiffResponseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("/diff body should be valid JSON: %v (body: %s)", err, w.Body.String())
	}
	if len(resp.Services) != 1 || resp.Services[0].Service != "testsvc" {
		t.Errorf("expected one service 'testsvc' in the response; got %+v", resp.Services)
	}
	if resp.GeneratedAt == "" {
		t.Error("expected GeneratedAt to be set")
	}

	// Cheap sanity spot-check: top-level JSON keys alphabetical.
	body := w.Body.String()
	gi, si := strings.Index(body, `"generated_at"`), strings.Index(body, `"services"`)
	if gi < 0 || si < 0 || gi > si {
		t.Errorf("expected top-level keys alphabetical (generated_at before services); body:\n%s", body)
	}
}

func TestLastDiffEndpoint_HappyPath_ReturnsJSON(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	recordConfigDiff("testsvc", []byte("a: 1\n"), []byte("a: 2\n"))
	recordVersionDiff("testsvc", "1.0.0", "1.1.0", true)
	recordEnvDiff("testsvc", map[string]string{"K": "old"}, map[string]string{"K": "new"})

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "testsvc", Selector: "testsvc"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/lastdiff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("/lastdiff: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/lastdiff Content-Type: got %q, want application/json", ct)
	}
	var resp DiffResponseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("/lastdiff body should be valid JSON: %v (body: %s)", err, w.Body.String())
	}
	if len(resp.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(resp.Services))
	}
	sd := resp.Services[0]
	if sd.Service != "testsvc" || !sd.Version.Changed || sd.Version.Current != "1.0.0" || sd.Version.Wanted != "1.1.0" {
		t.Errorf("unexpected cached version diff in response: %+v", sd.Version)
	}
	if !slices.Contains(sd.Env.Changed, "K") {
		t.Errorf("expected cached Env.Changed to include K; got %+v", sd.Env)
	}
	if len(sd.Config) == 0 {
		t.Error("expected cached Config diff to be non-empty")
	}
}

func TestDiffEndpoint_MultiService_AuditsEachService(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "")

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "svcA",
		HostSelector:    "selA",
		HostSelectors:   []string{"selA", "selB"},
		AllowedServices: []string{"svcA", "svcB"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "svcA", Selector: "selA"},
			{Service: "svcB", Selector: "selB"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelInfo)
	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/diff: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	out := buf.String()
	for _, svc := range []string{"svcA", "svcB"} {
		if !strings.Contains(out, "Service="+svc) {
			t.Errorf("expected an [AUDIT] line naming %q; got:\n%s", svc, out)
		}
	}
	if n := strings.Count(out, "Action=/diff"); n < 2 {
		t.Errorf("expected >= 2 /diff audit lines (one per managed service), got %d", n)
	}
	if strings.Contains(out, "Result=DENIED") || strings.Contains(out, "Result=FAILED") {
		t.Errorf("multi-service /diff should audit ALLOWED for each service; got:\n%s", out)
	}
}

func TestLastDiffEndpoint_MultiService_AuditsEachService(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	recordConfigDiff("svcA", nil, []byte("a: 1\n"))
	recordConfigDiff("svcB", nil, []byte("b: 1\n"))

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "svcA",
		HostSelector:    "selA",
		HostSelectors:   []string{"selA", "selB"},
		AllowedServices: []string{"svcA", "svcB"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "svcA", Selector: "selA"},
			{Service: "svcB", Selector: "selB"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelInfo)
	r := httptest.NewRequest(http.MethodGet, "/lastdiff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/lastdiff: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	out := buf.String()
	for _, svc := range []string{"svcA", "svcB"} {
		if !strings.Contains(out, "Service="+svc) {
			t.Errorf("expected an [AUDIT] line naming %q; got:\n%s", svc, out)
		}
	}
	if n := strings.Count(out, "Action=/lastdiff"); n < 2 {
		t.Errorf("expected >= 2 /lastdiff audit lines (one per managed service), got %d", n)
	}
	if strings.Contains(out, "Result=DENIED") || strings.Contains(out, "Result=FAILED") {
		t.Errorf("multi-service /lastdiff should audit ALLOWED for each service; got:\n%s", out)
	}
}

func TestDiffEndpoint_RequiresJWTWhenAuthEnabled(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: true},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/diff without token: expected 401, got %d", w.Code)
	}
}

func TestDiffEndpoint_ValidJWT_Runs(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "")
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "testsvc", Selector: "testsvc"},
		},
		Auth: AuthConfig{Enabled: true},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	r.Header.Set("Authorization", "Bearer "+mustSignToken(t, priv))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("/diff with valid token: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

func TestLastDiffEndpoint_RequiresJWTWhenAuthEnabled(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: true},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/lastdiff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/lastdiff without token: expected 401, got %d", w.Code)
	}
}

func TestLastDiffEndpoint_ValidJWT_Runs(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "testsvc", Selector: "testsvc"},
		},
		Auth: AuthConfig{Enabled: true},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/lastdiff", nil)
	r.Header.Set("Authorization", "Bearer "+mustSignToken(t, priv))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("/lastdiff with valid token: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestLastDiffEndpoint_NoGitCloneOrVaultRead pins that /lastdiff never clones
// or reads Vault: GIT_REPOSITORY would fail loudly, no fake Vault exists, and
// the pre-seeded cache must come back verbatim with no clone in the log.
func TestLastDiffEndpoint_NoGitCloneOrVaultRead(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	t.Setenv("GIT_REPOSITORY", "file:///this/path/does/not/exist/at/all")
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")
	// No fake Vault: /lastdiff must never read Vault.

	const fabricatedConfigLine = "+ fabricated.marker: only-the-cache-has-this"
	recordConfigDiff("testsvc", nil, []byte("fabricated:\n  marker: only-the-cache-has-this\n"))
	recordVersionDiff("testsvc", "9.9.9-seed-current", "9.9.9-seed-wanted", true)
	recordEnvDiff("testsvc", map[string]string{}, map[string]string{"SEEDED_KEY": "x"})

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "testsvc", Selector: "testsvc"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/lastdiff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("/lastdiff: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	var resp DiffResponseJSON
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("/lastdiff body should be valid JSON: %v", err)
	}
	if len(resp.Services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(resp.Services))
	}
	sd := resp.Services[0]
	if !slices.Contains(sd.Config, fabricatedConfigLine) {
		t.Errorf("expected the exact pre-seeded config line back verbatim; got %+v", sd.Config)
	}
	if sd.Version.Current != "9.9.9-seed-current" || sd.Version.Wanted != "9.9.9-seed-wanted" {
		t.Errorf("expected the exact pre-seeded version back verbatim; got %+v", sd.Version)
	}
	if !slices.Contains(sd.Env.Added, "SEEDED_KEY") {
		t.Errorf("expected the exact pre-seeded env key back verbatim; got %+v", sd.Env)
	}

	logOut := decodeLogMsgs(t, buf)
	if strings.Contains(logOut, "does/not/exist") || strings.Contains(strings.ToLower(logOut), "clone") {
		t.Errorf("/lastdiff must never attempt a git clone; log shows clone activity:\n%s", logOut)
	}
}

// ─── /diff concurrency limiting ──────────────────────────────────────────────

// TestDiffEndpoint_SingleRequest_Returns200 pins that a lone /diff is never
// rejected by the cap.
func TestDiffEndpoint_SingleRequest_Returns200(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "") // no clone

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "testsvc", Selector: "testsvc"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("/diff single request: expected 200, got %d", w.Code)
	}
}

// TestDiffEndpoint_ConcurrentBeyondLimit_Returns500 fills the semaphore
// (deterministically, no goroutines) and pins a JSON busy-500, not queueing.
func TestDiffEndpoint_ConcurrentBeyondLimit_Returns500(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_REPOSITORY", "") // no clone

	for i := 0; i < diffConcurrencyLimit; i++ {
		diffConcurrencySem <- struct{}{} // simulate an acquired slot
	}
	defer func() {
		for i := 0; i < diffConcurrencyLimit; i++ {
			<-diffConcurrencySem
		}
	}()

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "testsvc", Selector: "testsvc"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	// At capacity: rejected as busy.
	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("/diff over capacity: expected 500, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/diff busy response Content-Type: got %q, want application/json", ct)
	}
	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Errorf("/diff busy response should be JSON: %v (body: %s)", err, w.Body.String())
	}
	if _, ok := errResp["error"]; !ok {
		t.Errorf("expected 'error' key in JSON response; got %+v", errResp)
	}
	if !strings.Contains(errResp["error"], "retry shortly") {
		t.Errorf("expected 'retry shortly' message; got: %s", errResp["error"])
	}
}

// TestDiffEndpoint_ReleaseOnError_SubsequentRequestSucceeds pins that the slot
// is released even when ComputeCurrentDiffs errors (no managed service).
func TestDiffEndpoint_ReleaseOnError_SubsequentRequestSucceeds(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Cleanup(func() {
		// Drain whatever this test left in the semaphore.
		for i := 0; i < diffConcurrencyLimit; i++ {
			select {
			case <-diffConcurrencySem:
			default:
				break
			}
		}
	})
	t.Setenv("GIT_REPOSITORY", "") // no clone

	for i := 0; i < diffConcurrencyLimit; i++ {
		diffConcurrencySem <- struct{}{}
	}

	mux := http.NewServeMux()
	// No managed service: ComputeCurrentDiffs errors.
	cfg := &Config{
		ServiceName:          "",
		HostSelector:         "",
		AllowedServices:      []string{},
		ContinuousDeployment: []ServiceEntry{},
		Auth:                 AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	// Semaphore full: busy-500, and no slot was taken.
	r := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusInternalServerError {
		t.Errorf("first /diff (semaphore full): expected 500, got %d", w.Code)
	}
	var errResp map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
		t.Errorf("first /diff response should be JSON: %v", err)
	}
	if !strings.Contains(errResp["error"], "retry shortly") {
		t.Errorf("first /diff should fail with busy message; got: %s", errResp["error"])
	}

	// Free one slot: the next request takes it, errors in ComputeCurrentDiffs,
	// and handleDiff's defer releases it.
	<-diffConcurrencySem

	r2 := httptest.NewRequest(http.MethodGet, "/diff", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)

	if w2.Code != http.StatusInternalServerError {
		t.Errorf("second /diff after release: expected 500, got %d", w2.Code)
	}
	var errResp2 map[string]string
	if err := json.Unmarshal(w2.Body.Bytes(), &errResp2); err != nil {
		t.Errorf("second /diff response should be JSON: %v", err)
	}

	// The no-managed-service error, not "retry shortly": it acquired and ran.
	if strings.Contains(errResp2["error"], "retry shortly") {
		t.Error("second /diff should have acquired a slot (not busy); got busy message")
	}
	if !strings.Contains(errResp2["error"], "no managed service") {
		t.Errorf("expected 'no managed service' error; got: %s", errResp2["error"])
	}
}
