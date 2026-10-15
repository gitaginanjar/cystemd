// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/diff"
	"github.com/gitaginanjar/cystemd/internal/health"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// ResourceRef identifies a node of the resource tree.
type ResourceRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

// InfoItem is a name/value shown on a tree node.
type InfoItem struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ResourceNode is a node of the resource tree (Argo CD's ResourceNode).
type ResourceNode struct {
	ResourceRef
	ParentRefs      []ResourceRef    `json:"parentRefs,omitempty"`
	Info            []InfoItem       `json:"info,omitempty"`
	Health          *v1.HealthStatus `json:"health,omitempty"`
	CreatedAt       *time.Time       `json:"createdAt,omitempty"`
	Images          []string         `json:"images,omitempty"`
	ResourceVersion string           `json:"resourceVersion,omitempty"`
}

// ResourceTree is the live resource tree of an Application.
type ResourceTree struct {
	Nodes []ResourceNode `json:"nodes"`
	// OrphanedNodes are the jobs of its destination namespace no
	// Application tracks, when its project watches for them.
	OrphanedNodes []ResourceNode `json:"orphanedNodes,omitempty"`
}

// Live reads the live state of Applications for the API. It needs no
// leadership: any replica can serve it.
type Live struct {
	Clusters *nomad.Clusters
	Consul   ConsulKV
	Checks   ConsulChecks
	Repos    Repos
}

func jobUID(ns, id string) string { return "job:" + ns + "/" + id }

// Tree builds the resource tree from the live cluster.
func (l *Live) Tree(ctx context.Context, app *v1.Application) (*ResourceTree, error) {
	cluster, err := l.Clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, err
	}
	tree := &ResourceTree{Nodes: []ResourceNode{}}
	for _, r := range app.Status.Resources {
		switch r.Kind {
		case v1.KindJob:
			nodes, err := l.jobNodes(ctx, cluster, r)
			if err != nil {
				return nil, err
			}
			tree.Nodes = append(tree.Nodes, nodes...)
		case v1.KindConsulKV:
			node := ResourceNode{ResourceRef: ResourceRef{Kind: v1.KindConsulKV, Name: r.Name, UID: "kv:" + r.Name}, Health: r.Health}
			if l.Consul != nil {
				if v, ok, err := l.Consul.Get(ctx, r.Name); err == nil && ok {
					node.Info = append(node.Info, InfoItem{"Size", fmt.Sprintf("%d bytes", len(v))})
				}
			}
			tree.Nodes = append(tree.Nodes, node)
		}
	}
	return tree, nil
}

// jobNodes returns the job node and its live children.
func (l *Live) jobNodes(ctx context.Context, cluster nomad.API, r v1.ResourceStatus) ([]ResourceNode, error) {
	job, err := cluster.Job(ctx, r.Namespace, r.Name)
	if err != nil {
		return nil, err
	}
	if job == nil {
		return nil, nil
	}
	ns, id := r.Namespace, r.Name
	jobRef := ResourceRef{Kind: v1.KindJob, Namespace: ns, Name: id, UID: jobUID(ns, id)}
	jobNode := ResourceNode{ResourceRef: jobRef, Health: r.Health, ResourceVersion: strconv.FormatUint(derefU64(job.JobModifyIndex), 10)}
	jobNode.Info = []InfoItem{{"Type", deref(job.Type)}, {"Status", deref(job.Status)}, {"Version", strconv.FormatUint(derefU64(job.Version), 10)}}
	if job.NodePool != nil && *job.NodePool != "" {
		jobNode.Info = append(jobNode.Info, InfoItem{"Node pool", *job.NodePool})
	}
	if isHook(job.Meta) {
		jobNode.Info = append(jobNode.Info, InfoItem{"Hook", job.Meta[v1.HookMetaKey]})
	}
	if job.SubmitTime != nil {
		t := time.Unix(0, *job.SubmitTime).UTC()
		jobNode.CreatedAt = &t
	}
	jobNode.Images = imagesOf(job)
	nodes := []ResourceNode{jobNode}

	if d, err := cluster.LatestDeployment(ctx, ns, id); err == nil && d != nil {
		h := health.DeploymentHealth(d)
		dn := ResourceNode{
			ResourceRef: ResourceRef{Kind: v1.KindDeployment, Namespace: ns, Name: short8(d.ID), UID: "deployment:" + d.ID},
			ParentRefs:  []ResourceRef{jobRef},
			Health:      &h,
			Info:        []InfoItem{{"Status", d.Status}, {"Job version", strconv.FormatUint(d.JobVersion, 10)}, {"ID", d.ID}},
		}
		nodes = append(nodes, dn)
	}

	allocs, err := cluster.Allocations(ctx, ns, id)
	if err != nil {
		return nil, err
	}
	summary, _ := cluster.Summary(ctx, ns, id)
	checks := map[string]string{}
	(&Controller{checks: l.Checks}).collectChecks(ctx, cluster, job, allocs, checks)

	version := derefU64(job.Version)
	system := deref(job.Type) == "system" || deref(job.Type) == "sysbatch"
	for _, tg := range job.TaskGroups {
		group := deref(tg.Name)
		groupRef := ResourceRef{Kind: v1.KindTaskGroup, Namespace: ns, Name: id + "." + group, UID: "group:" + ns + "/" + id + "/" + group}
		desired := 0
		if tg.Count != nil {
			desired = *tg.Count
		}
		var gs api.TaskGroupSummary
		if summary != nil {
			gs = summary.Summary[group]
		}
		gh := health.GroupHealth(desired, gs, system)
		info := []InfoItem{{"Running", strconv.Itoa(gs.Running)}}
		if !system {
			info = append(info, InfoItem{"Desired", strconv.Itoa(desired)})
		}
		if gs.Queued > 0 {
			info = append(info, InfoItem{"Queued", strconv.Itoa(gs.Queued)})
		}
		if gs.Failed > 0 {
			info = append(info, InfoItem{"Failed", strconv.Itoa(gs.Failed)})
		}
		nodes = append(nodes, ResourceNode{ResourceRef: groupRef, ParentRefs: []ResourceRef{jobRef}, Health: &gh, Info: info})

		// Services and their checks.
		services := append([]*api.Service(nil), tg.Services...)
		for _, t := range tg.Tasks {
			services = append(services, t.Services...)
		}
		for _, s := range services {
			var states []string
			for _, a := range allocs {
				if a.TaskGroup == group && a.ClientStatus == "running" && a.JobVersion == version {
					states = append(states, checks[a.ID])
				}
			}
			sh := health.ChecksHealth(states)
			provider := s.Provider
			if provider == "" {
				provider = "consul"
			}
			sinfo := []InfoItem{{"Provider", provider}, {"Checks", strconv.Itoa(len(s.Checks))}}
			if s.PortLabel != "" {
				sinfo = append(sinfo, InfoItem{"Port", s.PortLabel})
			}
			nodes = append(nodes, ResourceNode{
				ResourceRef: ResourceRef{Kind: v1.KindService, Namespace: ns, Name: s.Name, UID: "service:" + ns + "/" + id + "/" + group + "/" + s.Name},
				ParentRefs:  []ResourceRef{groupRef},
				Health:      &sh,
				Info:        sinfo,
			})
		}

		for _, a := range groupAllocs(allocs, group, version) {
			ah := health.AllocHealth(a, checks[a.ID])
			allocRef := ResourceRef{Kind: v1.KindAllocation, Namespace: ns, Name: a.Name, UID: "alloc:" + a.ID}
			created := time.Unix(0, a.CreateTime).UTC()
			an := ResourceNode{
				ResourceRef: allocRef, ParentRefs: []ResourceRef{groupRef}, Health: &ah, CreatedAt: &created,
				ResourceVersion: strconv.FormatUint(a.ModifyIndex, 10),
				Info: []InfoItem{
					{"Node", a.NodeName}, {"Status", a.ClientStatus}, {"Desired", a.DesiredStatus},
					{"Job version", strconv.FormatUint(a.JobVersion, 10)}, {"ID", a.ID},
				},
			}
			nodes = append(nodes, an)
			taskNames := make([]string, 0, len(a.TaskStates))
			for name := range a.TaskStates {
				taskNames = append(taskNames, name)
			}
			sort.Strings(taskNames)
			for _, name := range taskNames {
				ts := a.TaskStates[name]
				th := health.TaskHealth(ts)
				tn := ResourceNode{
					ResourceRef: ResourceRef{Kind: v1.KindTask, Namespace: ns, Name: name, UID: "task:" + a.ID + "/" + name},
					ParentRefs:  []ResourceRef{allocRef},
					Health:      &th,
					Info:        []InfoItem{{"State", ts.State}, {"Restarts", strconv.FormatUint(ts.Restarts, 10)}},
				}
				if driver := taskDriver(tg, name); driver != "" {
					tn.Info = append(tn.Info, InfoItem{"Driver", driver})
				}
				if !ts.StartedAt.IsZero() {
					t := ts.StartedAt.UTC()
					tn.CreatedAt = &t
				}
				nodes = append(nodes, tn)
			}
		}
	}
	return nodes, nil
}

// groupAllocs keeps the allocations worth showing: anything not terminal,
// and the current version's failed ones.
func groupAllocs(allocs []*api.AllocationListStub, group string, version uint64) []*api.AllocationListStub {
	var out []*api.AllocationListStub
	for _, a := range allocs {
		if a.TaskGroup != group {
			continue
		}
		terminal := a.ClientStatus == "complete" || a.ClientStatus == "failed" || a.ClientStatus == "lost"
		if !terminal || (a.JobVersion == version && a.ClientStatus == "failed" && a.DesiredStatus == "run") ||
			(a.JobVersion == version && a.ClientStatus == "complete" && a.DesiredStatus == "run") {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func taskDriver(tg *api.TaskGroup, name string) string {
	for _, t := range tg.Tasks {
		if t.Name == name {
			return t.Driver
		}
	}
	return ""
}

func imagesOf(job *api.Job) []string {
	var out []string
	for _, tg := range job.TaskGroups {
		for _, t := range tg.Tasks {
			switch t.Driver {
			case "docker", "podman":
				if img, ok := t.Config["image"].(string); ok {
					out = append(out, img)
				}
			case "systemd":
				if pkgs, ok := t.Config["package"].([]any); ok && len(pkgs) > 0 {
					if p, ok := pkgs[0].(map[string]any); ok {
						out = append(out, fmt.Sprintf("pkg:%v@%v", p["name"], p["version"]))
						continue
					}
				}
				if u, ok := t.Config["unit"].(string); ok {
					out = append(out, "unit:"+u)
				}
			}
		}
	}
	return out
}

func short8(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// orphanedJobs are the jobs of the Application's destination namespace
// that no Application tracks: Argo CD's orphaned resources. Child jobs of
// periodic and parameterized jobs, and stopped jobs, are not orphans.
func orphanedJobs(ctx context.Context, cluster nomad.API, app *v1.Application, s *v1.OrphanedResourcesMonitorSettings) ([]*api.JobListStub, error) {
	ns := app.Spec.Destination.NamespaceOrDefault()
	stubs, err := cluster.Jobs(ctx, ns)
	if err != nil {
		return nil, err
	}
	var out []*api.JobListStub
	for _, j := range stubs {
		if j.Namespace == "" {
			j.Namespace = ns
		}
		if j.Meta[v1.AppMetaKey] != "" || j.ParentID != "" || j.Stop || s.Ignored(v1.KindJob, j.ID) {
			continue
		}
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].ID < out[k].ID })
	return out, nil
}

// OrphanedNodes are the Application's orphaned jobs, as tree nodes.
func (l *Live) OrphanedNodes(ctx context.Context, app *v1.Application, s *v1.OrphanedResourcesMonitorSettings) ([]ResourceNode, error) {
	cluster, err := l.Clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, err
	}
	jobs, err := orphanedJobs(ctx, cluster, app, s)
	if err != nil {
		return nil, err
	}
	out := make([]ResourceNode, 0, len(jobs))
	for _, j := range jobs {
		n := ResourceNode{
			ResourceRef:     ResourceRef{Kind: v1.KindJob, Namespace: j.Namespace, Name: j.ID, UID: jobUID(j.Namespace, j.ID)},
			Info:            []InfoItem{{"Type", j.Type}, {"Status", j.Status}},
			ResourceVersion: strconv.FormatUint(j.JobModifyIndex, 10),
		}
		if j.SubmitTime != 0 {
			t := time.Unix(0, j.SubmitTime).UTC()
			n.CreatedAt = &t
		}
		out = append(out, n)
	}
	return out, nil
}

// OrphanManifest returns the live manifest of one of the Application's
// orphaned jobs.
func (l *Live) OrphanManifest(ctx context.Context, app *v1.Application, s *v1.OrphanedResourcesMonitorSettings, namespace, name string) (string, error) {
	nodes, err := l.OrphanedNodes(ctx, app, s)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		if n.Namespace == namespace && n.Name == name {
			cluster, err := l.Clusters.Get(app.Spec.Destination.Name)
			if err != nil {
				return "", err
			}
			job, err := cluster.Job(ctx, namespace, name)
			if err != nil || job == nil {
				return "", orMissing(err)
			}
			m, err := diff.Normalize(job)
			if err != nil {
				return "", err
			}
			return diff.ToYAML(m), nil
		}
	}
	return "", ErrNotManaged
}

// LiveManifest returns the live object of a tree node as YAML.
func (l *Live) LiveManifest(ctx context.Context, app *v1.Application, kind, namespace, name, uid string) (string, error) {
	cluster, err := l.Clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return "", err
	}
	if !l.belongs(app, kind, namespace, name, uid) {
		return "", ErrNotManaged
	}
	switch kind {
	case v1.KindJob:
		job, err := cluster.Job(ctx, namespace, name)
		if err != nil || job == nil {
			return "", orMissing(err)
		}
		m, err := diff.Normalize(job)
		if err != nil {
			return "", err
		}
		return diff.ToYAML(m), nil
	case v1.KindAllocation:
		a, err := cluster.Allocation(ctx, strings.TrimPrefix(uid, "alloc:"))
		if err != nil || a == nil {
			return "", orMissing(err)
		}
		a.Job = nil // the job is shown on its own node
		return toYAML(a)
	case v1.KindDeployment:
		d, err := cluster.Deployment(ctx, strings.TrimPrefix(uid, "deployment:"))
		if err != nil || d == nil {
			return "", orMissing(err)
		}
		return toYAML(d)
	case v1.KindTask:
		allocID, task, _ := strings.Cut(strings.TrimPrefix(uid, "task:"), "/")
		a, err := cluster.Allocation(ctx, allocID)
		if err != nil || a == nil {
			return "", orMissing(err)
		}
		out := map[string]any{"State": a.TaskStates[task]}
		if a.Job != nil {
			for _, tg := range a.Job.TaskGroups {
				for _, t := range tg.Tasks {
					if t.Name == task && deref(tg.Name) == a.TaskGroup {
						out["Task"] = t
					}
				}
			}
		}
		return toYAML(out)
	case v1.KindConsulKV:
		if l.Consul == nil {
			return "", errors.New("consul is not configured")
		}
		v, ok, err := l.Consul.Get(ctx, name)
		if err != nil || !ok {
			return "", orMissing(err)
		}
		return string(v), nil
	case v1.KindTaskGroup, v1.KindService:
		parts := strings.Split(strings.SplitN(uid, ":", 2)[1], "/")
		if len(parts) < 3 {
			return "", fmt.Errorf("bad uid %q", uid)
		}
		job, err := cluster.Job(ctx, parts[0], parts[1])
		if err != nil || job == nil {
			return "", orMissing(err)
		}
		for _, tg := range job.TaskGroups {
			if deref(tg.Name) != parts[2] {
				continue
			}
			if kind == v1.KindTaskGroup {
				return toYAML(tg)
			}
			for _, s := range tg.Services {
				if s.Name == name {
					return toYAML(s)
				}
			}
			for _, t := range tg.Tasks {
				for _, s := range t.Services {
					if s.Name == name {
						return toYAML(s)
					}
				}
			}
		}
		return "", ErrMissing
	}
	return "", fmt.Errorf("unsupported kind %q", kind)
}

// belongs checks that a request targets the Application's own resources.
func (l *Live) belongs(app *v1.Application, kind, namespace, name, uid string) bool {
	jobs := map[string]bool{}
	for _, r := range app.Status.Resources {
		if r.Kind == v1.KindJob {
			jobs[r.Namespace+"/"+r.Name] = true
		}
		if r.Kind == kind && r.Name == name && r.Namespace == namespace {
			return true
		}
	}
	switch kind {
	case v1.KindTaskGroup, v1.KindService:
		parts := strings.Split(strings.SplitN(uid, ":", 2)[1], "/")
		return len(parts) >= 2 && jobs[parts[0]+"/"+parts[1]]
	case v1.KindAllocation, v1.KindDeployment, v1.KindTask:
		return true // verified against the job by the caller via ownedAlloc
	}
	return false
}

// ErrNotManaged rejects requests for resources outside the Application.
var ErrNotManaged = errors.New("resource is not managed by this application")

// ErrMissing reports a live object that no longer exists.
var ErrMissing = errors.New("resource not found")

func orMissing(err error) error {
	if err != nil {
		return err
	}
	return ErrMissing
}

func toYAML(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	var m any
	if err := json.Unmarshal(b, &m); err != nil {
		return "", err
	}
	return diff.ToYAML(m), nil
}

// OwnedAllocation returns the allocation when it belongs to one of the
// Application's jobs.
func (l *Live) OwnedAllocation(ctx context.Context, app *v1.Application, allocID string) (*api.Allocation, nomad.API, error) {
	cluster, err := l.Clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, nil, err
	}
	a, err := cluster.Allocation(ctx, allocID)
	if err != nil {
		return nil, nil, err
	}
	if a == nil {
		return nil, nil, ErrMissing
	}
	for _, r := range app.Status.Resources {
		if r.Kind == v1.KindJob && r.Namespace == a.Namespace && r.Name == a.JobID {
			return a, cluster, nil
		}
	}
	return nil, nil, ErrNotManaged
}

// ResourceDiff is one managed resource's desired and live state (Argo CD's
// ResourceDiff, the APP DIFF view).
type ResourceDiff struct {
	Kind                string `json:"kind"`
	Namespace           string `json:"namespace,omitempty"`
	Name                string `json:"name"`
	TargetState         string `json:"targetState"`
	LiveState           string `json:"liveState"`
	NormalizedLiveState string `json:"normalizedLiveState"`
	PlanDiff            string `json:"planDiff,omitempty"`
	Modified            bool   `json:"modified"`
	RequiresPruning     bool   `json:"requiresPruning,omitempty"`
}

// ManagedResources renders the desired state at the compared revision and
// pairs every managed resource with its live state.
func (l *Live) ManagedResources(ctx context.Context, app *v1.Application) ([]ResourceDiff, error) {
	cluster, err := l.Clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, err
	}
	revision := app.Status.Sync.Revision
	if revision == "" {
		if revision, err = l.Repos.Resolve(ctx, app.Spec.Source.RepoURL, app.Spec.Source.Revision()); err != nil {
			return nil, err
		}
	}
	manifests, err := l.Repos.Manifests(ctx, app, app.Spec.Source, revision, false)
	if err != nil {
		return nil, err
	}
	ignore := ignoredFields(app)
	out := []ResourceDiff{}
	desired := map[string]bool{}
	for _, job := range manifests.Jobs {
		ns, id := deref(job.Namespace), deref(job.ID)
		desired[ns+"/"+id] = true
		if isHook(job.Meta) {
			continue // run by syncs, not compared
		}
		rd := ResourceDiff{Kind: v1.KindJob, Namespace: ns, Name: id}
		live, err := cluster.Job(ctx, ns, id)
		if err != nil {
			return nil, err
		}
		if live != nil {
			if fields := ignore.fieldsFor(v1.KindJob, ns, id); len(fields) > 0 {
				job = withIgnoredFields(job, live, fields)
			}
		}
		target, _ := diff.Normalize(job)
		rd.TargetState = diff.ToYAML(target)
		if live == nil {
			rd.Modified = true
		} else {
			liveMap, _ := diff.Normalize(live)
			rd.LiveState = diff.ToYAML(liveMap)
			rd.NormalizedLiveState = diff.ToYAML(diff.PruneToShape(liveMap, target))
			if plan, err := cluster.Plan(ctx, job); err == nil {
				rd.Modified = diff.HasChanges(plan.Diff)
				rd.PlanDiff = diff.FormatJobDiff(plan.Diff)
			} else {
				rd.Modified = rd.NormalizedLiveState != rd.TargetState
			}
		}
		out = append(out, rd)
	}
	for _, kv := range manifests.KV {
		rd := ResourceDiff{Kind: v1.KindConsulKV, Name: kv.Key, TargetState: string(kv.Value)}
		if l.Consul != nil {
			if v, ok, err := l.Consul.Get(ctx, kv.Key); err == nil && ok {
				rd.LiveState, rd.NormalizedLiveState = string(v), string(v)
			}
		}
		rd.Modified = rd.LiveState != rd.TargetState
		out = append(out, rd)
	}
	for _, r := range app.Status.Resources {
		if !r.RequiresPruning {
			continue
		}
		rd := ResourceDiff{Kind: r.Kind, Namespace: r.Namespace, Name: r.Name, Modified: true, RequiresPruning: true}
		if r.Kind == v1.KindJob && !desired[r.Namespace+"/"+r.Name] {
			if live, err := cluster.Job(ctx, r.Namespace, r.Name); err == nil && live != nil {
				m, _ := diff.Normalize(live)
				rd.LiveState = diff.ToYAML(m)
				rd.NormalizedLiveState = rd.LiveState
			}
		}
		out = append(out, rd)
	}
	return out, nil
}

// Action is a resource action offered in the UI's node menu.
type Action struct {
	Name     string `json:"name"`
	Disabled bool   `json:"disabled,omitempty"`
}

// Actions lists the actions available on a node.
func (l *Live) Actions(kind string, h *v1.HealthStatus) []Action {
	switch kind {
	case v1.KindJob:
		return []Action{{Name: "restart"}, {Name: "stop"}, {Name: "start"}}
	case v1.KindAllocation:
		return []Action{{Name: "restart"}, {Name: "stop"}}
	case v1.KindTask:
		return []Action{{Name: "restart"}, {Name: "signal-hup"}}
	case v1.KindDeployment:
		return []Action{{Name: "promote"}, {Name: "pause"}, {Name: "resume"}, {Name: "fail"}}
	}
	return nil
}

// RunAction executes a resource action.
func (l *Live) RunAction(ctx context.Context, app *v1.Application, kind, namespace, name, uid, action string) error {
	cluster, err := l.Clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return err
	}
	switch kind {
	case v1.KindJob:
		if !l.belongs(app, kind, namespace, name, uid) {
			return ErrNotManaged
		}
		switch action {
		case "stop":
			_, err := cluster.Deregister(ctx, namespace, name, false)
			return err
		case "start":
			job, err := cluster.Job(ctx, namespace, name)
			if err != nil || job == nil {
				return orMissing(err)
			}
			job.Stop = new(false)
			_, err = cluster.Register(ctx, job, &api.RegisterOptions{EnforceIndex: true, ModifyIndex: derefU64(job.JobModifyIndex)})
			return err
		case "restart":
			allocs, err := cluster.Allocations(ctx, namespace, name)
			if err != nil {
				return err
			}
			// Rolling: one allocation at a time.
			for _, a := range allocs {
				if a.ClientStatus == "running" && a.DesiredStatus == "run" {
					if err := cluster.RestartAllocation(ctx, a.ID, ""); err != nil {
						return fmt.Errorf("restart %s: %w", a.Name, err)
					}
				}
			}
			return nil
		}
	case v1.KindAllocation:
		allocID := strings.TrimPrefix(uid, "alloc:")
		if _, _, err := l.OwnedAllocation(ctx, app, allocID); err != nil {
			return err
		}
		switch action {
		case "restart":
			return cluster.RestartAllocation(ctx, allocID, "")
		case "stop":
			return cluster.StopAllocation(ctx, allocID)
		}
	case v1.KindTask:
		allocID, task, _ := strings.Cut(strings.TrimPrefix(uid, "task:"), "/")
		if _, _, err := l.OwnedAllocation(ctx, app, allocID); err != nil {
			return err
		}
		switch action {
		case "restart":
			return cluster.RestartAllocation(ctx, allocID, task)
		case "signal-hup":
			return cluster.SignalAllocation(ctx, allocID, task, "SIGHUP")
		}
	case v1.KindDeployment:
		id := strings.TrimPrefix(uid, "deployment:")
		d, err := cluster.Deployment(ctx, id)
		if err != nil || d == nil {
			return orMissing(err)
		}
		if !l.belongs(app, v1.KindJob, d.Namespace, d.JobID, "") {
			return ErrNotManaged
		}
		switch action {
		case "promote":
			return cluster.PromoteDeployment(ctx, id)
		case "pause":
			return cluster.PauseDeployment(ctx, id, true)
		case "resume":
			return cluster.PauseDeployment(ctx, id, false)
		case "fail":
			return cluster.FailDeployment(ctx, id)
		}
	}
	return fmt.Errorf("action %q is not available on %s", action, kind)
}
