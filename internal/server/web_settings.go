// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/gitaginanjar/cystemd/internal/auth"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
	"github.com/gitaginanjar/cystemd/ui/views"
)

var settingsCrumb = views.Crumb{Label: "Settings", Href: "/ui/settings"}

// renderPage renders a full page, or only the panel for panel requests.
func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, panel templ.Component, page func(templ.Component) templ.Component) {
	if panel == nil {
		panel = views.EmptyPanel()
	}
	if htmxTarget(r) == "panel" {
		s.render(w, r, http.StatusOK, panel)
		return
	}
	s.render(w, r, http.StatusOK, page(panel))
}

func (s *Server) webSettings(w http.ResponseWriter, r *http.Request) {
	h := s.healthDetail(r.Context())
	p := views.SettingsPage{
		Page:   s.page(w, r, "Settings", "settings", "Settings", settingsCrumb),
		Leader: h.Leader,
		Mode:   s.Build.Mode,
	}
	for _, c := range h.Nomad {
		p.Health = append(p.Health, views.ComponentHealth{Kind: "Nomad cluster", Name: c.Name, Healthy: c.Healthy, Message: c.Message})
	}
	if h.Consul != nil {
		p.Health = append(p.Health, views.ComponentHealth{Kind: "Consul", Name: h.Consul.Name, Healthy: h.Consul.Healthy, Message: h.Consul.Message})
	}
	if h.Vault != nil {
		p.Health = append(p.Health, views.ComponentHealth{Kind: "Vault", Name: h.Vault.Name, Healthy: h.Vault.Healthy, Message: h.Vault.Message})
	}
	s.render(w, r, http.StatusOK, views.SettingsView(p))
}

// ─── repositories ───────────────────────────────────────────────────────

func (s *Server) projectNames(r *http.Request) []string {
	var out []string
	if projects, err := s.listProjects(r); err == nil {
		for _, p := range projects {
			out = append(out, p.Metadata.Name)
		}
	}
	return out
}

func (s *Server) webRepos(w http.ResponseWriter, r *http.Request) {
	p := views.ReposPage{
		Page:      s.page(w, r, "Repositories", "settings", "Repositories", settingsCrumb, views.Crumb{Label: "Repositories"}),
		CanCreate: s.allowed(r, "repositories", "create", "*"),
		CanDelete: s.allowed(r, "repositories", "delete", "*"),
	}
	repos, err := s.listRepos(r)
	if err != nil {
		p.Error = err.Error()
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Repo < repos[j].Repo })
	p.Repos = repos
	var panel templ.Component
	if r.URL.Query().Get("panel") == "connect" && p.CanCreate {
		panel = views.ConnectRepoPanelView(views.ConnectRepoPanel{Form: views.RepoForm{Method: "https"}, Projects: s.projectNames(r), CloseHref: "/ui/settings/repos"})
	}
	s.renderPage(w, r, panel, func(panel templ.Component) templ.Component {
		p.Panel = panel
		return views.ReposView(p)
	})
}

func (s *Server) webConnectRepo(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	repo, f := repoFromForm(form)
	out, err := s.createRepo(r, repo, f.Force)
	if err != nil {
		f.Password, f.SSHPrivateKey = "", ""
		s.render(w, r, errorCode(err), views.ConnectRepoPanelView(views.ConnectRepoPanel{
			Form: f, Projects: s.projectNames(r), Error: err.Error(), CloseHref: "/ui/settings/repos",
		}))
		return
	}
	msg := "Repository " + out.Repo + " connected."
	if out.ConnectionState.Status == v1.ConnectionFailed {
		msg = "Repository " + out.Repo + " saved, but the connection failed: " + out.ConnectionState.Message
	}
	s.navigate(w, "/ui/settings/repos", success(msg))
}

func (s *Server) webDisconnectRepo(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if err := s.deleteRepo(r, repo); err != nil {
		s.webFail(w, r, err)
		return
	}
	s.navigate(w, "/ui/settings/repos", success("Repository "+repo+" disconnected."))
}

// ─── projects ───────────────────────────────────────────────────────────

func (s *Server) webProjects(w http.ResponseWriter, r *http.Request) {
	p := views.ProjectsPage{
		Page:      s.page(w, r, "Projects", "settings", "Projects", settingsCrumb, views.Crumb{Label: "Projects"}),
		CanCreate: s.allowed(r, "projects", "create", "*"),
		AppCounts: map[string]int{},
	}
	projects, err := s.listProjects(r)
	if err != nil {
		p.Error = err.Error()
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Metadata.Name < projects[j].Metadata.Name })
	p.Projects = projects
	if apps, err := s.listApps(r, nil); err == nil {
		for _, a := range apps {
			p.AppCounts[a.Spec.Project]++
		}
	}
	var panel templ.Component
	if r.URL.Query().Get("panel") == "new" && p.CanCreate {
		panel = views.ProjectPanelView(views.ProjectPanel{New: true, Form: views.ProjectForm{SourceRepos: "*", Destinations: "*/*"}, CloseHref: "/ui/settings/projects"})
	}
	s.renderPage(w, r, panel, func(panel templ.Component) templ.Component {
		p.Panel = panel
		return views.ProjectsView(p)
	})
}

func (s *Server) webCreateProject(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	proj, f, err := projectFromForm(form)
	if err == nil {
		err = s.createProject(r, proj)
	}
	if err != nil {
		s.render(w, r, errorCode(err), views.ProjectPanelView(views.ProjectPanel{New: true, Form: f, Error: err.Error(), CloseHref: "/ui/settings/projects"}))
		return
	}
	s.navigate(w, "/ui/settings/projects/"+url.PathEscape(proj.Metadata.Name), success("Project "+proj.Metadata.Name+" created."))
}

func (s *Server) webProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	proj, err := s.getProject(r, name)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	p := views.ProjectPage{
		Page: s.page(w, r, "Project "+name, "settings", "Project details",
			settingsCrumb, views.Crumb{Label: "Projects", Href: "/ui/settings/projects"}, views.Crumb{Label: name}),
		Project:   proj,
		CanUpdate: s.allowed(r, "projects", "update", name),
		CanDelete: s.allowed(r, "projects", "delete", name),
	}
	p.Apps, _ = s.listApps(r, func(a *v1.Application) bool { return a.Spec.Project == name })
	sort.Slice(p.Apps, func(i, j int) bool { return p.Apps[i].Metadata.Name < p.Apps[j].Metadata.Name })
	var panel templ.Component
	if r.URL.Query().Get("panel") == "edit" && p.CanUpdate {
		panel = views.ProjectPanelView(views.ProjectPanel{Form: projectForm(proj), CloseHref: "/ui/settings/projects/" + url.PathEscape(name)})
	}
	s.renderPage(w, r, panel, func(panel templ.Component) templ.Component {
		p.Panel = panel
		return views.ProjectView(p)
	})
}

func (s *Server) webUpdateProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	form.Set("name", name)
	proj, f, err := projectFromForm(form)
	if err == nil {
		// The form edits the summary; the rest is kept.
		var existing *v1.AppProject
		if existing, err = s.getProject(r, name); err == nil {
			proj.Spec.SyncWindows = existing.Spec.SyncWindows
			proj.Spec.OrphanedResources = existing.Spec.OrphanedResources
			proj.Spec.Roles = existing.Spec.Roles
			err = s.updateProject(r, proj)
		}
	}
	if err != nil {
		s.render(w, r, errorCode(err), views.ProjectPanelView(views.ProjectPanel{Form: f, Error: err.Error(), CloseHref: "/ui/settings/projects/" + url.PathEscape(name)}))
		return
	}
	s.navigate(w, "/ui/settings/projects/"+url.PathEscape(name), success("Project "+name+" updated."))
}

func (s *Server) webDeleteProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.deleteProject(r, name); err != nil {
		s.webFail(w, r, err)
		return
	}
	s.navigate(w, "/ui/settings/projects", success("Project "+name+" deleted."))
}

// ─── clusters ───────────────────────────────────────────────────────────

func clusterView(c Cluster) views.ClusterView {
	return views.ClusterView{
		Name: c.Name, Server: c.Server, Version: c.Version, Region: c.Region, Regions: c.Regions,
		Namespaces: c.Namespaces, NodePools: c.NodePools, Nodes: c.Nodes, ReadyNodes: c.ReadyNodes,
		Applications: c.Applications, Status: c.ConnectionState.Status, Message: c.ConnectionState.Message,
	}
}

func (s *Server) webClusters(w http.ResponseWriter, r *http.Request) {
	p := views.ClustersPage{Page: s.page(w, r, "Clusters", "settings", "Clusters", settingsCrumb, views.Crumb{Label: "Clusters"})}
	clusters, err := s.listClusters(r)
	if err != nil {
		p.Error = err.Error()
	}
	for _, c := range clusters {
		p.Clusters = append(p.Clusters, clusterView(c))
	}
	s.render(w, r, http.StatusOK, views.ClustersView(p))
}

func (s *Server) webCluster(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p := views.ClusterPage{Page: s.page(w, r, "Cluster "+name, "settings", "Cluster details",
		settingsCrumb, views.Crumb{Label: "Clusters", Href: "/ui/settings/clusters"}, views.Crumb{Label: name})}
	clusters, err := s.listClusters(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	found := false
	for _, c := range clusters {
		if c.Name == name {
			p.Cluster, found = clusterView(c), true
		}
	}
	if !found {
		s.webFail(w, r, notFound("cluster %q not found", name))
		return
	}
	nodes, err := s.clusterNodes(r, name)
	if err != nil {
		p.Error = err.Error()
	}
	for _, n := range nodes {
		p.Nodes = append(p.Nodes, views.NodeView{
			ID: n.ID, Name: n.Name, Datacenter: n.Datacenter, NodePool: n.NodePool, Status: n.Status,
			Address: n.Address, Version: n.Version, Eligible: n.Eligible, Drain: n.Drain, Drivers: n.Drivers,
		})
	}
	s.render(w, r, http.StatusOK, views.ClusterDetailView(p))
}

// ─── accounts, appearance, user info ────────────────────────────────────

func accountView(a AccountInfo) views.AccountView {
	v := views.AccountView{Name: a.Name, Enabled: a.Enabled, Capabilities: a.Capabilities, Groups: a.Groups, PasswordMtime: a.PasswordMtime}
	for _, t := range a.Tokens {
		v.Tokens = append(v.Tokens, views.TokenView{ID: t.ID, IssuedAt: t.IssuedAt, ExpiresAt: t.ExpiresAt})
	}
	return v
}

func (s *Server) accountTokens(r *http.Request, a *AccountInfo) views.AccountTokens {
	return views.AccountTokens{
		Account: a.Name, APIKey: slices.Contains(a.Capabilities, auth.CapabilityAPIKey),
		CanUpdate: s.mayUpdateAccount(r, a.Name), Tokens: accountView(*a).Tokens,
	}
}

func (s *Server) webAccount(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a, err := s.getAccount(r, name)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	crumbs := []views.Crumb{settingsCrumb, {Label: "Accounts", Href: "/ui/settings/accounts"}, {Label: name}}
	if !s.allowed(r, "accounts", "get", "*") {
		crumbs = []views.Crumb{{Label: "User info", Href: "/ui/user-info"}, {Label: name}}
	}
	p := views.AccountPage{
		Page:    s.page(w, r, "Account "+name, "settings", "Account", crumbs...),
		Account: accountView(*a), Tokens: s.accountTokens(r, a),
	}
	s.render(w, r, http.StatusOK, views.AccountDetailView(p))
}

// webGenerateToken generates an API token and shows it, once, in the
// account's tokens section.
func (s *Server) webGenerateToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	a, err := s.getAccount(r, name)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	t := s.accountTokens(r, a)
	t.ExpiresIn, t.ID = form.Get("expiresIn"), form.Get("id")
	ttl, err := client.ParseLifetime(t.ExpiresIn)
	if err != nil {
		err = badRequest("expires in: %v", err)
	}
	var res *TokenResponse
	if err == nil {
		res, err = s.generateToken(r, name, tokenRequest{ID: strings.TrimSpace(t.ID), ExpiresIn: int64(ttl / time.Second)})
	}
	if err != nil {
		t.Error = err.Error()
		s.render(w, r, errorCode(err), views.AccountTokensView(t))
		return
	}
	if a, err = s.getAccount(r, name); err == nil {
		t.Tokens = accountView(*a).Tokens
	}
	t.NewToken, t.NewID, t.ExpiresIn, t.ID = res.Token, res.ID, "", ""
	s.render(w, r, http.StatusOK, templ.Join(views.AccountTokensView(t), views.Toasts(success("Token "+res.ID+" generated."))))
}

func (s *Server) webDeleteToken(w http.ResponseWriter, r *http.Request) {
	name, id := r.PathValue("name"), r.PathValue("id")
	if err := s.deleteToken(r, name, id); err != nil {
		s.webFail(w, r, err)
		return
	}
	a, err := s.getAccount(r, name)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, templ.Join(views.AccountTokensView(s.accountTokens(r, a)), views.Toasts(success("Token "+id+" deleted."))))
}

func (s *Server) webAccounts(w http.ResponseWriter, r *http.Request) {
	p := views.AccountsPage{Page: s.page(w, r, "Accounts", "settings", "Accounts", settingsCrumb, views.Crumb{Label: "Accounts"})}
	accts, err := s.listAccounts(r)
	if err != nil {
		p.Error = err.Error()
	}
	for _, a := range accts {
		p.Accounts = append(p.Accounts, accountView(a))
	}
	s.render(w, r, http.StatusOK, views.AccountsView(p))
}

func (s *Server) webAppearance(w http.ResponseWriter, r *http.Request) {
	p := views.AppearancePage{Page: s.page(w, r, "Appearance", "settings", "Appearance", settingsCrumb, views.Crumb{Label: "Appearance"})}
	s.render(w, r, http.StatusOK, views.AppearanceView(p))
}

func (s *Server) webSetAppearance(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	t := form.Get("theme")
	switch t {
	case "light", "dark", "auto":
	default:
		s.webFail(w, r, badRequest("unknown theme %q", t))
		return
	}
	http.SetCookie(w, &http.Cookie{Name: themeCookie, Value: t, Path: "/ui/", MaxAge: 400 * 24 * 3600, SameSite: http.SameSiteLaxMode})
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusOK)
}

func (s *Server) webUserInfo(w http.ResponseWriter, r *http.Request) {
	info := s.userInfo(r)
	p := views.UserInfoPage{
		Page:     s.page(w, r, "User info", "user-info", "User info", views.Crumb{Label: "User info"}),
		Username: info.Username, Provider: info.Provider, Groups: info.Groups, Roles: info.Roles,
	}
	if acct, err := s.account(r); err == nil {
		av := accountView(*acct)
		p.Account = &av
	}
	var panel templ.Component
	if r.URL.Query().Get("panel") == "password" && info.Provider == "local" {
		panel = views.PasswordPanelView(views.PasswordPanel{CloseHref: "/ui/user-info"})
	}
	s.renderPage(w, r, panel, func(panel templ.Component) templ.Component {
		p.Panel = panel
		return views.UserInfoView(p)
	})
}

func (s *Server) webUpdatePassword(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	p := views.PasswordPanel{CloseHref: "/ui/user-info"}
	switch {
	case form.Get("new") != form.Get("confirm"):
		err = badRequest("the new passwords do not match")
	case len(form.Get("new")) < 8:
		err = badRequest("the new password must have at least 8 characters")
	default:
		err = s.updatePassword(r, passwordRequest{CurrentPassword: form.Get("current"), NewPassword: form.Get("new")})
	}
	if err != nil {
		p.Error = err.Error()
		s.render(w, r, errorCode(err), views.PasswordPanelView(p))
		return
	}
	p.Done = true
	s.render(w, r, http.StatusOK, templ.Join(views.PasswordPanelView(p), views.Toasts(success("Password updated."))))
}
