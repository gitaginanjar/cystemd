// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/diff"
	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// errTerminated aborts an operation the user terminated.
var errTerminated = errors.New("operation terminated")

// runOperation executes app.Operation, records the outcome (status,
// history, events) and returns the updated Application (nil when it is gone).
func (c *Controller) runOperation(ctx context.Context, app *v1.Application, cmp *comparison) *v1.Application {
	op := app.Operation
	started := c.now().UTC().Truncate(time.Second)
	state := &v1.OperationState{Operation: *op, Phase: v1.OperationRunning, StartedAt: started, Message: "Sync in progress"}
	if prev := app.Status.OperationState; prev != nil && prev.Phase == v1.OperationRunning {
		state.RetryCount = prev.RetryCount // a retry, or a resume after a controller restart
		state.StartedAt = prev.StartedAt
	}
	app, err := c.store.MutateApplication(ctx, app.Metadata.Name, func(a *v1.Application) error {
		a.Status.OperationState = state
		return nil
	})
	if err != nil {
		c.logger.Warn("start operation", "error", err)
		return nil
	}
	c.recordEvent(ctx, app, "Normal", "OperationStarted", describeOperation(op), op.InitiatedBy.Username)

	var result *v1.SyncOperationResult
	var jobVersions map[string]uint64
	switch {
	case op.Sync == nil:
		err = errors.New("unsupported operation")
	case op.Sync.RollbackID != nil:
		result, jobVersions, err = c.rollback(ctx, app, op)
	default:
		result, jobVersions, err = c.sync(ctx, app, op, cmp)
	}

	finished := c.now().UTC().Truncate(time.Second)
	state.FinishedAt = &finished
	state.SyncResult = result
	switch {
	case errors.Is(err, errTerminated):
		state.Phase, state.Message = v1.OperationFailed, "Operation terminated"
	case err != nil:
		state.Phase, state.Message = v1.OperationError, err.Error()
	case result != nil && anyFailed(result):
		state.Phase, state.Message = v1.OperationFailed, "One or more resources failed to sync"
	default:
		state.Phase, state.Message = v1.OperationSucceeded, "Successfully synced (all tasks run)"
		if op.Sync != nil && op.Sync.DryRun {
			state.Message = "Dry run successful"
		}
	}

	// Retry a failed operation per its retry strategy.
	retry := op.Retry
	if retry == nil && app.Spec.SyncPolicy != nil {
		retry = app.Spec.SyncPolicy.Retry
	}
	if (state.Phase == v1.OperationFailed || state.Phase == v1.OperationError) && !errors.Is(err, errTerminated) &&
		retry != nil && (retry.Limit < 0 || state.RetryCount < retry.Limit) {
		state.RetryCount++
		state.Phase = v1.OperationRunning
		state.FinishedAt = nil
		delay := retryDelay(retry, state.RetryCount)
		state.Message = fmt.Sprintf("Retrying attempt #%d in %s: %s", state.RetryCount, delay, state.Message)
		updated, perr := c.store.MutateApplication(ctx, app.Metadata.Name, func(a *v1.Application) error {
			a.Status.OperationState = state
			return nil
		})
		if perr == nil {
			name := app.Metadata.Name
			time.AfterFunc(delay, func() { c.queue.add(name, false) })
		}
		return updated
	}

	updated, err := c.store.MutateApplication(ctx, app.Metadata.Name, func(a *v1.Application) error {
		a.Operation = nil
		a.Status.OperationState = state
		if state.Phase == v1.OperationSucceeded && op.Sync != nil && !op.Sync.DryRun && result != nil {
			a.Status.History = appendHistory(a.Status.History, v1.RevisionHistory{
				Revision:        result.Revision,
				DeployedAt:      finished,
				DeployStartedAt: &state.StartedAt,
				Source:          result.Source,
				InitiatedBy:     op.InitiatedBy,
				JobVersions:     jobVersions,
			}, a.Spec.HistoryLimit())
		}
		return nil
	})
	if err != nil {
		c.logger.Warn("finish operation", "app", app.Metadata.Name, "error", err)
		return nil
	}
	typ := "Normal"
	if state.Phase != v1.OperationSucceeded {
		typ = "Warning"
	}
	c.recordEvent(ctx, updated, typ, "OperationCompleted", fmt.Sprintf("Sync operation to %s %s: %s",
		short(revisionOf(result)), strings.ToLower(string(state.Phase)), state.Message), op.InitiatedBy.Username)
	c.metrics.SyncTotal.WithLabelValues(app.Metadata.Name, string(state.Phase)).Inc()
	c.logger.Info("operation completed", "app", app.Metadata.Name, "phase", state.Phase, "revision", short(revisionOf(result)), "message", state.Message)
	return updated
}

func describeOperation(op *v1.Operation) string {
	who := op.InitiatedBy.Username
	if op.InitiatedBy.Automated {
		who = "automated sync policy"
	}
	if who == "" {
		who = "unknown user"
	}
	switch {
	case op.Sync == nil:
		return who + " initiated an operation"
	case op.Sync.RollbackID != nil:
		return fmt.Sprintf("%s initiated rollback to history entry %d", who, *op.Sync.RollbackID)
	case op.Sync.Revision != "":
		return fmt.Sprintf("%s initiated sync to %s", who, op.Sync.Revision)
	}
	return who + " initiated sync"
}

func revisionOf(r *v1.SyncOperationResult) string {
	if r == nil {
		return ""
	}
	return r.Revision
}

func anyFailed(r *v1.SyncOperationResult) bool {
	for _, res := range r.Resources {
		if res.Status == v1.ResultSyncFailed {
			return true
		}
	}
	return false
}

func retryDelay(r *v1.RetryStrategy, attempt int64) time.Duration {
	base, factor, maxD := 5*time.Second, int64(2), 3*time.Minute
	if r.Backoff != nil {
		if d, err := time.ParseDuration(r.Backoff.Duration); err == nil && d > 0 {
			base = d
		}
		if r.Backoff.Factor != nil && *r.Backoff.Factor > 0 {
			factor = *r.Backoff.Factor
		}
		if d, err := time.ParseDuration(r.Backoff.MaxDuration); err == nil && d > 0 {
			maxD = d
		}
	}
	d := base
	for i := int64(1); i < attempt && d < maxD; i++ {
		d *= time.Duration(factor)
	}
	return min(d, maxD)
}

func appendHistory(h []v1.RevisionHistory, entry v1.RevisionHistory, limit int) []v1.RevisionHistory {
	var next int64
	for _, e := range h {
		if e.ID >= next {
			next = e.ID + 1
		}
	}
	entry.ID = next
	h = append(h, entry)
	if len(h) > limit {
		h = h[len(h)-limit:]
	}
	return h
}

// syncOptions merges the operation's and the policy's sync options.
func syncOptions(app *v1.Application, op *v1.SyncOperation) map[string]bool {
	out := map[string]bool{}
	if app.Spec.SyncPolicy != nil {
		for _, o := range app.Spec.SyncPolicy.SyncOptions {
			out[o] = true
		}
	}
	for _, o := range op.SyncOptions {
		out[o] = true
	}
	return out
}

func selected(op *v1.SyncOperation, kind, ns, name string) bool {
	if len(op.Resources) == 0 {
		return true
	}
	for _, r := range op.Resources {
		if r.Kind == kind && r.Name == name && (r.Namespace == "" || r.Namespace == ns) {
			return true
		}
	}
	return false
}

// terminated reports whether the user asked to terminate the running
// operation (the API sets the Terminating phase).
func (c *Controller) terminated(ctx context.Context, name string) bool {
	app, err := c.store.GetApplication(ctx, name)
	if err != nil {
		return false
	}
	return app.Status.OperationState != nil && app.Status.OperationState.Phase == v1.OperationTerminating
}

// sync applies the desired state: PreSync hooks, Consul keys (wave -1),
// jobs by sync wave, pruning, then PostSync hooks once the jobs are healthy
// (SyncFail hooks when something failed). Registration is guarded by the
// live job's modify index, so a change made after the comparison is never
// silently overwritten.
func (c *Controller) sync(ctx context.Context, app *v1.Application, op *v1.Operation, cmp *comparison) (*v1.SyncOperationResult, map[string]uint64, error) {
	so := op.Sync
	src := app.Spec.Source
	if so.Source != nil {
		src = *so.Source
	}
	revision := so.Revision
	if revision == "" {
		revision = src.Revision()
	}
	sha, err := c.repos.Resolve(ctx, src.RepoURL, revision)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve %s: %w", revision, err)
	}
	manifests, err := c.repos.Manifests(ctx, app, src, sha, false)
	if err != nil {
		return nil, nil, fmt.Errorf("render: %w", err)
	}
	cluster, err := c.clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, nil, err
	}
	opts := syncOptions(app, so)
	result := &v1.SyncOperationResult{Revision: sha, Source: src}
	jobVersions := map[string]uint64{}
	sameRevision := cmp != nil && cmp.revision == sha

	// Guard: an automated sync never prunes everything unless allowed.
	if op.InitiatedBy.Automated && len(manifests.Jobs) == 0 && len(manifests.KV) == 0 && len(manifests.Applications) == 0 &&
		!(app.Spec.SyncPolicy.Automated != nil && app.Spec.SyncPolicy.Automated.AllowEmpty) {
		return nil, nil, errors.New("automated sync refused: the desired state is empty (set syncPolicy.automated.allowEmpty to allow)")
	}
	hooks, jobs, err := splitHooks(manifests.Jobs)
	if err != nil {
		return nil, nil, err
	}
	// Hooks run on full syncs only, as in Argo CD.
	if len(so.Resources) > 0 {
		hooks = nil
	}
	hr := &hookRun{app: app, cluster: cluster, so: so, revision: sha, sources: manifests.Sources, result: result,
		attempt: attemptOf(app.Status.OperationState)}
	syncFail := func() (*v1.SyncOperationResult, map[string]uint64, error) {
		if _, err := c.runHooks(ctx, hr, v1.HookSyncFail, hooks[v1.HookSyncFail]); err != nil {
			return result, jobVersions, err
		}
		return result, jobVersions, nil
	}

	// 0. PreSync hooks.
	if ok, err := c.runHooks(ctx, hr, v1.HookPreSync, hooks[v1.HookPreSync]); err != nil {
		return result, jobVersions, err
	} else if !ok {
		return syncFail()
	}

	// 1. Consul keys.
	if len(manifests.KV) > 0 {
		owned, _ := c.store.OwnedKeys(ctx, app.Metadata.Name)
		ownedSet := map[string]bool{}
		for _, k := range owned {
			ownedSet[k] = true
		}
		for _, kv := range manifests.KV {
			if !selected(so, v1.KindConsulKV, "", kv.Key) {
				continue
			}
			res := &v1.ResourceResult{Kind: v1.KindConsulKV, Name: kv.Key, SyncPhase: "Sync"}
			switch {
			case c.consul == nil:
				res.Status, res.Message = v1.ResultSyncFailed, "Consul is not configured on the control plane"
			case so.DryRun:
				res.Status, res.Message = v1.ResultSynced, "would be written (dry run)"
			default:
				if err := c.consul.Put(ctx, kv.Key, kv.Value); err != nil {
					res.Status, res.Message = v1.ResultSyncFailed, err.Error()
				} else {
					res.Status, res.Message = v1.ResultSynced, "key written"
					ownedSet[kv.Key] = true
				}
			}
			result.Resources = append(result.Resources, res)
		}
		if !so.DryRun && c.consul != nil {
			keys := make([]string, 0, len(ownedSet))
			for k := range ownedSet {
				keys = append(keys, k)
			}
			if err := c.store.SetOwnedKeys(ctx, app.Metadata.Name, keys); err != nil {
				c.logger.Warn("record owned keys", "app", app.Metadata.Name, "error", err)
			}
		}
	}

	// 2. Jobs, by sync wave.
	sort.SliceStable(jobs, func(i, j int) bool { return syncWave(jobs[i]) < syncWave(jobs[j]) })
	ignore := ignoredFields(app)
	for _, job := range jobs {
		ns, id := deref(job.Namespace), deref(job.ID)
		key := ns + "/" + id
		if !selected(so, v1.KindJob, ns, id) {
			continue
		}
		if c.terminated(ctx, app.Metadata.Name) {
			return result, jobVersions, errTerminated
		}
		res := &v1.ResourceResult{Kind: v1.KindJob, Namespace: ns, Name: id, SyncPhase: "Sync"}
		result.Resources = append(result.Resources, res)

		live, err := cluster.Job(ctx, ns, id)
		if err != nil {
			res.Status, res.Message = v1.ResultSyncFailed, err.Error()
			continue
		}
		if live != nil {
			if fields := ignore.fieldsFor(v1.KindJob, ns, id); len(fields) > 0 {
				job = withIgnoredFields(job, live, fields)
			}
		}
		if opts[v1.SyncOptionApplyOutOfSyncOnly] && sameRevision && live != nil {
			if p := cmp.plans[key]; p != nil && !diff.HasChanges(p.Diff) {
				res.Status, res.Message = v1.ResultSynced, "unchanged (ApplyOutOfSyncOnly)"
				if live.Version != nil {
					jobVersions[key] = *live.Version
				}
				continue
			}
		}
		if opts[v1.SyncOptionCreateNamespace] && !so.DryRun {
			if err := cluster.EnsureNamespace(ctx, ns); err != nil {
				res.Status, res.Message = v1.ResultSyncFailed, fmt.Sprintf("create namespace %s: %v", ns, err)
				continue
			}
		}
		if so.DryRun {
			plan, err := cluster.Plan(ctx, job)
			if err != nil {
				res.Status, res.Message = v1.ResultSyncFailed, err.Error()
				continue
			}
			res.Status = v1.ResultSynced
			if diff.HasChanges(plan.Diff) {
				res.Message = "would be updated (dry run)"
			} else {
				res.Message = "unchanged (dry run)"
			}
			continue
		}
		var modifyIndex uint64
		if live != nil {
			modifyIndex = derefU64(live.JobModifyIndex)
		}
		regOpts := &api.RegisterOptions{
			EnforceIndex:   true,
			ModifyIndex:    modifyIndex,
			PreserveCounts: opts[v1.SyncOptionPreserveCounts],
		}
		if manifests.Sources != nil {
			regOpts.Submission = manifests.Sources[id]
		}
		resp, err := cluster.Register(ctx, job, regOpts)
		if err != nil {
			res.Status, res.Message = v1.ResultSyncFailed, err.Error()
			continue
		}
		res.Status, res.EvalID = v1.ResultSynced, resp.EvalID
		res.Message = "job registered"
		if resp.Warnings != "" {
			res.Message += " (warnings: " + strings.TrimSpace(resp.Warnings) + ")"
		}
		if after, err := cluster.Job(ctx, ns, id); err == nil && after != nil && after.Version != nil {
			v := *after.Version
			res.JobVersion = &v
			jobVersions[key] = v
			// Tag the version with the Git revision: `nomad job history`
			// and the Nomad UI then show where every version came from.
			_ = cluster.TagVersion(ctx, ns, id, v, "cystemd-"+short(sha)+"-"+fmt.Sprint(v),
				fmt.Sprintf("Synced from %s@%s by %s", src.RepoURL, short(sha), initiator(op)))
		}
	}

	// Declared Applications (app of apps).
	for _, want := range manifests.Applications {
		name := want.Metadata.Name
		if !selected(so, v1.KindApplication, "", name) {
			continue
		}
		if c.terminated(ctx, app.Metadata.Name) {
			return result, jobVersions, errTerminated
		}
		res := &v1.ResourceResult{Kind: v1.KindApplication, Name: name, SyncPhase: "Sync"}
		res.Status, res.Message = c.syncApp(ctx, app, want, so.DryRun)
		result.Resources = append(result.Resources, res)
	}

	// 3. Prune.
	prune := so.Prune && !opts[v1.SyncOptionPruneFalse]
	desired := map[string]bool{}
	for _, j := range manifests.Jobs {
		desired[render.JobKey(j)] = true
	}
	managed, err := cluster.ManagedJobs(ctx, "*", app.Metadata.Name)
	if err != nil {
		return result, jobVersions, fmt.Errorf("list managed jobs: %w", err)
	}
	for _, stub := range managed {
		key := stub.Namespace + "/" + stub.ID
		if desired[key] || stub.Stop || isHook(stub.Meta) || !selected(so, v1.KindJob, stub.Namespace, stub.ID) {
			continue
		}
		res := &v1.ResourceResult{Kind: v1.KindJob, Namespace: stub.Namespace, Name: stub.ID, SyncPhase: "Sync"}
		switch {
		case !prune:
			res.Status, res.Message = v1.ResultPruneSkipped, "ignored (requires pruning)"
		case so.DryRun:
			res.Status, res.Message = v1.ResultPruned, "would be pruned (dry run)"
		default:
			if _, err := cluster.Deregister(ctx, stub.Namespace, stub.ID, false); err != nil {
				res.Status, res.Message = v1.ResultSyncFailed, err.Error()
			} else {
				res.Status, res.Message = v1.ResultPruned, "job stopped (pruned)"
			}
		}
		result.Resources = append(result.Resources, res)
	}
	if c.consul != nil {
		desiredKV := map[string]bool{}
		for _, kv := range manifests.KV {
			desiredKV[kv.Key] = true
		}
		owned, _ := c.store.OwnedKeys(ctx, app.Metadata.Name)
		var keep []string
		for _, key := range owned {
			if desiredKV[key] || !selected(so, v1.KindConsulKV, "", key) {
				keep = append(keep, key)
				continue
			}
			res := &v1.ResourceResult{Kind: v1.KindConsulKV, Name: key, SyncPhase: "Sync"}
			switch {
			case !prune:
				res.Status, res.Message = v1.ResultPruneSkipped, "ignored (requires pruning)"
				keep = append(keep, key)
			case so.DryRun:
				res.Status, res.Message = v1.ResultPruned, "would be deleted (dry run)"
				keep = append(keep, key)
			default:
				if err := c.consul.Delete(ctx, key); err != nil {
					res.Status, res.Message = v1.ResultSyncFailed, err.Error()
					keep = append(keep, key)
				} else {
					res.Status, res.Message = v1.ResultPruned, "key deleted (pruned)"
				}
			}
			result.Resources = append(result.Resources, res)
		}
		if !so.DryRun && len(keep) != len(owned) {
			_ = c.store.SetOwnedKeys(ctx, app.Metadata.Name, keep)
		}
	}
	if tracksApps(app) {
		children, err := c.childApps(ctx, app)
		if err != nil {
			return result, jobVersions, fmt.Errorf("list applications: %w", err)
		}
		declared := map[string]bool{}
		for _, a := range manifests.Applications {
			declared[a.Metadata.Name] = true
		}
		for _, child := range children {
			name := child.Metadata.Name
			if declared[name] || child.Metadata.DeletionTimestamp != nil || !selected(so, v1.KindApplication, "", name) {
				continue
			}
			res := &v1.ResourceResult{Kind: v1.KindApplication, Name: name, SyncPhase: "Sync"}
			switch {
			case !prune:
				res.Status, res.Message = v1.ResultPruneSkipped, "ignored (requires pruning)"
			case so.DryRun:
				res.Status, res.Message = v1.ResultPruned, "would be deleted (dry run)"
			default:
				if err := c.deleteApp(ctx, name); err != nil {
					res.Status, res.Message = v1.ResultSyncFailed, err.Error()
				} else {
					res.Status, res.Message = v1.ResultPruned, "application deleted (pruned)"
				}
			}
			result.Resources = append(result.Resources, res)
		}
	}

	// 4. PostSync hooks, once every job is healthy; SyncFail hooks if
	// anything failed.
	if anyFailed(result) {
		return syncFail()
	}
	if post := hooks[v1.HookPostSync]; len(post) > 0 {
		if !so.DryRun {
			if err := c.waitHealthy(ctx, hr, jobs, postSyncTimeout(post)); err != nil {
				if errors.Is(err, errTerminated) || ctx.Err() != nil {
					return result, jobVersions, err
				}
				for _, h := range post {
					result.Resources = append(result.Resources, &v1.ResourceResult{Kind: v1.KindJob, Namespace: deref(h.Namespace),
						Name: deref(h.ID), SyncPhase: v1.HookPostSync, Status: v1.ResultSyncFailed, Message: "PostSync hook not run: " + err.Error()})
				}
				return syncFail()
			}
		}
		if ok, err := c.runHooks(ctx, hr, v1.HookPostSync, post); err != nil {
			return result, jobVersions, err
		} else if !ok {
			return syncFail()
		}
	}
	return result, jobVersions, nil
}

// withIgnoredFields returns job with the ignored fields taken from live.
func withIgnoredFields(job, live *api.Job, fields []string) *api.Job {
	dm, err := diff.ToMap(job)
	if err != nil {
		return job
	}
	lm, err := diff.ToMap(live)
	if err != nil {
		return job
	}
	diff.ApplyIgnoreDifferences(dm, lm, fields)
	out, err := diff.FromMap(dm)
	if err != nil {
		return job
	}
	return out
}

func initiator(op *v1.Operation) string {
	if op.InitiatedBy.Automated {
		return "automated sync"
	}
	if op.InitiatedBy.Username != "" {
		return op.InitiatedBy.Username
	}
	return "unknown user"
}

// rollback reverts the Application's jobs to the exact Nomad versions a
// history entry recorded (Nomad keeps them), restores the Consul keys of
// that revision, and records a new history entry.
func (c *Controller) rollback(ctx context.Context, app *v1.Application, op *v1.Operation) (*v1.SyncOperationResult, map[string]uint64, error) {
	if app.Spec.SyncPolicy.IsAutomated() {
		return nil, nil, errors.New("rollback cannot be initiated when auto-sync is enabled")
	}
	var entry *v1.RevisionHistory
	for i := range app.Status.History {
		if app.Status.History[i].ID == *op.Sync.RollbackID {
			entry = &app.Status.History[i]
		}
	}
	if entry == nil {
		return nil, nil, fmt.Errorf("history entry %d not found", *op.Sync.RollbackID)
	}
	cluster, err := c.clusters.Get(app.Spec.Destination.Name)
	if err != nil {
		return nil, nil, err
	}
	result := &v1.SyncOperationResult{Revision: entry.Revision, Source: entry.Source}
	jobVersions := map[string]uint64{}

	// Rendered manifests of that revision: Consul keys, and the fallback
	// for a job whose recorded version Nomad has garbage-collected.
	manifests, renderErr := c.repos.Manifests(ctx, app, entry.Source, entry.Revision, false)
	if renderErr == nil && c.consul != nil {
		for _, kv := range manifests.KV {
			res := &v1.ResourceResult{Kind: v1.KindConsulKV, Name: kv.Key, SyncPhase: "Sync"}
			if op.Sync.DryRun {
				res.Status, res.Message = v1.ResultSynced, "would be restored (dry run)"
			} else if err := c.consul.Put(ctx, kv.Key, kv.Value); err != nil {
				res.Status, res.Message = v1.ResultSyncFailed, err.Error()
			} else {
				res.Status, res.Message = v1.ResultSynced, "key restored"
			}
			result.Resources = append(result.Resources, res)
		}
	}
	rendered := map[string]*api.Job{}
	if renderErr == nil {
		for _, j := range manifests.Jobs {
			rendered[render.JobKey(j)] = j
		}
	}

	keys := make([]string, 0, len(entry.JobVersions))
	for k := range entry.JobVersions {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		version := entry.JobVersions[key]
		ns, id, _ := strings.Cut(key, "/")
		res := &v1.ResourceResult{Kind: v1.KindJob, Namespace: ns, Name: id, SyncPhase: "Sync"}
		result.Resources = append(result.Resources, res)
		if op.Sync.DryRun {
			res.Status, res.Message = v1.ResultSynced, fmt.Sprintf("would revert to version %d (dry run)", version)
			continue
		}
		live, _ := cluster.Job(ctx, ns, id)
		if live != nil && live.Version != nil && *live.Version == version {
			res.Status, res.Message = v1.ResultSynced, fmt.Sprintf("already at version %d", version)
			jobVersions[key] = version
			continue
		}
		var revErr error
		if live != nil {
			if resp, err := cluster.Revert(ctx, ns, id, version); err == nil {
				res.Status, res.EvalID = v1.ResultSynced, resp.EvalID
				res.Message = fmt.Sprintf("reverted to version %d", version)
			} else {
				revErr = err
			}
		} else {
			revErr = errors.New("job does not exist")
		}
		if revErr != nil {
			job, ok := rendered[key]
			if !ok {
				res.Status, res.Message = v1.ResultSyncFailed, fmt.Sprintf("revert to version %d: %v", version, revErr)
				continue
			}
			var modifyIndex uint64
			if live != nil {
				modifyIndex = derefU64(live.JobModifyIndex)
			}
			resp, err := cluster.Register(ctx, job, &api.RegisterOptions{EnforceIndex: true, ModifyIndex: modifyIndex})
			if err != nil {
				res.Status, res.Message = v1.ResultSyncFailed, err.Error()
				continue
			}
			res.Status, res.EvalID = v1.ResultSynced, resp.EvalID
			res.Message = fmt.Sprintf("re-registered from %s (version %d unavailable: %v)", short(entry.Revision), version, revErr)
		}
		if after, err := cluster.Job(ctx, ns, id); err == nil && after != nil && after.Version != nil {
			v := *after.Version
			res.JobVersion = &v
			jobVersions[key] = v
		}
	}

	if op.Sync.Prune && !op.Sync.DryRun {
		managed, err := cluster.ManagedJobs(ctx, "*", app.Metadata.Name)
		if err == nil {
			for _, stub := range managed {
				key := stub.Namespace + "/" + stub.ID
				if _, ok := entry.JobVersions[key]; ok || stub.Stop || isHook(stub.Meta) {
					continue
				}
				res := &v1.ResourceResult{Kind: v1.KindJob, Namespace: stub.Namespace, Name: stub.ID, SyncPhase: "Sync"}
				if _, err := cluster.Deregister(ctx, stub.Namespace, stub.ID, false); err != nil {
					res.Status, res.Message = v1.ResultSyncFailed, err.Error()
				} else {
					res.Status, res.Message = v1.ResultPruned, "job stopped (pruned)"
				}
				result.Resources = append(result.Resources, res)
			}
		}
	}
	return result, jobVersions, nil
}

// autoSyncOperation decides, like Argo CD, whether an automated sync is due.
// It syncs a new revision once; a failed attempt at a revision is not
// retried automatically (only Retry does); drift at an already-synced
// revision is corrected only with selfHeal, at most every SelfHealTimeout.
func (c *Controller) autoSyncOperation(app *v1.Application, cmp *comparison) *v1.Operation {
	policy := app.Spec.SyncPolicy
	if !policy.IsAutomated() || app.Operation != nil || cmp.sync != v1.SyncStatusOutOfSync {
		return nil
	}
	// Only pruning is pending but prune is off: nothing auto-sync may do.
	if !policy.Automated.Prune {
		onlyPrune := true
		for _, r := range cmp.resources {
			if r.Status == v1.SyncStatusOutOfSync && !r.RequiresPruning {
				onlyPrune = false
			}
		}
		if onlyPrune {
			return nil
		}
	}
	// The project's sync windows may forbid automated syncs now.
	if cmp.project != nil && !cmp.project.WindowState(app, c.now()).AutoSync {
		c.logger.Debug("skipping auto-sync: blocked by sync window", "app", app.Metadata.Name)
		return nil
	}
	if last := app.Status.OperationState; last != nil && last.SyncResult != nil && last.SyncResult.Revision == cmp.revision &&
		last.Operation.Sync != nil && last.Operation.Sync.RollbackID == nil {
		switch last.Phase {
		case v1.OperationFailed, v1.OperationError:
			return nil // "skipping auto-sync: failed previous sync attempt"
		case v1.OperationSucceeded:
			if !policy.Automated.SelfHeal {
				return nil
			}
			if last.FinishedAt != nil && c.now().Sub(*last.FinishedAt) < c.cfg.SelfHealTimeout {
				name := app.Metadata.Name
				time.AfterFunc(c.cfg.SelfHealTimeout, func() { c.queue.add(name, false) })
				return nil
			}
		case v1.OperationRunning:
			return nil
		}
	}
	return &v1.Operation{
		Sync: &v1.SyncOperation{
			Revision:    cmp.revision,
			Prune:       policy.Automated.Prune,
			SyncOptions: policy.SyncOptions,
		},
		InitiatedBy: v1.OperationInitiator{Automated: true},
		Retry:       policy.Retry,
	}
}

// finalizeDeletion deletes a deleted Application's resources when it carries
// the resources finalizer (cascade), then removes the Application.
func (c *Controller) finalizeDeletion(ctx context.Context, app *v1.Application) {
	name := app.Metadata.Name
	if app.Metadata.HasFinalizer(v1.ResourcesFinalizer) {
		var errs []error
		cluster, err := c.clusters.Get(app.Spec.Destination.Name)
		if err != nil {
			errs = append(errs, err)
		} else {
			managed, err := cluster.ManagedJobs(ctx, "*", name)
			if err != nil {
				errs = append(errs, err)
			}
			for _, stub := range managed {
				if _, err := cluster.Deregister(ctx, stub.Namespace, stub.ID, true); err != nil {
					errs = append(errs, fmt.Errorf("deregister %s/%s: %w", stub.Namespace, stub.ID, err))
				}
			}
		}
		if c.consul != nil {
			owned, _ := c.store.OwnedKeys(ctx, name)
			for _, key := range owned {
				if err := c.consul.Delete(ctx, key); err != nil {
					errs = append(errs, fmt.Errorf("delete key %s: %w", key, err))
				}
			}
		}
		// The Applications it declared go too; their own finalizers decide
		// about their jobs.
		if tracksApps(app) {
			children, err := c.childApps(ctx, app)
			if err != nil {
				errs = append(errs, err)
			}
			for _, child := range children {
				if err := c.deleteApp(ctx, child.Metadata.Name); err != nil {
					errs = append(errs, fmt.Errorf("delete application %s: %w", child.Metadata.Name, err))
				}
			}
		}
		if len(errs) > 0 {
			err := errors.Join(errs...)
			c.logger.Warn("cascade delete incomplete; will retry", "app", name, "error", err)
			_, _ = c.store.MutateApplication(ctx, name, func(a *v1.Application) error {
				a.Status.Conditions = mergeConditions(a.Status.Conditions, []v1.ApplicationCondition{condition(v1.ConditionDeletionError, "%v", err)})
				return nil
			})
			time.AfterFunc(30*time.Second, func() { c.queue.add(name, false) })
			return
		}
	}
	if err := c.store.DeleteApplication(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		c.logger.Warn("delete application", "app", name, "error", err)
		return
	}
	c.ownersMu.Lock()
	for k, v := range c.jobOwners {
		if v == name {
			delete(c.jobOwners, k)
		}
	}
	c.ownersMu.Unlock()
	c.metrics.ForgetApp(name)
	c.logger.Info("application deleted", "app", name, "cascade", app.Metadata.HasFinalizer(v1.ResourcesFinalizer))
}
