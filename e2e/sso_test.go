// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/testutil"
)

// TestEndToEndSSO logs in through single sign-on: an OIDC auth method of
// the real Nomad, against an identity provider, its binding rule giving
// the user a Nomad role, which the control plane's RBAC makes role:admin.
func TestEndToEndSSO(t *testing.T) {
	idp := testutil.StartOIDCProvider(t, "cystemd", "s3cret", map[string]any{
		"sub": "u-1", "email": "alice@example.com", "groups": []string{"platform", "staff"},
	})
	cliPort := testutil.FreePorts(t, 1)[0]
	s := newStack(t, stackOptions{acl: true, config: `
auth {
  sso {}
}
rbac {
  policy = "g, nomad:role:platform, role:admin"
}
`})
	// Nomad: the role users of the platform group get, and the auth method.
	if _, err := s.nomadAPI.ACLPolicies().Upsert(&nomadapi.ACLPolicy{Name: "platform", Rules: `namespace "*" { policy = "read" }`}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.nomadAPI.ACLRoles().Create(&nomadapi.ACLRole{Name: "platform", Policies: []*nomadapi.ACLRolePolicyLink{{Name: "platform"}}}, nil); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.nomadAPI.ACLAuthMethods().Create(&nomadapi.ACLAuthMethod{
		Name: "example-idp", Type: "OIDC", TokenLocality: "local", MaxTokenTTL: time.Hour, Default: true,
		TokenNameFormat: "${value.email}",
		Config: &nomadapi.ACLAuthMethodConfig{
			OIDCDiscoveryURL: idp.URL, OIDCClientID: idp.ClientID, OIDCClientSecret: idp.ClientSecret,
			BoundAudiences: []string{idp.ClientID}, OIDCScopes: []string{"email", "groups"},
			AllowedRedirectURIs: []string{s.url + "/ui/auth/callback", fmt.Sprintf("http://localhost:%d/auth/callback", cliPort)},
			ClaimMappings:       map[string]string{"email": "email"},
			ListClaimMappings:   map[string]string{"groups": "groups"},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.nomadAPI.ACLBindingRules().Create(&nomadapi.ACLBindingRule{
		AuthMethod: "example-idp", Selector: `"platform" in list.groups`, BindType: "role", BindName: "platform",
	}, nil); err != nil {
		t.Fatal(err)
	}

	// The login page offers single sign-on.
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := browser.Get(s.url + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if _, page := get("/ui/login"); !strings.Contains(page, "Log in via SSO") || !strings.Contains(page, "/ui/auth/login?method=example-idp") {
		t.Fatalf("login page:\n%s", page)
	}

	// The browser goes to the provider through Nomad and comes back signed
	// in, where it wanted to go.
	resp, page := get("/ui/auth/login?method=example-idp&return_url=%2Fui%2Fsettings")
	if resp.StatusCode != http.StatusOK || resp.Request.URL.Path != "/ui/settings" {
		t.Fatalf("after SSO: %s %s\n%s", resp.Status, resp.Request.URL, page)
	}
	_, body := get("/api/v1/session/userinfo")
	var info struct {
		Username string
		Provider string
		Groups   []string
		Roles    []string
		IsAdmin  bool
	}
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if info.Username != "alice@example.com" || info.Provider != "sso" || strings.Join(info.Groups, ",") != "nomad:role:platform" || !info.IsAdmin {
		t.Fatalf("user info: %+v", info)
	}

	// A user without a binding gets no session.
	idp.SetClaims(map[string]any{"sub": "u-2", "email": "bob@example.com", "groups": []string{"staff"}})
	jar, _ = cookiejar.New(nil)
	browser.Jar = jar
	resp, page = get("/ui/auth/login")
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(page, "no role or policy bindings matched") {
		t.Fatalf("unbound user: %s\n%s", resp.Status, page)
	}
	if resp, _ := get("/api/v1/session/userinfo"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unbound user has a session: %s", resp.Status)
	}

	// The command line: its browser lands on the CLI's own listener.
	idp.SetClaims(map[string]any{"sub": "u-1", "email": "alice@example.com", "groups": []string{"platform"}})
	o := agentOptions()
	o.Name = "cystemdctl"
	o.SessionPrefix, o.NomadFallback = "", false
	o.OpenBrowser = func(u string) error {
		resp, err := http.Get(u)
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	mustOK(t, runCLI(t, o, "", "login", "-sso", "-sso-port", fmt.Sprint(cliPort), s.url), "Log in via example-idp", "as alice@example.com")
	mustOK(t, cli(t, "", "account", "get-user-info"), "alice@example.com", "role:admin")
}
