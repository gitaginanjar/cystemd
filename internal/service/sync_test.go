package service

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── RunServiceSync — force overrides autosync ─────────────────────────────

// TestRunServiceSync_ForcesWriteWhenAutoSyncDisabled pins the core contract:
// with autosync OFF and real drift (no target.config.yml yet), /sync writes the
// extracted ${service_config} anyway (force only matters on drift).
func TestRunServiceSync_ForcesWriteWhenAutoSyncDisabled(t *testing.T) {
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "") // no auth → public file:// clone

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           false, // gate is OFF — only force can write
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workdir,
		}},
	}

	buf := captureLogs(t, LevelInfo)
	summary, _, err := RunServiceSync(appCfg)
	if err != nil {
		t.Fatalf("RunServiceSync returned error: %v", err)
	}

	destFile := filepath.Join(workdir, "myapplication", "target.config.yml")
	data, readErr := os.ReadFile(destFile)
	if readErr != nil {
		t.Fatalf("force sync must write target.config.yml even with autosync=false: %v", readErr)
	}
	if !strings.Contains(string(data), "myproject_myapplication") {
		t.Errorf("expected extracted content in target.config.yml; got:\n%s", string(data))
	}

	// Force opens the gate: no "out-of-date (autosync=false)" drift line.
	if strings.Contains(buf.String(), "out-of-date (autosync=false)") {
		t.Errorf("forced sync must NOT log the autosync-off drift line; got:\n%s", buf.String())
	}
	if !strings.Contains(summary, "forced sync of") || !strings.Contains(summary, "myapplication") {
		t.Errorf("summary should name the synced service; got: %q", summary)
	}
}

// TestRunServiceSync_ForcesVersionInstallWhenAutoSyncDisabled pins that /sync
// ATTEMPTS the RPM install with autosync OFF (asserted from the log; without dnf
// the install itself fails fail-soft), never the "not installing" drift line.
func TestRunServiceSync_ForcesVersionInstallWhenAutoSyncDisabled(t *testing.T) {
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            false, // gate OFF — only force can install
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workdir,
		}},
	}

	buf := captureLogs(t, LevelDebug)
	if _, _, err := RunServiceSync(appCfg); err != nil {
		t.Fatalf("RunServiceSync returned error: %v", err)
	}
	out := buf.String()

	// Force must drive the install attempt, NOT the autosync-off drift line.
	if !strings.Contains(out, "installing RPM") {
		t.Errorf("forced sync must attempt the version install despite autosync=false; got:\n%s", out)
	}
	if strings.Contains(out, "version out-of-date") && strings.Contains(out, "not installing") {
		t.Errorf("forced sync must NOT log the autosync-off 'not installing' drift line; got:\n%s", out)
	}
	if !strings.Contains(out, "abc123def456abc123def456abc123def456abc123") {
		t.Errorf("expected the upstream version string in the log; got:\n%s", out)
	}
}

// TestRunServiceSync_InSync_NoForcedWriteSideEffect pins that force is not
// "always rewrite": an in-sync file is left alone (no mtime churn, no restart).
func TestRunServiceSync_InSync_NoForcedWriteSideEffect(t *testing.T) {
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           false,
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workdir,
		}},
	}

	// First forced sync writes the file (drift → forced write).
	if _, _, err := RunServiceSync(appCfg); err != nil {
		t.Fatalf("first RunServiceSync: %v", err)
	}
	destFile := filepath.Join(workdir, "myapplication", "target.config.yml")
	fi1, err := os.Stat(destFile)
	if err != nil {
		t.Fatalf("expected file after first sync: %v", err)
	}

	// Second forced sync: content is already in sync, so no rewrite log.
	buf := captureLogs(t, LevelInfo)
	if _, _, err := RunServiceSync(appCfg); err != nil {
		t.Fatalf("second RunServiceSync: %v", err)
	}
	if strings.Contains(buf.String(), "wrote") && strings.Contains(buf.String(), "target.config.yml") {
		t.Errorf("in-sync forced sync must not rewrite the file; got:\n%s", buf.String())
	}
	if fi2, err := os.Stat(destFile); err == nil && !fi2.ModTime().Equal(fi1.ModTime()) {
		t.Errorf("in-sync forced sync must not touch the file mtime")
	}
}

// TestRunServiceSync_MultiService_AppliesAllManagedServices pins that one /sync
// forces EVERY managed service (two pools, autosync OFF), each in its own workdir.
func TestRunServiceSync_MultiService_AppliesAllManagedServices(t *testing.T) {
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml": "k: v\n",
		"v3.yaml":    multiSvcValues("app3", "svc3"),
		"v4.yaml":    multiSvcValues("app4", "svc4"),
	})
	t.Setenv("GIT_REPOSITORY", "file://"+repoDir)
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")

	workdir3 := mkTempDir(t)
	workdir4 := mkTempDir(t)
	appCfg := &Config{
		ServiceName:   "myproject_app3",
		HostSelector:  "app3",
		HostSelectors: []string{"app3", "app4"},
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: false, Service: "myproject_app3", Selector: "app3",
				GitValues: "v3.yaml", GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: workdir3},
			{AutoSync: false, Service: "myproject_app4", Selector: "app4",
				GitValues: "v4.yaml", GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: workdir4},
		},
	}

	summary, _, err := RunServiceSync(appCfg)
	if err != nil {
		t.Fatalf("RunServiceSync: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(workdir3, "myproject_app3", "target.config.yml")); err != nil {
		t.Errorf("forced sync must write app3 config despite autosync=false: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(workdir4, "myproject_app4", "target.config.yml")); err != nil {
		t.Errorf("forced sync must write app4 config (second managed service): %v", err)
	}
	if !strings.Contains(summary, "2 service(s)") {
		t.Errorf("summary should report 2 services; got: %q", summary)
	}
}

// TestRunServiceSync_Busy_ReturnsError pins the TryLock contract: with syncMu
// held in-test, RunServiceSync reports "already in progress" instead of queueing.
func TestRunServiceSync_Busy_ReturnsError(t *testing.T) {
	syncMu.Lock()
	defer syncMu.Unlock()

	appCfg := &Config{ServiceName: "svc", HostSelector: "svc"}
	_, _, err := RunServiceSync(appCfg)
	if err == nil {
		t.Fatal("expected a busy error while syncMu is held")
	}
	if !strings.Contains(err.Error(), "already in progress") {
		t.Errorf("expected 'already in progress' error; got: %v", err)
	}
}

// TestRunServiceSync_NilConfig_ReturnsError covers the nil-guard.
func TestRunServiceSync_NilConfig_ReturnsError(t *testing.T) {
	if _, _, err := RunServiceSync(nil); err == nil {
		t.Fatal("expected error for nil config")
	}
}

// TestRunServiceSync_NoManagedService_ReturnsError covers a host that resolves
// to no managed service.
func TestRunServiceSync_NoManagedService_ReturnsError(t *testing.T) {
	_, _, err := RunServiceSync(&Config{})
	if err == nil {
		t.Fatal("expected error when no managed service is configured")
	}
	if !strings.Contains(err.Error(), "no managed service") {
		t.Errorf("expected 'no managed service' error; got: %v", err)
	}
}

// TestRunServiceSync_NoGitRepo_SkipsValuesStillReturnsSummary pins that an unset
// GIT_REPOSITORY skips values/version/quota (not an error) and says so.
func TestRunServiceSync_NoGitRepo_SkipsValuesStillReturnsSummary(t *testing.T) {
	t.Setenv("GIT_REPOSITORY", "")

	summary, _, err := RunServiceSync(&Config{
		ServiceName:  "svc",
		HostSelector: "svc",
		ContinuousDeployment: []ServiceEntry{
			{Service: "svc", Selector: "svc"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(summary, "values/version/quota: skipped (GIT_REPOSITORY not set)") {
		t.Errorf("expected GIT_REPOSITORY skip in summary; got: %q", summary)
	}
	// Vault is not configured in this test → secrets skipped, not errored.
	if !strings.Contains(summary, "secrets: skipped") {
		t.Errorf("expected secrets skip in summary; got: %q", summary)
	}
}

// TestBuildUsageJSON_SyncEndpointAndExamplePresent pins /sync on the usage page:
// a JWT-required endpoint with curl examples that carry no ?service=.
func TestBuildUsageJSON_SyncEndpointAndExamplePresent(t *testing.T) {
	cfg := &Config{ServiceName: "myproject_myapp", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "lb.example.com"
	u := buildUsageJSON(r, cfg)

	var ep *UsageEndpoint
	for i := range u.Endpoints {
		if u.Endpoints[i].Path == "/sync" {
			ep = &u.Endpoints[i]
		}
	}
	if ep == nil {
		t.Fatal("usage page must list the /sync endpoint")
	}
	if !ep.AuthRequired || ep.ResponseType != "text/plain" || ep.Method != "GET" {
		t.Errorf("/sync endpoint metadata wrong: %+v", *ep)
	}

	inExamples := func(list []string) bool {
		for _, ex := range list {
			if strings.Contains(ex, "/sync") {
				return true
			}
		}
		return false
	}
	if !inExamples(u.Examples.ViaLoadBalancer) {
		t.Errorf("usage page ViaLoadBalancer examples should include /sync; got: %v", u.Examples.ViaLoadBalancer)
	}
	if !inExamples(u.Examples.ViaServerIP) {
		t.Errorf("usage page ViaServerIP examples should include /sync; got: %v", u.Examples.ViaServerIP)
	}
	// Host-scoped: the /sync example must not imply a ?service= parameter.
	for _, ex := range append(append([]string{}, u.Examples.ViaLoadBalancer...), u.Examples.ViaServerIP...) {
		if strings.Contains(ex, "/sync?service=") {
			t.Errorf("/sync example must not include ?service= (host-scoped); got: %s", ex)
		}
	}
}

// ─── /sync HTTP endpoint ───────────────────────────────────────────────────

// TestSyncEndpoint_RequiresJWTWhenAuthEnabled pins 401 without a token when auth
// is on.
func TestSyncEndpoint_RequiresJWTWhenAuthEnabled(t *testing.T) {
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

	r := httptest.NewRequest(http.MethodGet, "/sync?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("/sync without token: expected 401, got %d", w.Code)
	}
}

// TestSyncEndpoint_ValidJWT_Runs pins that a valid token reaches the forced sync
// (200 + summary); GIT_REPOSITORY is cleared, so no clone happens.
func TestSyncEndpoint_ValidJWT_Runs(t *testing.T) {
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)
	t.Setenv("GIT_REPOSITORY", "")

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

	r := httptest.NewRequest(http.MethodGet, "/sync?service=testsvc", nil)
	r.Header.Set("Authorization", "Bearer "+mustSignToken(t, priv))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("/sync with valid token: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("/sync Content-Type: got %q, want text/plain", ct)
	}
	if !strings.Contains(w.Body.String(), "forced sync of") || !strings.Contains(w.Body.String(), "testsvc") {
		t.Errorf("/sync body should summarise the sync and name the service; got: %s", w.Body.String())
	}
}

// TestSyncEndpoint_OpenWhenAuthDisabled pins /sync reachable without a token
// when auth is off.
func TestSyncEndpoint_OpenWhenAuthDisabled(t *testing.T) {
	t.Setenv("GIT_REPOSITORY", "")
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

	r := httptest.NewRequest(http.MethodGet, "/sync?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("/sync (auth disabled): expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}
}

// TestSyncEndpoint_RejectsNonGET pins 405 on POST with exactly one DENIED audit
// line, as on every request path.
func TestSyncEndpoint_RejectsNonGET(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodPost, "/sync?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /sync: expected 405, got %d", w.Code)
	}
	msgs := decodeLogMsgs(t, buf)
	if n := strings.Count(msgs, "[AUDIT]"); n != 1 {
		t.Errorf("expected exactly 1 [AUDIT] line for a 405 /sync request, got %d; log:\n%s", n, msgs)
	}
	if !strings.Contains(msgs, "Result=DENIED") {
		t.Errorf("expected a DENIED audit entry for method-not-allowed on /sync; got:\n%s", msgs)
	}
}

// TestSyncEndpoint_MultiService_AuditsEachService pins one [AUDIT] line per
// managed service on a multi-service node, so every mutated unit is recorded.
func TestSyncEndpoint_MultiService_AuditsEachService(t *testing.T) {
	t.Setenv("GIT_REPOSITORY", "") // no clone; sync is a no-op but still audits scope
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "svcA",
		HostSelector:    "selA",
		HostSelectors:   []string{"selA", "selB"}, // host in two pools → two services
		AllowedServices: []string{"svcA", "svcB"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "svcA", Selector: "selA"},
			{Service: "svcB", Selector: "selB"},
		},
		Auth: AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	buf := captureLogs(t, LevelInfo)
	r := httptest.NewRequest(http.MethodGet, "/sync", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/sync: expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	out := buf.String()
	for _, svc := range []string{"svcA", "svcB"} {
		if !strings.Contains(out, "Service="+svc) {
			t.Errorf("expected an [AUDIT] line naming %q; got:\n%s", svc, out)
		}
	}
	// Both audit lines are for /sync and ALLOWED, and there are at least two.
	if n := strings.Count(out, "Action=/sync"); n < 2 {
		t.Errorf("expected >= 2 /sync audit lines (one per managed service), got %d", n)
	}
	if strings.Contains(out, "Result=DENIED") || strings.Contains(out, "Result=FAILED") {
		t.Errorf("multi-service /sync should audit ALLOWED for each service; got:\n%s", out)
	}
}
