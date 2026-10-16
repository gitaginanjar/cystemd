// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package views

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
)

func render(t *testing.T, c templ.Component) string {
	t.Helper()
	var b bytes.Buffer
	if err := c.Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestYAMLHighlightEscapes(t *testing.T) {
	out := render(t, YAML("name: <script>alert(1)</script>\nport: 8080\n# a comment\nlist:\n  - \"quoted\"\nurl: http://x/y\n"))
	if strings.Contains(out, "<script>") {
		t.Fatalf("unescaped markup: %s", out)
	}
	for _, want := range []string{
		`<span class="y-key">name</span>: <span class="y-str">&lt;script&gt;alert(1)&lt;/script&gt;</span>`,
		`<span class="y-num">8080</span>`,
		`<span class="y-com"># a comment</span>`,
		`- <span class="y-str">&#34;quoted&#34;</span>`,
		`<span class="y-key">url</span>: <span class="y-str">http://x/y</span>`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %s in %s", want, out)
		}
	}
}

func TestStatusIcon(t *testing.T) {
	if out := render(t, StatusIcon("Progressing")); !strings.Contains(out, "icon--spin") || !strings.Contains(out, "st-Progressing") {
		t.Fatalf("progressing: %s", out)
	}
	if out := render(t, StatusIcon(`"><script>`)); strings.Contains(out, "<script>") || !strings.Contains(out, "Unknown") && !strings.Contains(out, "&#34;") {
		t.Fatalf("hostile status: %s", out)
	}
	if out := render(t, StatusIcon("Succeeded")); !strings.Contains(out, "st-Succeeded") {
		t.Fatalf("succeeded: %s", out)
	}
}

func TestHelpers(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	Now = func() time.Time { return now }
	defer func() { Now = time.Now }()
	cases := map[time.Duration]string{
		5 * time.Second:                  "just now",
		90 * time.Second:                 "1 minute ago",
		3 * time.Hour:                    "3 hours ago",
		72 * time.Hour:                   "3 days ago",
		-10 * time.Minute:                "in 10 minutes",
		-(30*24*time.Hour - time.Minute): "in 30 days",
		30*24*time.Hour - time.Minute:    "29 days ago",
		400 * 24 * time.Hour:             "13 months ago",
	}
	for d, want := range cases {
		if got := Ago(now.Add(-d)); got != want {
			t.Errorf("Ago(-%v) = %q, want %q", d, got, want)
		}
	}
	sha := "4531a8f0c3a4b2e6d9f8a7b6c5d4e3f2a1b0c9d8"
	if ShortRev(sha) != "4531a8f" || ShortRev("main") != "main" || ShortRev("") != "HEAD" {
		t.Fatal("ShortRev")
	}
	urls := map[string]string{
		"git@github.com:org/repo.git":                "https://github.com/org/repo/commit/" + sha,
		"https://user:pw@gitlab.example.com/a/b.git": "https://gitlab.example.com/a/b/-/commit/" + sha,
		"ssh://git@bitbucket.org/team/repo.git":      "https://bitbucket.org/team/repo/commits/" + sha,
		"file:///tmp/repo":                           "",
		"https://git.internal/x.git":                 "",
	}
	for repo, want := range urls {
		if got := CommitURL(repo, sha); got != want {
			t.Errorf("CommitURL(%s) = %q, want %q", repo, got, want)
		}
	}
	if ExternalURL("javascript:alert(1)") != "#" {
		t.Fatal("ExternalURL let a script URL through")
	}
	q := AppsQuery{View: "list", Sync: []string{"OutOfSync"}, Search: "web"}
	if got := q.Href("summary"); got != "/ui/applications?search=web&sync=OutOfSync&view=summary" {
		t.Fatalf("Href = %q", got)
	}
	if got := AppURL("web", "tree", "panel", "sync", "tab", ""); got != "/ui/applications/web?panel=sync" {
		t.Fatalf("AppURL = %q", got)
	}
}
