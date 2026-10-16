// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func paymentsProject() v1.AppProject {
	return v1.AppProject{
		Metadata: v1.ObjectMeta{Name: "payments"},
		Spec: v1.AppProjectSpec{
			SourceRepos:  []string{"*"},
			Destinations: []v1.ApplicationDestination{{Name: "*", Namespace: "*"}},
			Roles: []v1.ProjectRole{{
				Name: "ci", Description: "GitLab",
				Policies: []string{
					"p, proj:payments:ci, applications, sync, payments/*, allow",
					"p, proj:payments:ci, applications, sync, payments/db, deny",
				},
				Groups: []string{"team-payments"},
			}},
		},
	}
}

func canI(h *harness, token, path string) string {
	var out map[string]string
	h.do("GET", "/api/v1/account/can-i/"+path, token, nil, &out)
	return out["value"]
}

func TestProjectRolesAPI(t *testing.T) {
	h := newHarness(t)
	bad := paymentsProject()
	bad.Spec.Roles = []v1.ProjectRole{{Name: "ci", Policies: []string{"p, proj:payments:ci, applications, sync, */*, allow"}}}
	if code := h.do("POST", "/api/v1/projects", h.admin, bad, nil); code != 400 {
		t.Fatalf("a role granting on other projects: %d", code)
	}
	proj := paymentsProject()
	if code := h.do("POST", "/api/v1/projects", h.admin, proj, nil); code != 200 {
		t.Fatalf("create project: %d", code)
	}
	for name, project := range map[string]string{"web": "payments", "db": "payments", "other": "default"} {
		a := demoApp(name)
		a.Spec.Project = project
		if code := h.do("POST", "/api/v1/applications", h.admin, a, nil); code != 200 {
			t.Fatalf("create %s: %d", name, code)
		}
	}

	// Tokens need projects/update.
	if code := h.do("POST", "/api/v1/projects/payments/roles/ci/token", h.reader, map[string]any{}, nil); code != 403 {
		t.Fatalf("reader creating a token: %d", code)
	}
	for path, code := range map[string]int{
		"/api/v1/projects/payments/roles/nope/token": 404,
		"/api/v1/projects/billing/roles/ci/token":    404,
	} {
		if got := h.do("POST", path, h.admin, map[string]any{}, nil); got != code {
			t.Errorf("%s: %d, want %d", path, got, code)
		}
	}
	var tok struct {
		Token     string `json:"token"`
		ID        string `json:"id"`
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
	}
	if code := h.do("POST", "/api/v1/projects/payments/roles/ci/token", h.admin, map[string]any{"id": "gitlab", "expiresIn": 3600}, &tok); code != 200 || tok.Token == "" || tok.ID != "gitlab" || tok.ExpiresAt-tok.IssuedAt != 3600 {
		t.Fatalf("create token: %d %+v", code, tok)
	}

	// The token is the role: it syncs the project's applications but db,
	// and reads what the default role lets everyone read.
	var info struct {
		Username string   `json:"username"`
		Provider string   `json:"provider"`
		Roles    []string `json:"roles"`
	}
	if code := h.do("GET", "/api/v1/session/userinfo", tok.Token, nil, &info); code != 200 || info.Username != "proj:payments:ci" || info.Provider != "project" || !slices.Contains(info.Roles, "proj:payments:ci") {
		t.Fatalf("userinfo: %d %+v", code, info)
	}
	if code := h.do("POST", "/api/v1/applications/web/sync", tok.Token, map[string]any{}, nil); code != 200 {
		t.Fatalf("token syncing web: %d", code)
	}
	for _, app := range []string{"db", "other"} {
		if code := h.do("POST", "/api/v1/applications/"+app+"/sync", tok.Token, map[string]any{}, nil); code != 403 {
			t.Fatalf("token syncing %s: %d", app, code)
		}
	}
	if canI(h, tok.Token, "applications/get/default/other") != "yes" {
		t.Fatal("the default role applies to role tokens")
	}
	if code := h.do("POST", "/api/v1/projects/payments/roles/ci/token", tok.Token, map[string]any{}, nil); code != 403 {
		t.Fatalf("a role token creating a token: %d", code)
	}
	var got v1.AppProject
	if h.do("GET", "/api/v1/projects/payments", h.admin, nil, &got); len(got.RoleTokens("ci")) != 1 || got.RoleTokens("ci")[0].ID != "gitlab" {
		t.Fatalf("project status: %+v", got.Status)
	}

	// A member of the role's group has it too.
	st := h.store
	ctx := context.Background()
	hash, _ := auth.HashPassword("carol-password")
	if err := st.PutAccount(ctx, &store.Account{Name: "carol", PasswordHash: hash, Enabled: true, Capabilities: []string{"login"}, Groups: []string{"team-payments"}}); err != nil {
		t.Fatal(err)
	}
	carol := h.login("carol", "carol-password")
	if canI(h, carol, "applications/sync/payments/web") != "yes" || canI(h, carol, "applications/sync/payments/db") != "no" || canI(h, h.reader, "applications/sync/payments/web") != "no" {
		t.Fatal("group membership")
	}

	// Re-applying the project keeps its tokens; dropping the role ends
	// both its tokens and what its groups had.
	got.Spec.Description = "re-applied"
	got.Status = v1.AppProjectStatus{}
	if code := h.do("PUT", "/api/v1/projects/payments", h.admin, got, &got); code != 200 || len(got.RoleTokens("ci")) != 1 {
		t.Fatalf("update: %d %+v", code, got.Status)
	}
	if code := h.do("GET", "/api/v1/session/userinfo", tok.Token, nil, nil); code != 200 {
		t.Fatalf("token after an update: %d", code)
	}
	got.Spec.Roles = nil
	if code := h.do("PUT", "/api/v1/projects/payments", h.admin, got, nil); code != 200 {
		t.Fatalf("update: %d", code)
	}
	if code := h.do("GET", "/api/v1/session/userinfo", tok.Token, nil, nil); code != 401 {
		t.Fatalf("token of a removed role: %d", code)
	}
	if canI(h, carol, "applications/sync/payments/web") != "no" {
		t.Fatal("group of a removed role")
	}
}

func TestProjectRoleTokenRevocation(t *testing.T) {
	h := newHarness(t)
	if code := h.do("POST", "/api/v1/projects", h.admin, paymentsProject(), nil); code != 200 {
		t.Fatalf("create project: %d", code)
	}
	var tok struct {
		Token    string `json:"token"`
		IssuedAt int64  `json:"iat"`
	}
	if code := h.do("POST", "/api/v1/projects/payments/roles/ci/token", h.admin, map[string]any{}, &tok); code != 200 {
		t.Fatalf("create token: %d", code)
	}
	if code := h.do("GET", "/api/v1/applications", tok.Token, nil, nil); code != 200 {
		t.Fatalf("token: %d", code)
	}
	// Argo CD names tokens by their issue time.
	iat := strconv.FormatInt(tok.IssuedAt, 10)
	if code := h.do("DELETE", "/api/v1/projects/payments/roles/ci/token/"+iat, h.reader, nil, nil); code != 403 {
		t.Fatalf("reader deleting: %d", code)
	}
	if code := h.do("DELETE", "/api/v1/projects/payments/roles/ci/token/"+iat, h.admin, nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code := h.do("GET", "/api/v1/applications", tok.Token, nil, nil); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}
	if code := h.do("DELETE", "/api/v1/projects/payments/roles/ci/token/"+iat, h.admin, nil, nil); code != 404 {
		t.Fatalf("delete twice: %d", code)
	}
}

func TestWebProjectRoles(t *testing.T) {
	h := newHarness(t)
	proj := paymentsProject()
	proj.Spec.Roles = nil
	if code := h.do("POST", "/api/v1/projects", h.admin, proj, nil); code != 200 {
		t.Fatalf("create project: %d", code)
	}
	_, body := h.web("GET", "/ui/settings/projects/payments", h.admin, nil, false, "")
	mustContain(t, body, "No roles.", "Add role")
	_, body = h.web("GET", "/ui/settings/projects/payments?panel=new-role", h.admin, nil, true, "panel")
	mustContain(t, body, "Add role to payments", `name="name"`)

	resp, body := h.web("POST", "/ui/settings/projects/payments/roles", h.admin, url.Values{"name": {"-bad"}}, true, "panel")
	if resp.StatusCode != 400 {
		t.Fatalf("bad name: %d", resp.StatusCode)
	}
	mustContain(t, body, "role name")
	resp, _ = h.web("POST", "/ui/settings/projects/payments/roles", h.admin, url.Values{"name": {"ci"}, "description": {"GitLab"}}, true, "panel")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("HX-Location"), "panel=role") {
		t.Fatalf("create role: %d %v", resp.StatusCode, resp.Header)
	}
	_, body = h.web("GET", "/ui/settings/projects/payments", h.admin, nil, false, "")
	mustContain(t, body, `href="/ui/settings/projects/payments?panel=role&amp;role=ci"`, "GitLab")

	op := func(vals url.Values, want ...string) string {
		t.Helper()
		resp, body := h.web("POST", "/ui/settings/projects/payments/roles/ci", h.admin, vals, true, "panel")
		if resp.StatusCode != 200 {
			t.Fatalf("%v: %d %s", vals, resp.StatusCode, body)
		}
		mustContain(t, body, want...)
		return body
	}
	op(url.Values{"op": {"add-policy"}, "resource": {"applications"}, "action": {"sync"}, "object": {"*"}, "permission": {"allow"}}, "payments/*", "Policy added")
	op(url.Values{"op": {"add-group"}, "group": {"team-payments"}}, "team-payments")
	body = op(url.Values{"op": {"create-token"}, "expiresIn": {"30d"}, "id": {"ui"}}, "Token ui created", "data-copy=")
	if !strings.Contains(body, "in 30 days") {
		t.Fatalf("expiry not shown:\n%s", body)
	}
	_, body = h.web("GET", "/ui/settings/projects/payments?panel=role&role=ci", h.admin, nil, true, "panel")
	mustContain(t, body, "proj:payments:ci", ">ui<")
	if strings.Contains(body, "data-copy=") {
		t.Fatal("a token is shown once")
	}
	resp, body = h.web("POST", "/ui/settings/projects/payments/roles/ci", h.admin, url.Values{"op": {"create-token"}, "expiresIn": {"soon"}}, true, "panel")
	if resp.StatusCode != 400 || !strings.Contains(body, "is not a lifetime") {
		t.Fatalf("bad lifetime: %d", resp.StatusCode)
	}

	// The reader sees the role, without controls, and cannot change it.
	_, body = h.web("GET", "/ui/settings/projects/payments?panel=role&role=ci", h.reader, nil, true, "panel")
	if strings.Contains(body, "Add policy") || strings.Contains(body, "Create token") {
		t.Fatal("reader sees controls")
	}
	if resp, _ := h.web("POST", "/ui/settings/projects/payments/roles/ci", h.reader, url.Values{"op": {"add-group"}, "group": {"x"}}, true, "panel"); resp.StatusCode != 403 {
		t.Fatalf("reader editing: %d", resp.StatusCode)
	}

	op(url.Values{"op": {"delete-token"}, "token": {"ui"}}, "No tokens.", "Token ui deleted.")
	op(url.Values{"op": {"remove-policy"}, "policy": {"p, proj:payments:ci, applications, sync, payments/*, allow"}}, "No policies")
	op(url.Values{"op": {"remove-group"}, "group": {"team-payments"}}, "No groups")
	resp, _ = h.web("POST", "/ui/settings/projects/payments/roles/ci", h.admin, url.Values{"op": {"delete-role"}}, true, "panel")
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("HX-Location"), "/ui/settings/projects/payments") {
		t.Fatalf("delete role: %d %v", resp.StatusCode, resp.Header)
	}
	_, body = h.web("GET", "/ui/settings/projects/payments?panel=role&role=ci", h.admin, nil, false, "")
	mustContain(t, body, `project payments has no role &#34;ci&#34;`)
}
