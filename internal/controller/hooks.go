// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/nomad"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Resource hooks are Argo CD's: batch jobs of the source whose meta
// cystemd.hook names the phases they run in. They are not synced like the
// other jobs: PreSync hooks run, one after the other, before anything is
// synced; PostSync hooks once the synced jobs are healthy; SyncFail hooks
// when the sync fails. A hook must finish successfully for the operation to
// go on. Hooks are not compared, not pruned, and do not run on partial
// syncs or rollbacks.

const (
	// defaultHookTimeout bounds a hook run without cystemd.hook-timeout.
	defaultHookTimeout = 30 * time.Minute
	// hookPlacementGrace is how long a dead job without allocations may
	// still be about to get some.
	hookPlacementGrace = time.Minute
)

// hookPoll is how often a running hook is checked (a variable for tests).
var hookPoll = 2 * time.Second

// hookPhases returns the phases job is a hook of (none for a plain job).
func hookPhases(job *api.Job) ([]string, error) {
	var out []string
	for _, p := range strings.Split(job.Meta[v1.HookMetaKey], ",") {
		switch p = strings.TrimSpace(p); p {
		case "":
		case v1.HookPreSync, v1.HookPostSync, v1.HookSyncFail:
			if !slices.Contains(out, p) {
				out = append(out, p)
			}
		default:
			return nil, fmt.Errorf("job %s: unknown hook %q (PreSync, PostSync or SyncFail)", deref(job.ID), p)
		}
	}
	return out, nil
}

// isHook reports whether job meta marks a hook.
func isHook(meta map[string]string) bool { return strings.TrimSpace(meta[v1.HookMetaKey]) != "" }

// splitHooks separates the hooks, by phase and in run order (sync wave,
// then ID), from the jobs to sync.
func splitHooks(jobs []*api.Job) (map[string][]*api.Job, []*api.Job, error) {
	hooks := map[string][]*api.Job{}
	var rest []*api.Job
	for _, job := range jobs {
		phases, err := hookPhases(job)
		if err != nil {
			return nil, nil, err
		}
		if len(phases) == 0 {
			rest = append(rest, job)
			continue
		}
		if t := deref(job.Type); t != "batch" {
			return nil, nil, fmt.Errorf("hook job %s must be a batch job, not %s", deref(job.ID), orDefault(t, "service"))
		}
		if job.Periodic != nil || job.ParameterizedJob != nil {
			return nil, nil, fmt.Errorf("hook job %s cannot be periodic or parameterized", deref(job.ID))
		}
		if v := job.Meta[v1.HookTimeoutMetaKey]; v != "" {
			if d, err := time.ParseDuration(v); err != nil || d <= 0 {
				return nil, nil, fmt.Errorf("hook job %s: %s %q is not a positive duration", deref(job.ID), v1.HookTimeoutMetaKey, v)
			}
		}
		for p := range hookDeletePolicies(job) {
			switch p {
			case v1.HookDeleteBeforeCreation, v1.HookDeleteSucceeded, v1.HookDeleteFailed:
			default:
				return nil, nil, fmt.Errorf("hook job %s: unknown delete policy %q (BeforeHookCreation, HookSucceeded or HookFailed)", deref(job.ID), p)
			}
		}
		for _, p := range phases {
			hooks[p] = append(hooks[p], job)
		}
	}
	for _, list := range hooks {
		sort.SliceStable(list, func(i, j int) bool {
			if wi, wj := syncWave(list[i]), syncWave(list[j]); wi != wj {
				return wi < wj
			}
			return deref(list[i].ID) < deref(list[j].ID)
		})
	}
	return hooks, rest, nil
}

// hookDeletePolicies are when a hook's job is purged.
func hookDeletePolicies(job *api.Job) map[string]bool {
	out := map[string]bool{}
	for _, p := range strings.Split(job.Meta[v1.HookDeletePolicyMetaKey], ",") {
		if p = strings.TrimSpace(p); p != "" {
			out[p] = true
		}
	}
	if len(out) == 0 {
		out[v1.HookDeleteBeforeCreation] = true
	}
	return out
}

func hookTimeout(job *api.Job) time.Duration {
	if d, err := time.ParseDuration(job.Meta[v1.HookTimeoutMetaKey]); err == nil && d > 0 {
		return d
	}
	return defaultHookTimeout
}

// hookRun is what running a sync's hooks needs.
type hookRun struct {
	app      *v1.Application
	cluster  nomad.API
	so       *v1.SyncOperation
	revision string
	sources  map[string]*api.JobSubmission
	result   *v1.SyncOperationResult
	// attempt identifies the operation's attempt (see attemptOf).
	attempt string
}

// attemptOf identifies an operation's attempt: its start and retry count,
// which a resumed operation keeps and a retry does not.
func attemptOf(st *v1.OperationState) string {
	if st == nil {
		return ""
	}
	return fmt.Sprintf("%s/%d", st.StartedAt.UTC().Format(time.RFC3339), st.RetryCount)
}

// runHooks runs a phase's hooks wave by wave, as Argo CD does: the hooks of
// a wave run together, and the next wave starts when they all succeeded.
// It records a result for each hook and stops after a wave with a failure
// (false). err is errTerminated or the context's.
func (c *Controller) runHooks(ctx context.Context, r *hookRun, phase string, hooks []*api.Job) (bool, error) {
	for i := 0; i < len(hooks); {
		j := i + 1
		for j < len(hooks) && syncWave(hooks[j]) == syncWave(hooks[i]) {
			j++
		}
		if ok, err := c.runHookWave(ctx, r, phase, hooks[i:j]); err != nil || !ok {
			return false, err
		}
		i = j
	}
	return true, nil
}

// hookAttempt is one hook job of a wave.
type hookAttempt struct {
	hook       *api.Job
	res        *v1.ResourceResult
	version    uint64
	registered time.Time
	ok         bool
	msg        string
	err        error
}

// runHookWave creates the wave's hook jobs, waits for all of them, and
// applies their delete policies.
func (c *Controller) runHookWave(ctx context.Context, r *hookRun, phase string, wave []*api.Job) (bool, error) {
	if c.terminated(ctx, r.app.Metadata.Name) {
		return false, errTerminated
	}
	ok := true
	var running []*hookAttempt
	var jobs []*api.Job
	for _, hook := range wave {
		res := &v1.ResourceResult{Kind: v1.KindJob, Namespace: deref(hook.Namespace), Name: deref(hook.ID), SyncPhase: phase}
		r.result.Resources = append(r.result.Resources, res)
		if r.so.DryRun {
			res.Status, res.Message = v1.ResultSynced, phase+" hook would run (dry run)"
			continue
		}
		job, version, err := c.startHook(ctx, r, phase, hook)
		if err != nil {
			res.Status, res.Message = v1.ResultSyncFailed, phase+" hook: "+err.Error()
			ok = false
			continue
		}
		res.Status, res.Message, res.JobVersion = v1.ResultSynced, phase+" hook running", &version
		running = append(running, &hookAttempt{hook: hook, res: res, version: version, registered: c.now()})
		jobs = append(jobs, job)
	}
	if len(running) == 0 {
		return ok, nil
	}
	names := make([]string, len(running))
	for i, a := range running {
		names[i] = a.res.Name
	}
	c.progress(ctx, r, fmt.Sprintf("Running %s hook %s", phase, strings.Join(names, ", ")), jobs...)

	var wg sync.WaitGroup
	for _, a := range running {
		wg.Go(func() {
			a.ok, a.msg, a.err = c.waitHook(ctx, r.app, r.cluster, a.res.Namespace, a.res.Name, a.version, a.registered, hookTimeout(a.hook))
		})
	}
	wg.Wait()

	var interrupted error
	for _, a := range running {
		ns, id := a.res.Namespace, a.res.Name
		if a.err != nil {
			a.res.Status, a.res.Message = v1.ResultSyncFailed, phase+" hook interrupted: "+a.err.Error()
			// Terminating the operation stops its running hooks, as in Argo CD.
			if errors.Is(a.err, errTerminated) {
				if _, err := r.cluster.Deregister(ctx, ns, id, false); err == nil {
					a.res.Message += " (job stopped)"
				}
			}
			if interrupted == nil || errors.Is(a.err, errTerminated) {
				interrupted = a.err
			}
			continue
		}
		if a.ok {
			a.res.Status, a.res.Message = v1.ResultSynced, phase+" hook succeeded"
		} else {
			a.res.Status, a.res.Message = v1.ResultSyncFailed, phase+" hook failed: "+a.msg
			ok = false
		}
		policies := hookDeletePolicies(a.hook)
		if (a.ok && policies[v1.HookDeleteSucceeded]) || (!a.ok && policies[v1.HookDeleteFailed]) {
			if _, err := r.cluster.Deregister(ctx, ns, id, true); err != nil {
				c.logger.Warn("delete hook job", "app", r.app.Metadata.Name, "job", id, "error", err)
			} else {
				a.res.Message += " (job deleted)"
			}
		}
	}
	if interrupted != nil {
		return false, interrupted
	}
	return ok, nil
}

// startHook creates a hook's job (deleting its previous run first, unless
// its delete policy keeps it) and returns it and its version.
func (c *Controller) startHook(ctx context.Context, r *hookRun, phase string, hook *api.Job) (*api.Job, uint64, error) {
	ns, id := deref(hook.Namespace), deref(hook.ID)
	run := fmt.Sprintf("%s %s %s", phase, r.attempt, short(r.revision))
	live, err := r.cluster.Job(ctx, ns, id)
	if err != nil {
		return nil, 0, err
	}
	var modifyIndex uint64
	if live != nil {
		if live.Meta[v1.AppMetaKey] != r.app.Metadata.Name || !isHook(live.Meta) {
			return nil, 0, fmt.Errorf("job %s/%s exists and is not a hook of this application", ns, id)
		}
		// The operation resumes after a restart of the controller, and
		// created this run already: wait for it rather than start over.
		if r.attempt != "" && live.Meta[v1.HookRunMetaKey] == run && live.Version != nil {
			return live, *live.Version, nil
		}
		modifyIndex = derefU64(live.JobModifyIndex)
		if hookDeletePolicies(hook)[v1.HookDeleteBeforeCreation] {
			if _, err := r.cluster.Deregister(ctx, ns, id, true); err != nil {
				return nil, 0, fmt.Errorf("delete the previous run: %w", err)
			}
			modifyIndex = 0
		}
	}

	// A copy (the rendered jobs are cached and shared) marked with this run,
	// so that it is a new version even when the previous run is kept: Nomad
	// runs a batch job again only when it changed.
	job := *hook
	job.Meta = maps.Clone(hook.Meta)
	job.Meta[v1.HookRunMetaKey] = run
	opts := &api.RegisterOptions{EnforceIndex: true, ModifyIndex: modifyIndex}
	if r.sources != nil {
		opts.Submission = r.sources[id]
	}
	if _, err := r.cluster.Register(ctx, &job, opts); err != nil {
		return nil, 0, err
	}
	after, err := r.cluster.Job(ctx, ns, id)
	if err != nil || after == nil || after.Version == nil {
		return nil, 0, fmt.Errorf("read the created job: %w", orMissing(err))
	}
	return &job, *after.Version, nil
}

// waitHook waits for a hook's batch job, at version, to finish.
func (c *Controller) waitHook(ctx context.Context, app *v1.Application, cluster nomad.API, ns, id string, version uint64,
	registered time.Time, timeout time.Duration) (bool, string, error) {
	deadline := registered.Add(timeout)
	status := "pending"
	for {
		if c.terminated(ctx, app.Metadata.Name) {
			return false, "", errTerminated
		}
		job, err := cluster.Job(ctx, ns, id)
		switch {
		case err != nil:
			c.logger.Debug("hook status", "job", id, "error", err)
		case job == nil:
			return false, "the job was deleted", nil
		case job.Stop != nil && *job.Stop:
			return false, "the job was stopped", nil
		default:
			status = deref(job.Status)
			if status != "dead" {
				break
			}
			allocs, err := cluster.Allocations(ctx, ns, id)
			if err != nil {
				c.logger.Debug("hook allocations", "job", id, "error", err)
				break
			}
			ok, msg, done := hookOutcome(allocs, version)
			if done || c.now().Sub(registered) > hookPlacementGrace {
				return ok, msg, nil
			}
		}
		if c.now().After(deadline) {
			return false, fmt.Sprintf("did not finish within %s (job %s)", timeout, status), nil
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(hookPoll):
		}
	}
}

// hookOutcome reads a finished batch job's allocations of version: the last
// attempt of each (the one not replaced) must have run to completion. done
// is false while there is none.
func hookOutcome(allocs []*api.AllocationListStub, version uint64) (ok bool, msg string, done bool) {
	var last []*api.AllocationListStub
	for _, a := range allocs {
		if a.JobVersion == version && a.NextAllocation == "" {
			last = append(last, a)
		}
	}
	if len(last) == 0 {
		return false, "no allocation ran", false
	}
	sort.Slice(last, func(i, j int) bool { return last[i].Name < last[j].Name })
	for _, a := range last {
		switch {
		case a.ClientStatus != "complete":
			return false, fmt.Sprintf("allocation %s %s%s", a.Name, a.ClientStatus, failureDetail(a)), true
		case a.DesiredStatus != "run":
			return false, fmt.Sprintf("allocation %s was stopped (%s)", a.Name, a.DesiredStatus), true
		}
	}
	return true, "", true
}

// failureDetail is why a failed task failed, for the result: its last
// event but the restart policy's verdict and the kill's acknowledgement.
func failureDetail(a *api.AllocationListStub) string {
	for _, name := range slices.Sorted(maps.Keys(a.TaskStates)) {
		ts := a.TaskStates[name]
		if ts == nil || !ts.Failed {
			continue
		}
		for i := len(ts.Events) - 1; i >= 0; i-- {
			if e := ts.Events[i]; e.Type != api.TaskNotRestarting && e.Type != api.TaskKilled {
				return fmt.Sprintf(" (task %s: %s)", name, orDefault(e.DisplayMessage, e.Type))
			}
		}
	}
	return ""
}

// waitHealthy waits until the synced jobs are healthy (or suspended), as
// Argo CD does before PostSync hooks. A degraded job fails at once.
func (c *Controller) waitHealthy(ctx context.Context, r *hookRun, jobs []*api.Job, timeout time.Duration) error {
	deadline := c.now().Add(timeout)
	last := ""
	for {
		if c.terminated(ctx, r.app.Metadata.Name) {
			return errTerminated
		}
		waiting := ""
		for _, j := range jobs {
			ns, id := deref(j.Namespace), deref(j.ID)
			live, err := r.cluster.Job(ctx, ns, id)
			if err != nil {
				waiting = fmt.Sprintf("job %s: %v", id, err)
				break
			}
			if live == nil {
				return fmt.Errorf("job %s/%s does not exist", ns, id)
			}
			h := c.jobHealth(ctx, r.cluster, live)
			if h.Status == v1.HealthStatusHealthy || h.Status == v1.HealthStatusSuspended {
				continue
			}
			if h.Status == v1.HealthStatusDegraded {
				return fmt.Errorf("job %s/%s is degraded: %s", ns, id, h.Message)
			}
			waiting = fmt.Sprintf("job %s is %s", id, strings.ToLower(string(h.Status)))
			if h.Message != "" {
				waiting += ": " + h.Message
			}
			break
		}
		if waiting == "" {
			return nil
		}
		if c.now().After(deadline) {
			return fmt.Errorf("not healthy within %s: %s", timeout, waiting)
		}
		if waiting != last {
			c.progress(ctx, r, "Waiting for healthy jobs before the PostSync hooks: "+waiting)
			last = waiting
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(hookPoll):
		}
	}
}

// postSyncTimeout is how long the synced jobs may take to become healthy:
// the longest timeout of the PostSync hooks.
func postSyncTimeout(hooks []*api.Job) time.Duration {
	var d time.Duration
	for _, h := range hooks {
		d = max(d, hookTimeout(h))
	}
	return d
}

// progress records what a running operation is doing: its message, its
// results so far and, once a hook's job exists, the hook among the
// Application's resources, so that the tree shows it while it runs.
func (c *Controller) progress(ctx context.Context, r *hookRun, msg string, hooks ...*api.Job) {
	_, err := c.store.MutateApplication(ctx, r.app.Metadata.Name, func(a *v1.Application) error {
		st := a.Status.OperationState
		if st == nil || st.Phase != v1.OperationRunning {
			return nil
		}
		st.Message = msg
		st.SyncResult = r.result
		for _, hook := range hooks {
			ns, id := deref(hook.Namespace), deref(hook.ID)
			if !slices.ContainsFunc(a.Status.Resources, func(rs v1.ResourceStatus) bool {
				return rs.Kind == v1.KindJob && rs.Namespace == ns && rs.Name == id
			}) {
				a.Status.Resources = append(a.Status.Resources, v1.ResourceStatus{Kind: v1.KindJob, Namespace: ns, Name: id, Hook: true,
					Health: &v1.HealthStatus{Status: v1.HealthStatusProgressing, Message: "Hook running"}})
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		c.logger.Debug("operation progress", "app", r.app.Metadata.Name, "error", err)
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
