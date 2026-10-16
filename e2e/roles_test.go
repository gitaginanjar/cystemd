// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chromedp/chromedp"
)

// TestEndToEndProjectRoles confines a pipeline to one project: the admin
// gives the payments project a role in the browser, with a policy and a
// token; the pipeline syncs the project's application with the token, and
// nothing else; deleting the token in the browser locks it out. The
// control plane runs with the least-privilege policies of deploy/.
func TestEndToEndProjectRoles(t *testing.T) {
	s := newStack(t, stackOptions{acl: true})
	s.repo.Commit(t, "web", map[string]string{"apps/web/web.nomad.hcl": tickJob("web", 1)})
	mustOK(t, cli(t, s.password+"\n", "login", "-password-stdin", s.url), "Logged in")
	manifest := filepath.Join(t.TempDir(), "payments.yaml")
	if err := os.WriteFile(manifest, []byte(`
apiVersion: cystemd.io/v1alpha1
kind: AppProject
metadata:
  name: payments
spec:
  sourceRepos: ["*"]
  destinations: [{name: "*", namespace: "*"}]
`), 0o600); err != nil {
		t.Fatal(err)
	}
	mustOK(t, cli(t, "", "proj", "create", "-f", manifest), "payments")
	mustOK(t, cli(t, "", "app", "create", "web", "-project", "payments", "-repo", s.repo.URL(), "-path", "apps/web", "-revision", "master"), "created")
	mustOK(t, cli(t, "", "app", "create", "other", "-repo", s.repo.URL(), "-path", "apps/web", "-revision", "master"), "created")

	p := newPage(t)
	var token string
	p.run(
		chromedp.Navigate(s.url+"/ui/settings/projects/payments"),
		chromedp.WaitVisible(`input[name=username]`),
		chromedp.SendKeys(`input[name=username]`, "admin"),
		chromedp.SendKeys(`input[name=password]`, s.password),
		chromedp.Click(`form.login__form button[type=submit]`),
		until(`location.pathname === "/ui/settings/projects/payments" && `+text("main")+`.includes("No roles.")`),
		click(`[...document.querySelectorAll("a.btn")].find(a => a.textContent.trim() === "Add role")`),
		until(`document.querySelector(".sliding-panel input[name=name]") !== null`),
		chromedp.SendKeys(`.sliding-panel input[name=name]`, "ci"),
		chromedp.SendKeys(`.sliding-panel input[name=description]`, "CI pipelines"),
		click(`document.querySelector(".sliding-panel form button[type=submit]")`),
		// The role's panel opens over the project, which lists it.
		until(`document.querySelector(".sliding-panel h2") !== null && `+text(".sliding-panel h2")+` === "Role ci" && `+text("main")+`.includes("CI pipelines")`),
		chromedp.SendKeys(`.sliding-panel form input[name=action]`, "sync"),
		chromedp.SendKeys(`.sliding-panel form input[name=object]`, "*"),
		click(`[...document.querySelectorAll(".sliding-panel form button[type=submit]")].find(b => b.textContent.trim() === "Add policy")`),
		until(text(".sliding-panel")+`.includes("payments/*")`),
		chromedp.SendKeys(`.sliding-panel form input[name=id]`, "pipeline"),
		click(`[...document.querySelectorAll(".sliding-panel form button[type=submit]")].find(b => b.textContent.trim() === "Create token")`),
		until(`document.querySelector(".sliding-panel [data-token]") !== null`),
		p.shot("project-role"),
		chromedp.Evaluate(text(".sliding-panel [data-token]"), &token),
	)
	token = strings.TrimSpace(token)
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token shown: %q", token)
	}

	// The pipeline, with the token alone.
	isolate(t)
	t.Setenv("CYSTEMD_ADDR", s.url)
	t.Setenv("CYSTEMD_TOKEN", token)
	mustOK(t, cli(t, "", "account", "get-user-info"), "Username:   proj:payments:ci")
	mustOK(t, cli(t, "", "app", "sync", "web", "-timeout", "3m"), "Phase:      Succeeded")
	mustExit(t, cli(t, "", "app", "sync", "other", "-async"), 1, "permission denied")
	mustExit(t, cli(t, "", "proj", "role", "create-token", "payments", "ci"), 1, "permission denied")

	// Revoked from the role's panel.
	p.run(
		chromedp.Navigate(s.url+"/ui/settings/projects/payments?panel=role&role=ci"),
		until(`[...document.querySelectorAll("#role-tokens td")].some(td => td.innerText.trim() === "pipeline")`),
		chromedp.Evaluate(`window.confirm = () => true`, nil),
		click(`[...document.querySelectorAll("#role-tokens button")].find(b => b.textContent.trim() === "Delete")`),
		until(text("#role-tokens")+`.includes("No tokens.")`),
	)
	mustExit(t, cli(t, "", "app", "list"), 1, "Log in with")
	if problems := p.problemList(); len(problems) > 0 {
		t.Errorf("browser problems:\n%s", strings.Join(problems, "\n"))
	}
}
