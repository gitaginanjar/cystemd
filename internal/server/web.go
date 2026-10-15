// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/a-h/templ"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/git"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/ui"
	"github.com/gitaginanjar/cystemd/ui/views"
)

// The web UI is server-rendered: templ components (ui/views) turned into
// HTML here, with htmx swapping fragments for interactivity. Every handler
// renders a full page for a normal (or boosted, or history) request, and
// only the fragment named by HX-Target for an htmx request — one URL per
// page state, so every view, panel and tab can be bookmarked. Writes go
// through the same operations as the JSON API (RBAC, audit, events).

// uiCSP is the standalone server's Content-Security-Policy for the UI: no
// inline script, same-origin requests and assets only. In agent mode Nomad
// applies its own ui.content_security_policy instead.
const uiCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"connect-src 'self'; form-action 'none'; frame-ancestors 'none'; base-uri 'self'"

const (
	themeCookie     = "cystemd.theme"
	favoritesCookie = "cystemd.favorites"
	flashCookie     = "cystemd.flash"
)

// UIHandler returns the web UI for mounting under /ui/ (paths keep the
// /ui/ prefix). The agent mounts it through the fork's UI hook, where Nomad
// adds its Content-Security-Policy.
func (s *Server) UIHandler() http.Handler { return s.webHandler(false) }

func (s *Server) webHandler(csp bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /ui/static/", ui.Handler())
	mux.HandleFunc("GET /ui/{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/applications", http.StatusTemporaryRedirect)
	})

	s.webRoute(mux, "GET /ui/login", true, s.webLoginPage)
	s.webRoute(mux, "POST /ui/login", true, s.webLogin)
	s.webRoute(mux, "POST /ui/logout", true, s.webLogout)
	s.webRoute(mux, "GET /ui/auth/login", true, s.webSSOLogin)
	s.webRoute(mux, "GET "+ssoCallbackPath, true, s.webSSOCallback)

	s.webRoute(mux, "GET /ui/applications", false, s.webApps)
	s.webRoute(mux, "POST /ui/applications", false, s.webCreateApp)
	s.webRoute(mux, "POST /ui/applications/new/yaml", false, s.webNewAppYAML)
	s.webRoute(mux, "POST /ui/applications/new/form", false, s.webNewAppForm)
	s.webRoute(mux, "POST /ui/applications/sync", false, s.webSyncApps)
	s.webRoute(mux, "POST /ui/applications/refresh", false, s.webRefreshApps)
	s.webRoute(mux, "GET /ui/applications/{name}", false, s.webApp)
	s.webRoute(mux, "POST /ui/applications/{name}/sync", false, s.webSyncApp)
	s.webRoute(mux, "POST /ui/applications/{name}/refresh", false, s.webRefreshApp)
	s.webRoute(mux, "POST /ui/applications/{name}/rollback", false, s.webRollback)
	s.webRoute(mux, "POST /ui/applications/{name}/terminate", false, s.webTerminate)
	s.webRoute(mux, "POST /ui/applications/{name}/delete", false, s.webDeleteApp)
	s.webRoute(mux, "POST /ui/applications/{name}/spec", false, s.webUpdateSpec)
	s.webRoute(mux, "POST /ui/applications/{name}/manifest", false, s.webUpdateManifest)
	s.webRoute(mux, "POST /ui/applications/{name}/autosync", false, s.webAutoSync)
	s.webRoute(mux, "POST /ui/applications/{name}/favorite", false, s.webFavorite)
	s.webRoute(mux, "POST /ui/applications/{name}/actions", false, s.webResourceAction)
	s.webRoute(mux, "GET /ui/applications/{name}/logs", false, s.webLogStream)
	s.webRoute(mux, "GET /ui/applications/{name}/logs/download", false, s.webLogDownload)
	s.webRoute(mux, "GET /ui/repositories/refs", false, s.webRepoRefs)

	s.webRoute(mux, "GET /ui/stream/applications", false, s.webStreamApps)
	s.webRoute(mux, "GET /ui/stream/applications/{name}", false, s.webStreamApp)

	s.webRoute(mux, "GET /ui/settings", false, s.webSettings)
	s.webRoute(mux, "GET /ui/settings/repos", false, s.webRepos)
	s.webRoute(mux, "POST /ui/settings/repos", false, s.webConnectRepo)
	s.webRoute(mux, "POST /ui/settings/repos/delete", false, s.webDisconnectRepo)
	s.webRoute(mux, "GET /ui/settings/projects", false, s.webProjects)
	s.webRoute(mux, "POST /ui/settings/projects", false, s.webCreateProject)
	s.webRoute(mux, "GET /ui/settings/projects/{name}", false, s.webProject)
	s.webRoute(mux, "POST /ui/settings/projects/{name}", false, s.webUpdateProject)
	s.webRoute(mux, "POST /ui/settings/projects/{name}/delete", false, s.webDeleteProject)
	s.webRoute(mux, "GET /ui/settings/clusters", false, s.webClusters)
	s.webRoute(mux, "GET /ui/settings/clusters/{name}", false, s.webCluster)
	s.webRoute(mux, "GET /ui/settings/accounts", false, s.webAccounts)
	s.webRoute(mux, "GET /ui/settings/appearance", false, s.webAppearance)
	s.webRoute(mux, "POST /ui/settings/appearance", false, s.webSetAppearance)
	s.webRoute(mux, "GET /ui/user-info", false, s.webUserInfo)
	s.webRoute(mux, "POST /ui/user-info/password", false, s.webUpdatePassword)
	s.webRoute(mux, "/ui/", false, func(w http.ResponseWriter, r *http.Request) {
		s.webFail(w, r, notFound("page not found"))
	})

	var h http.Handler = mux
	if csp {
		h = withCSP(h)
	}
	return h
}

func withCSP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", uiCSP)
		next.ServeHTTP(w, r)
	})
}

// webRoute registers a UI route with metrics, recovery, authentication
// (redirecting to the login page) and, for writes, the CSRF check.
func (s *Server) webRoute(mux *http.ServeMux, pattern string, public bool, h http.HandlerFunc) {
	label := pattern
	if i := strings.Index(label, " "); i >= 0 {
		label = label[i+1:]
	}
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if p := recover(); p != nil {
				s.logger.Error("panic in UI handler", "route", label, "panic", p, "stack", string(debug.Stack()))
				if !rec.wrote {
					http.Error(rec, "internal error", http.StatusInternalServerError)
				}
			}
			s.Metrics.HTTPRequests.WithLabelValues(label, r.Method, strconv.Itoa(rec.status)).Inc()
			s.Metrics.HTTPDuration.WithLabelValues(label).Observe(time.Since(start).Seconds())
		}()
		if r.Method == http.MethodPost && !sameOriginHTMX(r) {
			// Every UI write is an htmx request: a cross-site form cannot
			// add the HX-Request header (CSRF defence with SameSite).
			http.Error(rec, "cross-site or non-htmx write rejected", http.StatusForbidden)
			return
		}
		if !public {
			id, err := s.Auth.FromRequest(r)
			if err != nil {
				s.loginRedirect(rec, r)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), identityKey{}, id))
		}
		h(rec, r)
	})
}

func sameOriginHTMX(r *http.Request) bool {
	if r.Header.Get("HX-Request") != "true" {
		return false
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil {
			return false
		}
		host := r.Host
		if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
			host = fh
		}
		if u.Host != host && u.Host != r.Host {
			return false
		}
	}
	return true
}

// isHTMX reports whether r is an htmx request (any kind).
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// htmxTarget is the id of the element an htmx request swaps, or "" when
// the request wants a full page (normal, boosted or history restore).
func htmxTarget(r *http.Request) string {
	if !isHTMX(r) || r.Header.Get("HX-Boosted") == "true" || r.Header.Get("HX-History-Restore-Request") == "true" {
		return ""
	}
	return r.Header.Get("HX-Target")
}

// pageQuery is the page state of a request. A live refresh (_live=1, sent
// when a server event says something changed) re-renders the page the
// browser shows, whose state is in HX-Current-URL.
func pageQuery(r *http.Request) url.Values {
	q := r.URL.Query()
	if q.Get("_live") != "1" {
		return q
	}
	if cu, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && cu.Path == r.URL.Path {
		return cu.Query()
	}
	q.Del("_live")
	return q
}

// safeReturn accepts only local UI paths as a redirect target.
func safeReturn(u string) string {
	if strings.HasPrefix(u, "/ui/") && !strings.HasPrefix(u, "//") && !strings.ContainsAny(u, "\\\r\n") && !strings.HasPrefix(u, "/ui/login") {
		return u
	}
	return "/ui/applications"
}

func (s *Server) loginRedirect(w http.ResponseWriter, r *http.Request) {
	ret := r.URL.RequestURI()
	if cu, err := url.Parse(r.Header.Get("HX-Current-URL")); err == nil && cu.Path != "" {
		ret = cu.RequestURI()
	}
	target := "/ui/login?return_url=" + url.QueryEscape(safeReturn(ret))
	switch {
	case isHTMX(r):
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusUnauthorized)
	case strings.Contains(r.Header.Get("Accept"), "text/event-stream"):
		w.WriteHeader(http.StatusUnauthorized)
	default:
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
}

// render writes a component; it is rendered first so a failure becomes a
// clean 500 instead of a truncated page.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		s.logger.Error("UI render failed", "path", r.URL.Path, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Vary", "HX-Request, HX-Target")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// webFail reports an error: as a notification for htmx requests (the page
// stays as it is), as an error page otherwise.
func (s *Server) webFail(w http.ResponseWriter, r *http.Request, err error) {
	code := errorCode(err)
	if code >= 500 {
		s.logger.Warn("UI request failed", "path", r.URL.Path, "error", err)
	}
	if htmxTarget(r) != "" || (isHTMX(r) && r.Method != http.MethodGet) {
		w.Header().Set("HX-Reswap", "none")
		s.render(w, r, code, views.Toasts(views.Toast{Kind: "error", Message: err.Error()}))
		return
	}
	title := http.StatusText(code)
	s.render(w, r, code, views.ErrorView(views.ErrorPage{Page: s.page(w, r, title, "", title, views.Crumb{Label: title}), Code: code, Message: err.Error()}))
}

// notify answers a write with notifications only (the page stays).
func (s *Server) notify(w http.ResponseWriter, r *http.Request, toasts ...views.Toast) {
	w.Header().Set("HX-Reswap", "none")
	s.render(w, r, http.StatusOK, views.Toasts(toasts...))
}

// closePanel answers a panel form: the panel closes (and the URL with it)
// and notifications are shown.
func (s *Server) closePanel(w http.ResponseWriter, r *http.Request, closeHref string, toasts ...views.Toast) {
	w.Header().Set("HX-Push-Url", safeReturn(closeHref))
	s.render(w, r, http.StatusOK, templ.Join(views.EmptyPanel(), views.Toasts(toasts...)))
}

// navigate loads another page (without a full reload) and shows toasts on
// it through a flash cookie.
func (s *Server) navigate(w http.ResponseWriter, path string, toasts ...views.Toast) {
	if len(toasts) > 0 {
		setFlash(w, toasts[0])
	}
	w.Header().Set("HX-Location", `{"path":`+strconv.Quote(path)+`,"target":"body"}`)
	w.WriteHeader(http.StatusOK)
}

func success(msg string) views.Toast { return views.Toast{Kind: "success", Message: msg} }

func failure(msg string) views.Toast { return views.Toast{Kind: "error", Message: msg} }

func setFlash(w http.ResponseWriter, t views.Toast) {
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: url.QueryEscape(t.Kind + ":" + t.Message), Path: "/ui/",
		MaxAge: 30, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// takeFlash returns the pending flash message and clears it.
func takeFlash(w http.ResponseWriter, r *http.Request) []views.Toast {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return nil
	}
	http.SetCookie(w, &http.Cookie{Name: flashCookie, Value: "", Path: "/ui/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return nil
	}
	kind, msg, ok := strings.Cut(v, ":")
	if !ok || (kind != "success" && kind != "error" && kind != "warning") || len(msg) > 500 {
		return nil
	}
	return []views.Toast{{Kind: kind, Message: msg}}
}

// page builds the frame of a signed-in page.
func (s *Server) page(w http.ResponseWriter, r *http.Request, title, nav, heading string, crumbs ...views.Crumb) views.Page {
	id := identity(r)
	p := views.Page{
		Title:   title,
		Nav:     nav,
		Heading: heading,
		Crumbs:  crumbs,
		User: views.User{
			Name:    id.Username,
			IsAdmin: s.RBAC.Enforce(id.Username, id.Groups, "accounts", "update", "*"),
			Local:   id.Provider == auth.ProviderLocal,
		},
		Version: s.Build.Version,
		Theme:   theme(r),
	}
	if htmxTarget(r) == "" && r.Method == http.MethodGet {
		p.Toasts = takeFlash(w, r)
	}
	return p
}

func theme(r *http.Request) string {
	if c, err := r.Cookie(themeCookie); err == nil {
		switch c.Value {
		case "light", "dark", "auto":
			return c.Value
		}
	}
	return "light"
}

// favorites are kept per browser, in a cookie (as Argo CD keeps them in
// local storage).
func favorites(r *http.Request) map[string]bool {
	out := map[string]bool{}
	c, err := r.Cookie(favoritesCookie)
	if err != nil {
		return out
	}
	v, err := url.QueryUnescape(c.Value)
	if err != nil {
		return out
	}
	for _, n := range strings.Split(v, ",") {
		if n != "" {
			out[n] = true
		}
	}
	return out
}

func setFavorites(w http.ResponseWriter, favs map[string]bool) {
	names := make([]string, 0, len(favs))
	for n, on := range favs {
		if on {
			names = append(names, n)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name: favoritesCookie, Value: url.QueryEscape(strings.Join(names, ",")), Path: "/ui/",
		MaxAge: 400 * 24 * 3600, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// allowed checks a permission without auditing (for showing UI controls).
func (s *Server) allowed(r *http.Request, resource, action, object string) bool {
	id := identity(r)
	return s.RBAC.Enforce(id.Username, id.Groups, resource, action, object)
}

// formValues parses a form body (URL-encoded, bounded).
func formValues(r *http.Request) (url.Values, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		return nil, badRequest("invalid form: %v", err)
	}
	return r.PostForm, nil
}

// commitCache keeps commit metadata (immutable per hash) for the status
// panel, which live updates re-render often.
type commitCache struct {
	mu sync.Mutex
	m  map[string]*git.CommitMetadata
}

func (c *commitCache) get(key string) (*git.CommitMetadata, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	md, ok := c.m[key]
	return md, ok
}

func (c *commitCache) put(key string, md *git.CommitMetadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 512 {
		c.m = map[string]*git.CommitMetadata{}
	}
	c.m[key] = md
}

// commit returns a revision's metadata, nil when unavailable.
func (s *Server) commit(ctx context.Context, repoURL, sha string) *views.Commit {
	if s.Repos == nil || !views.IsSHA(sha) {
		return nil
	}
	key := git.NormalizeURL(repoURL) + "@" + sha
	md, ok := s.commits.get(key)
	if !ok {
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		var err error
		if md, err = s.Repos.Commit(ctx, repoURL, sha); err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				s.commits.put(key, nil)
			}
			return nil
		}
		s.commits.put(key, md)
	}
	if md == nil {
		return nil
	}
	return &views.Commit{SHA: sha, Author: md.Author, Message: md.Message, Date: md.Date, URL: views.CommitURL(repoURL, sha)}
}

// ─── login ──────────────────────────────────────────────────────────────

func (s *Server) webLoginPage(w http.ResponseWriter, r *http.Request) {
	ret := safeReturn(r.URL.Query().Get("return_url"))
	if id, err := s.Auth.FromRequest(r); err == nil && id.Provider != auth.ProviderAnonymous {
		http.Redirect(w, r, ret, http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, views.LoginView(s.loginPage(r, ret, "")))
}

func (s *Server) loginPage(r *http.Request, ret, errMsg string) views.LoginPage {
	return views.LoginPage{
		Version:         s.Build.Version,
		ReturnURL:       ret,
		Error:           errMsg,
		LocalEnabled:    true,
		NomadTokenLogin: *s.Config.Auth.NomadTokenLogin,
		SSOMethods:      s.ssoMethods(r.Context()),
		Theme:           theme(r),
	}
}

// webLoginFailed shows the login page with an error (a failed single
// sign-on, which comes back as a full page load).
func (s *Server) webLoginFailed(w http.ResponseWriter, r *http.Request, code int, msg string) {
	s.render(w, r, code, views.LoginView(s.loginPage(r, safeReturn(r.URL.Query().Get("return_url")), msg)))
}

func (s *Server) webLogin(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.render(w, r, http.StatusBadRequest, views.LoginError(err.Error()))
		return
	}
	token, _, err := s.login(r, loginRequest{
		Username:   strings.TrimSpace(form.Get("username")),
		Password:   form.Get("password"),
		NomadToken: strings.TrimSpace(form.Get("nomad_token")),
	})
	if err != nil {
		s.render(w, r, errorCode(err), views.LoginError(loginMessage(err)))
		return
	}
	s.setSessionCookie(w, r, token)
	w.Header().Set("HX-Redirect", safeReturn(form.Get("return_url")))
	w.WriteHeader(http.StatusOK)
}

// loginMessage keeps failures vague (no account enumeration) except for
// throttling, which the user must know about.
func loginMessage(err error) string {
	switch errorCode(err) {
	case http.StatusTooManyRequests:
		return "Too many failed attempts. Wait a minute and try again."
	case http.StatusForbidden:
		return err.Error()
	}
	return "Invalid username, password or token."
}

func (s *Server) webLogout(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w)
	w.Header().Set("HX-Redirect", "/ui/login")
	w.WriteHeader(http.StatusOK)
}

// ─── shared lookups ─────────────────────────────────────────────────────

// formOptions lists the projects, clusters and repositories the caller may
// use in application forms.
func (s *Server) formOptions(r *http.Request) views.FormOptions {
	var o views.FormOptions
	if projects, err := s.listProjects(r); err == nil {
		for _, p := range projects {
			o.Projects = append(o.Projects, p.Metadata.Name)
		}
	}
	if len(o.Projects) == 0 {
		o.Projects = []string{v1.DefaultProject}
	}
	o.Clusters = s.Clusters.Names()
	if s.allowed(r, "repositories", "get", "*") {
		if repos, err := s.listRepos(r); err == nil {
			for _, rp := range repos {
				o.Repos = append(o.Repos, rp.Repo)
			}
		}
	}
	return o
}

func (s *Server) webRepoRefs(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repoURL"))
	var branches, tags []string
	if repo != "" && s.allowed(r, "repositories", "get", repo) {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		branches, tags, _ = s.Git.Refs(ctx, repo)
	}
	s.render(w, r, http.StatusOK, views.RefOptions(branches, tags))
}
