// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/a-h/templ"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/ui/views"
)

func appsQuery(q url.Values) views.AppsQuery {
	aq := views.AppsQuery{
		View:       q.Get("view"),
		Search:     strings.TrimSpace(q.Get("search")),
		Sync:       q["sync"],
		Health:     q["health"],
		Projects:   q["proj"],
		Clusters:   q["cluster"],
		Namespaces: q["ns"],
		Labels:     strings.TrimSpace(q.Get("labels")),
		Favorites:  q.Get("fav") == "1",
	}
	switch aq.View {
	case "list", "summary":
	default:
		aq.View = "tiles"
	}
	return aq
}

func inList(list []string, v string) bool {
	if len(list) == 0 {
		return true
	}
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// matchApp applies the page filters.
func matchApp(a *v1.Application, q views.AppsQuery, favs map[string]bool) bool {
	if q.Search != "" && !strings.Contains(strings.ToLower(a.Metadata.Name), strings.ToLower(q.Search)) {
		return false
	}
	if q.Favorites && !favs[a.Metadata.Name] {
		return false
	}
	if !inList(q.Sync, views.SyncOf(a)) || !inList(q.Health, views.HealthOf(a)) || !inList(q.Projects, a.Spec.Project) ||
		!inList(q.Clusters, a.Spec.Destination.ClusterName()) || !inList(q.Namespaces, a.Spec.Destination.NamespaceOrDefault()) {
		return false
	}
	for _, sel := range splitList(q.Labels) {
		k, v, hasValue := strings.Cut(sel, "=")
		lv, ok := a.Metadata.Labels[strings.TrimSpace(k)]
		if !ok || (hasValue && lv != strings.TrimSpace(v)) {
			return false
		}
	}
	return true
}

func facet(values []string, counts map[string]int, checked []string) []views.Facet {
	out := make([]views.Facet, 0, len(values))
	for _, v := range values {
		out = append(out, views.Facet{Value: v, Count: counts[v], Checked: views.Checked(checked, v)})
	}
	return out
}

// facetValues returns the values present plus checked ones, sorted.
func facetValues(counts map[string]int, checked []string) []string {
	set := map[string]bool{}
	for v := range counts {
		set[v] = true
	}
	for _, v := range checked {
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func appFacets(apps []*v1.Application, q views.AppsQuery) views.AppFacets {
	sync, health, proj, cluster, ns := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	for _, a := range apps {
		sync[views.SyncOf(a)]++
		health[views.HealthOf(a)]++
		proj[a.Spec.Project]++
		cluster[a.Spec.Destination.ClusterName()]++
		ns[a.Spec.Destination.NamespaceOrDefault()]++
	}
	return views.AppFacets{
		Sync:       facet([]string{"Unknown", "Synced", "OutOfSync"}, sync, q.Sync),
		Health:     facet([]string{"Unknown", "Progressing", "Suspended", "Healthy", "Degraded", "Missing"}, health, q.Health),
		Projects:   facet(facetValues(proj, q.Projects), proj, q.Projects),
		Clusters:   facet(facetValues(cluster, q.Clusters), cluster, q.Clusters),
		Namespaces: facet(facetValues(ns, q.Namespaces), ns, q.Namespaces),
	}
}

// canCreateApps reports whether the caller may create applications in at
// least one project.
func (s *Server) canCreateApps(r *http.Request) bool {
	if s.allowed(r, "applications", "create", "*/*") {
		return true
	}
	projects, err := s.listProjects(r)
	if err != nil {
		return false
	}
	for _, p := range projects {
		if s.allowed(r, "applications", "create", p.Metadata.Name+"/*") {
			return true
		}
	}
	return false
}

func (s *Server) webApps(w http.ResponseWriter, r *http.Request) {
	q := pageQuery(r)
	aq := appsQuery(q)
	all, err := s.listApps(r, nil)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Metadata.Name < all[j].Metadata.Name })
	favs := favorites(r)
	var apps []*v1.Application
	for _, a := range all {
		if matchApp(a, aq, favs) {
			apps = append(apps, a)
		}
	}
	p := views.AppsPage{
		Page:      s.page(w, r, "Applications", "applications", "Applications "+aq.View, views.Crumb{Label: "Applications", Href: "/ui/applications"}),
		Q:         aq,
		Apps:      apps,
		Total:     len(all),
		Facets:    appFacets(all, aq),
		Favorites: favs,
		CanCreate: s.canCreateApps(r),
	}
	switch htmxTarget(r) {
	case "panel":
		s.render(w, r, http.StatusOK, s.appsPanel(r, q, aq, apps))
	case "apps-content":
		s.render(w, r, http.StatusOK, views.AppsContent(p))
	default:
		p.Panel = s.appsPanel(r, q, aq, apps)
		s.render(w, r, http.StatusOK, views.AppsView(p))
	}
}

// appsPanel is the sliding panel the URL asks for on the list page.
func (s *Server) appsPanel(r *http.Request, q url.Values, aq views.AppsQuery, apps []*v1.Application) templ.Component {
	closeHref := aq.Href(aq.View)
	switch q.Get("panel") {
	case "new":
		return views.NewAppPanelView(views.NewAppPanel{
			Form: newAppForm(q.Get("project"), q.Get("repo")), Mode: "form",
			Options: s.formOptions(r), CloseHref: closeHref,
		})
	case "sync-apps":
		return views.SyncAppsPanel(apps, closeHref, "", false)
	case "refresh-apps":
		return views.SyncAppsPanel(apps, closeHref, "", true)
	case "sync", "delete":
		app, err := s.loadApp(r, q.Get("app"), "get")
		if err != nil {
			return views.EmptyPanel()
		}
		if q.Get("panel") == "delete" {
			return views.DeletePanelView(views.DeletePanel{App: app, CloseHref: closeHref})
		}
		return views.SyncPanelView(views.SyncPanel{App: app, Resources: syncResources(app), CloseHref: closeHref})
	}
	return views.EmptyPanel()
}

func syncResources(app *v1.Application) []views.SyncResource {
	out := make([]views.SyncResource, 0, len(app.Status.Resources))
	for _, r := range app.Status.Resources {
		sr := views.SyncResource{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Sync: string(r.Status), Pruning: r.RequiresPruning}
		if r.Health != nil {
			sr.Health = string(r.Health.Status)
		}
		out = append(out, sr)
	}
	return out
}

// ─── bulk operations ────────────────────────────────────────────────────

func (s *Server) webSyncApps(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	names := form["app"]
	if len(names) == 0 {
		s.notify(w, r, failure("Select at least one application."))
		return
	}
	var errs []error
	ok := 0
	for _, name := range names {
		app, err := s.loadApp(r, name, "sync")
		if err == nil {
			_, err = s.syncApp(r, app, SyncRequest{
				Prune: form.Get("prune") == "true", DryRun: form.Get("dryRun") == "true", SyncOptions: form["syncOption"],
			})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		ok++
	}
	s.closePanel(w, r, form.Get("close"), bulkToasts("Sync requested for", ok, errs)...)
}

func (s *Server) webRefreshApps(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	var errs []error
	ok := 0
	for _, name := range form["app"] {
		app, err := s.loadApp(r, name, "get")
		if err == nil {
			_, err = s.refreshApp(r, app, form.Get("mode"))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		ok++
	}
	s.closePanel(w, r, form.Get("close"), bulkToasts("Refresh requested for", ok, errs)...)
}

func bulkToasts(what string, ok int, errs []error) []views.Toast {
	var out []views.Toast
	if ok > 0 {
		out = append(out, success(fmt.Sprintf("%s %s.", what, views.Plural(ok, "application"))))
	}
	if len(errs) > 0 {
		out = append(out, failure(describeErrors(errs)))
	}
	return out
}

func (s *Server) webFavorite(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "get")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	favs := favorites(r)
	name := app.Metadata.Name
	favs[name] = !favs[name]
	if !favs[name] {
		delete(favs, name)
	}
	setFavorites(w, favs)
	s.render(w, r, http.StatusOK, views.FavStar(name, favs[name]))
}

// ─── new application ────────────────────────────────────────────────────

func (s *Server) webCreateApp(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	panel := views.NewAppPanel{Mode: form.Get("mode"), Options: s.formOptions(r), CloseHref: form.Get("close")}
	if panel.CloseHref == "" {
		panel.CloseHref = "/ui/applications"
	}
	var app *v1.Application
	if panel.Mode == "yaml" {
		panel.YAML = form.Get("yaml")
		app, err = appFromYAML(panel.YAML)
	} else {
		panel.Mode = "form"
		panel.Form = appFormFromValues(form)
		app = &v1.Application{}
		err = applyAppForm(app, panel.Form)
	}
	if err == nil {
		app, err = s.createApp(r, app, false)
	}
	if err != nil {
		panel.Error = err.Error()
		s.render(w, r, errorCode(err), views.NewAppPanelView(panel))
		return
	}
	s.navigate(w, views.AppHref(app.Metadata.Name), success(fmt.Sprintf("Application %s created.", app.Metadata.Name)))
}

// webNewAppYAML switches the NEW APP panel to the YAML editor, carrying
// over what the form holds.
func (s *Server) webNewAppYAML(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	panel := views.NewAppPanel{Mode: "yaml", Options: s.formOptions(r), CloseHref: form.Get("close")}
	app := &v1.Application{}
	if err := applyAppForm(app, appFormFromValues(form)); err != nil {
		panel.Error = err.Error()
	}
	app.Metadata.ResourceVersion = ""
	panel.YAML = appYAML(app)
	s.render(w, r, http.StatusOK, views.NewAppPanelView(panel))
}

// webNewAppForm switches the NEW APP panel back to the form.
func (s *Server) webNewAppForm(w http.ResponseWriter, r *http.Request) {
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	panel := views.NewAppPanel{Mode: "form", Options: s.formOptions(r), CloseHref: form.Get("close")}
	app, err := appFromYAML(form.Get("yaml"))
	if err != nil {
		panel.Mode, panel.YAML, panel.Error = "yaml", form.Get("yaml"), err.Error()
	} else {
		panel.Form = appForm(app)
	}
	s.render(w, r, http.StatusOK, views.NewAppPanelView(panel))
}
