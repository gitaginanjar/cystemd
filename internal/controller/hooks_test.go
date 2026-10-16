// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func hookJob(id, typ string, meta map[string]string) *api.Job {
	return &api.Job{ID: &id, Type: &typ, Meta: meta}
}

func TestSplitHooks(t *testing.T) {
	jobs := []*api.Job{
		hookJob("web", "service", map[string]string{"cystemd.app": "a"}),
		hookJob("migrate", "batch", map[string]string{v1.HookMetaKey: "PreSync", "cystemd.sync-wave": "1"}),
		hookJob("schema", "batch", map[string]string{v1.HookMetaKey: " PreSync , PreSync"}),
		hookJob("smoke", "batch", map[string]string{v1.HookMetaKey: "PostSync,SyncFail"}),
		hookJob("backup", "batch", map[string]string{v1.HookMetaKey: "PreSync", "cystemd.sync-wave": "-1"}),
	}
	hooks, rest, err := splitHooks(jobs)
	if err != nil {
		t.Fatal(err)
	}
	ids := func(js []*api.Job) string {
		var out []string
		for _, j := range js {
			out = append(out, *j.ID)
		}
		return strings.Join(out, ",")
	}
	if got := ids(rest); got != "web" {
		t.Errorf("rest = %s", got)
	}
	// By wave, then ID; a phase listed twice runs once.
	if got := ids(hooks[v1.HookPreSync]); got != "backup,schema,migrate" {
		t.Errorf("PreSync = %s", got)
	}
	if got := ids(hooks[v1.HookPostSync]); got != "smoke" {
		t.Errorf("PostSync = %s", got)
	}
	if got := ids(hooks[v1.HookSyncFail]); got != "smoke" {
		t.Errorf("SyncFail = %s", got)
	}

	for _, tc := range []struct {
		job  *api.Job
		want string
	}{
		{hookJob("x", "batch", map[string]string{v1.HookMetaKey: "PreSync,Sync"}), `unknown hook "Sync"`},
		{hookJob("x", "service", map[string]string{v1.HookMetaKey: "PreSync"}), "must be a batch job, not service"},
		{hookJob("x", "", map[string]string{v1.HookMetaKey: "PreSync"}), "must be a batch job, not service"},
		{hookJob("x", "batch", map[string]string{v1.HookMetaKey: "PreSync", v1.HookTimeoutMetaKey: "soon"}), "not a positive duration"},
		{hookJob("x", "batch", map[string]string{v1.HookMetaKey: "PreSync", v1.HookDeletePolicyMetaKey: "Always"}), `unknown delete policy "Always"`},
		{&api.Job{ID: new("x"), Type: new("batch"), Meta: map[string]string{v1.HookMetaKey: "PostSync"}, Periodic: &api.PeriodicConfig{}}, "periodic"},
	} {
		if _, _, err := splitHooks([]*api.Job{tc.job}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.job.Meta, err, tc.want)
		}
	}
}

func TestHookSettings(t *testing.T) {
	j := hookJob("x", "batch", map[string]string{v1.HookMetaKey: "PreSync"})
	if p := hookDeletePolicies(j); len(p) != 1 || !p[v1.HookDeleteBeforeCreation] {
		t.Errorf("default delete policy = %v", p)
	}
	if d := hookTimeout(j); d != defaultHookTimeout {
		t.Errorf("default timeout = %s", d)
	}
	j.Meta[v1.HookDeletePolicyMetaKey] = "HookSucceeded, HookFailed"
	j.Meta[v1.HookTimeoutMetaKey] = "90s"
	if p := hookDeletePolicies(j); len(p) != 2 || !p[v1.HookDeleteSucceeded] || !p[v1.HookDeleteFailed] {
		t.Errorf("delete policies = %v", p)
	}
	if d := hookTimeout(j); d != 90*time.Second {
		t.Errorf("timeout = %s", d)
	}
	short := hookJob("y", "batch", map[string]string{v1.HookTimeoutMetaKey: "5m"})
	if d := postSyncTimeout([]*api.Job{short, j}); d != 5*time.Minute {
		t.Errorf("post sync timeout = %s", d)
	}
	if !isHook(map[string]string{v1.HookMetaKey: "PostSync"}) || isHook(map[string]string{v1.HookMetaKey: " "}) || isHook(nil) {
		t.Error("isHook")
	}
}

func TestHookOutcome(t *testing.T) {
	alloc := func(name string, version uint64, client, desired, next string) *api.AllocationListStub {
		return &api.AllocationListStub{Name: name, JobVersion: version, ClientStatus: client, DesiredStatus: desired, NextAllocation: next}
	}
	failed := alloc("migrate.run[0]", 3, "failed", "run", "")
	failed.TaskStates = map[string]*api.TaskState{
		"main": {Failed: true, Events: []*api.TaskEvent{{Type: "Started"}, {Type: "Terminated", DisplayMessage: "Exit Code: 3"},
			{Type: api.TaskNotRestarting, DisplayMessage: "Policy allows no restarts"}}},
	}
	for _, tc := range []struct {
		name   string
		allocs []*api.AllocationListStub
		ok     bool
		done   bool
		msg    string
	}{
		{"none yet", nil, false, false, "no allocation ran"},
		{"only an older version", []*api.AllocationListStub{alloc("a[0]", 2, "complete", "run", "")}, false, false, ""},
		{"completed", []*api.AllocationListStub{alloc("a[0]", 3, "complete", "run", ""), alloc("a[0]", 2, "failed", "run", "")}, true, true, ""},
		{"rescheduled then completed", []*api.AllocationListStub{
			alloc("a[0]", 3, "failed", "run", "next"), alloc("a[0]", 3, "complete", "run", ""),
		}, true, true, ""},
		{"failed", []*api.AllocationListStub{failed}, false, true, "allocation migrate.run[0] failed (task main: Exit Code: 3)"},
		{"stopped", []*api.AllocationListStub{alloc("a[0]", 3, "complete", "stop", "")}, false, true, "allocation a[0] was stopped (stop)"},
		{"one of two failed", []*api.AllocationListStub{alloc("a[1]", 3, "complete", "run", ""), alloc("a[0]", 3, "lost", "run", "")}, false, true, "allocation a[0] lost"},
	} {
		ok, msg, done := hookOutcome(tc.allocs, 3)
		if ok != tc.ok || done != tc.done || (tc.msg != "" && msg != tc.msg) {
			t.Errorf("%s: ok=%v done=%v msg=%q", tc.name, ok, done, msg)
		}
	}
}

// fakeCluster is a Nomad cluster where a registered batch job finishes at
// once, its single allocation ending as outcome[id] says (complete by
// default). Unused API methods panic.
type fakeCluster struct {
	nomad.API
	mu         sync.Mutex
	index      uint64
	jobs       map[string]*api.Job
	allocs     map[string][]*api.AllocationListStub
	outcome    map[string]string
	registered []string
	purged     []string
}

func newFakeCluster() *fakeCluster {
	return &fakeCluster{jobs: map[string]*api.Job{}, allocs: map[string][]*api.AllocationListStub{}, outcome: map[string]string{}}
}

func (f *fakeCluster) Job(_ context.Context, _, id string) (*api.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return nil, nil
	}
	cp := *j
	return &cp, nil
}

func (f *fakeCluster) Register(_ context.Context, job *api.Job, opts *api.RegisterOptions) (*api.JobRegisterResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := *job.ID
	live := f.jobs[id]
	if opts != nil && opts.EnforceIndex {
		if (live == nil && opts.ModifyIndex != 0) || (live != nil && *live.JobModifyIndex != opts.ModifyIndex) {
			return nil, errors.New("enforcing job modify index: conflict")
		}
	}
	var version uint64
	if live != nil {
		version = *live.Version + 1
	}
	f.index++
	j := *job
	status := "dead"
	if f.outcome[id] == "running" {
		status = "running"
	}
	j.Version, j.JobModifyIndex, j.Status = &version, new(f.index), &status
	f.jobs[id] = &j
	f.registered = append(f.registered, id)
	if *j.Type == "batch" {
		f.allocs[id] = append(f.allocs[id], &api.AllocationListStub{Name: id + ".run[0]", JobVersion: version,
			ClientStatus: orDefault(f.outcome[id], "complete"), DesiredStatus: "run"})
	}
	return &api.JobRegisterResponse{EvalID: "eval-" + id}, nil
}

func (f *fakeCluster) Deregister(_ context.Context, _, id string, purge bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if purge {
		delete(f.jobs, id)
		delete(f.allocs, id)
		f.purged = append(f.purged, id)
	} else if j := f.jobs[id]; j != nil {
		j.Stop = new(true)
	}
	return "", nil
}

func (f *fakeCluster) Allocations(_ context.Context, _, id string) ([]*api.AllocationListStub, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*api.AllocationListStub(nil), f.allocs[id]...), nil
}

func (f *fakeCluster) LatestDeployment(context.Context, string, string) (*api.Deployment, error) {
	return nil, nil
}

func (f *fakeCluster) Summary(context.Context, string, string) (*api.JobSummary, error) {
	return nil, nil
}

func hookTestController(t *testing.T) (*Controller, *v1.Application) {
	t.Helper()
	old := hookPoll
	hookPoll = time.Millisecond
	t.Cleanup(func() { hookPoll = old })
	st := store.New(store.NewMemoryKV(), "test")
	app, err := st.CreateApplication(context.Background(), &v1.Application{Metadata: v1.ObjectMeta{Name: "shop"},
		Spec: v1.ApplicationSpec{Project: "default", Source: v1.ApplicationSource{RepoURL: "https://git.example.com/shop", Path: "."}}})
	if err != nil {
		t.Fatal(err)
	}
	app, err = st.MutateApplication(context.Background(), "shop", func(a *v1.Application) error {
		a.Status.OperationState = &v1.OperationState{Phase: v1.OperationRunning, StartedAt: time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return &Controller{store: st, logger: slog.New(slog.DiscardHandler), now: time.Now}, app
}

func batchHook(id, phases string, meta ...string) *api.Job {
	j := hookJob(id, "batch", map[string]string{v1.AppMetaKey: "shop", v1.HookMetaKey: phases})
	j.Namespace = new("default")
	for i := 0; i+1 < len(meta); i += 2 {
		j.Meta[meta[i]] = meta[i+1]
	}
	return j
}

func TestRunHooks(t *testing.T) {
	c, app := hookTestController(t)
	ctx := context.Background()
	run := func(cluster *fakeCluster, phase string, hooks ...*api.Job) (bool, error, string) {
		r := &hookRun{app: app, cluster: cluster, so: &v1.SyncOperation{}, revision: "0123456789", result: &v1.SyncOperationResult{},
			attempt: attemptOf(app.Status.OperationState)}
		ok, err := c.runHooks(ctx, r, phase, hooks)
		var out []string
		for _, res := range r.result.Resources {
			out = append(out, res.Name+": "+string(res.Status)+": "+res.Message)
		}
		return ok, err, strings.Join(out, "\n")
	}

	// Waves run in order; a wave's hooks all run, and a failure stops
	// the next wave.
	cluster := newFakeCluster()
	cluster.outcome["a"] = "failed"
	ok, err, got := run(cluster, v1.HookPreSync, batchHook("a", "PreSync"), batchHook("b", "PreSync"),
		batchHook("c", "PreSync", "cystemd.sync-wave", "1"))
	if ok || err != nil || got != "a: SyncFailed: PreSync hook failed: allocation a.run[0] failed\nb: Synced: PreSync hook succeeded" {
		t.Fatalf("ok=%v err=%v results:\n%s", ok, err, got)
	}
	if strings.Join(cluster.registered, ",") != "a,b" {
		t.Fatalf("registered = %v", cluster.registered)
	}
	if run := cluster.jobs["b"].Meta[v1.HookRunMetaKey]; run != "PreSync 2026-10-15T09:00:00Z/0 0123456" {
		t.Fatalf("run marker = %q", run)
	}

	// Delete policies; the default keeps the job until the next run.
	cluster = newFakeCluster()
	cluster.outcome["fails"] = "failed"
	ok, _, got = run(cluster, v1.HookPostSync,
		batchHook("kept", "PostSync"),
		batchHook("passes", "PostSync", v1.HookDeletePolicyMetaKey, "HookSucceeded"),
		batchHook("fails", "PostSync", v1.HookDeletePolicyMetaKey, "HookFailed"))
	if ok || got != "kept: Synced: PostSync hook succeeded\npasses: Synced: PostSync hook succeeded (job deleted)\n"+
		"fails: SyncFailed: PostSync hook failed: allocation fails.run[0] failed (job deleted)" {
		t.Fatalf("results:\n%s", got)
	}
	if cluster.jobs["kept"] == nil || strings.Join(cluster.purged, ",") != "passes,fails" {
		t.Fatalf("jobs=%v purged=%v", slices.Sorted(maps.Keys(cluster.jobs)), cluster.purged)
	}

	// The next run deletes the kept one first (BeforeHookCreation)...
	cluster.registered = nil
	ok, _, _ = run(cluster, v1.HookSyncFail, batchHook("kept", "PostSync,SyncFail"))
	if !ok || cluster.purged[len(cluster.purged)-1] != "kept" || *cluster.jobs["kept"].Version != 0 {
		t.Fatalf("ok=%v purged=%v", ok, cluster.purged)
	}
	// ...but a resumed operation waits for the run it created.
	cluster.registered = nil
	ok, _, got = run(cluster, v1.HookSyncFail, batchHook("kept", "PostSync,SyncFail"))
	if !ok || len(cluster.registered) != 0 || got != "kept: Synced: SyncFail hook succeeded" {
		t.Fatalf("resume: ok=%v registered=%v results:\n%s", ok, cluster.registered, got)
	}

	// A job that is not this Application's hook is left alone.
	cluster = newFakeCluster()
	cluster.jobs["web"] = &api.Job{ID: new("web"), Type: new("service"), Version: new(uint64(3)), JobModifyIndex: new(uint64(9)),
		Meta: map[string]string{v1.AppMetaKey: "shop"}}
	ok, _, got = run(cluster, v1.HookPreSync, batchHook("web", "PreSync"))
	if ok || got != "web: SyncFailed: PreSync hook: job default/web exists and is not a hook of this application" || len(cluster.registered) != 0 {
		t.Fatalf("ok=%v results:\n%s", ok, got)
	}

	// A dry run creates nothing.
	cluster = newFakeCluster()
	r := &hookRun{app: app, cluster: cluster, so: &v1.SyncOperation{DryRun: true}, result: &v1.SyncOperationResult{}}
	if ok, err := c.runHooks(ctx, r, v1.HookPreSync, []*api.Job{batchHook("a", "PreSync")}); !ok || err != nil || len(cluster.registered) != 0 ||
		r.result.Resources[0].Message != "PreSync hook would run (dry run)" {
		t.Fatalf("dry run: ok=%v err=%v registered=%v", ok, err, cluster.registered)
	}

	// The running hook shows among the Application's resources.
	a, err := c.store.GetApplication(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(a.Status.Resources, func(rs v1.ResourceStatus) bool { return rs.Name == "kept" && rs.Hook }) {
		t.Fatalf("resources = %+v", a.Status.Resources)
	}
}

func TestRunHooksTerminated(t *testing.T) {
	c, app := hookTestController(t)
	ctx := context.Background()
	cluster := newFakeCluster()
	// The job never finishes: the user terminates the operation.
	cluster.outcome["slow"] = "running"
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = c.store.MutateApplication(ctx, "shop", func(a *v1.Application) error {
			a.Status.OperationState.Phase = v1.OperationTerminating
			return nil
		})
	}()
	r := &hookRun{app: app, cluster: cluster, so: &v1.SyncOperation{}, result: &v1.SyncOperationResult{}}
	ok, err := c.runHooks(ctx, r, v1.HookPreSync, []*api.Job{batchHook("slow", "PreSync")})
	if ok || !errors.Is(err, errTerminated) {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if res := r.result.Resources[0]; res.Status != v1.ResultSyncFailed || res.Message != "PreSync hook interrupted: operation terminated (job stopped)" {
		t.Fatalf("result = %+v", res)
	}
	if j := cluster.jobs["slow"]; j.Stop == nil || !*j.Stop {
		t.Fatal("the running hook was not stopped")
	}
}

func TestWaitHealthy(t *testing.T) {
	c, app := hookTestController(t)
	ctx := context.Background()
	cluster := newFakeCluster()
	web := &api.Job{ID: new("web"), Namespace: new("default"), Type: new("service"), Version: new(uint64(2)), Status: new("running"),
		TaskGroups: []*api.TaskGroup{{Name: new("app"), Count: new(1)}}}
	cluster.jobs["web"] = web
	r := &hookRun{app: app, cluster: cluster, so: &v1.SyncOperation{}, result: &v1.SyncOperationResult{}}

	cluster.allocs["web"] = []*api.AllocationListStub{{Name: "web.app[0]", JobVersion: 2, ClientStatus: "running", DesiredStatus: "run"}}
	if err := c.waitHealthy(ctx, r, []*api.Job{web}, time.Second); err != nil {
		t.Fatal(err)
	}
	cluster.allocs["web"][0].ClientStatus = "failed"
	if err := c.waitHealthy(ctx, r, []*api.Job{web}, time.Second); err == nil || !strings.Contains(err.Error(), "job default/web is degraded") {
		t.Fatalf("degraded: %v", err)
	}
	cluster.allocs["web"][0].ClientStatus = "pending"
	if err := c.waitHealthy(ctx, r, []*api.Job{web}, 20*time.Millisecond); err == nil || !strings.Contains(err.Error(), "not healthy within 20ms: job web is progressing") {
		t.Fatalf("progressing: %v", err)
	}
}
