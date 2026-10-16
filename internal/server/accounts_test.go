// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/config"
)

func accountsHarness(t *testing.T) *harness {
	yes := true
	return newHarnessWith(t, func(c *config.Config) {
		c.Auth.Accounts = []config.AccountConfig{
			{Name: "ci", Capabilities: []string{"apiKey"}, Enabled: &yes},
			{Name: "bob", Capabilities: []string{"login", "apiKey"}, Enabled: &yes},
		}
		c.RBAC.Policy = "p, ci, applications, sync, default/*, allow\n"
	})
}

type tokenOut struct {
	Token     string     `json:"token"`
	ID        string     `json:"id"`
	IssuedAt  time.Time  `json:"issuedAt"`
	ExpiresAt *time.Time `json:"expiresAt"`
}

type accountOut struct {
	Name         string     `json:"name"`
	Capabilities []string   `json:"capabilities"`
	Tokens       []tokenOut `json:"tokens"`
}

func TestAPITokensAPI(t *testing.T) {
	h := accountsHarness(t)

	var list struct {
		Items []accountOut `json:"items"`
	}
	if code := h.do("GET", "/api/v1/accounts", h.admin, nil, &list); code != 200 || len(list.Items) != 4 {
		t.Fatalf("list: %d %+v", code, list)
	}

	// The admin generates a token for ci.
	var tok tokenOut
	if code := h.do("POST", "/api/v1/accounts/ci/token", h.admin, map[string]any{"id": "pipeline", "expiresIn": 3600}, &tok); code != 200 || tok.Token == "" || tok.ID != "pipeline" || tok.ExpiresAt == nil {
		t.Fatalf("generate: %d %+v", code, tok)
	}
	var acct accountOut
	if code := h.do("GET", "/api/v1/accounts/ci", h.admin, nil, &acct); code != 200 || len(acct.Tokens) != 1 || acct.Tokens[0].ID != "pipeline" || acct.Tokens[0].Token != "" {
		t.Fatalf("get: %d %+v", code, acct)
	}

	// The token is ci: readonly plus syncing in the default project.
	var info struct {
		Username string `json:"username"`
		Provider string `json:"provider"`
	}
	if code := h.do("GET", "/api/v1/session/userinfo", tok.Token, nil, &info); code != 200 || info.Username != "ci" || info.Provider != "local" {
		t.Fatalf("userinfo: %d %+v", code, info)
	}
	if code := h.do("GET", "/api/v1/applications", tok.Token, nil, nil); code != 200 {
		t.Fatalf("list applications: %d", code)
	}
	for path, want := range map[string]string{
		"/api/v1/account/can-i/applications/sync/default/guestbook":  "yes",
		"/api/v1/account/can-i/applications/sync/platform/guestbook": "no",
		"/api/v1/account/can-i/applications/get/platform/guestbook":  "yes",
		"/api/v1/account/can-i/projects/update/default":              "no",
	} {
		var out map[string]string
		if code := h.do("GET", path, tok.Token, nil, &out); code != 200 || out["value"] != want {
			t.Errorf("%s: %d %v", path, code, out)
		}
	}
	var out map[string]string
	if h.do("GET", "/api/v1/account/can-i/projects/update/default", h.admin, nil, &out); out["value"] != "yes" {
		t.Errorf("admin can-i: %v", out)
	}

	// A token reads its own account, but neither mints tokens nor revokes
	// them, nor changes passwords.
	if code := h.do("GET", "/api/v1/accounts/ci", tok.Token, nil, nil); code != 200 {
		t.Fatalf("own account with a token: %d", code)
	}
	if code := h.do("GET", "/api/v1/accounts/bob", tok.Token, nil, nil); code != 403 {
		t.Fatalf("another account with a token: %d", code)
	}
	if code := h.do("POST", "/api/v1/accounts/ci/token", tok.Token, map[string]any{}, nil); code != 403 {
		t.Fatalf("token minting a token: %d", code)
	}
	if code := h.do("DELETE", "/api/v1/accounts/ci/token/pipeline", tok.Token, nil, nil); code != 403 {
		t.Fatalf("token revoking a token: %d", code)
	}
	if code := h.do("PUT", "/api/v1/account/password", tok.Token, map[string]string{"currentPassword": "x", "newPassword": "something-long"}, nil); code != 403 {
		t.Fatalf("token changing a password: %d", code)
	}

	// Others need accounts/get and accounts/update.
	if code := h.do("GET", "/api/v1/accounts/ci", h.reader, nil, nil); code != 403 {
		t.Fatalf("reader reading ci: %d", code)
	}
	if code := h.do("POST", "/api/v1/accounts/ci/token", h.reader, map[string]any{}, nil); code != 403 {
		t.Fatalf("reader generating for ci: %d", code)
	}

	// Refusals.
	for name, tc := range map[string]struct {
		path string
		body map[string]any
		code int
	}{
		"no apiKey capability": {"/api/v1/accounts/reader/token", map[string]any{}, 400},
		"no such account":      {"/api/v1/accounts/nobody/token", map[string]any{}, 404},
		"duplicate ID":         {"/api/v1/accounts/ci/token", map[string]any{"id": "pipeline"}, 400},
		"negative lifetime":    {"/api/v1/accounts/ci/token", map[string]any{"expiresIn": -1}, 400},
		"bad ID":               {"/api/v1/accounts/ci/token", map[string]any{"id": "a b"}, 400},
	} {
		if code := h.do("POST", tc.path, h.admin, tc.body, nil); code != tc.code {
			t.Errorf("%s: %d, want %d", name, code, tc.code)
		}
	}

	// Revoking.
	if code := h.do("DELETE", "/api/v1/accounts/ci/token/pipeline", h.admin, nil, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code := h.do("GET", "/api/v1/session/userinfo", tok.Token, nil, nil); code != 401 {
		t.Fatalf("revoked token: %d", code)
	}
	if code := h.do("DELETE", "/api/v1/accounts/ci/token/pipeline", h.admin, nil, nil); code != 404 {
		t.Fatalf("delete twice: %d", code)
	}
}

func TestOwnTokensAndAdminSetsPasswords(t *testing.T) {
	h := accountsHarness(t)

	// bob has no password until the admin sets one, confirming with the
	// admin's own.
	set := map[string]string{"name": "bob", "currentPassword": "wrong", "newPassword": "bob-password"}
	if code := h.do("PUT", "/api/v1/account/password", h.admin, set, nil); code != 400 {
		t.Fatalf("wrong admin password: %d", code)
	}
	set["currentPassword"] = h.adminPassword
	if code := h.do("PUT", "/api/v1/account/password", h.reader, map[string]string{"name": "bob", "currentPassword": "reader-password", "newPassword": "bob-password"}, nil); code != 403 {
		t.Fatalf("reader setting bob's password: %d", code)
	}
	if code := h.do("PUT", "/api/v1/account/password", h.admin, set, nil); code != 200 {
		t.Fatalf("admin setting bob's password: %d", code)
	}
	set["name"] = "nobody"
	if code := h.do("PUT", "/api/v1/account/password", h.admin, set, nil); code != 404 {
		t.Fatalf("setting a missing account's password: %d", code)
	}
	bob := h.login("bob", "bob-password")

	// bob manages its own tokens (no accounts/update needed).
	var tok tokenOut
	if code := h.do("POST", "/api/v1/accounts/bob/token", bob, map[string]any{}, &tok); code != 200 || tok.ID == "" || tok.ExpiresAt != nil {
		t.Fatalf("own token: %d %+v", code, tok)
	}
	if code := h.do("POST", "/api/v1/accounts/ci/token", bob, map[string]any{}, nil); code != 403 {
		t.Fatalf("bob generating for ci: %d", code)
	}
	// A password change ends bob's sessions, not its tokens.
	if code := h.do("PUT", "/api/v1/account/password", bob, map[string]string{"currentPassword": "bob-password", "newPassword": "bob-password-2"}, nil); code != 200 {
		t.Fatalf("bob's password change: %d", code)
	}
	if code := h.do("GET", "/api/v1/account", bob, nil, nil); code != 401 {
		t.Fatalf("old session: %d", code)
	}
	if code := h.do("GET", "/api/v1/account", tok.Token, nil, nil); code != 200 {
		t.Fatalf("token after a password change: %d", code)
	}
	bob = h.login("bob", "bob-password-2")
	if code := h.do("DELETE", "/api/v1/accounts/bob/token/"+tok.ID, bob, nil, nil); code != 200 {
		t.Fatalf("own delete: %d", code)
	}
	if code := h.do("GET", "/api/v1/account", tok.Token, nil, nil); code != 401 {
		t.Fatalf("deleted token: %d", code)
	}
}

func TestWebAccountTokens(t *testing.T) {
	h := accountsHarness(t)
	_, body := h.web("GET", "/ui/settings/accounts", h.admin, nil, false, "")
	mustContain(t, body, `href="/ui/settings/accounts/ci"`, "API tokens")
	resp, body := h.web("GET", "/ui/settings/accounts/ci", h.admin, nil, false, "")
	if resp.StatusCode != 200 {
		t.Fatalf("account page: %d", resp.StatusCode)
	}
	mustContain(t, body, "Generate new", "No API tokens.")
	// An account without the capability says so.
	_, body = h.web("GET", "/ui/settings/accounts/reader", h.admin, nil, false, "")
	mustContain(t, body, "lacks the <code>apiKey</code> capability")

	resp, body = h.web("POST", "/ui/settings/accounts/ci/token", h.admin, url.Values{"expiresIn": {"30d"}, "id": {"ui-made"}}, true, "tokens")
	if resp.StatusCode != 200 {
		t.Fatalf("generate: %d %s", resp.StatusCode, body)
	}
	mustContain(t, body, "Token ui-made generated", "data-copy=", "Copy it now")
	resp, body = h.web("POST", "/ui/settings/accounts/ci/token", h.admin, url.Values{"expiresIn": {"soon"}}, true, "tokens")
	if resp.StatusCode != 400 {
		t.Fatalf("bad lifetime: %d %s", resp.StatusCode, body)
	}
	mustContain(t, body, "is not a lifetime", `value="soon"`)
	_, body = h.web("GET", "/ui/settings/accounts/ci", h.admin, nil, false, "")
	mustContain(t, body, "ui-made")
	if strings.Contains(body, "data-copy=") {
		t.Fatal("the token list shows the ID, never the token again")
	}
	resp, body = h.web("POST", "/ui/settings/accounts/ci/token/ui-made/delete", h.admin, url.Values{}, true, "tokens")
	if resp.StatusCode != 200 {
		t.Fatalf("delete: %d %s", resp.StatusCode, body)
	}
	mustContain(t, body, "No API tokens.", "Token ui-made deleted.")

	// The reader sees neither other accounts nor their controls; its own
	// page is reachable from its user info.
	if resp, _ := h.web("GET", "/ui/settings/accounts/ci", h.reader, nil, false, ""); resp.StatusCode != 403 {
		t.Fatalf("reader on ci: %d", resp.StatusCode)
	}
	if resp, _ := h.web("POST", "/ui/settings/accounts/ci/token", h.reader, url.Values{}, true, "tokens"); resp.StatusCode != 403 {
		t.Fatalf("reader generating: %d", resp.StatusCode)
	}
	resp, body = h.web("GET", "/ui/settings/accounts/reader", h.reader, nil, false, "")
	if resp.StatusCode != 200 || strings.Contains(body, "Generate new") {
		t.Fatalf("reader on its own page: %d", resp.StatusCode)
	}
	mustContain(t, body, `href="/ui/user-info"`)
}
