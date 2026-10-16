// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/gitaginanjar/cystemd/internal/controller"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/ui/views"
)

// Resource tree layout, left to right as in Argo CD: the Application, its
// managed resources (jobs, Consul keys), then their live children (task
// groups, deployments, services, allocations, tasks). The tree is a real
// tree (one parent each), so a tidy layout replaces Argo CD's dagre: leaves
// take consecutive rows and parents centre on their children.
const (
	nodeWidth    = 282
	nodeHeight   = 52
	rankSep      = 80 // horizontal gap between columns
	nodeSep      = 18 // vertical gap between rows
	treePad      = 8
	maxSiblings  = 30 // allocations shown per task group before "+N more"
	edgeRadius   = 8
	rootNodeKind = v1.KindApplication
)

// kindRank orders siblings.
var kindRank = map[string]int{
	v1.KindJob: 0, v1.KindConsulKV: 1, v1.KindDeployment: 2, v1.KindTaskGroup: 3,
	v1.KindService: 4, v1.KindAllocation: 5, v1.KindTask: 6,
}

type layoutNode struct {
	views.TreeNode
	children []*layoutNode
	sortKey  string
}

// treeContext carries what node building needs.
type treeContext struct {
	app    *v1.Application
	view   string
	perms  views.AppPerms
	filter string
	live   *controller.Live
	// orphaned shows the orphaned jobs of the tree.
	orphaned bool
}

func nodeHref(app, view, uid, tab string) string {
	return views.AppURL(app, view, "node", uid, "tab", tab)
}

// buildTree turns the live resource tree into positioned view nodes.
func buildTree(tc treeContext, rt *controller.ResourceTree) *views.Tree {
	app := tc.app
	name := app.Metadata.Name
	root := &layoutNode{TreeNode: views.TreeNode{
		UID: "app:" + name, Kind: rootNodeKind, Name: name, Root: true,
		Health: views.HealthOf(app), HealthMsg: app.Status.Health.Message, Sync: views.SyncOf(app),
		Href: views.AppURL(name, tc.view, "panel", "details"),
	}}
	if rev := app.Status.Sync.Revision; rev != "" {
		root.Info = append(root.Info, views.ShortRev(rev))
	}
	root.Age = views.Age(app.Metadata.CreationTimestamp)

	statuses := map[string]v1.ResourceStatus{}
	for _, r := range app.Status.Resources {
		statuses[r.Kind+"/"+r.Namespace+"/"+r.Name] = r
	}

	nodes := map[string]*layoutNode{}
	var order []*layoutNode
	if rt != nil {
		for _, n := range rt.Nodes {
			ln := &layoutNode{TreeNode: tc.viewNode(n)}
			if st, ok := statuses[n.Kind+"/"+n.Namespace+"/"+n.Name]; ok && len(n.ParentRefs) == 0 {
				ln.Sync = string(st.Status)
				ln.Orphan = st.RequiresPruning
				if ln.Health == "" && st.Health != nil {
					ln.Health = string(st.Health.Status)
				}
				delete(statuses, n.Kind+"/"+n.Namespace+"/"+n.Name)
			}
			nodes[n.UID] = ln
			order = append(order, ln)
		}
		if tc.orphaned {
			for _, n := range rt.OrphanedNodes {
				ln := &layoutNode{TreeNode: tc.orphanNode(n)}
				nodes[n.UID] = ln
				order = append(order, ln)
			}
		}
	}
	// Managed resources without a live object (not created yet, or deleted
	// outside CYSTEMD) still show, as Missing.
	for _, st := range app.Status.Resources {
		key := st.Kind + "/" + st.Namespace + "/" + st.Name
		// A hook's job that is gone was deleted by its delete policy.
		if _, missing := statuses[key]; !missing || st.Hook {
			continue
		}
		uid := "missing:" + key
		// Without a live tree (Nomad unreachable) nothing is known missing.
		health := string(v1.HealthStatusUnknown)
		if rt != nil {
			health = string(v1.HealthStatusMissing)
		}
		if st.Health != nil && st.Health.Status != "" {
			health = string(st.Health.Status)
		}
		ln := &layoutNode{TreeNode: views.TreeNode{
			UID: uid, Kind: st.Kind, Namespace: st.Namespace, Name: st.Name,
			Health: health, Sync: string(st.Status), Orphan: st.RequiresPruning,
			Href: nodeHref(name, tc.view, uid, ""),
		}}
		ln.Actions = tc.actions(ln.TreeNode, "")
		nodes[uid] = ln
		order = append(order, ln)
	}

	parentOf := map[*layoutNode]*layoutNode{}
	if rt != nil {
		for _, n := range rt.Nodes {
			child := nodes[n.UID]
			parent := root
			for _, p := range n.ParentRefs {
				if pn, ok := nodes[p.UID]; ok {
					parent = pn
					break
				}
			}
			parentOf[child] = parent
		}
	}
	for _, ln := range order {
		if _, ok := parentOf[ln]; !ok {
			parentOf[ln] = root
		}
	}

	// Filter: keep matching nodes and their ancestors.
	keep := map[*layoutNode]bool{root: true}
	filter := strings.ToLower(strings.TrimSpace(tc.filter))
	for _, ln := range order {
		if filter != "" && !strings.Contains(strings.ToLower(ln.Name), filter) && !strings.Contains(strings.ToLower(ln.Kind), filter) {
			continue
		}
		for n := ln; n != nil && !keep[n]; n = parentOf[n] {
			keep[n] = true
		}
	}
	for _, ln := range order {
		if keep[ln] {
			p := parentOf[ln]
			p.children = append(p.children, ln)
		}
	}
	return layout(root)
}

// orphanNode converts an orphaned job's node: shown, never acted upon.
func (tc treeContext) orphanNode(n controller.ResourceNode) views.TreeNode {
	vn := tc.viewNode(n)
	vn.Unmanaged, vn.Actions = true, nil
	vn.Info = append([]string{"orphaned"}, vn.Info...)
	return vn
}

// viewNode converts a live node.
func (tc treeContext) viewNode(n controller.ResourceNode) views.TreeNode {
	vn := views.TreeNode{
		UID: n.UID, Kind: n.Kind, Namespace: n.Namespace, Name: n.Name,
		Age:  views.Age(n.CreatedAt),
		Href: nodeHref(tc.app.Metadata.Name, tc.view, n.UID, ""),
	}
	if n.Health != nil {
		vn.Health, vn.HealthMsg = string(n.Health.Status), n.Health.Message
	}
	info := map[string]string{}
	for _, i := range n.Info {
		info[i.Name] = i.Value
	}
	switch n.Kind {
	case v1.KindJob:
		vn.Hook = info["Hook"] != ""
		vn.Info = nonEmpty(suffixed(info["Hook"], " hook"), info["Type"], prefixed("v", info["Version"]), statusTag(info["Status"], "running"))
	case v1.KindTaskGroup:
		running := info["Running"]
		if d, ok := info["Desired"]; ok {
			running += "/" + d
		}
		vn.Info = nonEmpty(running+" running", prefixed("queued ", info["Queued"]), prefixed("failed ", info["Failed"]))
	case v1.KindAllocation:
		vn.Info = nonEmpty(info["Node"], statusTag(info["Status"], "running"), prefixed("v", info["Job version"]))
		vn.LogsHref = nodeHref(tc.app.Metadata.Name, tc.view, n.UID, "logs")
	case v1.KindTask:
		vn.Info = nonEmpty(info["Driver"], statusTag(info["State"], "running"), restarts(info["Restarts"]))
		vn.LogsHref = nodeHref(tc.app.Metadata.Name, tc.view, n.UID, "logs")
	case v1.KindDeployment:
		vn.Info = nonEmpty(info["Status"], prefixed("v", info["Job version"]))
	case v1.KindService:
		vn.Info = nonEmpty(info["Provider"], prefixed("port ", info["Port"]))
	case v1.KindConsulKV:
		vn.Info = nonEmpty(info["Size"])
	default:
		for _, i := range n.Info {
			vn.Info = append(vn.Info, i.Value)
		}
	}
	status := info["Status"]
	if n.Kind == v1.KindTask {
		status = info["State"]
	}
	vn.Actions = tc.actions(vn, status)
	return vn
}

func nonEmpty(items ...string) []string {
	var out []string
	for _, s := range items {
		if strings.TrimSpace(s) != "" && s != " running" {
			out = append(out, s)
		}
	}
	return out
}

func prefixed(prefix, v string) string {
	if v == "" || v == "0" {
		return ""
	}
	return prefix + v
}

func suffixed(v, suffix string) string {
	if v == "" {
		return ""
	}
	return v + suffix
}

// statusTag shows a status unless it is the boring default.
func statusTag(status, boring string) string {
	if status == boring {
		return ""
	}
	return status
}

func restarts(n string) string {
	if n == "" || n == "0" {
		return ""
	}
	if n == "1" {
		return "1 restart"
	}
	return n + " restarts"
}

// actions lists a node's menu actions the caller may run, given the live
// status of the resource (a stopped job can only be started, a finished
// deployment has nothing left to do...).
func (tc treeContext) actions(n views.TreeNode, status string) []views.NodeAction {
	name := tc.app.Metadata.Name
	var out []views.NodeAction
	// A hook runs with full syncs; it cannot be synced on its own.
	if tc.perms.Sync && (n.Kind == v1.KindJob || n.Kind == v1.KindConsulKV || n.Kind == v1.KindApplication) && !n.Hook {
		q := url.Values{"resource": {n.Kind + "/" + n.Namespace + "/" + n.Name}}
		out = append(out, views.NodeAction{
			Name: "sync", Label: "Sync", Post: views.AppHref(name) + "/sync?" + q.Encode(),
			Confirm: fmt.Sprintf("Sync %s %s only?", n.Kind, n.Name),
		})
	}
	if !tc.perms.Action || tc.live == nil || strings.HasPrefix(n.UID, "missing:") {
		return out
	}
	for _, a := range tc.live.Actions(n.Kind, nil) {
		if !actionApplies(n.Kind, a.Name, status) || (n.Hook && a.Name == "start") {
			continue
		}
		q := url.Values{"kind": {n.Kind}, "namespace": {n.Namespace}, "resourceName": {n.Name}, "uid": {n.UID}, "action": {a.Name}}
		act := views.NodeAction{Name: a.Name, Label: actionLabel(a.Name), Post: views.AppHref(name) + "/actions?" + q.Encode()}
		switch a.Name {
		case "stop", "fail":
			act.Danger = true
			act.Confirm = fmt.Sprintf("%s %s %s?", actionLabel(a.Name), n.Kind, n.Name)
		case "restart":
			act.Confirm = fmt.Sprintf("Restart %s %s?", n.Kind, n.Name)
		}
		out = append(out, act)
	}
	return out
}

// actionApplies filters actions by the resource's live status.
func actionApplies(kind, action, status string) bool {
	switch kind {
	case v1.KindJob:
		if status == "dead" {
			return action == "start"
		}
		return action != "start"
	case v1.KindDeployment:
		switch status {
		case "running":
			return action != "resume"
		case "paused":
			return action != "pause"
		case "pending", "blocked", "unblocking":
			return action == "fail"
		}
		return false
	case v1.KindAllocation:
		return status == "running" || status == "pending"
	case v1.KindTask:
		return status == "running"
	}
	return true
}

func actionLabel(a string) string {
	switch a {
	case "signal-hup":
		return "Send SIGHUP"
	}
	return strings.ToUpper(a[:1]) + a[1:]
}

func sortChildren(n *layoutNode) {
	sort.SliceStable(n.children, func(i, j int) bool {
		a, b := n.children[i], n.children[j]
		if kindRank[a.Kind] != kindRank[b.Kind] {
			return kindRank[a.Kind] < kindRank[b.Kind]
		}
		return a.Name < b.Name
	})
	// Many allocations: show the first ones and a summary node.
	if len(n.children) > maxSiblings {
		hidden := len(n.children) - maxSiblings
		n.children = append(n.children[:maxSiblings:maxSiblings], &layoutNode{TreeNode: views.TreeNode{UID: n.UID + "#more", More: hidden}})
	}
	for _, c := range n.children {
		sortChildren(c)
	}
}

// layout positions the tree and draws the edges.
func layout(root *layoutNode) *views.Tree {
	sortChildren(root)
	t := &views.Tree{}
	row := 0
	maxDepth := 0
	var place func(n *layoutNode, depth int) int
	place = func(n *layoutNode, depth int) int {
		maxDepth = max(maxDepth, depth)
		n.Depth = depth
		n.X = treePad + depth*(nodeWidth+rankSep)
		if len(n.children) == 0 {
			n.Y = treePad + row*(nodeHeight+nodeSep)
			row++
			return n.Y
		}
		first, last := 0, 0
		for i, c := range n.children {
			y := place(c, depth+1)
			if i == 0 {
				first = y
			}
			last = y
		}
		n.Y = (first + last) / 2
		return n.Y
	}
	place(root, 0)

	var walk func(n *layoutNode)
	walk = func(n *layoutNode) {
		t.Nodes = append(t.Nodes, n.TreeNode)
		for _, c := range n.children {
			t.Edges = append(t.Edges, edgePath(n.X+nodeWidth, n.Y+nodeHeight/2, c.X, c.Y+nodeHeight/2))
			walk(c)
		}
	}
	walk(root)
	t.Width = 2*treePad + (maxDepth+1)*nodeWidth + maxDepth*rankSep
	t.Height = 2*treePad + max(row, 1)*(nodeHeight+nodeSep) - nodeSep
	return t
}

// edgePath is an elbow connector with rounded corners.
func edgePath(x1, y1, x2, y2 int) string {
	if y1 == y2 {
		return fmt.Sprintf("M%d %dH%d", x1, y1, x2)
	}
	mid := x1 + rankSep/2
	r := min(edgeRadius, abs(y2-y1)/2)
	dir := 1
	if y2 < y1 {
		dir = -1
	}
	return fmt.Sprintf("M%d %dH%dQ%d %d %d %dV%dQ%d %d %d %dH%d",
		x1, y1, mid-r, mid, y1, mid, y1+dir*r, y2-dir*r, mid, y2, mid+r, y2, x2)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// nodeGroups groups allocations by Nomad client node (the Nodes view).
func nodeGroups(t *views.Tree, rt *controller.ResourceTree) []views.NodeGroup {
	if t == nil || rt == nil {
		return nil
	}
	nodeOf := map[string]string{}
	for _, n := range rt.Nodes {
		if n.Kind != v1.KindAllocation {
			continue
		}
		for _, i := range n.Info {
			if i.Name == "Node" {
				nodeOf[n.UID] = i.Value
			}
		}
	}
	groups := map[string]*views.NodeGroup{}
	var names []string
	for _, n := range t.Nodes {
		if n.Kind != v1.KindAllocation {
			continue
		}
		node := nodeOf[n.UID]
		if node == "" {
			node = "(unplaced)"
		}
		g, ok := groups[node]
		if !ok {
			g = &views.NodeGroup{Node: node}
			groups[node] = g
			names = append(names, node)
		}
		g.Allocs = append(g.Allocs, n)
	}
	sort.Strings(names)
	out := make([]views.NodeGroup, 0, len(names))
	for _, n := range names {
		out = append(out, *groups[n])
	}
	return out
}
