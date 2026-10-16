// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/diff"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/ui/views"
)

func (s *Server) appPerms(r *http.Request, app *v1.Application) views.AppPerms {
	obj := appObject(app)
	return views.AppPerms{
		Sync:   s.allowed(r, "applications", "sync", obj),
		Update: s.allowed(r, "applications", "update", obj),
		Delete: s.allowed(r, "applications", "delete", obj),
		Action: s.allowed(r, "applications", "action", obj),
		Logs:   s.allowed(r, "logs", "get", obj),
	}
}

func appView(q url.Values) string {
	switch v := q.Get("view"); v {
	case "list", "nodes":
		return v
	}
	return "tree"
}

// liveTree reads the resource tree with a bounded wait.
func (s *Server) liveTree(ctx context.Context, app *v1.Application) (*controller.ResourceTree, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	rt, err := s.Live.Tree(ctx, app)
	if err == nil {
		s.addOrphans(ctx, app, rt)
	}
	return rt, err
}

// orphanSettings is how the Application's project watches for orphaned
// jobs (nil: it does not).
func (s *Server) orphanSettings(ctx context.Context, app *v1.Application) *v1.OrphanedResourcesMonitorSettings {
	proj, err := s.Store.GetProject(ctx, app.Spec.Project)
	if err != nil {
		return nil
	}
	return proj.Spec.OrphanedResources
}

// addOrphans adds the orphaned jobs to a tree when the project watches
// for them.
func (s *Server) addOrphans(ctx context.Context, app *v1.Application, rt *controller.ResourceTree) {
	settings := s.orphanSettings(ctx, app)
	if settings == nil {
		return
	}
	nodes, err := s.Live.OrphanedNodes(ctx, app, settings)
	if err != nil {
		s.logger.Debug("list orphaned jobs", "app", app.Metadata.Name, "error", err)
		return
	}
	rt.OrphanedNodes = nodes
}

// appPage loads what the details page shows; the live tree (the costly
// part) only when withTree.
func (s *Server) appPage(w http.ResponseWriter, r *http.Request, app *v1.Application, q url.Values, withTree bool) views.AppPage {
	name := app.Metadata.Name
	view := appView(q)
	p := views.AppPage{
		Page: s.page(w, r, name, "applications", "Application details "+view,
			views.Crumb{Label: "Applications", Href: "/ui/applications"}, views.Crumb{Label: name}),
		App:          app,
		View:         view,
		Perms:        s.appPerms(r, app),
		Filter:       strings.TrimSpace(q.Get("filter")),
		ShowOrphaned: q.Get("orphaned") == "true",
	}
	if withTree {
		rt, err := s.liveTree(r.Context(), app)
		if err != nil {
			p.TreeErr = err.Error()
		}
		if rt != nil && s.orphanSettings(r.Context(), app) != nil {
			p.Orphaned = len(rt.OrphanedNodes)
		} else {
			p.Orphaned, p.ShowOrphaned = -1, false
		}
		tc := treeContext{app: app, view: view, perms: p.Perms, filter: p.Filter, live: s.Live, orphaned: p.ShowOrphaned}
		p.Tree = buildTree(tc, rt)
		if sel := q.Get("node"); sel != "" {
			for i := range p.Tree.Nodes {
				p.Tree.Nodes[i].Selected = p.Tree.Nodes[i].UID == sel
			}
		}
		if view == "nodes" {
			p.NodeGroups = nodeGroups(p.Tree, rt)
		}
	}
	if proj, err := s.Store.GetProject(r.Context(), app.Spec.Project); err == nil {
		p.Windows = proj.WindowState(app, time.Now())
	}
	p.SyncCommit = s.commit(r.Context(), app.Spec.Source.RepoURL, app.Status.Sync.Revision)
	if st := app.Status.OperationState; st != nil && st.SyncResult != nil {
		p.LastCommit = s.commit(r.Context(), app.Spec.Source.RepoURL, st.SyncResult.Revision)
	}
	return p
}

func (s *Server) webApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "get")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	q := pageQuery(r)
	switch htmxTarget(r) {
	case "panel":
		s.render(w, r, http.StatusOK, s.appPanel(r, app, q))
	case "app-status":
		s.render(w, r, http.StatusOK, views.AppStatus(s.appPage(w, r, app, q, false)))
	case "app-resources":
		s.render(w, r, http.StatusOK, views.AppResources(s.appPage(w, r, app, q, true)))
	default:
		p := s.appPage(w, r, app, q, true)
		p.Panel = s.appPanel(r, app, q)
		s.render(w, r, http.StatusOK, views.AppView(p))
	}
}

// appPanel is the sliding panel the URL asks for on the details page.
func (s *Server) appPanel(r *http.Request, app *v1.Application, q url.Values) templ.Component {
	name := app.Metadata.Name
	view := appView(q)
	closeHref := views.AppURL(name, view, "filter", q.Get("filter"))
	if uid := q.Get("node"); uid != "" {
		return s.nodePanel(r, app, q, uid, closeHref)
	}
	switch q.Get("panel") {
	case "details":
		return s.detailsPanel(r, app, q, closeHref, "")
	case "diff":
		return s.diffPanel(r, app, q, closeHref)
	case "sync":
		return views.SyncPanelView(views.SyncPanel{App: app, Resources: syncResources(app), CloseHref: closeHref})
	case "status":
		return views.OperationPanelView(views.OperationPanel{App: app, CanSync: s.allowed(r, "applications", "sync", appObject(app)), CloseHref: closeHref})
	case "history":
		return s.historyPanel(r, app, closeHref)
	case "delete":
		return views.DeletePanelView(views.DeletePanel{App: app, CloseHref: closeHref})
	}
	return views.EmptyPanel()
}

func (s *Server) detailsPanel(r *http.Request, app *v1.Application, q url.Values, closeHref, errMsg string) templ.Component {
	name := app.Metadata.Name
	view := appView(q)
	tab := q.Get("tab")
	switch tab {
	case "parameters", "manifest", "events":
	default:
		tab = "summary"
	}
	d := views.AppDetailsPanel{
		App: app, Tab: tab, Edit: q.Get("edit") == "1", Perms: s.appPerms(r, app), Error: errMsg, CloseHref: closeHref,
		EditHref: views.AppURL(name, view, "panel", "details", "tab", tab, "edit", "1"),
		ViewHref: views.AppURL(name, view, "panel", "details", "tab", tab),
	}
	d.Edit = d.Edit && d.Perms.Update
	for _, t := range []struct{ id, label string }{{"summary", "Summary"}, {"parameters", "Parameters"}, {"manifest", "Manifest"}, {"events", "Events"}} {
		d.Tabs = append(d.Tabs, views.Tab{Label: t.label, Href: views.AppURL(name, view, "panel", "details", "tab", t.id), Active: t.id == tab})
	}
	switch tab {
	case "summary":
		if d.Edit {
			d.Form = appForm(app)
			d.Options = s.formOptions(r)
		}
	case "manifest":
		d.Manifest = appYAML(app)
	case "events":
		d.Events, _ = s.appEvents(r, app, "", "")
		sort.SliceStable(d.Events, func(i, j int) bool { return d.Events[i].LastSeen.After(d.Events[j].LastSeen) })
	}
	return views.AppDetailsPanelView(d)
}

func (s *Server) diffPanel(r *http.Request, app *v1.Application, q url.Values, closeHref string) templ.Component {
	name := app.Metadata.Name
	view := appView(q)
	compact := q.Get("compact") != "false"
	toggle := views.AppURL(name, view, "panel", "diff", "compact", strconv.FormatBool(!compact))
	p := views.DiffPanel{App: app, Compact: compact, CloseHref: closeHref, ToggleURL: toggle}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	diffs, err := s.Live.ManagedResources(ctx, app)
	if err != nil {
		p.Error = err.Error()
		return views.DiffPanelView(p)
	}
	context := 3
	if !compact {
		context = -1
	}
	for _, d := range diffs {
		rd := views.ResourceDiff{Kind: d.Kind, Namespace: d.Namespace, Name: d.Name, Modified: d.Modified, Pruning: d.RequiresPruning, PlanDiff: d.PlanDiff}
		rd.Missing = d.LiveState == "" && !d.RequiresPruning
		target := d.TargetState
		if d.RequiresPruning {
			target = ""
		}
		rd.Lines = diff.Lines(d.NormalizedLiveState, target, context)
		p.Diffs = append(p.Diffs, rd)
	}
	return views.DiffPanelView(p)
}

func (s *Server) historyPanel(r *http.Request, app *v1.Application, closeHref string) templ.Component {
	p := views.HistoryPanel{App: app, CanSync: s.allowed(r, "applications", "sync", appObject(app)), CloseHref: closeHref}
	h := app.Status.History
	for i := len(h) - 1; i >= 0; i-- {
		e := views.HistoryEntry{RevisionHistory: h[i], Active: i == len(h)-1}
		if i+1 < len(h) {
			until := h[i+1].DeployedAt
			e.Until = &until
		}
		e.Commit = s.commit(r.Context(), h[i].Source.RepoURL, h[i].Revision)
		p.Entries = append(p.Entries, e)
	}
	return views.HistoryPanelView(p)
}

// ─── resource node panel ────────────────────────────────────────────────

func (s *Server) nodePanel(r *http.Request, app *v1.Application, q url.Values, uid, closeHref string) templ.Component {
	name := app.Metadata.Name
	view := appView(q)
	perms := s.appPerms(r, app)
	tc := treeContext{app: app, view: view, perms: perms, live: s.Live}
	p := views.NodePanel{App: app, CloseHref: closeHref}

	rt, err := s.liveTree(r.Context(), app)
	if err != nil {
		p.Error = err.Error()
	}
	var live, orphan *controller.ResourceNode
	if rt != nil {
		for i := range rt.Nodes {
			if rt.Nodes[i].UID == uid {
				live = &rt.Nodes[i]
			}
		}
		for i := range rt.OrphanedNodes {
			if rt.OrphanedNodes[i].UID == uid {
				orphan = &rt.OrphanedNodes[i]
			}
		}
	}
	switch {
	case live != nil:
		p.Node = tc.viewNode(*live)
		p.Summary = nodeSummary(*live)
	case orphan != nil:
		p.Node = tc.orphanNode(*orphan)
		p.Summary = append(nodeSummary(*orphan), [2]string{"Managed", "No: no application tracks this job (orphaned)"})
	case strings.HasPrefix(uid, "missing:"):
		parts := strings.SplitN(strings.TrimPrefix(uid, "missing:"), "/", 3)
		if len(parts) == 3 {
			p.Node = views.TreeNode{UID: uid, Kind: parts[0], Namespace: parts[1], Name: parts[2], Health: string(v1.HealthStatusMissing)}
			p.Node.Actions = tc.actions(p.Node, "")
			p.Summary = [][2]string{{"Kind", parts[0]}, {"Namespace", parts[1]}, {"Name", parts[2]}, {"Live state", "does not exist"}}
		}
	default:
		if p.Error == "" {
			p.Error = "This resource no longer exists. It may have been replaced: close this panel to see the current tree."
		}
		p.Node = views.TreeNode{UID: uid, Name: uid}
		return views.NodePanelView(p)
	}
	hook := false
	for _, st := range app.Status.Resources {
		if st.Kind == p.Node.Kind && st.Namespace == p.Node.Namespace && st.Name == p.Node.Name {
			p.Node.Sync, hook = string(st.Status), st.Hook
		}
	}
	// A hook's job has no desired state to compare with.
	managed := (p.Node.Kind == v1.KindJob || p.Node.Kind == v1.KindConsulKV || p.Node.Kind == v1.KindApplication) && !hook
	hasLogs := (p.Node.Kind == v1.KindAllocation || p.Node.Kind == v1.KindTask) && perms.Logs
	hasEvents := (p.Node.Kind == v1.KindAllocation || p.Node.Kind == v1.KindTask || p.Node.Kind == v1.KindJob || managed) &&
		orphan == nil && p.Node.Kind != v1.KindApplication
	if orphan != nil {
		managed = false
	}

	tabs := []struct{ id, label string }{{"summary", "Summary"}}
	if !strings.HasPrefix(uid, "missing:") {
		tabs = append(tabs, struct{ id, label string }{"live", "Live manifest"})
	}
	if managed {
		tabs = append(tabs, struct{ id, label string }{"diff", "Diff"}, struct{ id, label string }{"desired", "Desired manifest"})
	}
	if hasEvents {
		tabs = append(tabs, struct{ id, label string }{"events", "Events"})
	}
	if hasLogs {
		tabs = append(tabs, struct{ id, label string }{"logs", "Logs"})
	}
	p.Tab = q.Get("tab")
	valid := false
	for _, t := range tabs {
		valid = valid || t.id == p.Tab
		p.Tabs = append(p.Tabs, views.Tab{Label: t.label, Href: nodeHref(name, view, uid, t.id), Active: false})
	}
	if !valid {
		p.Tab = "summary"
	}
	for i := range p.Tabs {
		p.Tabs[i].Active = p.Tabs[i].Href == nodeHref(name, view, uid, p.Tab) || (p.Tab == "summary" && i == 0)
	}

	rq := resourceQuery{kind: p.Node.Kind, namespace: p.Node.Namespace, name: p.Node.Name, uid: uid}
	switch p.Tab {
	case "live":
		if p.Manifest, err = s.resourceManifest(r, app, rq); err != nil {
			p.Error = err.Error()
		}
	case "desired", "diff":
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		diffs, err := s.Live.ManagedResources(ctx, app)
		if err != nil {
			p.Error = err.Error()
			break
		}
		for _, d := range diffs {
			if d.Kind == p.Node.Kind && d.Namespace == p.Node.Namespace && d.Name == p.Node.Name {
				p.Desired = d.TargetState
				target := d.TargetState
				if d.RequiresPruning {
					target = ""
				}
				p.Diff = diff.Lines(d.NormalizedLiveState, target, 3)
				p.PlanDiff = d.PlanDiff
			}
		}
	case "events":
		p.Events = s.nodeEvents(r, app, p.Node)
	case "logs":
		p.Logs = s.logsView(r, app, q, uid)
		if p.Logs == nil {
			p.Error = "Logs are not available for this resource."
		}
	}
	return views.NodePanelView(p)
}

func nodeSummary(n controller.ResourceNode) [][2]string {
	out := [][2]string{{"Kind", n.Kind}, {"Name", n.Name}}
	if n.Namespace != "" {
		out = append(out, [2]string{"Namespace", n.Namespace})
	}
	if n.CreatedAt != nil {
		out = append(out, [2]string{"Created", views.Timestamp(*n.CreatedAt) + " (" + views.Ago(*n.CreatedAt) + ")"})
	}
	if n.Health != nil {
		out = append(out, [2]string{"Health", string(n.Health.Status)})
	}
	for _, i := range n.Info {
		out = append(out, [2]string{i.Name, i.Value})
	}
	if len(n.Images) > 0 {
		out = append(out, [2]string{"Images / packages", strings.Join(n.Images, ", ")})
	}
	return out
}

// allocOf returns the allocation id of an allocation or task node.
func allocOf(uid string) string {
	id := strings.TrimPrefix(strings.TrimPrefix(uid, "alloc:"), "task:")
	id, _, _ = strings.Cut(id, "/")
	return id
}

func (s *Server) nodeEvents(r *http.Request, app *v1.Application, n views.TreeNode) []views.NodeEvent {
	var out []views.NodeEvent
	if n.Kind == v1.KindAllocation || n.Kind == v1.KindTask {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if alloc, _, err := s.Live.OwnedAllocation(ctx, app, allocOf(n.UID)); err == nil {
			_, onlyTask, _ := strings.Cut(strings.TrimPrefix(n.UID, "task:"), "/")
			for task, ts := range alloc.TaskStates {
				if n.Kind == v1.KindTask && task != onlyTask {
					continue
				}
				for _, e := range ts.Events {
					msg := e.DisplayMessage
					if msg == "" {
						msg = e.Message
					}
					out = append(out, views.NodeEvent{Time: time.Unix(0, e.Time), Reason: e.Type, Message: msg, Source: "task " + task})
				}
			}
		}
	}
	if events, err := s.appEvents(r, app, n.Kind, n.Name); err == nil {
		for _, e := range events {
			out = append(out, views.NodeEvent{Time: e.LastSeen, Type: e.Type, Reason: e.Reason, Message: e.Message, Source: "cystemd"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out
}

func (s *Server) logsView(r *http.Request, app *v1.Application, q url.Values, uid string) *views.LogsView {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	alloc, _, err := s.Live.OwnedAllocation(ctx, app, allocOf(uid))
	if err != nil {
		return nil
	}
	lv := &views.LogsView{Type: q.Get("type"), Follow: q.Get("follow") != "false"}
	for t := range alloc.TaskStates {
		lv.Tasks = append(lv.Tasks, t)
	}
	sort.Strings(lv.Tasks)
	if len(lv.Tasks) == 0 {
		return nil
	}
	lv.Task = q.Get("task")
	if _, task, ok := strings.Cut(strings.TrimPrefix(uid, "task:"), "/"); ok && strings.HasPrefix(uid, "task:") {
		lv.Task = task
	}
	if alloc.TaskStates[lv.Task] == nil {
		lv.Task = lv.Tasks[0]
	}
	if lv.Type != "stderr" {
		lv.Type = "stdout"
	}
	lv.Tail, _ = strconv.Atoi(q.Get("tail"))
	if lv.Tail <= 0 {
		lv.Tail = 500
	}
	name := app.Metadata.Name
	params := url.Values{"alloc": {alloc.ID}, "task": {lv.Task}, "type": {lv.Type}, "tail": {strconv.Itoa(lv.Tail)}, "follow": {strconv.FormatBool(lv.Follow)}}
	lv.StreamURL = views.AppHref(name) + "/logs?" + params.Encode()
	params.Del("follow")
	lv.Download = views.AppHref(name) + "/logs/download?" + params.Encode()
	lv.FormURL = views.AppURL(name, appView(q), "node", uid)
	return lv
}

// ─── logs ───────────────────────────────────────────────────────────────

// logTarget resolves and authorizes a log request.
func (s *Server) logTarget(r *http.Request) (*v1.Application, logRequest, func(context.Context, func([]string) error) error, error) {
	app, err := s.loadApp(r, r.PathValue("name"), "get")
	if err != nil {
		return nil, logRequest{}, nil, err
	}
	if err := s.authorize(r, "logs", "get", appObject(app)); err != nil {
		return nil, logRequest{}, nil, err
	}
	alloc, cluster, err := s.Live.OwnedAllocation(r.Context(), app, r.URL.Query().Get("alloc"))
	if err != nil {
		return nil, logRequest{}, nil, err
	}
	lr := parseLogRequest(r.URL.Query(), alloc)
	read := func(ctx context.Context, emit func([]string) error) error {
		return readLogs(ctx, cluster, alloc, lr, emit)
	}
	return app, lr, read, nil
}

// webLogStream streams log lines as HTML fragments (htmx SSE extension:
// "log" events append lines, "done" closes the stream).
func (s *Server) webLogStream(w http.ResponseWriter, r *http.Request) {
	_, lr, read, err := s.logTarget(r)
	sse, ok := startSSE(w)
	if !ok {
		return
	}
	if err != nil {
		sse.event("log", logLineHTML(0, "Cannot stream logs: "+err.Error(), "log-line--err"))
		sse.event("done", "")
		return
	}
	if r.Header.Get("Last-Event-ID") != "" {
		// A reconnect: continue from the end instead of repeating the tail.
		lr.tail = 0
	}
	n := 0
	err = read(r.Context(), func(lines []string) error {
		if lines == nil {
			sse.comment("keepalive")
			return nil
		}
		var b strings.Builder
		for i, l := range lines {
			n++
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(logLineHTML(n, l, ""))
		}
		return sse.eventID("log", b.String(), strconv.Itoa(n))
	})
	if r.Context().Err() != nil {
		return
	}
	if err != nil {
		sse.event("log", logLineHTML(0, "Log stream ended: "+err.Error(), "log-line--err"))
	} else if n == 0 {
		sse.event("log", logLineHTML(0, "No "+lr.logType+" output yet.", "log-line--meta"))
	}
	sse.event("done", "")
}

func logLineHTML(n int, text, class string) string {
	ln := ""
	if n > 0 {
		ln = strconv.Itoa(n)
	}
	return `<div class="log-line ` + class + `"><span class="log-line__ln">` + ln + `</span><span>` + html.EscapeString(text) + `</span></div>`
}

func (s *Server) webLogDownload(w http.ResponseWriter, r *http.Request) {
	app, lr, _, err := s.logTarget(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	lr.follow = false
	alloc, cluster, err := s.Live.OwnedAllocation(r.Context(), app, r.URL.Query().Get("alloc"))
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	file := fmt.Sprintf("%s-%s-%s.%s.log", app.Metadata.Name, alloc.ID[:min(8, len(alloc.ID))], lr.task, lr.logType)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(file, `"`, "")+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_ = readLogs(r.Context(), cluster, alloc, lr, func(lines []string) error {
		for _, l := range lines {
			if _, err := fmt.Fprintln(w, l); err != nil {
				return err
			}
		}
		return nil
	})
}

// ─── writes ─────────────────────────────────────────────────────────────

func (s *Server) webSyncApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "sync")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	// One resource from a node menu: a partial sync with the defaults.
	if res := r.URL.Query().Get("resource"); res != "" {
		form = url.Values{"resource": {res}, "partial": {"1"}}
		if app.Spec.SyncPolicy.IsAutomated() && app.Spec.SyncPolicy.Automated.Prune {
			form.Set("prune", "true")
		}
		req, err := syncRequestFromForm(form, app)
		if err == nil && len(req.Resources) == 0 {
			// The only resource: the whole application.
			req.Resources = nil
		}
		if err == nil {
			_, err = s.syncApp(r, app, req)
		}
		if err != nil {
			s.webFail(w, r, err)
			return
		}
		s.notify(w, r, success(fmt.Sprintf("Sync of %s requested.", res)))
		return
	}
	req, err := syncRequestFromForm(form, app)
	if err == nil {
		_, err = s.syncApp(r, app, req)
	}
	if err != nil {
		s.render(w, r, errorCode(err), views.SyncPanelView(views.SyncPanel{App: app, Resources: syncResources(app), Error: err.Error(), CloseHref: form.Get("close")}))
		return
	}
	what := "Sync"
	if req.DryRun {
		what = "Dry run"
	}
	s.closePanel(w, r, form.Get("close"), success(fmt.Sprintf("%s of %s requested.", what, app.Metadata.Name)))
}

func (s *Server) webRefreshApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "get")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	mode := r.URL.Query().Get("mode")
	if _, err := s.refreshApp(r, app, mode); err != nil {
		s.webFail(w, r, err)
		return
	}
	msg := "Refresh of " + app.Metadata.Name + " requested."
	if mode == "hard" {
		msg = "Hard refresh of " + app.Metadata.Name + " requested: the repository is read again."
	}
	s.notify(w, r, success(msg))
}

func (s *Server) webRollback(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "sync")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	id, err := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		s.webFail(w, r, badRequest("bad history id"))
		return
	}
	if _, err := s.rollbackApp(r, app, RollbackRequest{ID: id}); err != nil {
		s.webFail(w, r, err)
		return
	}
	s.closePanel(w, r, views.AppHref(app.Metadata.Name), success(fmt.Sprintf("Rollback of %s to history entry %d requested.", app.Metadata.Name, id)))
}

func (s *Server) webTerminate(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "sync")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	if _, err := s.terminateOperation(r, app); err != nil {
		s.webFail(w, r, err)
		return
	}
	s.notify(w, r, success("Terminating the operation of "+app.Metadata.Name+"."))
}

func (s *Server) webDeleteApp(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "delete")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	closeHref := form.Get("close")
	if strings.TrimSpace(form.Get("confirm")) != app.Metadata.Name {
		s.render(w, r, http.StatusBadRequest, views.DeletePanelView(views.DeletePanel{App: app, CloseHref: closeHref,
			Error: "The name you typed does not match the application name."}))
		return
	}
	cascade := form.Get("cascade") != "false"
	if err := s.deleteApp(r, app, cascade); err != nil {
		s.render(w, r, errorCode(err), views.DeletePanelView(views.DeletePanel{App: app, CloseHref: closeHref, Error: err.Error()}))
		return
	}
	msg := fmt.Sprintf("Deleting %s: its jobs and keys are removed first.", app.Metadata.Name)
	if !cascade {
		msg = fmt.Sprintf("Application %s deleted; its jobs and keys keep running.", app.Metadata.Name)
	}
	if strings.HasPrefix(closeHref, views.AppHref(app.Metadata.Name)) {
		s.navigate(w, "/ui/applications", success(msg))
		return
	}
	s.closePanel(w, r, closeHref, success(msg))
}

func (s *Server) webUpdateSpec(w http.ResponseWriter, r *http.Request) {
	cur, err := s.loadApp(r, r.PathValue("name"), "update")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	f := appFormFromValues(form)
	next := cur.DeepCopy()
	err = applyAppForm(next, f)
	if err == nil {
		next, err = s.updateApp(r, cur, next)
	}
	q := url.Values{"panel": {"details"}, "tab": {"summary"}}
	if err != nil {
		q.Set("edit", "1")
		panel := s.detailsPanel(r, cur, q, form.Get("close"), err.Error())
		s.render(w, r, errorCode(err), panel)
		return
	}
	w.Header().Set("HX-Push-Url", views.AppURL(next.Metadata.Name, "", "panel", "details"))
	s.render(w, r, http.StatusOK, templ.Join(s.detailsPanel(r, next, q, form.Get("close"), ""), views.Toasts(success("Application "+next.Metadata.Name+" updated."))))
}

func (s *Server) webUpdateManifest(w http.ResponseWriter, r *http.Request) {
	cur, err := s.loadApp(r, r.PathValue("name"), "update")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	in, err := appFromYAML(form.Get("yaml"))
	if err == nil && in.Metadata.Name != "" && in.Metadata.Name != cur.Metadata.Name {
		err = badRequest("metadata.name cannot change (%q)", in.Metadata.Name)
	}
	var out *v1.Application
	if err == nil {
		out, err = s.updateApp(r, cur, in)
	}
	q := url.Values{"panel": {"details"}, "tab": {"manifest"}}
	if err != nil {
		q.Set("edit", "1")
		d := s.detailsPanel(r, cur, q, form.Get("close"), err.Error())
		s.render(w, r, errorCode(err), d)
		return
	}
	w.Header().Set("HX-Push-Url", views.AppURL(out.Metadata.Name, "", "panel", "details", "tab", "manifest"))
	s.render(w, r, http.StatusOK, templ.Join(s.detailsPanel(r, out, q, form.Get("close"), ""), views.Toasts(success("Application "+out.Metadata.Name+" updated."))))
}

func (s *Server) webAutoSync(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "update")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	enable := r.URL.Query().Get("enable") == "true"
	next := app.DeepCopy()
	if next.Spec.SyncPolicy == nil {
		next.Spec.SyncPolicy = &v1.SyncPolicy{}
	}
	if enable {
		if next.Spec.SyncPolicy.Automated == nil {
			next.Spec.SyncPolicy.Automated = &v1.SyncPolicyAutomated{}
		}
	} else {
		next.Spec.SyncPolicy.Automated = nil
	}
	next.Metadata.ResourceVersion = ""
	out, err := s.updateApp(r, app, next)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	msg := "Auto-sync disabled for " + out.Metadata.Name + "."
	if enable {
		msg = "Auto-sync enabled for " + out.Metadata.Name + "."
	}
	q := url.Values{"panel": {"details"}, "tab": {"summary"}}
	s.render(w, r, http.StatusOK, templ.Join(s.detailsPanel(r, out, q, views.AppHref(out.Metadata.Name), ""), views.Toasts(success(msg))))
}

func (s *Server) webResourceAction(w http.ResponseWriter, r *http.Request) {
	app, err := s.loadApp(r, r.PathValue("name"), "action")
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	q := r.URL.Query()
	rq := resourceQuery{kind: q.Get("kind"), namespace: q.Get("namespace"), name: q.Get("resourceName"), uid: q.Get("uid")}
	action := q.Get("action")
	if err := s.runResourceAction(r, app, rq, action); err != nil {
		s.webFail(w, r, err)
		return
	}
	s.notify(w, r, success(fmt.Sprintf("%s: %s %s.", actionLabel(action), rq.kind, rq.name)))
}
