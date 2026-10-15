// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/gitaginanjar/cystemd/internal/controller"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/ui/views"
)

func testTree(allocs int) (*v1.Application, *controller.ResourceTree) {
	app := &v1.Application{Metadata: v1.ObjectMeta{Name: "web"}}
	app.Status.Resources = []v1.ResourceStatus{
		{Kind: v1.KindJob, Namespace: "default", Name: "web", Status: v1.SyncStatusSynced},
		{Kind: v1.KindConsulKV, Name: "cystemd/web/config", Status: v1.SyncStatusOutOfSync},
		{Kind: v1.KindJob, Namespace: "default", Name: "gone", Status: v1.SyncStatusOutOfSync},
	}
	job := controller.ResourceRef{Kind: v1.KindJob, Namespace: "default", Name: "web", UID: "job:default/web"}
	group := controller.ResourceRef{Kind: v1.KindTaskGroup, Namespace: "default", Name: "web.app", UID: "group:default/web/app"}
	healthy := &v1.HealthStatus{Status: v1.HealthStatusHealthy}
	rt := &controller.ResourceTree{Nodes: []controller.ResourceNode{
		{ResourceRef: job, Health: healthy, Info: []controller.InfoItem{{Name: "Type", Value: "service"}, {Name: "Status", Value: "running"}, {Name: "Version", Value: "4"}}},
		{ResourceRef: controller.ResourceRef{Kind: v1.KindConsulKV, Name: "cystemd/web/config", UID: "kv:cystemd/web/config"}},
		{ResourceRef: group, ParentRefs: []controller.ResourceRef{job}, Health: healthy, Info: []controller.InfoItem{{Name: "Running", Value: "2"}, {Name: "Desired", Value: "2"}}},
	}}
	for i := 0; i < allocs; i++ {
		ref := controller.ResourceRef{Kind: v1.KindAllocation, Namespace: "default", Name: fmt.Sprintf("web.app[%02d]", i), UID: fmt.Sprintf("alloc:%02d", i)}
		rt.Nodes = append(rt.Nodes,
			controller.ResourceNode{ResourceRef: ref, ParentRefs: []controller.ResourceRef{group}, Health: healthy,
				Info: []controller.InfoItem{{Name: "Node", Value: fmt.Sprintf("node-%d", i%2)}, {Name: "Status", Value: "running"}}},
			controller.ResourceNode{ResourceRef: controller.ResourceRef{Kind: v1.KindTask, Namespace: "default", Name: "app", UID: fmt.Sprintf("task:%02d/app", i)},
				ParentRefs: []controller.ResourceRef{ref}, Health: healthy, Info: []controller.InfoItem{{Name: "State", Value: "running"}}},
		)
	}
	return app, rt
}

func TestTreeLayout(t *testing.T) {
	app, rt := testTree(2)
	perms := views.AppPerms{Sync: true, Action: true}
	tree := buildTree(treeContext{app: app, view: "tree", perms: perms, live: &controller.Live{}}, rt)

	byUID := map[string]views.TreeNode{}
	for _, n := range tree.Nodes {
		byUID[n.UID] = n
	}
	// app, job, kv, missing job, group, 2 allocs, 2 tasks.
	if len(tree.Nodes) != 9 || len(tree.Edges) != 8 {
		t.Fatalf("nodes=%d edges=%d", len(tree.Nodes), len(tree.Edges))
	}
	root, job, group := byUID["app:web"], byUID["job:default/web"], byUID["group:default/web/app"]
	if !root.Root || root.X != treePad || job.X != treePad+nodeWidth+rankSep || group.X != treePad+2*(nodeWidth+rankSep) {
		t.Fatalf("columns: root=%d job=%d group=%d", root.X, job.X, group.X)
	}
	// Parents centre on their children.
	a0, a1 := byUID["alloc:00"], byUID["alloc:01"]
	if group.Y != (a0.Y+a1.Y)/2 || a1.Y <= a0.Y {
		t.Fatalf("group y=%d allocs %d,%d", group.Y, a0.Y, a1.Y)
	}
	// Managed resources carry their sync status; missing ones show up.
	if job.Sync != "Synced" || byUID["kv:cystemd/web/config"].Sync != "OutOfSync" {
		t.Fatalf("sync status: job=%q kv=%q", job.Sync, byUID["kv:cystemd/web/config"].Sync)
	}
	if missing, ok := byUID["missing:Job/default/gone"]; !ok || missing.Health != "Missing" {
		t.Fatalf("missing job node: %+v", missing)
	}
	// State-aware actions: a running job can be restarted, not started.
	var names []string
	for _, a := range job.Actions {
		names = append(names, a.Name)
	}
	if strings.Join(names, ",") != "sync,restart,stop" {
		t.Fatalf("job actions = %v", names)
	}
	if byUID["alloc:00"].LogsHref == "" || !strings.Contains(byUID["alloc:00"].LogsHref, "tab=logs") {
		t.Fatalf("alloc logs link: %q", byUID["alloc:00"].LogsHref)
	}
	if tree.Width != 2*treePad+5*nodeWidth+4*rankSep {
		t.Fatalf("width = %d", tree.Width)
	}

	// Filtering keeps the matches and their ancestors.
	filtered := buildTree(treeContext{app: app, view: "tree", filter: "web.app[01]"}, rt)
	var uids []string
	for _, n := range filtered.Nodes {
		uids = append(uids, n.UID)
	}
	if strings.Join(uids, " ") != "app:web job:default/web group:default/web/app alloc:01" {
		t.Fatalf("filtered = %v", uids)
	}

	// Large groups are capped with a "+N more" node.
	app, rt = testTree(maxSiblings + 5)
	big := buildTree(treeContext{app: app, view: "tree"}, rt)
	more := 0
	for _, n := range big.Nodes {
		if n.More > 0 {
			more = n.More
		}
	}
	if more != 5 {
		t.Fatalf("more = %d", more)
	}
	groups := nodeGroups(big, rt)
	if len(groups) != 2 || groups[0].Node != "node-0" {
		t.Fatalf("node groups: %+v", groups)
	}
}

func TestTreeHooks(t *testing.T) {
	app, rt := testTree(0)
	app.Status.Resources = append(app.Status.Resources,
		v1.ResourceStatus{Kind: v1.KindJob, Namespace: "default", Name: "migrate", Hook: true},
		v1.ResourceStatus{Kind: v1.KindJob, Namespace: "default", Name: "smoke", Hook: true}, // deleted by its policy
	)
	rt.Nodes = append(rt.Nodes, controller.ResourceNode{
		ResourceRef: controller.ResourceRef{Kind: v1.KindJob, Namespace: "default", Name: "migrate", UID: "job:default/migrate"},
		Health:      &v1.HealthStatus{Status: v1.HealthStatusHealthy},
		Info:        []controller.InfoItem{{Name: "Type", Value: "batch"}, {Name: "Status", Value: "dead"}, {Name: "Hook", Value: "PreSync"}},
	})
	tree := buildTree(treeContext{app: app, view: "tree", perms: views.AppPerms{Sync: true, Action: true}, live: &controller.Live{}}, rt)
	var hook *views.TreeNode
	for i, n := range tree.Nodes {
		switch n.UID {
		case "job:default/migrate":
			hook = &tree.Nodes[i]
		case "missing:Job/default/smoke":
			t.Fatal("a hook job deleted by its policy shows as missing")
		}
	}
	if hook == nil || !hook.Hook || hook.Sync != "" || strings.Join(hook.Info, ",") != "PreSync hook,batch,dead" || len(hook.Actions) != 0 {
		t.Fatalf("hook node = %+v", hook)
	}

	// The sync panel offers the other resources only; selecting all of them
	// is a full sync, which runs the hooks.
	var offered []string
	for _, r := range syncResources(app) {
		offered = append(offered, r.Name)
	}
	if strings.Join(offered, ",") != "web,cystemd/web/config,gone" {
		t.Fatalf("sync panel resources = %v", offered)
	}
	all := url.Values{"resource": {"Job/default/web", "ConsulKV//cystemd/web/config", "Job/default/gone"}}
	if req, err := syncRequestFromForm(all, app); err != nil || len(req.Resources) != 0 {
		t.Fatalf("all resources selected: %+v, %v", req.Resources, err)
	}
	some := url.Values{"resource": {"Job/default/web"}}
	if req, err := syncRequestFromForm(some, app); err != nil || len(req.Resources) != 1 {
		t.Fatalf("one resource selected: %+v, %v", req.Resources, err)
	}
}

func TestTreeOrphans(t *testing.T) {
	app, rt := testTree(0)
	rt.OrphanedNodes = []controller.ResourceNode{{
		ResourceRef: controller.ResourceRef{Kind: v1.KindJob, Namespace: "default", Name: "stray", UID: "job:default/stray"},
		Info:        []controller.InfoItem{{Name: "Type", Value: "service"}, {Name: "Status", Value: "running"}},
	}}
	perms := views.AppPerms{Sync: true, Action: true}
	hidden := buildTree(treeContext{app: app, view: "tree", perms: perms, live: &controller.Live{}}, rt)
	for _, n := range hidden.Nodes {
		if n.UID == "job:default/stray" {
			t.Fatal("orphans shown without asking")
		}
	}
	shown := buildTree(treeContext{app: app, view: "tree", perms: perms, live: &controller.Live{}, orphaned: true}, rt)
	var stray *views.TreeNode
	for i, n := range shown.Nodes {
		if n.UID == "job:default/stray" {
			stray = &shown.Nodes[i]
		}
	}
	if stray == nil || !stray.Unmanaged || stray.Sync != "" || len(stray.Actions) != 0 || strings.Join(stray.Info, ",") != "orphaned,service" {
		t.Fatalf("orphan node = %+v", stray)
	}
}

func TestEdgePath(t *testing.T) {
	if p := edgePath(0, 10, 100, 10); p != "M0 10H100" {
		t.Fatalf("straight edge = %q", p)
	}
	if p := edgePath(0, 10, 100, 60); !strings.HasPrefix(p, "M0 10H32Q40 10 40 18V52Q40 60 48 60H100") {
		t.Fatalf("elbow edge = %q", p)
	}
}

func TestAppFormRoundTrip(t *testing.T) {
	replicas := 0
	limit := int64(5)
	apps := []*v1.Application{
		{
			Metadata: v1.ObjectMeta{Name: "web", Labels: map[string]string{"team": "a"}},
			Spec: v1.ApplicationSpec{
				Project: "default",
				Source: v1.ApplicationSource{RepoURL: "https://git.example.com/ops.git", Path: "apps/web", TargetRevision: "main",
					Nomad: &v1.NomadSource{Files: []string{"*.hcl"}, VarFiles: []string{"prod.vars.hcl"}, Vars: map[string]string{"tag": "1"}, ConsulKV: "kv.yaml"}},
				Destination: v1.ApplicationDestination{Namespace: "web", Region: "eu"},
				SyncPolicy: &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{Prune: true},
					SyncOptions: []string{v1.SyncOptionPruneFalse, v1.SyncOptionCreateNamespace}},
				RevisionHistoryLimit: &limit,
			},
		},
		{
			Metadata: v1.ObjectMeta{Name: "svc"},
			Spec: v1.ApplicationSpec{
				Project: "default",
				Source: v1.ApplicationSource{RepoURL: "git@git.example.com:ops.git", Systemd: &v1.SystemdSource{
					ValuesFile: "values.yaml", Selector: "prod", ConfigKey: ".config", VersionKey: ".image.tag", Unit: "svc.service",
					Replicas: &replicas, NodeSelector: "web", Datacenters: []string{"dc1", "dc2"}, SecretsDelivery: "vault-agent",
					Secrets: []v1.SystemdSecret{{VaultPath: "kv/app"}, {VaultPath: "kv/db", Destination: "db.env"}},
					Service: &v1.ServiceSpec{Port: 8080, Check: &v1.CheckSpec{Type: "http", Path: "/healthz"}},
				}},
			},
		},
	}
	for _, a := range apps {
		next := a.DeepCopy()
		if err := applyAppForm(next, appForm(a)); err != nil {
			t.Fatalf("%s: %v", a.Metadata.Name, err)
		}
		next.APIVersion, next.Kind = "", ""
		if !reflect.DeepEqual(next.Spec, a.Spec) || !reflect.DeepEqual(next.Metadata.Labels, a.Metadata.Labels) {
			t.Fatalf("%s: form round trip changed the spec:\n got %+v\nwant %+v", a.Metadata.Name, next.Spec, a.Spec)
		}
		back, err := appFromYAML(appYAML(a))
		if err != nil {
			t.Fatalf("%s: yaml: %v", a.Metadata.Name, err)
		}
		if !reflect.DeepEqual(back.Spec, a.Spec) {
			t.Fatalf("%s: yaml round trip changed the spec:\n got %+v\nwant %+v", a.Metadata.Name, back.Spec, a.Spec)
		}
	}
	if _, err := appFromYAML("kind: AppProject\nmetadata: {name: x}\n"); err == nil {
		t.Fatal("wrong kind accepted")
	}
}
