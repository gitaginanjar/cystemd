// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

func TestCanonicalRepoURL(t *testing.T) {
	same := []string{
		"https://git.example.com/org/app.git",
		"https://user:pw@Git.Example.com:443/org/app",
		"http://git.example.com/org/app/",
		"git@git.example.com:org/app.git",
		"git.example.com:org/app",
		"ssh://git@git.example.com/org/app.git",
		"ssh://git@git.example.com:2222/org/app",
	}
	for _, u := range same {
		if got := canonicalRepoURL(u); got != "git.example.com/org/app" {
			t.Errorf("canonicalRepoURL(%q) = %q", u, got)
		}
	}
	if got := canonicalRepoURL("file:///srv/git/ops"); got != "/srv/git/ops" {
		t.Errorf("file URL: %q", got)
	}
	if canonicalRepoURL("https://git.example.com/org/app") == canonicalRepoURL("https://git.example.com/org/app2") {
		t.Error("different repositories match")
	}
	if canonicalRepoURL("") != "" || canonicalRepoURL("git@host") != "" {
		t.Error("invalid URLs must not match anything")
	}
}

func TestRevisionPushed(t *testing.T) {
	main := push{refs: []string{"refs/heads/main"}, defaultBranch: "main"}
	feature := push{refs: []string{"refs/heads/feature"}, defaultBranch: "main"}
	unknownDefault := push{refs: []string{"refs/heads/feature"}}
	tag := push{refs: []string{"refs/tags/v1.2.0"}}
	for _, tc := range []struct {
		rev  string
		ev   push
		want bool
	}{
		{"", main, true}, {"HEAD", main, true}, {"HEAD", feature, false}, {"HEAD", unknownDefault, true},
		{"main", main, true}, {"refs/heads/main", main, true}, {"main", feature, false},
		{"v1.2.0", tag, true}, {"refs/tags/v1.2.0", tag, true}, {"HEAD", tag, false},
		{"0123456789abcdef0123456789abcdef01234567", main, false},
	} {
		if got := revisionPushed(tc.rev, tc.ev); got != tc.want {
			t.Errorf("revisionPushed(%q, %v) = %v", tc.rev, tc.ev.refs, got)
		}
	}
}

func header(kv ...string) http.Header {
	h := http.Header{}
	for i := 0; i < len(kv); i += 2 {
		h.Set(kv[i], kv[i+1])
	}
	return h
}

func TestParseWebhook(t *testing.T) {
	for _, tc := range []struct {
		name    string
		h       http.Header
		body    string
		ok      bool
		url     string // one of the URLs, canonical
		refs    string
		dfltBr  string
		provide string
	}{{
		name: "github", h: header("X-GitHub-Event", "push"),
		body: `{"ref":"refs/heads/main","repository":{"html_url":"https://github.com/org/app","clone_url":"https://github.com/org/app.git","ssh_url":"git@github.com:org/app.git","default_branch":"main"}}`,
		ok:   true, url: "github.com/org/app", refs: "refs/heads/main", dfltBr: "main", provide: "github",
	}, {
		name: "github ping", h: header("X-GitHub-Event", "ping"), body: `{"zen":"hi"}`,
	}, {
		name: "gitea", h: header("X-Gitea-Event", "push", "X-GitHub-Event", "push"),
		body: `{"ref":"refs/tags/v1","repository":{"clone_url":"https://gitea.example.com/org/app.git","default_branch":"trunk"}}`,
		ok:   true, url: "gitea.example.com/org/app", refs: "refs/tags/v1", dfltBr: "trunk", provide: "gitea",
	}, {
		name: "gitlab", h: header("X-Gitlab-Event", "Push Hook"),
		body: `{"object_kind":"push","ref":"refs/heads/dev","project":{"git_ssh_url":"git@gitlab.example.com:grp/app.git","git_http_url":"https://gitlab.example.com/grp/app.git","default_branch":"main"}}`,
		ok:   true, url: "gitlab.example.com/grp/app", refs: "refs/heads/dev", dfltBr: "main", provide: "gitlab",
	}, {
		name: "gitlab merge request", h: header("X-Gitlab-Event", "Merge Request Hook"), body: `{"object_kind":"merge_request"}`,
	}, {
		name: "bitbucket cloud", h: header("X-Event-Key", "repo:push"),
		body: `{"push":{"changes":[{"new":{"type":"branch","name":"main"}},{"new":null},{"new":{"type":"tag","name":"v2"}}]},"repository":{"full_name":"team/app","links":{"html":{"href":"https://bitbucket.org/team/app"}},"mainbranch":{"name":"main"}}}`,
		ok:   true, url: "bitbucket.org/team/app", refs: "refs/heads/main refs/tags/v2", dfltBr: "main", provide: "bitbucket",
	}, {
		name: "bitbucket server", h: header("X-Event-Key", "repo:refs_changed"),
		body: `{"changes":[{"ref":{"id":"refs/heads/main"},"type":"UPDATE"},{"ref":{"id":"refs/heads/old"},"type":"DELETE"}],"repository":{"links":{"clone":[{"href":"ssh://git@bb.example.com:7999/prj/app.git","name":"ssh"},{"href":"https://bb.example.com/scm/prj/app.git","name":"http"}]}}}`,
		ok:   true, url: "bb.example.com/prj/app", refs: "refs/heads/main", provide: "bitbucket-server",
	}, {
		name: "unknown", h: header("X-Something", "push"), body: `{}`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			ev, ok, err := parseWebhook(tc.h, []byte(tc.body))
			if err != nil || ok != tc.ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if !ok {
				return
			}
			found := false
			for _, u := range ev.urls {
				found = found || canonicalRepoURL(u) == tc.url
			}
			if !found || strings.Join(ev.refs, " ") != tc.refs || ev.defaultBranch != tc.dfltBr || ev.provider != tc.provide {
				t.Errorf("parsed %+v", ev)
			}
		})
	}
	if _, _, err := parseWebhook(header("X-GitHub-Event", "push"), []byte("{")); err == nil {
		t.Error("broken JSON accepted")
	}
}

func TestWebhookAuthentic(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))
	for _, tc := range []struct {
		h    http.Header
		want bool
	}{
		{header("X-Hub-Signature-256", "sha256="+sig), true},
		{header("X-Hub-Signature", "sha256="+sig), true},
		{header("X-Gitea-Signature", sig), true},
		{header("X-Gogs-Signature", sig), true},
		{header("X-Gitlab-Token", "s3cret"), true},
		{header("X-Gitlab-Token", "wrong"), false},
		{header("X-Hub-Signature-256", "sha256=00"+sig[2:]), false},
		{header("X-Hub-Signature-256", "not-hex"), false},
		{header(), false},
	} {
		if got := webhookAuthentic(tc.h, body, "s3cret"); got != tc.want {
			t.Errorf("webhookAuthentic(%v) = %v", tc.h, got)
		}
	}
}
