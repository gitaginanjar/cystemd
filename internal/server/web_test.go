// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// web performs a UI request. hx sets the htmx headers (HX-Target when
// target is not empty); form is sent URL-encoded.
func (h *harness) web(method, path, token string, form url.Values, hx bool, target string) (*http.Response, string) {
	h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, h.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if hx {
		req.Header.Set("HX-Request", "true")
		if target != "" {
			req.Header.Set("HX-Target", target)
		}
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func mustContain(t *testing.T, body string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(body, w) {
			t.Fatalf("response does not contain %q:\n%s", w, abbreviate(body))
		}
	}
}

func mustNotContain(t *testing.T, body string, unwanted ...string) {
	t.Helper()
	for _, w := range unwanted {
		if strings.Contains(body, w) {
			t.Fatalf("response contains %q:\n%s", w, abbreviate(body))
		}
	}
}

func abbreviate(s string) string {
	if len(s) > 3000 {
		return s[:3000] + "..."
	}
	return s
}

func TestWebLoginFlow(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.web("GET", "/ui/applications?view=list", "", nil, false, "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/login?return_url=%2Fui%2Fapplications%3Fview%3Dlist" {
		t.Fatalf("unauthenticated page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := h.web("GET", "/ui/login?return_url=/ui/settings", "", nil, false, "")
	if resp.StatusCode != 200 {
		t.Fatalf("login page: %d", resp.StatusCode)
	}
	mustContain(t, body, "Sign in", `name="return_url" value="/ui/settings"`, "htmx-2.0.11.min.js")
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "form-action 'none'") {
		t.Fatalf("CSP = %q", csp)
	}

	// Writes must come from htmx (CSRF).
	form := url.Values{"username": {"reader"}, "password": {"reader-password"}, "return_url": {"/ui/settings"}}
	if resp, _ := h.web("POST", "/ui/login", "", form, false, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("non-htmx login: %d", resp.StatusCode)
	}
	bad := url.Values{"username": {"reader"}, "password": {"wrong"}}
	resp, body = h.web("POST", "/ui/login", "", bad, true, "login-error")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", resp.StatusCode)
	}
	mustContain(t, body, "Invalid username, password or token.")

	resp, _ = h.web("POST", "/ui/login", "", form, true, "login-error")
	if resp.StatusCode != 200 || resp.Header.Get("HX-Redirect") != "/ui/settings" {
		t.Fatalf("login: %d redirect %q", resp.StatusCode, resp.Header.Get("HX-Redirect"))
	}
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == "cystemd.token" {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly {
		t.Fatalf("session cookie: %+v", resp.Cookies())
	}

	// An open redirect is refused.
	evil := url.Values{"username": {"reader"}, "password": {"reader-password"}, "return_url": {"//evil.example.com/"}}
	resp, _ = h.web("POST", "/ui/login", "", evil, true, "login-error")
	if resp.Header.Get("HX-Redirect") != "/ui/applications" {
		t.Fatalf("open redirect: %q", resp.Header.Get("HX-Redirect"))
	}

	// The cookie works for pages, and logout clears it.
	jar, _ := cookiejar.New(nil)
	u, _ := url.Parse(h.srv.URL)
	jar.SetCookies(u, []*http.Cookie{cookie})
	client := &http.Client{Jar: jar}
	r2, err := client.Get(h.srv.URL + "/ui/settings")
	if err != nil || r2.StatusCode != 200 {
		t.Fatalf("settings with cookie: %v %v", err, r2)
	}
	r2.Body.Close()
}

func TestWebCSRFChecks(t *testing.T) {
	h := newHarness(t)
	for name, hdr := range map[string][2]string{
		"cross-site fetch": {"Sec-Fetch-Site", "cross-site"},
		"foreign origin":   {"Origin", "https://evil.example.com"},
	} {
		req, _ := http.NewRequest("POST", h.srv.URL+"/ui/logout", nil)
		req.Header.Set("HX-Request", "true")
		req.Header.Set(hdr[0], hdr[1])
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s: %d", name, resp.StatusCode)
		}
	}
}

func TestWebApplicationsPage(t *testing.T) {
	h := newHarness(t)
	_, body := h.web("GET", "/ui/applications", h.admin, nil, false, "")
	mustContain(t, body, "No applications available to you just yet", "New app", `sse-connect="/ui/stream/applications"`)

	web := demoApp("web")
	web.Metadata.Labels = map[string]string{"team": "payments"}
	if code := h.do("POST", "/api/v1/applications", h.admin, web, nil); code != 200 {
		t.Fatalf("create: %d", code)
	}
	if code := h.do("POST", "/api/v1/applications", h.admin, demoApp("api"), nil); code != 200 {
		t.Fatalf("create: %d", code)
	}
	_, body = h.web("GET", "/ui/applications", h.admin, nil, false, "")
	mustContain(t, body, `href="/ui/applications/web"`, `href="/ui/applications/api"`, "team=payments", "2 applications")

	// Fragment: only the live content, no document.
	resp, body := h.web("GET", "/ui/applications?search=we", h.admin, nil, true, "apps-content")
	if resp.StatusCode != 200 {
		t.Fatalf("fragment: %d", resp.StatusCode)
	}
	mustContain(t, body, `id="apps-content"`, "Showing 1 of 2 applications", `href="/ui/applications/web"`)
	mustNotContain(t, body, "<html", `href="/ui/applications/api"`)

	// Filters: label selector, sync status, list and summary views.
	_, body = h.web("GET", "/ui/applications?labels=team%3Dpayments&view=list", h.admin, nil, true, "apps-content")
	mustContain(t, body, "apps-table", ">web</a>")
	mustNotContain(t, body, ">api</a>")
	_, body = h.web("GET", "/ui/applications?sync=Synced", h.admin, nil, true, "apps-content")
	mustContain(t, body, "No matching applications")
	_, body = h.web("GET", "/ui/applications?view=summary", h.admin, nil, true, "apps-content")
	mustContain(t, body, "donut", "Auto sync")

	// A live refresh re-renders the page the browser shows.
	req, _ := http.NewRequest("GET", h.srv.URL+"/ui/applications?_live=1", nil)
	req.Header.Set("Authorization", "Bearer "+h.admin)
	req.Header.Set("HX-Request", "true")
	req.Header.Set("HX-Target", "apps-content")
	req.Header.Set("HX-Current-URL", h.srv.URL+"/ui/applications?search=api")
	r2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r2.Body)
	r2.Body.Close()
	mustContain(t, string(b), `href="/ui/applications/api"`)
	mustNotContain(t, string(b), `href="/ui/applications/web"`)

	// Panels on the list page.
	_, body = h.web("GET", "/ui/applications?panel=new", h.admin, nil, true, "panel")
	mustContain(t, body, "Create application", `name="repoURL"`, "Edit as YAML")
	_, body = h.web("GET", "/ui/applications?panel=sync&app=web", h.admin, nil, true, "panel")
	mustContain(t, body, "Synchronize web", `hx-post="/ui/applications/web/sync"`)
	_, body = h.web("GET", "/ui/applications?panel=sync-apps", h.admin, nil, true, "panel")
	mustContain(t, body, "Sync applications", `value="api"`, `value="web"`)

	// The read-only role sees no create button.
	_, body = h.web("GET", "/ui/applications", h.reader, nil, false, "")
	mustNotContain(t, body, "New app")
}

func TestWebApplicationDetailsAndPanels(t *testing.T) {
	h := newHarness(t)
	if code := h.do("POST", "/api/v1/applications", h.admin, demoApp("web"), nil); code != 200 {
		t.Fatalf("create: %d", code)
	}
	resp, body := h.web("GET", "/ui/applications/web", h.admin, nil, false, "")
	if resp.StatusCode != 200 {
		t.Fatalf("details: %d", resp.StatusCode)
	}
	mustContain(t, body, "App health", "Sync status", `id="app-resources"`, "tree__canvas", "panel=details",
		`sse-connect="/ui/stream/applications/web"`, "History and rollback")
	// With a managed job the tree needs Nomad, unreachable here: the page
	// still renders, with the error.
	if _, err := h.store.MutateApplication(context.Background(), "web", func(a *v1.Application) error {
		a.Status.Resources = []v1.ResourceStatus{{Kind: v1.KindJob, Namespace: "default", Name: "web", Status: v1.SyncStatusSynced}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, body = h.web("GET", "/ui/applications/web", h.admin, nil, true, "app-resources")
	mustContain(t, body, "Unable to load the live resource tree", "node=missing%3AJob%2Fdefault%2Fweb", "tree-node--Unknown")

	panels := map[string]string{
		"details":                "Summary",
		"details&tab=parameters": "Nomad source",
		"details&tab=manifest":   `<span class="y-key">kind</span>: <span class="y-str">Application</span>`,
		"details&tab=events":     "created application",
		"sync":                   "Synchronize web",
		"status":                 "No sync yet",
		"history":                "No deployments yet",
		"delete":                 "Type the application name to confirm",
	}
	for q, want := range panels {
		resp, body := h.web("GET", "/ui/applications/web?panel="+q, h.admin, nil, true, "panel")
		if resp.StatusCode != 200 {
			t.Fatalf("panel %s: %d", q, resp.StatusCode)
		}
		mustContain(t, body, want, "sliding-panel")
		mustNotContain(t, body, "<html")
	}
	_, body = h.web("GET", "/ui/applications/web?panel=details&edit=1", h.admin, nil, true, "panel")
	mustContain(t, body, `hx-post="/ui/applications/web/spec"`, `name="repoURL" value="https://git.example.com/ops.git"`)

	// Fragments of the live regions.
	_, body = h.web("GET", "/ui/applications/web", h.admin, nil, true, "app-status")
	mustContain(t, body, `id="app-status"`)
	mustNotContain(t, body, "tree__canvas")

	// Unknown applications and pages.
	if resp, _ := h.web("GET", "/ui/applications/nope", h.admin, nil, false, ""); resp.StatusCode != 404 {
		t.Fatalf("unknown app: %d", resp.StatusCode)
	}
	if resp, _ := h.web("GET", "/ui/nothing-here", h.admin, nil, false, ""); resp.StatusCode != 404 {
		t.Fatalf("unknown page: %d", resp.StatusCode)
	}

	// The read-only role sees no write controls and cannot write.
	_, body = h.web("GET", "/ui/applications/web", h.reader, nil, false, "")
	mustNotContain(t, body, "panel=delete", "panel=sync\"")
	resp, body = h.web("POST", "/ui/applications/web/delete", h.reader, url.Values{"confirm": {"web"}}, true, "panel")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("reader delete: %d", resp.StatusCode)
	}
	mustContain(t, body, "permission denied")
}

func TestWebApplicationWrites(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// Create from the form.
	form := url.Values{
		"name": {"shop"}, "project": {"default"}, "labels": {"team=web\nenv=prod"},
		"repoURL": {"https://git.example.com/ops.git"}, "targetRevision": {"main"}, "path": {"apps/shop"},
		"destName": {v1.InClusterName}, "destNamespace": {"shop"}, "syncPolicy": {"automated"}, "prune": {"true"},
		"syncOption": {v1.SyncOptionCreateNamespace}, "sourceType": {"Nomad"}, "nomadVars": {"image=1.2.3"},
		"close": {"/ui/applications"},
	}
	resp, body := h.web("POST", "/ui/applications", h.admin, form, true, "panel")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("HX-Location"), "/ui/applications/shop") {
		t.Fatalf("create: %d %q\n%s", resp.StatusCode, resp.Header.Get("HX-Location"), abbreviate(body))
	}
	app, err := h.store.GetApplication(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if app.Metadata.Labels["env"] != "prod" || !app.Spec.SyncPolicy.IsAutomated() || !app.Spec.SyncPolicy.Automated.Prune ||
		!app.Spec.SyncPolicy.HasOption(v1.SyncOptionCreateNamespace) || app.Spec.Source.Nomad.Vars["image"] != "1.2.3" ||
		app.Spec.Destination.Name != "" || app.Spec.Destination.Namespace != "shop" {
		t.Fatalf("created app: %+v", app)
	}

	// A bad form re-renders the panel with the error.
	bad := url.Values{"name": {"Bad Name"}, "repoURL": {"x"}, "sourceType": {"Nomad"}}
	resp, body = h.web("POST", "/ui/applications", h.admin, bad, true, "panel")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad create: %d", resp.StatusCode)
	}
	mustContain(t, body, "invalid name", "Create application")

	// Create from YAML.
	yamlForm := url.Values{"mode": {"yaml"}, "yaml": {"apiVersion: cystemd.io/v1alpha1\nkind: Application\nmetadata:\n  name: from-yaml\nspec:\n  project: default\n  source:\n    repoURL: https://git.example.com/ops.git\n  destination: {}\n"}}
	if resp, body := h.web("POST", "/ui/applications", h.admin, yamlForm, true, "panel"); resp.StatusCode != 200 {
		t.Fatalf("yaml create: %d\n%s", resp.StatusCode, abbreviate(body))
	}
	typo := url.Values{"mode": {"yaml"}, "yaml": {"metadata:\n  name: x\nspec:\n  projct: default\n"}}
	resp, body = h.web("POST", "/ui/applications", h.admin, typo, true, "panel")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("yaml typo: %d", resp.StatusCode)
	}
	mustContain(t, body, "projct")

	// Switching the new-app panel between form and YAML keeps the values.
	_, body = h.web("POST", "/ui/applications/new/yaml", h.admin, form, true, "panel")
	mustContain(t, body, "name: shop", "image: 1.2.3", "Edit as form")

	// Sync from the panel: a full sync with options.
	syncForm := url.Values{"revision": {"main"}, "prune": {"true"}, "syncOption": {v1.SyncOptionPruneLast}, "close": {"/ui/applications/shop"}}
	resp, body = h.web("POST", "/ui/applications/shop/sync", h.admin, syncForm, true, "panel")
	if resp.StatusCode != 200 || resp.Header.Get("HX-Push-Url") != "/ui/applications/shop" {
		t.Fatalf("sync: %d %q\n%s", resp.StatusCode, resp.Header.Get("HX-Push-Url"), abbreviate(body))
	}
	mustContain(t, body, "Sync of shop requested", `hx-swap-oob="beforeend"`)
	app, _ = h.store.GetApplication(ctx, "shop")
	if app.Operation == nil || !app.Operation.Sync.Prune || !app.Operation.Sync.HasOption(v1.SyncOptionPruneLast) || app.Operation.Sync.Revision != "" {
		t.Fatalf("operation: %+v", app.Operation)
	}
	// A second sync conflicts and keeps the panel open with the error.
	resp, body = h.web("POST", "/ui/applications/shop/sync", h.admin, syncForm, true, "panel")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second sync: %d", resp.StatusCode)
	}
	mustContain(t, body, "another operation is already in progress")

	// Terminate the requested operation, refresh, toggle auto-sync.
	if resp, _ := h.web("POST", "/ui/applications/shop/terminate", h.admin, url.Values{}, true, ""); resp.StatusCode != 200 {
		t.Fatalf("terminate: %d", resp.StatusCode)
	}
	if resp, _ := h.web("POST", "/ui/applications/shop/refresh?mode=hard", h.admin, url.Values{}, true, ""); resp.StatusCode != 200 {
		t.Fatalf("refresh: %d", resp.StatusCode)
	}
	app, _ = h.store.GetApplication(ctx, "shop")
	if app.Operation != nil || app.Metadata.Annotations["cystemd.io/refresh"] != "hard" {
		t.Fatalf("after terminate/refresh: op=%+v annotations=%v", app.Operation, app.Metadata.Annotations)
	}
	_, body = h.web("POST", "/ui/applications/shop/autosync?enable=false", h.admin, url.Values{}, true, "panel")
	mustContain(t, body, "Auto-sync disabled for shop")
	app, _ = h.store.GetApplication(ctx, "shop")
	if app.Spec.SyncPolicy.IsAutomated() || !app.Spec.SyncPolicy.HasOption(v1.SyncOptionCreateNamespace) {
		t.Fatalf("after disabling auto-sync: %+v", app.Spec.SyncPolicy)
	}

	// Edit the spec from the form: fields the form does not show survive.
	limit := int64(3)
	if _, err := h.store.MutateApplication(ctx, "shop", func(a *v1.Application) error { a.Spec.RevisionHistoryLimit = &limit; return nil }); err != nil {
		t.Fatal(err)
	}
	edit := url.Values{"name": {"shop"}, "project": {"default"}, "repoURL": {"https://git.example.com/ops.git"}, "targetRevision": {"v2"},
		"path": {"apps/shop"}, "destName": {v1.InClusterName}, "syncPolicy": {"manual"}, "sourceType": {"Nomad"}}
	resp, body = h.web("POST", "/ui/applications/shop/spec", h.admin, edit, true, "panel")
	if resp.StatusCode != 200 {
		t.Fatalf("spec edit: %d\n%s", resp.StatusCode, abbreviate(body))
	}
	app, _ = h.store.GetApplication(ctx, "shop")
	if app.Spec.Source.TargetRevision != "v2" || app.Spec.RevisionHistoryLimit == nil || *app.Spec.RevisionHistoryLimit != 3 {
		t.Fatalf("after spec edit: %+v", app.Spec)
	}

	// Favorites live in a cookie.
	resp, body = h.web("POST", "/ui/applications/shop/favorite", h.admin, url.Values{}, true, "")
	mustContain(t, body, "star--on")
	found := false
	for _, c := range resp.Cookies() {
		found = found || (c.Name == "cystemd.favorites" && strings.Contains(c.Value, "shop"))
	}
	if !found {
		t.Fatalf("favorites cookie not set: %v", resp.Cookies())
	}

	// Delete needs the name typed; then it cascades.
	resp, body = h.web("POST", "/ui/applications/shop/delete", h.admin, url.Values{"confirm": {"nope"}, "close": {"/ui/applications/shop"}}, true, "panel")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("delete without confirmation: %d", resp.StatusCode)
	}
	mustContain(t, body, "does not match")
	resp, _ = h.web("POST", "/ui/applications/shop/delete", h.admin, url.Values{"confirm": {"shop"}, "cascade": {"true"}, "close": {"/ui/applications/shop"}}, true, "panel")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("HX-Location"), `"/ui/applications"`) {
		t.Fatalf("delete: %d %q", resp.StatusCode, resp.Header.Get("HX-Location"))
	}
	app, _ = h.store.GetApplication(ctx, "shop")
	if app.Metadata.DeletionTimestamp == nil || !app.Metadata.HasFinalizer(v1.ResourcesFinalizer) {
		t.Fatalf("after delete: %+v", app.Metadata)
	}
}

func TestWebSettingsPages(t *testing.T) {
	h := newHarness(t)
	pages := map[string]string{
		"/ui/settings":                  "Repositories",
		"/ui/settings/repos":            "No repositories connected",
		"/ui/settings/projects":         "default",
		"/ui/settings/clusters":         "in-cluster",
		"/ui/settings/accounts":         "reader",
		"/ui/settings/appearance":       "Follow the system",
		"/ui/user-info":                 "admin",
		"/ui/settings/projects/default": "Source repositories",
	}
	for path, want := range pages {
		resp, body := h.web("GET", path, h.admin, nil, false, "")
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		mustContain(t, body, want)
	}
	// The read-only role cannot list accounts: the page says so.
	_, body := h.web("GET", "/ui/settings/accounts", h.reader, nil, false, "")
	mustContain(t, body, "permission denied")

	// Projects: create, update, delete.
	form := url.Values{"name": {"payments"}, "description": {"Payment services"}, "sourceRepos": {"https://git.example.com/*"}, "destinations": {"in-cluster/payments"}}
	if resp, body := h.web("POST", "/ui/settings/projects", h.admin, form, true, "panel"); resp.StatusCode != 200 {
		t.Fatalf("create project: %d\n%s", resp.StatusCode, abbreviate(body))
	}
	p, err := h.store.GetProject(context.Background(), "payments")
	if err != nil || len(p.Spec.Destinations) != 1 || p.Spec.Destinations[0].Namespace != "payments" {
		t.Fatalf("project: %+v %v", p, err)
	}
	bad := url.Values{"name": {"x"}, "destinations": {"just-a-cluster"}}
	if resp, body := h.web("POST", "/ui/settings/projects/x", h.admin, bad, true, "panel"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad project: %d\n%s", resp.StatusCode, abbreviate(body))
	}
	if resp, _ := h.web("POST", "/ui/settings/projects/payments/delete", h.admin, url.Values{}, true, ""); resp.StatusCode != 200 {
		t.Fatalf("delete project: %d", resp.StatusCode)
	}

	// Theme preference.
	resp, _ := h.web("POST", "/ui/settings/appearance", h.admin, url.Values{"theme": {"dark"}}, true, "")
	if resp.Header.Get("HX-Refresh") != "true" {
		t.Fatalf("appearance: %d", resp.StatusCode)
	}
}

func TestWebStaticAssets(t *testing.T) {
	h := newHarness(t)
	resp, body := h.web("GET", "/ui/static/css/cystemd.css?v=abc", "", nil, false, "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/css") ||
		!strings.Contains(resp.Header.Get("Cache-Control"), "immutable") || !strings.Contains(body, "--healthy") {
		t.Fatalf("css: %d %v", resp.StatusCode, resp.Header)
	}
	if resp, _ := h.web("GET", "/ui/static/vendor/htmx-2.0.11.min.js", "", nil, false, ""); resp.StatusCode != 200 {
		t.Fatalf("htmx: %d", resp.StatusCode)
	}
	if resp, _ := h.web("GET", "/ui/static/vendor/SHA256SUMS", "", nil, false, ""); resp.StatusCode != 404 {
		t.Fatalf("checksums are not served: %d", resp.StatusCode)
	}
	if resp, _ := h.web("GET", "/ui/static/", "", nil, false, ""); resp.StatusCode != 404 {
		t.Fatalf("no directory listing: %d", resp.StatusCode)
	}
	if resp, _ := h.web("GET", "/", "", nil, false, ""); resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "/ui/" {
		t.Fatalf("root redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestWebSyncWindows(t *testing.T) {
	h := newHarness(t)
	proj := v1.AppProject{
		Metadata: v1.ObjectMeta{Name: "frozen"},
		Spec: v1.AppProjectSpec{
			Description:  "Change freeze",
			SourceRepos:  []string{"*"},
			Destinations: []v1.ApplicationDestination{{Name: "*", Namespace: "*"}},
			SyncWindows: v1.SyncWindows{
				{Kind: "deny", Schedule: "* * * * *", Duration: "2m", Applications: []string{"web"}, Description: "freeze"},
				{Kind: "allow", Schedule: "0 2 * * *", Duration: "1h", Namespaces: []string{"batch"}, ManualSync: true, TimeZone: "Asia/Jakarta"},
			},
			OrphanedResources: &v1.OrphanedResourcesMonitorSettings{},
		},
	}
	if code := h.do("POST", "/api/v1/projects", h.admin, proj, nil); code != 200 {
		t.Fatalf("create project: %d", code)
	}
	app := demoApp("web")
	app.Spec.Project = "frozen"
	if code := h.do("POST", "/api/v1/applications", h.admin, app, nil); code != 200 {
		t.Fatalf("create app: %d", code)
	}

	// The application's status panel says syncs are denied, and why.
	_, body := h.web("GET", "/ui/applications/web", h.admin, nil, false, "")
	mustContain(t, body, "Sync window", ">Denied", "Active: deny * * * * * for 2m")

	// The project page lists the windows.
	_, body = h.web("GET", "/ui/settings/projects/frozen", h.admin, nil, false, "")
	mustContain(t, body, "Sync windows", "<td>deny</td>", "<b>Active</b>", "Asia/Jakarta", ">Inactive<", "Orphaned resources", ">Monitored<")

	// Editing the summary keeps the windows.
	form := url.Values{"description": {"Freeze for the release"}, "sourceRepos": {"*"}, "destinations": {"*/*"}}
	if resp, body := h.web("POST", "/ui/settings/projects/frozen", h.admin, form, true, ""); resp.StatusCode != 200 {
		t.Fatalf("update: %s\n%s", resp.Status, abbreviate(body))
	}
	var got v1.AppProject
	h.do("GET", "/api/v1/projects/frozen", h.admin, nil, &got)
	if got.Spec.Description != "Freeze for the release" || len(got.Spec.SyncWindows) != 2 || got.Spec.OrphanedResources == nil {
		t.Fatalf("project after the form: %+v", got.Spec)
	}
}

func TestWebAppOfApps(t *testing.T) {
	h := newHarness(t)
	child := demoApp("web")
	if code := h.do("POST", "/api/v1/applications", h.admin, child, nil); code != 200 {
		t.Fatalf("child: %d", code)
	}
	parent := demoApp("platform")
	parent.Spec.Source.Applications = &v1.ApplicationsSource{}
	if code := h.do("POST", "/api/v1/applications", h.admin, parent, nil); code != 200 {
		t.Fatalf("parent: %d", code)
	}
	if _, err := h.store.MutateApplication(context.Background(), "platform", func(a *v1.Application) error {
		a.Status.Resources = []v1.ResourceStatus{{Kind: v1.KindApplication, Name: "web", Status: v1.SyncStatusSynced,
			Health: &v1.HealthStatus{Status: v1.HealthStatusHealthy}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, body := h.web("GET", "/ui/applications/platform", h.admin, nil, false, "")
	mustContain(t, body, `aria-label="Application web"`, "node=application%3Aweb")
	_, body = h.web("GET", "/ui/applications/platform?node=application%3Aweb", h.admin, nil, true, "panel")
	mustContain(t, body, "Open application", `href="/ui/applications/web"`, "Desired manifest", "Sync Application web only?")
}
