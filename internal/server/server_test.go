// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/git"
	"github.com/gitaginanjar/cystemd/internal/metrics"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/rbac"
	"github.com/gitaginanjar/cystemd/internal/server"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

type harness struct {
	t      *testing.T
	srv    *httptest.Server
	store  *store.Store
	admin  string // admin bearer token
	reader string // readonly bearer token
	// adminPassword is the admin's password.
	adminPassword string
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil) }

// newHarnessWith lets configure change the configuration first.
func newHarnessWith(t *testing.T, configure func(*config.Config)) *harness {
	t.Helper()
	return newHarnessOpts(t, configure, nil)
}

// newHarnessOpts also lets options change the server's options.
func newHarnessOpts(t *testing.T, configure func(*config.Config), options func(*server.Options)) *harness {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Nomad.Address = "http://127.0.0.1:1" // unreachable: these tests need no Nomad
	if configure != nil {
		configure(cfg)
	}
	st := store.New(store.NewMemoryKV(), "cystemd")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	am, err := auth.NewManager(ctx, auth.Options{Store: st, AdminEnabled: true, DataDir: cfg.DataDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	// The accounts declared by the configuration, and "reader".
	declared := []auth.DeclaredAccount{{Name: "reader", Capabilities: []string{auth.CapabilityLogin}, Enabled: true}}
	for _, a := range cfg.Auth.Accounts {
		d := auth.DeclaredAccount{Name: a.Name, Capabilities: a.Capabilities, Enabled: a.Enabled == nil || *a.Enabled}
		if len(d.Capabilities) == 0 {
			d.Capabilities = []string{auth.CapabilityLogin}
		}
		declared = append(declared, d)
	}
	if err := am.SyncAccounts(ctx, declared); err != nil {
		t.Fatal(err)
	}
	if err := am.SetPassword(ctx, "reader", "reader-password"); err != nil {
		t.Fatal(err)
	}
	enforcer, err := rbac.New(cfg.RBAC.Policy, rbac.RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	clusters := nomad.NewClusters()
	nc, _ := nomad.New(nomad.Config{Address: cfg.Nomad.Address})
	clusters.Add(v1.InClusterName, nc)
	gitClient := git.NewClient(git.Options{CacheDir: t.TempDir()})
	repos := &controller.RepoService{Git: gitClient, Clusters: clusters}
	o := server.Options{
		Config: cfg, Store: st, Auth: am, RBAC: enforcer, Clusters: clusters, Git: gitClient, Repos: repos,
		Live: &controller.Live{Clusters: clusters, Repos: repos, Apps: st}, Metrics: metrics.New(), Logger: logger,
		Build: server.BuildInfo{Version: "test", Mode: "standalone"},
	}
	if options != nil {
		options(&o)
	}
	s := server.New(o)
	h := &harness{t: t, srv: httptest.NewServer(s.Handler()), store: st}
	t.Cleanup(h.srv.Close)
	pw, _ := os.ReadFile(filepath.Join(cfg.DataDir, "initial-admin-password"))
	h.adminPassword = strings.TrimSpace(string(pw))
	h.admin = h.login("admin", h.adminPassword)
	h.reader = h.login("reader", "reader-password")
	return h
}

func (h *harness) login(user, pw string) string {
	var out map[string]string
	code := h.do("POST", "/api/v1/session", "", map[string]string{"username": user, "password": pw}, &out)
	if code != 200 || out["token"] == "" {
		h.t.Fatalf("login %s: %d %v", user, code, out)
	}
	return out["token"]
}

func (h *harness) do(method, path, token string, body any, out any) int {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func demoApp(name string) *v1.Application {
	return &v1.Application{
		Metadata: v1.ObjectMeta{Name: name},
		Spec: v1.ApplicationSpec{
			Source: v1.ApplicationSource{RepoURL: "https://git.example.com/ops.git", Path: "apps/" + name},
		},
	}
}

func TestPublicAndAuthenticatedRoutes(t *testing.T) {
	h := newHarness(t)
	var settings server.Settings
	if code := h.do("GET", "/api/v1/settings", "", nil, &settings); code != 200 || settings.AppLabelKey != v1.AppMetaKey {
		t.Fatalf("settings: %d %+v", code, settings)
	}
	if code := h.do("GET", "/api/v1/applications", "", nil, nil); code != 401 {
		t.Fatalf("unauthenticated list: %d", code)
	}
	var info server.UserInfo
	h.do("GET", "/api/v1/session/userinfo", h.reader, nil, &info)
	if !info.LoggedIn || info.Username != "reader" || info.IsAdmin {
		t.Fatalf("reader info = %+v", info)
	}
	if code := h.do("POST", "/api/v1/session", "", map[string]string{"username": "admin", "password": "nope"}, nil); code != 401 {
		t.Fatalf("bad login: %d", code)
	}
}

func TestApplicationLifecycleAndRBAC(t *testing.T) {
	h := newHarness(t)
	// The reader cannot create.
	if code := h.do("POST", "/api/v1/applications", h.reader, demoApp("web"), nil); code != 403 {
		t.Fatalf("reader create: %d", code)
	}
	var created v1.Application
	if code := h.do("POST", "/api/v1/applications", h.admin, demoApp("web"), &created); code != 200 {
		t.Fatalf("admin create: %d", code)
	}
	if created.Spec.Project != "default" || created.APIVersion != v1.APIVersion {
		t.Fatalf("defaults not applied: %+v", created)
	}
	if code := h.do("POST", "/api/v1/applications", h.admin, demoApp("web"), nil); code != 409 {
		t.Fatalf("duplicate create: %d", code)
	}
	bad := demoApp("Bad_Name")
	if code := h.do("POST", "/api/v1/applications", h.admin, bad, nil); code != 400 {
		t.Fatalf("invalid name: %d", code)
	}

	// The reader sees it but cannot sync.
	var list server.ApplicationList
	h.do("GET", "/api/v1/applications", h.reader, nil, &list)
	if len(list.Items) != 1 {
		t.Fatalf("reader list = %d", len(list.Items))
	}
	if code := h.do("POST", "/api/v1/applications/web/sync", h.reader, map[string]any{}, nil); code != 403 {
		t.Fatalf("reader sync: %d", code)
	}

	// Admin sync records the operation; a second one conflicts.
	var synced v1.Application
	if code := h.do("POST", "/api/v1/applications/web/sync", h.admin, map[string]any{"prune": true}, &synced); code != 200 {
		t.Fatalf("sync: %d", code)
	}
	if synced.Operation == nil || synced.Operation.InitiatedBy.Username != "admin" || !synced.Operation.Sync.Prune {
		t.Fatalf("operation = %+v", synced.Operation)
	}
	if code := h.do("POST", "/api/v1/applications/web/sync", h.admin, map[string]any{}, nil); code != 409 {
		t.Fatalf("concurrent sync: %d", code)
	}
	// Terminating a not-yet-started operation clears it.
	if code := h.do("DELETE", "/api/v1/applications/web/operation", h.admin, map[string]any{}, nil); code != 200 {
		t.Fatalf("terminate: %d", code)
	}
	app, _ := h.store.GetApplication(context.Background(), "web")
	if app.Operation != nil {
		t.Fatal("operation not cleared")
	}

	// A stale resourceVersion is a conflict; the current one updates.
	created.Spec.SyncPolicy = &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{}}
	if code := h.do("PUT", "/api/v1/applications/web", h.admin, created, nil); code != 409 {
		t.Fatalf("stale update: %d", code)
	}
	var current v1.Application
	h.do("GET", "/api/v1/applications/web", h.admin, nil, &current)
	current.Spec.SyncPolicy = &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{}}
	if code := h.do("PUT", "/api/v1/applications/web", h.admin, current, nil); code != 200 {
		t.Fatalf("update: %d", code)
	}

	// Rollback is refused while auto-sync is on.
	if code := h.do("POST", "/api/v1/applications/web/rollback", h.admin, map[string]any{"id": 0}, nil); code != 400 {
		t.Fatalf("rollback with auto-sync: %d", code)
	}

	// Cascade delete marks the app for the controller's finalizer.
	if code := h.do("DELETE", "/api/v1/applications/web?cascade=true", h.admin, map[string]any{}, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	app, _ = h.store.GetApplication(context.Background(), "web")
	if app.Metadata.DeletionTimestamp == nil || !app.Metadata.HasFinalizer(v1.ResourcesFinalizer) {
		t.Fatalf("cascade delete not requested: %+v", app.Metadata)
	}
}

func TestProjectRestrictions(t *testing.T) {
	h := newHarness(t)
	proj := v1.AppProject{
		Metadata: v1.ObjectMeta{Name: "team-a"},
		Spec: v1.AppProjectSpec{
			SourceRepos:  []string{"https://git.example.com/team-a/*"},
			Destinations: []v1.ApplicationDestination{{Name: "in-cluster", Namespace: "team-a"}},
		},
	}
	if code := h.do("POST", "/api/v1/projects", h.admin, proj, nil); code != 200 {
		t.Fatalf("create project: %d", code)
	}
	app := demoApp("svc")
	app.Spec.Project = "team-a"
	if code := h.do("POST", "/api/v1/applications", h.admin, app, nil); code != 400 {
		t.Fatalf("source outside project accepted: %d", code)
	}
	app.Spec.Source.RepoURL = "https://git.example.com/team-a/svc.git"
	if code := h.do("POST", "/api/v1/applications", h.admin, app, nil); code != 400 {
		t.Fatalf("destination outside project accepted: %d", code)
	}
	app.Spec.Destination.Namespace = "team-a"
	if code := h.do("POST", "/api/v1/applications", h.admin, app, nil); code != 200 {
		t.Fatalf("valid app refused: %d", code)
	}
	if code := h.do("DELETE", "/api/v1/projects/team-a", h.admin, map[string]any{}, nil); code != 400 {
		t.Fatalf("project in use deleted: %d", code)
	}
}

func TestCookieWritesMustBeJSON(t *testing.T) {
	h := newHarness(t)
	// A cross-site form post (cookie, form content type) is refused.
	req, _ := http.NewRequest("POST", h.srv.URL+"/api/v1/applications", strings.NewReader("metadata.name=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: h.admin})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("form post with cookie: %d", resp.StatusCode)
	}
}

func TestPasswordChange(t *testing.T) {
	h := newHarness(t)
	if code := h.do("PUT", "/api/v1/account/password", h.reader, map[string]string{"currentPassword": "wrong", "newPassword": "something-long"}, nil); code != 400 {
		t.Fatalf("wrong current password: %d", code)
	}
	if code := h.do("PUT", "/api/v1/account/password", h.reader, map[string]string{"currentPassword": "reader-password", "newPassword": "something-long"}, nil); code != 200 {
		t.Fatalf("change: %d", code)
	}
	h.login("reader", "something-long")
}

func TestHealthAndVersion(t *testing.T) {
	h := newHarness(t)
	var bi server.BuildInfo
	if code := h.do("GET", "/api/version", "", nil, &bi); code != 200 || bi.Version != "test" {
		t.Fatalf("version: %d %+v", code, bi)
	}
	resp, err := http.Get(h.srv.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", err, resp)
	}
	resp.Body.Close()
	resp, _ = http.Get(h.srv.URL + "/metrics")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "cystemd_http_requests_total") {
		t.Fatal("metrics missing")
	}
}

func TestWebhookRefreshesTheApplicationsOfThePush(t *testing.T) {
	h := newHarnessWith(t, func(c *config.Config) { c.Webhook = &config.WebhookConfig{Secret: "s3cret"} })
	apps := map[string]v1.ApplicationSource{
		"web":    {RepoURL: "https://git.example.com/ops.git", Path: "apps/web", TargetRevision: "main"},
		"api":    {RepoURL: "git@git.example.com:ops", Path: "apps/api"}, // HEAD, another URL form
		"canary": {RepoURL: "https://git.example.com/ops.git", Path: "apps/web", TargetRevision: "canary"},
		"pinned": {RepoURL: "https://git.example.com/ops.git", Path: "apps/web", TargetRevision: "0123456789abcdef0123456789abcdef01234567"},
		"other":  {RepoURL: "https://git.example.com/other.git", Path: "apps/web", TargetRevision: "main"},
	}
	for name, src := range apps {
		a := demoApp(name)
		a.Spec.Source = src
		if code := h.do("POST", "/api/v1/applications", h.admin, a, nil); code != 200 {
			t.Fatalf("create %s: %d", name, code)
		}
	}
	body := []byte(`{"ref":"refs/heads/main","repository":{"clone_url":"https://git.example.com/ops.git","ssh_url":"git@git.example.com:ops.git","default_branch":"main"}}`)
	post := func(headers map[string]string, body []byte) (int, map[string]any) {
		req, _ := http.NewRequest("POST", h.srv.URL+"/api/webhook", bytes.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post(map[string]string{"X-GitHub-Event": "push"}, body); code != 401 {
		t.Fatalf("unsigned webhook: %d", code)
	}
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	code, out := post(map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": sig}, body)
	if code != 200 || fmt.Sprint(out["refreshed"]) != "[api web]" {
		t.Fatalf("webhook: %d %v", code, out)
	}
	for name := range apps {
		a, err := h.store.GetApplication(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		refreshed := a.Metadata.Annotations["cystemd.io/refresh"] == "normal"
		if refreshed != (name == "web" || name == "api") {
			t.Errorf("%s refreshed = %v", name, refreshed)
		}
	}
	// The same push as a form (GitHub's other content type).
	form := []byte("payload=" + url.QueryEscape(string(body)))
	mac = hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(form)
	if code, out := post(map[string]string{"X-GitHub-Event": "push", "X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		"Content-Type": "application/x-www-form-urlencoded"}, form); code != 200 || fmt.Sprint(out["refreshed"]) != "[api web]" {
		t.Fatalf("form webhook: %d %v", code, out)
	}
	ping := []byte(`{"zen":"hi"}`)
	mac = hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(ping)
	if code, out := post(map[string]string{"X-GitHub-Event": "ping", "X-Hub-Signature-256": "sha256=" + hex.EncodeToString(mac.Sum(nil))}, ping); code != 200 || out["message"] == nil {
		t.Fatalf("ping: %d %v", code, out)
	}
}

func TestSyncWindows(t *testing.T) {
	h := newHarness(t)
	// Fires every minute and lasts two: always active.
	proj := v1.AppProject{
		Metadata: v1.ObjectMeta{Name: "frozen"},
		Spec: v1.AppProjectSpec{
			SourceRepos:  []string{"*"},
			Destinations: []v1.ApplicationDestination{{Name: "*", Namespace: "*"}},
			SyncWindows:  v1.SyncWindows{{Kind: "deny", Schedule: "* * * * *", Duration: "2m", Applications: []string{"web*"}}},
		},
	}
	if code := h.do("POST", "/api/v1/projects", h.admin, proj, nil); code != 200 {
		t.Fatalf("create project: %d", code)
	}
	for _, name := range []string{"web", "web-preview", "api"} {
		app := demoApp(name)
		app.Spec.Project = "frozen"
		if code := h.do("POST", "/api/v1/applications", h.admin, app, nil); code != 200 {
			t.Fatalf("create %s: %d", name, code)
		}
	}

	var st v1.SyncWindowState
	if h.do("GET", "/api/v1/applications/web/syncwindows", h.admin, nil, &st); st.CanSync || st.AutoSync || len(st.ActiveWindows) != 1 {
		t.Fatalf("window state: %+v", st)
	}
	var e struct{ Message string }
	if code := h.do("POST", "/api/v1/applications/web/sync", h.admin, map[string]any{}, &e); code != 403 || e.Message != "cannot sync: blocked by sync window" {
		t.Fatalf("sync during a deny window: %d %+v", code, e)
	}
	// A dry run changes nothing; an application the window does not
	// match syncs.
	if code := h.do("POST", "/api/v1/applications/web-preview/sync", h.admin, map[string]any{"dryRun": true}, nil); code != 200 {
		t.Fatalf("dry run: %d", code)
	}
	if code := h.do("POST", "/api/v1/applications/api/sync", h.admin, map[string]any{}, nil); code != 200 {
		t.Fatalf("unmatched application: %d", code)
	}

	// manualSync lets manual syncs through; the update is conditional.
	var current v1.AppProject
	h.do("GET", "/api/v1/projects/frozen", h.admin, nil, &current)
	stale := current
	current.Spec.SyncWindows[0].ManualSync = true
	if code := h.do("PUT", "/api/v1/projects/frozen", h.admin, current, nil); code != 200 {
		t.Fatalf("update project: %d", code)
	}
	if code := h.do("PUT", "/api/v1/projects/frozen", h.admin, stale, nil); code != 409 {
		t.Fatalf("stale project update: %d", code)
	}
	if code := h.do("POST", "/api/v1/applications/web/sync", h.admin, map[string]any{}, nil); code != 200 {
		t.Fatalf("manual sync with manualSync: %d", code)
	}
}

func TestAppOfAppsProjects(t *testing.T) {
	h := newHarness(t)
	parent := demoApp("platform")
	parent.Spec.Source.Applications = &v1.ApplicationsSource{Projects: []string{"nowhere"}}
	var e struct{ Message string }
	if code := h.do("POST", "/api/v1/applications", h.admin, parent, &e); code != 400 || !strings.Contains(e.Message, `project "nowhere" does not exist`) {
		t.Fatalf("unknown project: %d %+v", code, e)
	}
	parent.Spec.Source.Applications.Projects = []string{"default"}
	parent.Spec.Source.Nomad = &v1.NomadSource{}
	if code := h.do("POST", "/api/v1/applications", h.admin, parent, &e); code != 400 || !strings.Contains(e.Message, "only one of nomad, systemd and applications") {
		t.Fatalf("two sources: %d %+v", code, e)
	}
	parent.Spec.Source.Nomad = nil
	// Whoever may not create applications in a project may not let an app
	// of apps declare them there.
	if code := h.do("POST", "/api/v1/applications", h.reader, parent, nil); code != 403 {
		t.Fatalf("reader: %d", code)
	}
	var created v1.Application
	if code := h.do("POST", "/api/v1/applications", h.admin, parent, &created); code != 200 || created.Spec.Source.Type() != v1.SourceTypeApplications {
		t.Fatalf("admin: %d %+v", code, created.Spec.Source)
	}
}
