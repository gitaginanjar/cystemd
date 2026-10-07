package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the branch-grouping and clone-once scaffolding (groupEntriesByBranch,
// cloneBranchTreeOnce) shared by the CD write path and the read-only /diff path.
// Why: DOCS/CLAUDE.md § Continuous Deployment.

// ─── groupEntriesByBranch ───────────────────────────────────────────────────

// Entries on one branch form one group, in declaration order.
func TestGroupEntriesByBranch_SameBranchCollapsesIntoOneGroupInOrder(t *testing.T) {
	entries := []ServiceEntry{
		{Service: "a", GitValues: "v1.yaml"},
		{Service: "b", GitValues: "v2.yaml"},
		{Service: "c", GitValues: "v3.yaml"},
	}
	groups := groupEntriesByBranch(entries)
	if len(groups) != 1 {
		t.Fatalf("expected 1 group (all default branch), got %d: %+v", len(groups), groups)
	}
	if groups[0].branch != "master" {
		t.Errorf("expected default branch %q, got %q", "master", groups[0].branch)
	}
	if len(groups[0].entries) != 3 {
		t.Fatalf("expected all 3 entries in the single group, got %d", len(groups[0].entries))
	}
	gotOrder := []string{groups[0].entries[0].Service, groups[0].entries[1].Service, groups[0].entries[2].Service}
	wantOrder := []string{"a", "b", "c"}
	for i := range wantOrder {
		if gotOrder[i] != wantOrder[i] {
			t.Errorf("entry order not preserved: got %v, want %v", gotOrder, wantOrder)
			break
		}
	}
}

// Distinct branches form separate groups in first-seen order, which keeps deploy
// and /diff output deterministic despite the internal map.
func TestGroupEntriesByBranch_DistinctBranchesProduceSeparateGroupsInFirstSeenOrder(t *testing.T) {
	entries := []ServiceEntry{
		{Service: "a", GitValues: "v1.yaml", GitBranch: "releasedev"},
		{Service: "b", GitValues: "v2.yaml"}, // default -> "master"
		{Service: "c", GitValues: "v3.yaml", GitBranch: "releasedev"},
		{Service: "d", GitValues: "v4.yaml", GitBranch: "hotfix"},
	}
	groups := groupEntriesByBranch(entries)
	if len(groups) != 3 {
		t.Fatalf("expected 3 distinct branch groups, got %d: %+v", len(groups), groups)
	}
	wantBranches := []string{"releasedev", "master", "hotfix"}
	for i, want := range wantBranches {
		if groups[i].branch != want {
			t.Errorf("group %d: branch = %q, want %q (order: %v)", i, groups[i].branch, want, groups)
		}
	}
	// releasedev must carry both "a" and "c", in that order.
	if len(groups[0].entries) != 2 || groups[0].entries[0].Service != "a" || groups[0].entries[1].Service != "c" {
		t.Errorf("expected releasedev group = [a, c], got %+v", groups[0].entries)
	}
}

// An entry without Service or GitValues is dropped, not grouped.
func TestGroupEntriesByBranch_SkipsEntriesMissingServiceOrGitValues(t *testing.T) {
	entries := []ServiceEntry{
		{Service: "", GitValues: "v1.yaml"},  // no Service
		{Service: "b", GitValues: ""},        // no GitValues
		{Service: "c", GitValues: "v3.yaml"}, // the only deployable/diffable entry
	}
	groups := groupEntriesByBranch(entries)
	if len(groups) != 1 || len(groups[0].entries) != 1 || groups[0].entries[0].Service != "c" {
		t.Fatalf("expected exactly one group containing only 'c', got %+v", groups)
	}
}

// No entries, no groups: deploy and /diff are no-ops and clone nothing.
func TestGroupEntriesByBranch_EmptyInputProducesNoGroups(t *testing.T) {
	if groups := groupEntriesByBranch(nil); len(groups) != 0 {
		t.Errorf("expected no groups for nil input, got %+v", groups)
	}
}

// ─── cloneBranchTreeOnce ─────────────────────────────────────────────────────

// Success: withTree runs exactly once on a tree holding the committed content,
// onCloneResult(true) fires, and the temp dir is gone when cloneBranchTreeOnce returns.
func TestCloneBranchTreeOnce_SuccessInvokesWithTreeExactlyOnceAndReportsSuccess(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")
	repoDir := setupLocalGitRepo(t, map[string]string{"marker.txt": "hello-from-repo\n"})

	var (
		withTreeCalls int
		observedTree  string
		markerContent []byte
		markerReadErr error
		cloneResults  []bool
	)
	cloneBranchTreeOnce("file://"+repoDir, "master", nil, 1, "cystemd_testclone_*", "testcaller",
		func(ok bool) { cloneResults = append(cloneResults, ok) },
		func(treeDir string) {
			withTreeCalls++
			observedTree = treeDir
			// Read inside the callback: the temp dir is removed as soon as withTree returns.
			markerContent, markerReadErr = os.ReadFile(filepath.Join(treeDir, "marker.txt"))
		})

	if withTreeCalls != 1 {
		t.Fatalf("expected withTree to be invoked exactly once, got %d", withTreeCalls)
	}
	if markerReadErr != nil {
		t.Fatalf("expected the cloned tree to contain marker.txt: %v", markerReadErr)
	}
	if string(markerContent) != "hello-from-repo\n" {
		t.Errorf("marker.txt content = %q, want %q", markerContent, "hello-from-repo\n")
	}
	if len(cloneResults) != 1 || cloneResults[0] != true {
		t.Errorf("expected onCloneResult(true) exactly once, got %v", cloneResults)
	}
	if _, err := os.Stat(observedTree); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected temp clone dir %q to be removed after cloneBranchTreeOnce returns, stat err=%v", observedTree, err)
	}
}

// Failure is contained: withTree never runs; onCloneResult(false) fires once.
func TestCloneBranchTreeOnce_FailureNeverInvokesWithTreeAndReportsFailure(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")
	buf := captureLogs(t, LevelDebug)

	var (
		withTreeCalls int
		cloneResults  []bool
	)
	cloneBranchTreeOnce("file:///nonexistent/path/to/repo", "master", nil, 2, "cystemd_testclone_*", "testcaller",
		func(ok bool) { cloneResults = append(cloneResults, ok) },
		func(treeDir string) { withTreeCalls++ })

	if withTreeCalls != 0 {
		t.Errorf("expected withTree to never be invoked on clone failure, got %d call(s)", withTreeCalls)
	}
	if len(cloneResults) != 1 || cloneResults[0] != false {
		t.Errorf("expected onCloneResult(false) exactly once, got %v", cloneResults)
	}
	if !strings.Contains(buf.String(), "testcaller: values clone of") || !strings.Contains(buf.String(), "failed") {
		t.Errorf("expected a %q-prefixed clone-failure log line; got:\n%s", "testcaller", buf.String())
	}
}

// onCloneResult is optional (diffBranchGroup passes nil to keep /diff out of
// cystemd_cd_clone_total); neither path may panic on nil.
func TestCloneBranchTreeOnce_NilOnCloneResultIsSafe(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")
	repoDir := setupLocalGitRepo(t, map[string]string{"f.txt": "x\n"})

	calls := 0
	cloneBranchTreeOnce("file://"+repoDir, "master", nil, 1, "cystemd_testclone_*", "testcaller", nil,
		func(treeDir string) { calls++ })
	if calls != 1 {
		t.Errorf("expected withTree to fire once on success even with nil onCloneResult, got %d", calls)
	}

	// Failure path with nil onCloneResult must also not panic.
	cloneBranchTreeOnce("file:///nonexistent/path/to/repo", "master", nil, 1, "cystemd_testclone_*", "testcaller", nil,
		func(treeDir string) { t.Fatalf("withTree must not be called on clone failure") })
}

// Each caller's temp-dir and log prefixes take effect: "cystemd_vals_*"/"cd" for
// deploy, "cystemd_diff_*"/"diff" for /diff.
func TestCloneBranchTreeOnce_UsesGivenTmpPrefixAndLogPrefix(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")
	repoDir := setupLocalGitRepo(t, map[string]string{"f.txt": "x\n"})
	buf := captureLogs(t, LevelDebug)

	var observedTree string
	cloneBranchTreeOnce("file://"+repoDir, "master", nil, 1, "cystemd_diff_*", "diff", nil,
		func(treeDir string) { observedTree = treeDir })

	base := filepath.Base(observedTree)
	if !strings.HasPrefix(base, "cystemd_diff_") {
		t.Errorf("expected temp dir basename to start with %q, got %q", "cystemd_diff_", base)
	}
	if !strings.Contains(buf.String(), "diff: cloned") {
		t.Errorf("expected a 'diff: cloned' log line; got:\n%s", buf.String())
	}
}

// ─── Cross-caller proof: CD and /diff clone exactly once per branch ────────

// /diff counterpart of TestRunContinuousDeployment_MultiService_SameBranch_ClonesOnce:
// one clone per shared branch, and a diff recorded for every service.
func TestComputeCurrentDiffs_MultiService_SameBranch_ClonesOnce(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"v1.yaml": multiSvcValues("a", "sa"),
		"v2.yaml": multiSvcValues("b", "sb"),
		"v3.yaml": multiSvcValues("c", "sc"),
	})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)

	wd := mkTempDir(t)
	mkEntry := func(svc, sel, vals string) ServiceEntry {
		return ServiceEntry{Service: svc, Selector: sel, GitValues: vals,
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: wd}
	}
	appCfg := &Config{
		ServiceName:   "sa",
		HostSelector:  "a",
		HostSelectors: []string{"a", "b", "c"},
		ContinuousDeployment: []ServiceEntry{
			mkEntry("sa", "a", "v1.yaml"),
			mkEntry("sb", "b", "v2.yaml"),
			mkEntry("sc", "c", "v3.yaml"),
		},
	}

	buf := captureLogs(t, LevelDebug)
	results, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("expected 3 diffed services, got %d: %+v", len(results), results)
	}
	for _, sd := range results {
		if len(sd.Config) == 0 {
			t.Errorf("service %s: expected a non-empty config diff (no on-disk file yet), got %+v", sd.Service, sd.Config)
		}
	}
	// All three services share the default branch -> exactly ONE values clone.
	if n := strings.Count(buf.String(), "diff: cloned"); n != 1 {
		t.Errorf("expected exactly 1 shared values clone for 3 same-branch services; got %d:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "once for 3 managed service(s)") {
		t.Errorf("expected the clone line to report 3 services; got:\n%s", buf.String())
	}
}

// /diff counterpart of TestRunContinuousDeployment_MultiService_DistinctBranches_ClonesEach:
// one clone per distinct branch.
func TestComputeCurrentDiffs_MultiService_DistinctBranches_ClonesEach(t *testing.T) {
	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"v1.yaml": multiSvcValues("a", "sa"),
	})
	addBranchWithFile(t, repoDir, "releasedev", "v2.yaml", multiSvcValues("b", "sb"))
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)

	wd := mkTempDir(t)
	appCfg := &Config{
		ServiceName:   "sa",
		HostSelector:  "a",
		HostSelectors: []string{"a", "b"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "sa", Selector: "a", GitValues: "v1.yaml",
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: wd}, // default branch
			{Service: "sb", Selector: "b", GitBranch: "releasedev", GitValues: "v2.yaml",
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: wd},
		},
	}

	buf := captureLogs(t, LevelDebug)
	results, err := ComputeCurrentDiffs(appCfg)
	if err != nil {
		t.Fatalf("ComputeCurrentDiffs: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 diffed services, got %d: %+v", len(results), results)
	}
	for _, sd := range results {
		if len(sd.Config) == 0 {
			t.Errorf("service %s: expected a non-empty config diff, got %+v", sd.Service, sd.Config)
		}
	}
	if n := strings.Count(buf.String(), "diff: cloned"); n != 2 {
		t.Errorf("expected 2 clones (one per distinct branch); got %d:\n%s", n, buf.String())
	}

	// Read-only: /diff never writes a managed-service file.
	for _, svc := range []string{"sa", "sb"} {
		if _, err := os.Stat(filepath.Join(wd, svc, "target.config.yml")); err == nil {
			t.Errorf("service %s: /diff must not write target.config.yml", svc)
		}
	}
}
