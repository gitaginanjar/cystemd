// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/diff"
	"github.com/gitaginanjar/cystemd/internal/health"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/render"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// comparison is the outcome of comparing desired with live state.
type comparison struct {
	revision  string
	manifests *render.Manifests
	resources []v1.ResourceStatus
	sync      v1.SyncStatusCode
	health    v1.HealthStatus
	// desired jobs after ignoreDifferences, keyed "<ns>/<id>".
	desired map[string]*api.Job
	live    map[string]*api.Job
	plans   map[string]*api.JobPlanResponse
	// kv: desired values; liveKV: live values (nil = missing).
	kv     map[string][]byte
	liveKV map[string][]byte
	// prune lists resources that are live, tracked and no longer desired.
	prune      []v1.ResourceStatus
	conditions []v1.ApplicationCondition
}

// syncWave orders job registration (meta cystemd.sync-wave, default 0).
func syncWave(job *api.Job) int64 {
	if job == nil || job.Meta == nil {
		return 0
	}
	w, err := strconv.ParseInt(job.Meta["cystemd.sync-wave"], 10, 64)
	if err != nil {
		return 0
	}
	return w
}

// planCache memoises Nomad plans: a plan depends only on the desired spec
// and the live job's modify index, so unchanged pairs are not re-planned.
type planCache struct {
	mu      sync.Mutex
	entries map[string]planEntry
}

type planEntry struct {
	resp *api.JobPlanResponse
	at   time.Time
}

const planCacheTTL = 30 * time.Minute

func (p *planCache) get(key string) *api.JobPlanResponse {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok && time.Since(e.at) < planCacheTTL {
		return e.resp
	}
	return nil
}

func (p *planCache) put(key string, resp *api.JobPlanResponse) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.entries == nil || len(p.entries) > 4096 {
		p.entries = map[string]planEntry{}
	}
	p.entries[key] = planEntry{resp: resp, at: time.Now()}
}

func planKey(job *api.Job, liveIndex uint64) string {
	b, _ := json.Marshal(job)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]) + ":" + strconv.FormatUint(liveIndex, 10)
}

// compare renders the desired state at the resolved revision and compares
// it with the cluster. noCache forces a fresh render (hard refresh).
func (c *Controller) compare(ctx context.Context, app *v1.Application, noCache bool) (*comparison, error) {
	src := app.Spec.Source
	cmp := &comparison{
		desired: map[string]*api.Job{}, live: map[string]*api.Job{}, plans: map[string]*api.JobPlanResponse{},
		kv: map[string][]byte{}, liveKV: map[string][]byte{},
	}

	project, err := c.store.GetProject(ctx, app.Spec.Project)
	if err != nil {
		return nil, fmt.Errorf("project %q: %w", app.Spec.Project, err)
	}
	if !project.IsSourcePermitted(src.RepoURL) {
		return nil, fmt.Errorf("application repository %s is not permitted in project %q", src.RepoURL, project.Metadata.Name)
	}

	cluster, err := c.clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, err
	}
	sha, err := c.repos.Resolve(ctx, src.RepoURL, src.Revision())
	if err != nil {
		return nil, fmt.Errorf("resolve %s@%s: %w", src.RepoURL, src.Revision(), err)
	}
	cmp.revision = sha
	manifests, err := c.repos.Manifests(ctx, app, src, sha, noCache)
	if err != nil {
		return nil, fmt.Errorf("render %s@%s: %w", src.Path, short(sha), err)
	}
	cmp.manifests = manifests

	// Desired jobs.
	for _, job := range manifests.Jobs {
		key := render.JobKey(job)
		dest := app.Spec.Destination
		dest.Namespace = deref(job.Namespace)
		if job.Region != nil && *job.Region != "" {
			dest.Region = *job.Region
		}
		if !project.IsDestinationPermitted(dest) {
			return nil, fmt.Errorf("job %s: destination %s/%s/%s is not permitted in project %q",
				key, dest.ClusterName(), dest.Region, dest.Namespace, project.Metadata.Name)
		}
		cmp.desired[key] = job
	}

	// Live state and per-job comparison.
	var resources []v1.ResourceStatus
	var healths []v1.HealthStatus
	ignore := ignoredFields(app)
	for key, desired := range cmp.desired {
		ns, id := deref(desired.Namespace), deref(desired.ID)
		rs := v1.ResourceStatus{Kind: v1.KindJob, Namespace: ns, Name: id, SyncWave: syncWave(desired)}
		live, err := cluster.Job(ctx, ns, id)
		if err != nil {
			rs.Status = v1.SyncStatusUnknown
			rs.Health = &v1.HealthStatus{Status: v1.HealthStatusUnknown, Message: err.Error()}
			cmp.conditions = append(cmp.conditions, condition(v1.ConditionComparisonError, "job %s: %v", key, err))
			resources = append(resources, rs)
			healths = append(healths, *rs.Health)
			continue
		}
		if live == nil {
			rs.Status = v1.SyncStatusOutOfSync
			rs.Health = &v1.HealthStatus{Status: v1.HealthStatusMissing, Message: "Job does not exist"}
			resources = append(resources, rs)
			healths = append(healths, *rs.Health)
			continue
		}
		cmp.live[key] = live
		if owner := live.Meta[v1.AppMetaKey]; owner != "" && owner != app.Metadata.Name {
			cmp.conditions = append(cmp.conditions, condition("SharedResourceWarning",
				"job %s is also managed by application %q", key, owner))
		}
		rs.Version = live.Version

		desiredForPlan := desired
		if fields := ignore.fieldsFor(v1.KindJob, ns, id); len(fields) > 0 {
			if dm, err := diff.ToMap(desired); err == nil {
				if lm, err := diff.ToMap(live); err == nil {
					diff.ApplyIgnoreDifferences(dm, lm, fields)
					if j, err := diff.FromMap(dm); err == nil {
						desiredForPlan = j
					}
				}
			}
		}
		cmp.desired[key] = desiredForPlan

		pk := planKey(desiredForPlan, derefU64(live.JobModifyIndex))
		plan := c.plans.get(pk)
		if plan == nil {
			plan, err = cluster.Plan(ctx, desiredForPlan)
			if err != nil {
				rs.Status = v1.SyncStatusUnknown
				cmp.conditions = append(cmp.conditions, condition(v1.ConditionComparisonError, "plan %s: %v", key, err))
			} else {
				c.plans.put(pk, plan)
			}
		}
		if plan != nil {
			cmp.plans[key] = plan
			rs.Status = v1.SyncStatusSynced
			if diff.HasChanges(plan.Diff) {
				rs.Status = v1.SyncStatusOutOfSync
			}
		}
		h := c.jobHealth(ctx, cluster, live)
		rs.Health = &h
		healths = append(healths, h)
		resources = append(resources, rs)
	}

	// Tracked jobs no longer in Git: candidates for pruning.
	managed, err := cluster.ManagedJobs(ctx, "*", app.Metadata.Name)
	if err != nil {
		cmp.conditions = append(cmp.conditions, condition(v1.ConditionComparisonError, "list managed jobs: %v", err))
	}
	for _, stub := range managed {
		key := stub.Namespace + "/" + stub.ID
		if _, ok := cmp.desired[key]; ok {
			continue
		}
		// Pruning stops a job; Nomad keeps it until garbage collection. A
		// stopped job no longer in Git is already pruned.
		if stub.Stop {
			continue
		}
		rs := v1.ResourceStatus{Kind: v1.KindJob, Namespace: stub.Namespace, Name: stub.ID,
			Status: v1.SyncStatusOutOfSync, RequiresPruning: true}
		if live, err := cluster.Job(ctx, stub.Namespace, stub.ID); err == nil && live != nil {
			cmp.live[key] = live
			h := c.jobHealth(ctx, cluster, live)
			rs.Health = &h
		}
		resources = append(resources, rs)
		cmp.prune = append(cmp.prune, rs)
	}

	// Consul keys.
	for _, kv := range manifests.KV {
		cmp.kv[kv.Key] = kv.Value
		rs := v1.ResourceStatus{Kind: v1.KindConsulKV, Name: kv.Key, SyncWave: -1}
		if c.consul == nil {
			rs.Status = v1.SyncStatusUnknown
			rs.Health = &v1.HealthStatus{Status: v1.HealthStatusUnknown, Message: "Consul is not configured"}
			cmp.conditions = append(cmp.conditions, condition(v1.ConditionComparisonError, "key %s: Consul is not configured on the control plane", kv.Key))
			resources = append(resources, rs)
			continue
		}
		live, ok, err := c.consul.Get(ctx, kv.Key)
		switch {
		case err != nil:
			rs.Status = v1.SyncStatusUnknown
			rs.Health = &v1.HealthStatus{Status: v1.HealthStatusUnknown, Message: err.Error()}
		case !ok:
			rs.Status = v1.SyncStatusOutOfSync
			rs.Health = &v1.HealthStatus{Status: v1.HealthStatusMissing, Message: "Key does not exist"}
		default:
			cmp.liveKV[kv.Key] = live
			rs.Status = v1.SyncStatusSynced
			if !bytes.Equal(live, kv.Value) {
				rs.Status = v1.SyncStatusOutOfSync
			}
			rs.Health = &v1.HealthStatus{Status: v1.HealthStatusHealthy}
		}
		healths = append(healths, *rs.Health)
		resources = append(resources, rs)
	}
	if c.consul != nil {
		owned, err := c.store.OwnedKeys(ctx, app.Metadata.Name)
		if err != nil {
			cmp.conditions = append(cmp.conditions, condition(v1.ConditionComparisonError, "owned keys: %v", err))
		}
		for _, key := range owned {
			if _, ok := cmp.kv[key]; ok {
				continue
			}
			live, ok, err := c.consul.Get(ctx, key)
			if err != nil || !ok {
				continue
			}
			cmp.liveKV[key] = live
			rs := v1.ResourceStatus{Kind: v1.KindConsulKV, Name: key, Status: v1.SyncStatusOutOfSync, RequiresPruning: true,
				Health: &v1.HealthStatus{Status: v1.HealthStatusHealthy}}
			resources = append(resources, rs)
			cmp.prune = append(cmp.prune, rs)
		}
	}

	sort.Slice(resources, func(i, j int) bool {
		a, b := resources[i], resources[j]
		if a.SyncWave != b.SyncWave {
			return a.SyncWave < b.SyncWave
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	cmp.resources = resources

	cmp.sync = v1.SyncStatusSynced
	for _, r := range resources {
		switch {
		case r.Status == v1.SyncStatusUnknown && cmp.sync == v1.SyncStatusSynced:
			cmp.sync = v1.SyncStatusUnknown
		case r.Status == v1.SyncStatusOutOfSync:
			cmp.sync = v1.SyncStatusOutOfSync
		}
	}
	cmp.health = health.Aggregate(healths)
	if len(manifests.Jobs) == 0 && len(manifests.KV) == 0 {
		cmp.health = v1.HealthStatus{Status: v1.HealthStatusHealthy, Message: "No resources"}
	}
	return cmp, nil
}

// jobHealth gathers what health.JobHealth needs from the cluster.
func (c *Controller) jobHealth(ctx context.Context, cluster nomad.API, live *api.Job) v1.HealthStatus {
	ns, id := deref(live.Namespace), deref(live.ID)
	st := health.JobState{Job: live, Checks: map[string]string{}}
	var err error
	if st.Allocs, err = cluster.Allocations(ctx, ns, id); err != nil {
		return v1.HealthStatus{Status: v1.HealthStatusUnknown, Message: err.Error()}
	}
	st.Deployment, _ = cluster.LatestDeployment(ctx, ns, id)
	st.Summary, _ = cluster.Summary(ctx, ns, id)
	c.collectChecks(ctx, cluster, live, st.Allocs, st.Checks)
	return health.JobHealth(st)
}

// collectChecks fills checks (allocation → worst check state) from Consul
// for consul-provider services and from Nomad for nomad-provider ones.
func (c *Controller) collectChecks(ctx context.Context, cluster nomad.API, job *api.Job, allocs []*api.AllocationListStub, checks map[string]string) {
	consulServices, nomadServices := map[string]bool{}, false
	for _, tg := range job.TaskGroups {
		services := append([]*api.Service(nil), tg.Services...)
		for _, t := range tg.Tasks {
			services = append(services, t.Services...)
		}
		for _, s := range services {
			if len(s.Checks) == 0 {
				continue
			}
			if s.Provider == "nomad" {
				nomadServices = true
			} else {
				consulServices[s.Name] = true
			}
		}
	}
	if c.checks != nil {
		for name := range consulServices {
			if strings.Contains(name, "${") {
				continue // interpolated name: resolved per allocation only
			}
			byAlloc, err := c.checks.ServiceChecks(ctx, name)
			if err != nil {
				continue
			}
			for alloc, state := range byAlloc {
				checks[alloc] = health.WorstCheck(checks[alloc], state)
			}
		}
	}
	if nomadServices {
		for _, a := range allocs {
			if a.ClientStatus != "running" {
				continue
			}
			res, err := cluster.AllocChecks(ctx, a.ID)
			if err != nil {
				continue
			}
			for _, r := range res {
				state := health.CheckPassing
				switch r.Status {
				case "failure":
					state = health.CheckCritical
				case "pending":
					state = health.CheckPending
				}
				checks[a.ID] = health.WorstCheck(checks[a.ID], state)
			}
		}
	}
}

// ignoreIndex maps "<kind>/<namespace>/<name>" to ignored field paths; an
// empty namespace or name in an ignoreDifferences entry matches any.
type ignoreIndex map[string][]string

// ignoredFields indexes the Application's ignoreDifferences.
func ignoredFields(app *v1.Application) ignoreIndex {
	out := ignoreIndex{}
	for _, ig := range app.Spec.IgnoreDifferences {
		kind := ig.Kind
		if kind == "" {
			kind = v1.KindJob
		}
		k := ignoreKey(kind, ig.Namespace, ig.Name)
		out[k] = append(out[k], ig.JSONFields...)
	}
	return out
}

func ignoreKey(kind, ns, name string) string { return kind + "/" + ns + "/" + name }

func (ix ignoreIndex) fieldsFor(kind, ns, name string) []string {
	var out []string
	for _, k := range []string{ignoreKey(kind, ns, name), ignoreKey(kind, "", name), ignoreKey(kind, ns, ""), ignoreKey(kind, "", "")} {
		out = append(out, ix[k]...)
	}
	return out
}

func condition(typ, format string, args ...any) v1.ApplicationCondition {
	now := time.Now().UTC().Truncate(time.Second)
	return v1.ApplicationCondition{Type: typ, Message: fmt.Sprintf(format, args...), LastTransitionTime: &now}
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefU64(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}
