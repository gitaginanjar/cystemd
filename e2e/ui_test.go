// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cdplog "github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// browser finds a Chromium: CHROME_PATH, Playwright's, or one on PATH.
func browser(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("CHROME_PATH"); p != "" {
		return p
	}
	if p := "/opt/pw-browsers/chromium"; fileExists(p) {
		return p
	}
	for _, name := range []string{"chromium", "chromium-browser", "google-chrome", "google-chrome-stable"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	t.Skip("no Chromium (set CHROME_PATH)")
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// page drives one browser tab and collects what goes wrong in it:
// uncaught exceptions and console or browser errors (which include
// Content-Security-Policy violations).
type page struct {
	t        *testing.T
	ctx      context.Context
	shots    string
	mu       sync.Mutex
	step     string // the last screenshot taken, to place problems
	problems []string
}

func newPage(t *testing.T) *page {
	t.Helper()
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(browser(t)), chromedp.WindowSize(1440, 900), chromedp.NoSandbox)
	actx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelCtx := chromedp.NewContext(actx)
	ctx, cancelTimeout := context.WithTimeout(ctx, 8*time.Minute)
	t.Cleanup(func() { cancelTimeout(); cancelCtx(); cancelAlloc() })
	p := &page{t: t, ctx: ctx, shots: os.Getenv("CYSTEMD_E2E_SCREENSHOTS")}
	chromedp.ListenTarget(ctx, func(ev any) {
		p.mu.Lock()
		defer p.mu.Unlock()
		after := " (after " + p.step + ")"
		switch e := ev.(type) {
		case *runtime.EventExceptionThrown:
			p.problems = append(p.problems, "exception: "+e.ExceptionDetails.Error()+after)
		case *runtime.EventConsoleAPICalled:
			if e.Type == runtime.APITypeError {
				var args []string
				for _, a := range e.Args {
					args = append(args, string(a.Value))
				}
				p.problems = append(p.problems, "console error: "+strings.Join(args, " ")+after)
			}
		case *cdplog.EventEntryAdded:
			if e.Entry.Level == cdplog.LevelError {
				p.problems = append(p.problems, fmt.Sprintf("%s error: %s %s%s", e.Entry.Source, e.Entry.Text, e.Entry.URL, after))
			}
		}
	})
	if err := chromedp.Run(ctx, cdplog.Enable(), runtime.Enable()); err != nil {
		t.Fatal(err)
	}
	return p
}

func (p *page) run(actions ...chromedp.Action) {
	p.t.Helper()
	if err := chromedp.Run(p.ctx, actions...); err != nil {
		p.t.Fatalf("browser: %v (problems so far: %q)", err, p.problemList())
	}
}

func (p *page) problemList() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.problems...)
}

// shot saves a full-page screenshot when CYSTEMD_E2E_SCREENSHOTS is set.
func (p *page) shot(name string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		p.mu.Lock()
		p.step = name
		p.mu.Unlock()
		if p.shots == "" {
			return nil
		}
		var buf []byte
		if err := chromedp.FullScreenshot(&buf, 90).Do(ctx); err != nil {
			return err
		}
		if err := os.MkdirAll(p.shots, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(p.shots, name+".png"), buf, 0o644)
	})
}

// until polls a JavaScript condition, through navigations (an evaluation
// failing while the page changes is retried).
func until(js string) chromedp.Action {
	return chromedp.ActionFunc(func(ctx context.Context) error {
		deadline := time.Now().Add(2 * time.Minute)
		var lastErr error
		for time.Now().Before(deadline) {
			var ok bool
			err := chromedp.Evaluate(js, &ok).Do(ctx)
			if err == nil && ok {
				return nil
			}
			lastErr = err
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		return fmt.Errorf("timed out waiting for %s (last error: %v)", js, lastErr)
	})
}

func click(js string) chromedp.Action { return chromedp.Evaluate(js+".click()", nil) }

func text(sel string) string {
	return fmt.Sprintf(`((document.querySelector(%q)||{}).innerText||"")`, sel)
}

// TestEndToEndUI uses the web UI as a person would: log in, find the
// application, read its diff, sync it, watch the page update itself over
// the live stream, read its logs, see a Git change arrive, browse the
// settings and log out. No page may raise a script error or break the
// Content-Security-Policy.
func TestEndToEndUI(t *testing.T) {
	s := newStack(t, stackOptions{})
	p := newPage(t)
	s.repo.Commit(t, "web v1", map[string]string{
		"apps/web/web.nomad.hcl":  tickJob("web", 1),
		"apps/web/consul-kv.yaml": "- key: config/web/flags\n  value: a=1\n",
	})
	app := &v1.Application{
		Metadata: v1.ObjectMeta{Name: "web", Labels: map[string]string{"team": "e2e"}},
		Spec: v1.ApplicationSpec{Source: v1.ApplicationSource{RepoURL: s.repo.URL(), Path: "apps/web", TargetRevision: "master",
			Nomad: &v1.NomadSource{ConsulKV: "consul-kv.yaml"}}},
	}
	if _, err := s.api.CreateApplication(s.ctx, app, false); err != nil {
		t.Fatal(err)
	}
	s.waitApp("web", time.Minute, func(a *v1.Application) bool { return a.Status.Sync.Status == v1.SyncStatusOutOfSync })

	var status, marker string
	p.run(
		// Unauthenticated: the login page.
		chromedp.Navigate(s.url+"/ui/applications"),
		chromedp.WaitVisible(`input[name=username]`),
		p.shot("01-login"),
		chromedp.SendKeys(`input[name=username]`, "admin"),
		chromedp.SendKeys(`input[name=password]`, s.password),
		chromedp.Click(`form.login__form button[type=submit]`),
		// The applications page, with the application's tile.
		until(`location.pathname === "/ui/applications" && document.querySelector(".tile__title") !== null`),
		until(text(".tile")+`.includes("OutOfSync")`),
		p.shot("02-applications"),
		// The application: a tree of what Git wants, missing so far.
		click(`document.querySelector(".tile__title")`),
		until(`location.pathname === "/ui/applications/web" && document.querySelector("#app-status") !== null`),
		until(`document.querySelectorAll(".tree-node").length >= 2`),
		p.shot("03-app-outofsync"),
		// The diff panel.
		click(`document.querySelector("a.status-panel__link[href*='panel=diff']")`),
		until(`document.querySelector(".diff__line--add") !== null`),
		p.shot("04-diff"),
		click(`document.querySelector(".sliding-panel__close")`),
		until(`document.querySelector(".sliding-panel") === null`),
		// Sync from the toolbar, and stay on the page: the live stream
		// updates it.
		chromedp.Evaluate(`window.__e2e = "same page"`, nil),
		click(`[...document.querySelectorAll(".toolbar a.btn, .toolbar button.btn")].find(b => b.textContent.trim() === "Sync")`),
		until(`document.querySelector("form[hx-post$='/sync'] button[type=submit]") !== null`),
		p.shot("05-sync-panel"),
		click(`document.querySelector("form[hx-post$='/sync'] button[type=submit]")`),
		until(`document.querySelector(".toast") !== null`),
		until(text("#app-status")+`.includes("Synced") && `+text("#app-status")+`.includes("Healthy")`),
		until(`[...document.querySelectorAll(".tree-node")].some(n => n.innerText.includes("web.app[0]"))`),
		chromedp.Evaluate(`window.__e2e || "reloaded"`, &marker),
		chromedp.Evaluate(text("#app-status"), &status),
		p.shot("06-synced"),
	)
	if marker != "same page" {
		t.Errorf("the page reloaded instead of updating itself (%q)", marker)
	}
	if !strings.Contains(status, "Sync OK") {
		t.Errorf("status panel after the sync:\n%s", status)
	}

	// The allocation's logs, streamed.
	var href string
	p.run(chromedp.Evaluate(`[...document.querySelectorAll(".tree-node__link")].map(a => a.getAttribute("href")).find(h => h.includes("node=alloc"))||""`, &href))
	if href == "" {
		t.Fatal("no allocation node")
	}
	p.run(
		chromedp.Navigate(s.url+href+"&tab=logs"),
		until(`document.querySelectorAll(".log-line").length >= 3`),
		until(`[...document.querySelectorAll(".log-line")].some(l => l.innerText.includes("tick"))`),
		p.shot("07-logs"),
	)

	// A Git change reaches the open page (Refresh does what a webhook
	// would).
	s.repo.Commit(t, "web v2", map[string]string{"apps/web/web.nomad.hcl": tickJob("web", 2)})
	p.run(
		chromedp.Navigate(s.url+"/ui/applications/web"),
		until(text("#app-status")+`.includes("Synced")`),
		chromedp.Evaluate(`window.__e2e = "same page"`, nil),
		click(`document.querySelector(".toolbar button[hx-post$='/refresh']")`),
		until(text("#app-status")+`.includes("OutOfSync")`),
		chromedp.Evaluate(`window.__e2e || "reloaded"`, &marker),
		p.shot("08-outofsync-live"),
	)
	if marker != "same page" {
		t.Errorf("the page reloaded on the Git change (%q)", marker)
	}

	// List view, settings, then log out.
	p.run(
		chromedp.Navigate(s.url+"/ui/applications?view=list"),
		until(`document.body.innerText.includes("web") && document.body.innerText.includes("OutOfSync")`),
		p.shot("09-applications-list"),
		chromedp.Navigate(s.url+"/ui/settings"),
		until(`document.body.innerText.includes("Repositories") && document.body.innerText.includes("Clusters")`),
		chromedp.Navigate(s.url+"/ui/settings/clusters"),
		until(`document.body.innerText.includes("in-cluster")`),
		p.shot("10-clusters"),
		chromedp.Navigate(s.url+"/ui/user-info"),
		until(`document.body.innerText.includes("admin")`),
		click(`document.querySelector("button[hx-post='/ui/logout']")`),
		until(`location.pathname === "/ui/login" && document.querySelector("input[name=username]") !== null`),
	)
	if problems := p.problemList(); len(problems) > 0 {
		t.Errorf("browser problems:\n%s", strings.Join(problems, "\n"))
	}
}
