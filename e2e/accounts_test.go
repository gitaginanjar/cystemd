// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestEndToEndAPITokens gives a pipeline an API token: an account declared
// for it, which the RBAC policy lets sync the default project's
// applications. The admin generates the token in the UI, the pipeline
// syncs with it from the command line, and deleting the token in the UI
// revokes it. The control plane runs with the least-privilege Consul and
// Nomad policies of deploy/.
func TestEndToEndAPITokens(t *testing.T) {
	s := newStack(t, stackOptions{acl: true, config: `
auth {
  account "ci" {
    capabilities = ["apiKey"]
  }
}
rbac {
  policy = "p, ci, applications, sync, default/*, allow"
}
`})
	s.repo.Commit(t, "web", map[string]string{"apps/web/web.nomad.hcl": tickJob("web", 1)})
	mustOK(t, cli(t, s.password+"\n", "login", "-password-stdin", s.url), "Logged in")
	mustOK(t, cli(t, "", "app", "create", "web", "-repo", s.repo.URL(), "-path", "apps/web", "-revision", "master"), "created")
	mustOK(t, cli(t, "", "account", "list"), "ci", "apiKey")

	p := newPage(t)
	var token string
	p.run(
		chromedp.Navigate(s.url+"/ui/settings/accounts"),
		chromedp.WaitVisible(`input[name=username]`),
		chromedp.SendKeys(`input[name=username]`, "admin"),
		chromedp.SendKeys(`input[name=password]`, s.password),
		chromedp.Click(`form.login__form button[type=submit]`),
		until(`location.pathname === "/ui/settings/accounts" && document.querySelector("a[href='/ui/settings/accounts/ci']") !== null`),
		p.shot("accounts"),
		click(`document.querySelector("a[href='/ui/settings/accounts/ci']")`),
		until(`location.pathname === "/ui/settings/accounts/ci" && document.querySelector("form.token-form") !== null`),
		chromedp.SendKeys(`form.token-form input[name=expiresIn]`, "30d"),
		chromedp.SendKeys(`form.token-form input[name=id]`, "pipeline"),
		click(`document.querySelector("form.token-form button[type=submit]")`),
		until(`document.querySelector("[data-token]") !== null && document.querySelector(".toast") !== null`),
		p.shot("account-token"),
		chromedp.Evaluate(text("[data-token]"), &token),
	)
	token = strings.TrimSpace(token)
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token shown: %q", token)
	}

	// The pipeline: the token alone, no saved context.
	isolate(t)
	t.Setenv("CYSTEMD_ADDR", s.url)
	t.Setenv("CYSTEMD_TOKEN", token)
	mustOK(t, cli(t, "", "account", "get-user-info"), "Username:   ci")
	mustOK(t, cli(t, "", "account", "can-i", "sync", "applications", "default/web"), "yes")
	mustOK(t, cli(t, "", "app", "sync", "web", "-timeout", "3m"), "Phase:      Succeeded")
	mustExit(t, cli(t, "", "app", "delete", "web", "-y"), 1, "permission denied")
	mustExit(t, cli(t, "", "account", "generate-token"), 1, "permission denied")

	// Revoked from the UI: the pipeline is out at once.
	p.run(
		chromedp.Navigate(s.url+"/ui/settings/accounts/ci"),
		until(`[...document.querySelectorAll("#tokens td")].some(td => td.innerText.trim() === "pipeline")`),
		chromedp.Evaluate(`window.confirm = () => true`, nil),
		click(`document.querySelector("#tokens button[hx-post$='/token/pipeline/delete']")`),
		until(text("#tokens")+`.includes("No API tokens.")`),
		p.shot("account-token-deleted"),
	)
	mustExit(t, cli(t, "", "app", "list"), 1, "Log in with")
	if problems := p.problemList(); len(problems) > 0 {
		t.Errorf("browser problems:\n%s", strings.Join(problems, "\n"))
	}
}
