// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/server"
)

// fakeSSO stands for Nomad: it remembers the nonce of each login it
// started, by state, and completes those whose code is "good".
type fakeSSO struct {
	methods []string
	mu      sync.Mutex
	nonces  map[string]string
}

func (f *fakeSSO) Methods(context.Context) ([]string, error) { return f.methods, nil }

func (f *fakeSSO) AuthURL(_ context.Context, method, redirectURI, nonce string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	state := "state-" + method + "-" + nonce[:6]
	f.nonces[state] = nonce
	return "https://idp.example.com/authorize?" + url.Values{"state": {state}, "redirect_uri": {redirectURI}}.Encode(), nil
}

func (f *fakeSSO) Complete(_ context.Context, cb server.SSOCallback) (*auth.NomadToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.nonces[cb.State] != cb.ClientNonce:
		return nil, errors.New("invalid state or nonce")
	case cb.Code != "good":
		return nil, errors.New("no role or policy bindings matched")
	}
	return &auth.NomadToken{Name: "alice@example.com", AccessorID: "0123456789", Roles: []string{"platform"}}, nil
}

func ssoHarness(t *testing.T, methods ...string) (*harness, *fakeSSO) {
	f := &fakeSSO{methods: methods, nonces: map[string]string{}}
	return newHarnessOpts(t, nil, func(o *server.Options) { o.SSO = f }), f
}

func TestSSOBrowserLogin(t *testing.T) {
	h, _ := ssoHarness(t, "okta")
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := browser.Get(h.srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}

	if _, page := get("/ui/login?return_url=%2Fui%2Fsettings"); !strings.Contains(page, "Log in via SSO") ||
		!strings.Contains(page, `href="/ui/auth/login?method=okta&amp;return_url=%2Fui%2Fsettings"`) {
		t.Fatalf("login page:\n%s", page)
	}

	// Off to the provider, with the callback on this server.
	resp, _ := get("/ui/auth/login?method=okta&return_url=%2Fui%2Fsettings")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusSeeOther || loc.Host != "idp.example.com" || loc.Query().Get("redirect_uri") != h.srv.URL+"/ui/auth/callback" {
		t.Fatalf("start: %s %s", resp.Status, loc)
	}
	state := loc.Query().Get("state")

	// The provider refused: no session, and the sign-in is over.
	if resp, page := get("/ui/auth/callback?error=access_denied&error_description=MFA+required&state=" + state); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(page, "The identity provider refused the sign-in: MFA required") {
		t.Fatalf("refused: %s\n%s", resp.Status, page)
	}
	if resp, page := get("/ui/auth/callback?code=good&state=" + state); resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "expired") {
		t.Fatalf("callback without a sign-in in progress: %s\n%s", resp.Status, page)
	}
	// A code Nomad does not accept.
	resp, _ = get("/ui/auth/login?method=okta")
	loc, _ = url.Parse(resp.Header.Get("Location"))
	if resp, page := get("/ui/auth/callback?code=bad&state=" + loc.Query().Get("state")); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(page, "SSO login failed: no role or policy bindings matched") {
		t.Fatalf("unbound: %s\n%s", resp.Status, page)
	}
	// A callback for another browser's login fails on its nonce.
	resp, _ = get("/ui/auth/login?method=okta")
	if resp, page := get("/ui/auth/callback?code=good&state=" + state); resp.StatusCode != http.StatusUnauthorized ||
		!strings.Contains(page, "invalid state or nonce") {
		t.Fatalf("someone else's state: %s\n%s", resp.Status, page)
	}
	if resp, _ := get("/api/v1/session/userinfo"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a failed sign-in left a session: %s", resp.Status)
	}

	// Signed in, back where the user wanted to go.
	resp, _ = get("/ui/auth/login?method=okta&return_url=%2Fui%2Fsettings")
	loc, _ = url.Parse(resp.Header.Get("Location"))
	resp, _ = get("/ui/auth/callback?code=good&state=" + loc.Query().Get("state"))
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/ui/settings" {
		t.Fatalf("callback: %s %s", resp.Status, resp.Header.Get("Location"))
	}
	if _, body := get("/api/v1/session/userinfo"); !strings.Contains(body, `"username":"alice@example.com"`) ||
		!strings.Contains(body, `"provider":"sso"`) || !strings.Contains(body, `"nomad:role:platform"`) {
		t.Fatalf("user info: %s", body)
	}

	// Unknown auth methods are refused.
	if resp, page := get("/ui/auth/login?method=github"); resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, `unknown SSO auth method`) {
		t.Fatalf("unknown method: %s\n%s", resp.Status, page)
	}
}

func TestSSOLoginPageWithSeveralMethods(t *testing.T) {
	h, _ := ssoHarness(t, "okta", "github")
	resp, err := http.Get(h.srv.URL + "/ui/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if page := string(b); !strings.Contains(page, "Log in via okta") || !strings.Contains(page, "Log in via github") || strings.Contains(page, "Log in via SSO") {
		t.Fatalf("login page:\n%s", page)
	}
}

func TestSSOAPI(t *testing.T) {
	h, _ := ssoHarness(t, "okta", "github")
	var methods server.SSOMethods
	if code := h.do("GET", "/api/v1/session/sso", "", nil, &methods); code != http.StatusOK || strings.Join(methods.AuthMethods, ",") != "okta,github" {
		t.Fatalf("methods: %d %+v", code, methods)
	}
	var settings server.Settings
	if h.do("GET", "/api/v1/settings", "", nil, &settings); strings.Join(settings.SSOAuthMethods, ",") != "okta,github" {
		t.Fatalf("settings: %+v", settings)
	}

	redirect := "http://localhost:8085/auth/callback"
	if code := h.do("POST", "/api/v1/session/sso/auth-url", "", map[string]string{"redirectURI": redirect, "clientNonce": "short"}, nil); code != http.StatusBadRequest {
		t.Fatalf("short nonce: %d", code)
	}
	nonce := strings.Repeat("ab", 12)
	var start server.SSOAuthURL
	if code := h.do("POST", "/api/v1/session/sso/auth-url", "", map[string]string{"redirectURI": redirect, "clientNonce": nonce}, &start); code != http.StatusOK || start.AuthMethod != "okta" {
		t.Fatalf("auth url: %d %+v", code, start)
	}
	u, _ := url.Parse(start.AuthURL)
	cb := server.SSOCallback{AuthMethod: "okta", ClientNonce: nonce, State: u.Query().Get("state"), Code: "bad", RedirectURI: redirect}
	if code := h.do("POST", "/api/v1/session/sso/complete", "", cb, nil); code != http.StatusUnauthorized {
		t.Fatalf("bad code: %d", code)
	}
	cb.Code = "good"
	var out struct{ Token string }
	if code := h.do("POST", "/api/v1/session/sso/complete", "", cb, &out); code != http.StatusOK || out.Token == "" {
		t.Fatalf("complete: %d %+v", code, out)
	}
	var info server.UserInfo
	if h.do("GET", "/api/v1/session/userinfo", out.Token, nil, &info); info.Username != "alice@example.com" || info.Provider != auth.ProviderSSO {
		t.Fatalf("user info: %+v", info)
	}

	// Without SSO the endpoints say so.
	plain := newHarness(t)
	if code := plain.do("GET", "/api/v1/session/sso", "", nil, &methods); code != http.StatusOK || len(methods.AuthMethods) != 0 {
		t.Fatalf("no SSO methods: %d %+v", code, methods)
	}
	if code := plain.do("POST", "/api/v1/session/sso/auth-url", "", map[string]string{"redirectURI": redirect, "clientNonce": nonce}, nil); code != http.StatusNotFound {
		t.Fatalf("no SSO auth url: %d", code)
	}
}
