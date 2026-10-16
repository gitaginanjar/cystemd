// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controller"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// maxWebhookBody bounds a webhook payload (a push of many commits is large).
const maxWebhookBody = 25 << 20

// push is what a webhook says happened: refs were pushed to a repository
// known by these URLs.
type push struct {
	provider      string
	urls          []string // clone and web URLs of the repository
	refs          []string // full refs (refs/heads/main, refs/tags/v1)
	defaultBranch string   // when the provider says
}

// handleWebhook receives Git push events from GitHub, GitLab, Gitea, Gogs
// and Bitbucket (Cloud and Server), and refreshes the Applications that
// track a pushed branch or tag of the repository, so that they are
// compared now rather than at the next poll (Argo CD's /api/webhook).
//
// With webhook.secret configured, a request must be signed with it
// (GitHub, Gitea, Gogs and Bitbucket HMAC-SHA256 signatures) or carry it
// (GitLab's X-Gitlab-Token). Without one, any request is accepted: a
// refresh changes nothing but when the comparison happens.
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody+1))
	if err != nil {
		s.fail(w, badRequest("read payload: %v", err))
		return
	}
	if len(body) > maxWebhookBody {
		s.fail(w, statusErr(http.StatusRequestEntityTooLarge, "payload larger than %d bytes", maxWebhookBody))
		return
	}
	secret, err := s.webhookSecret()
	if err != nil {
		s.logger.Warn("webhook secret", "error", err)
		s.fail(w, statusErr(http.StatusInternalServerError, "webhook secret unavailable"))
		return
	}
	if secret != "" && !webhookAuthentic(r.Header, body, secret) {
		s.logger.Warn("webhook rejected: bad or missing signature", "client", clientIP(r))
		s.fail(w, statusErr(http.StatusUnauthorized, "webhook signature or token does not match the configured secret"))
		return
	}
	// GitHub, Gitea and Gogs can post the JSON as a form field (the
	// signature covers the form).
	if ct := r.Header.Get("Content-Type"); strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		form, err := url.ParseQuery(string(body))
		if err != nil {
			s.fail(w, badRequest("form payload: %v", err))
			return
		}
		body = []byte(form.Get("payload"))
	}
	ev, ok, err := parseWebhook(r.Header, body)
	if err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"refreshed": []string{}, "message": "ignored: not a push event"})
		return
	}
	apps, err := s.Store.ListApplications(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	repos := map[string]bool{}
	for _, u := range ev.urls {
		if c := canonicalRepoURL(u); c != "" {
			repos[c] = true
		}
	}
	refreshed := []string{}
	for _, app := range apps {
		src := app.Spec.Source
		if !repos[canonicalRepoURL(src.RepoURL)] || !revisionPushed(src.Revision(), ev) {
			continue
		}
		_, err := s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
			if a.Metadata.Annotations == nil {
				a.Metadata.Annotations = map[string]string{}
			}
			if a.Metadata.Annotations[controller.RefreshAnnotation] == "" {
				a.Metadata.Annotations[controller.RefreshAnnotation] = "normal"
			}
			return nil
		})
		if err != nil {
			s.logger.Warn("webhook refresh", "app", app.Metadata.Name, "error", err)
			continue
		}
		refreshed = append(refreshed, app.Metadata.Name)
	}
	sort.Strings(refreshed)
	refreshedSets := s.refreshAppSetsForPush(r.Context(), repos)
	s.logger.Info("webhook", "provider", ev.provider, "repository", firstOf(ev.urls), "refs", ev.refs, "refreshed", refreshed,
		"applicationsets", refreshedSets)
	writeJSON(w, http.StatusOK, map[string]any{"refreshed": refreshed, "applicationSets": refreshedSets})
}

func (s *Server) webhookSecret() (string, error) {
	if s.Config == nil || s.Config.Webhook == nil {
		return "", nil
	}
	return config.Secret(s.Config.Webhook.Secret, s.Config.Webhook.SecretFile)
}

// webhookAuthentic checks the provider's signature or token header.
func webhookAuthentic(h http.Header, body []byte, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	sigOK := func(v string) bool {
		got, err := hex.DecodeString(strings.TrimPrefix(v, "sha256="))
		return err == nil && hmac.Equal(got, want)
	}
	for _, name := range []string{"X-Hub-Signature-256", "X-Hub-Signature", "X-Gitea-Signature", "X-Gogs-Signature"} {
		if v := h.Get(name); v != "" && sigOK(v) {
			return true
		}
	}
	if v := h.Get("X-Gitlab-Token"); v != "" {
		return subtle.ConstantTimeCompare([]byte(v), []byte(secret)) == 1
	}
	return false
}

// parseWebhook reads a push event; ok is false for other events.
func parseWebhook(h http.Header, body []byte) (push, bool, error) {
	switch {
	case h.Get("X-GitHub-Event") != "", h.Get("X-Gitea-Event") != "", h.Get("X-Gogs-Event") != "":
		provider, event := "github", h.Get("X-GitHub-Event")
		if v := h.Get("X-Gitea-Event"); v != "" {
			provider, event = "gitea", v
		} else if v := h.Get("X-Gogs-Event"); v != "" {
			provider, event = "gogs", v
		}
		if event != "push" {
			return push{}, false, nil
		}
		var p struct {
			Ref        string `json:"ref"`
			Repository struct {
				HTMLURL       string `json:"html_url"`
				CloneURL      string `json:"clone_url"`
				SSHURL        string `json:"ssh_url"`
				GitURL        string `json:"git_url"`
				DefaultBranch string `json:"default_branch"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return push{}, false, err
		}
		r := p.Repository
		return push{provider: provider, urls: []string{r.HTMLURL, r.CloneURL, r.SSHURL, r.GitURL}, refs: []string{p.Ref}, defaultBranch: r.DefaultBranch}, true, nil

	case h.Get("X-Gitlab-Event") != "":
		if e := h.Get("X-Gitlab-Event"); e != "Push Hook" && e != "Tag Push Hook" && e != "System Hook" {
			return push{}, false, nil
		}
		var p struct {
			ObjectKind string `json:"object_kind"`
			Ref        string `json:"ref"`
			Project    struct {
				WebURL        string `json:"web_url"`
				GitSSHURL     string `json:"git_ssh_url"`
				GitHTTPURL    string `json:"git_http_url"`
				DefaultBranch string `json:"default_branch"`
			} `json:"project"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return push{}, false, err
		}
		if p.ObjectKind != "push" && p.ObjectKind != "tag_push" {
			return push{}, false, nil
		}
		pr := p.Project
		return push{provider: "gitlab", urls: []string{pr.WebURL, pr.GitSSHURL, pr.GitHTTPURL}, refs: []string{p.Ref}, defaultBranch: pr.DefaultBranch}, true, nil

	case h.Get("X-Event-Key") == "repo:push": // Bitbucket Cloud
		var p struct {
			Push struct {
				Changes []struct {
					New *struct {
						Type string `json:"type"`
						Name string `json:"name"`
					} `json:"new"`
				} `json:"changes"`
			} `json:"push"`
			Repository struct {
				FullName string `json:"full_name"`
				Links    struct {
					HTML struct {
						Href string `json:"href"`
					} `json:"html"`
				} `json:"links"`
				MainBranch *struct {
					Name string `json:"name"`
				} `json:"mainbranch"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return push{}, false, err
		}
		ev := push{provider: "bitbucket"}
		if fn := p.Repository.FullName; fn != "" {
			ev.urls = append(ev.urls, "https://bitbucket.org/"+fn, "git@bitbucket.org:"+fn+".git")
		}
		ev.urls = append(ev.urls, p.Repository.Links.HTML.Href)
		if p.Repository.MainBranch != nil {
			ev.defaultBranch = p.Repository.MainBranch.Name
		}
		for _, c := range p.Push.Changes {
			if c.New == nil {
				continue // a deleted ref
			}
			switch c.New.Type {
			case "branch":
				ev.refs = append(ev.refs, "refs/heads/"+c.New.Name)
			case "tag", "annotated_tag":
				ev.refs = append(ev.refs, "refs/tags/"+c.New.Name)
			}
		}
		return ev, true, nil

	case h.Get("X-Event-Key") == "repo:refs_changed": // Bitbucket Server / Data Center
		var p struct {
			Changes []struct {
				Ref struct {
					ID string `json:"id"`
				} `json:"ref"`
				Type string `json:"type"`
			} `json:"changes"`
			Repository struct {
				Links struct {
					Clone []struct {
						Href string `json:"href"`
					} `json:"clone"`
				} `json:"links"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &p); err != nil {
			return push{}, false, err
		}
		ev := push{provider: "bitbucket-server"}
		for _, c := range p.Repository.Links.Clone {
			ev.urls = append(ev.urls, c.Href)
		}
		for _, c := range p.Changes {
			if c.Type != "DELETE" {
				ev.refs = append(ev.refs, c.Ref.ID)
			}
		}
		return ev, true, nil
	}
	return push{}, false, nil
}

// canonicalRepoURL reduces the URL forms of one repository to one key:
// host and path, lowercased, without scheme, user, port or .git.
// https://user@Git.example.com:443/org/app.git, git@git.example.com:org/app
// and ssh://git@git.example.com/org/app/ are all git.example.com/org/app.
func canonicalRepoURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		s = u.Hostname() + u.Path
	} else if at := strings.Index(s, "@"); at >= 0 {
		// scp-like: [user@]host:path
		host, path, ok := strings.Cut(s[at+1:], ":")
		if !ok {
			return ""
		}
		s = host + "/" + path
	} else if host, path, ok := strings.Cut(s, ":"); ok && !strings.Contains(host, "/") {
		s = host + "/" + path
	}
	s = strings.TrimSuffix(strings.TrimRight(s, "/"), ".git")
	s = strings.ReplaceAll(s, "//", "/")
	return strings.ToLower(s)
}

// revisionPushed reports whether a push concerns an Application tracking
// revision: its branch or tag was pushed, or it tracks HEAD and the
// default branch was pushed (or the provider does not say which branch is
// the default). A commit hash never moves.
func revisionPushed(revision string, ev push) bool {
	for _, ref := range ev.refs {
		branch, isBranch := strings.CutPrefix(ref, "refs/heads/")
		tag, isTag := strings.CutPrefix(ref, "refs/tags/")
		switch {
		case revision == "" || revision == "HEAD":
			if isBranch && (ev.defaultBranch == "" || branch == ev.defaultBranch) {
				return true
			}
		case revision == ref:
			return true
		case isBranch && revision == branch, isTag && revision == tag:
			return true
		}
	}
	return false
}

func firstOf(list []string) string {
	for _, s := range list {
		if s != "" {
			return s
		}
	}
	return ""
}
