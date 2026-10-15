// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller_test

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/git"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/internal/store"
	"github.com/gitaginanjar/cystemd/internal/testutil"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func sleepJob(id string, count int, extra string) string {
	return fmt.Sprintf(`
job %q {
  type        = "service"
  datacenters = ["*"]
  group "app" {
    count = %d
    restart {
      attempts = 0
      mode     = "fail"
    }
    task "sleep" {
      driver = "raw_exec"
      config {
        command = "/bin/sleep"
        args    = ["3600"]
      }
      resources {
        cpu    = 20
        memory = 16
      }
%s
    }
  }
}
`, id, count, extra)
}

type env struct {
	t        *testing.T
	ctx      context.Context
	store    *store.Store
	repo     *testutil.GitRepo
	nomad    *api.Client
	consul   *consulapi.Client
	repos    *controller.RepoService
	ctrl     *controller.Controller
	clusters *nomad.Clusters
}

func setup(t *testing.T) *env {
	if testing.Short() {
		t.Skip("integration test")
	}
	consulAgent := testutil.StartConsul(t)
	nomadAgent := testutil.StartNomad(t, testutil.NomadOptions{ConsulAddr: consulAgent.Addr})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	nc, err := nomad.New(nomad.Config{Address: nomadAgent.URL})
	if err != nil {
		t.Fatal(err)
	}
	clusters := nomad.NewClusters()
	clusters.Add(v1.InClusterName, nc)

	ccfg := consulapi.DefaultConfig()
	ccfg.Address = consulAgent.Addr
	cc, err := consulapi.NewClient(ccfg)
	if err != nil {
		t.Fatal(err)
	}
	consul := &controller.Consul{Client: cc}

	st := store.New(store.NewMemoryKV(), "cystemd")
	repos := &controller.RepoService{
		Git:      git.NewClient(git.Options{CacheDir: t.TempDir()}),
		Clusters: clusters,
		Options:  render.Options{ConsulEnabled: true, KVPrefix: "cystemd"},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	ctrl := controller.New(controller.Options{
		Store: st, Clusters: clusters, Repos: repos, Consul: consul, Checks: consul, Logger: logger,
		Config: controller.Config{ReconcileInterval: 2 * time.Second, SelfHealTimeout: time.Second, Workers: 4, EventDebounce: 300 * time.Millisecond},
	})
	go ctrl.Run(ctx)
	return &env{t: t, ctx: ctx, store: st, repo: testutil.NewGitRepo(t), nomad: nc.Raw(), consul: cc, repos: repos, ctrl: ctrl, clusters: clusters}
}

func (e *env) app(name string) *v1.Application {
	a, err := e.store.GetApplication(e.ctx, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return a
}

func (e *env) waitApp(name string, timeout time.Duration, cond func(a *v1.Application) bool) *v1.Application {
	e.t.Helper()
	var last *v1.Application
	testutil.Eventually(e.t, timeout, func() (bool, string) {
		a, err := e.store.GetApplication(e.ctx, name)
		if err != nil {
			return false, err.Error()
		}
		last = a
		return cond(a), fmt.Sprintf("sync=%s health=%s (%s) revision=%s op=%+v conditions=%+v",
			a.Status.Sync.Status, a.Status.Health.Status, a.Status.Health.Message, short(a.Status.Sync.Revision),
			a.Status.OperationState, a.Status.Conditions)
	})
	return last
}

func (e *env) job(id string) *api.Job {
	j, _, err := e.nomad.Jobs().Info(id, nil)
	if err != nil {
		return nil
	}
	return j
}

func (e *env) refresh(name string) {
	_, err := e.store.MutateApplication(e.ctx, name, func(a *v1.Application) error {
		if a.Metadata.Annotations == nil {
			a.Metadata.Annotations = map[string]string{}
		}
		a.Metadata.Annotations[controller.RefreshAnnotation] = "hard"
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func TestControllerEndToEnd(t *testing.T) {
	e := setup(t)
	first := e.repo.Commit(t, "web v1", map[string]string{
		"apps/web/web.nomad.hcl": sleepJob("web", 1, ""),
		"apps/web/kv.yaml":       "- key: web/config/greeting\n  value: hello\n",
	})

	_, err := e.store.CreateApplication(e.ctx, &v1.Application{
		Metadata: v1.ObjectMeta{Name: "web"},
		Spec: v1.ApplicationSpec{
			Project: "default",
			Source: v1.ApplicationSource{RepoURL: e.repo.URL(), Path: "apps/web", TargetRevision: "master",
				Nomad: &v1.NomadSource{ConsulKV: "kv.yaml"}},
			SyncPolicy: &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{Prune: true, SelfHeal: true}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Auto-sync registers the job and writes the key; it becomes Healthy.
	a := e.waitApp("web", 90*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusSynced && a.Status.Health.Status == v1.HealthStatusHealthy &&
			a.Status.Sync.Revision == first
	})
	if len(a.Status.History) != 1 || a.Status.History[0].Revision != first || len(a.Status.History[0].JobVersions) != 1 {
		t.Fatalf("history = %+v", a.Status.History)
	}
	job := e.job("web")
	if job == nil || job.Meta[v1.AppMetaKey] != "web" {
		t.Fatalf("job not registered with tracking meta: %+v", job)
	}
	if sub, _, err := e.nomad.Jobs().Submission("web", int(*job.Version), nil); err != nil || !strings.Contains(sub.Source, `job "web"`) {
		t.Fatalf("jobspec submission not stored: %v %+v", err, sub)
	}
	if p, _, _ := e.consul.KV().Get("web/config/greeting", nil); p == nil || string(p.Value) != "hello" {
		t.Fatalf("consul key not written: %+v", p)
	}

	// The resource tree shows the job, its group, allocation and task.
	live := &controller.Live{Clusters: e.clusters, Consul: &controller.Consul{Client: e.consul}, Repos: e.repos}
	tree, err := live.Tree(e.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, n := range tree.Nodes {
		kinds[n.Kind]++
	}
	if kinds[v1.KindJob] != 1 || kinds[v1.KindTaskGroup] != 1 || kinds[v1.KindAllocation] < 1 || kinds[v1.KindTask] < 1 || kinds[v1.KindConsulKV] != 1 {
		t.Fatalf("tree kinds = %v", kinds)
	}

	// 2. A new commit is synced automatically.
	second := e.repo.Commit(t, "web v2", map[string]string{"apps/web/web.nomad.hcl": sleepJob("web", 2, "")})
	e.refresh("web")
	e.waitApp("web", 90*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusSynced && a.Status.Sync.Revision == second && len(a.Status.History) == 2
	})
	if j := e.job("web"); *j.TaskGroups[0].Count != 2 {
		t.Fatalf("count = %d", *j.TaskGroups[0].Count)
	}

	// 3. Drift made outside Git is self-healed.
	j := e.job("web")
	j.TaskGroups[0].Count = new(5)
	if _, _, err := e.nomad.Jobs().Register(j, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.consul.KV().Put(&consulapi.KVPair{Key: "web/config/greeting", Value: []byte("tampered")}, nil); err != nil {
		t.Fatal(err)
	}
	e.refresh("web")
	testutil.Eventually(t, 90*time.Second, func() (bool, string) {
		j := e.job("web")
		p, _, _ := e.consul.KV().Get("web/config/greeting", nil)
		return j != nil && *j.TaskGroups[0].Count == 2 && p != nil && string(p.Value) == "hello",
			fmt.Sprintf("count=%d key=%q", *j.TaskGroups[0].Count, p.Value)
	})

	// 4. Replacing the job prunes the old one and its key.
	third := e.repo.Commit(t, "replace web with api", map[string]string{
		"apps/web/web.nomad.hcl": "",
		"apps/web/kv.yaml":       "",
		"apps/web/api.nomad.hcl": sleepJob("api", 1, ""),
	})
	_, err = e.store.MutateApplication(e.ctx, "web", func(a *v1.Application) error {
		a.Spec.Source.Nomad = nil
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.refresh("web")
	a = e.waitApp("web", 90*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusSynced && a.Status.Sync.Revision == third &&
			a.Status.OperationState != nil && a.Status.OperationState.SyncResult != nil &&
			a.Status.OperationState.SyncResult.Revision == third
	})
	if old := e.job("web"); old == nil || old.Stop == nil || !*old.Stop {
		t.Fatalf("pruned job still running: %+v", old)
	}
	if p, _, _ := e.consul.KV().Get("web/config/greeting", nil); p != nil {
		t.Fatalf("pruned key still present")
	}
	if e.job("api") == nil {
		t.Fatal("api job not registered")
	}

	// 5. Rollback to the first sync: auto-sync must be off (Argo CD rule).
	_, err = e.store.MutateApplication(e.ctx, "web", func(a *v1.Application) error {
		a.Spec.SyncPolicy = nil
		id := a.Status.History[0].ID
		a.Operation = &v1.Operation{Sync: &v1.SyncOperation{RollbackID: &id, Prune: true}, InitiatedBy: v1.OperationInitiator{Username: "admin"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a = e.waitApp("web", 90*time.Second, func(a *v1.Application) bool {
		return a.Operation == nil && a.Status.OperationState != nil && a.Status.OperationState.Operation.Sync.RollbackID != nil &&
			a.Status.OperationState.Phase.Completed()
	})
	if a.Status.OperationState.Phase != v1.OperationSucceeded {
		t.Fatalf("rollback: %+v", a.Status.OperationState)
	}
	restored := e.job("web")
	if restored == nil || (restored.Stop != nil && *restored.Stop) || *restored.TaskGroups[0].Count != 1 {
		t.Fatalf("rollback did not restore web v1: %+v", restored)
	}
	if p, _, _ := e.consul.KV().Get("web/config/greeting", nil); p == nil || string(p.Value) != "hello" {
		t.Fatal("rollback did not restore the consul key")
	}
	if api := e.job("api"); api == nil || api.Stop == nil || !*api.Stop {
		t.Fatal("rollback with prune left the api job running")
	}
	// Live state is now the old revision: OutOfSync with Git HEAD.
	e.refresh("web")
	e.waitApp("web", 60*time.Second, func(a *v1.Application) bool { return a.Status.Sync.Status == v1.SyncStatusOutOfSync })

	// 6. Cascade delete removes every job the Application tracks.
	_, err = e.store.MutateApplication(e.ctx, "web", func(a *v1.Application) error {
		now := time.Now()
		a.Metadata.DeletionTimestamp = &now
		a.Metadata.Finalizers = append(a.Metadata.Finalizers, v1.ResourcesFinalizer)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 60*time.Second, func() (bool, string) {
		_, err := e.store.GetApplication(e.ctx, "web")
		stubs, _, _ := e.nomad.Jobs().List(nil)
		var live []string
		for _, s := range stubs {
			if s.Status != "dead" {
				live = append(live, s.ID+"="+s.Status)
			}
		}
		return err == store.ErrNotFound && len(stubs) == 0, fmt.Sprintf("app err=%v jobs=%v", err, live)
	})
}

func TestControllerManualSyncAndDiff(t *testing.T) {
	e := setup(t)
	e.repo.Commit(t, "batch", map[string]string{"jobs/hello.nomad.hcl": sleepJob("hello", 1, "")})
	_, err := e.store.CreateApplication(e.ctx, &v1.Application{
		Metadata: v1.ObjectMeta{Name: "hello"},
		Spec: v1.ApplicationSpec{Project: "default",
			Source: v1.ApplicationSource{RepoURL: e.repo.URL(), Path: "jobs"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Without auto-sync the job is reported missing, never created.
	a := e.waitApp("hello", 60*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusOutOfSync && a.Status.Health.Status == v1.HealthStatusMissing
	})
	if e.job("hello") != nil {
		t.Fatal("job created without a sync")
	}
	live := &controller.Live{Clusters: e.clusters, Repos: e.repos}
	diffs, err := live.ManagedResources(e.ctx, a)
	if err != nil || len(diffs) != 1 || !diffs[0].Modified || !strings.Contains(diffs[0].TargetState, "hello") {
		t.Fatalf("managed resources = %+v, %v", diffs, err)
	}

	// Dry run changes nothing.
	_, err = e.store.MutateApplication(e.ctx, "hello", func(a *v1.Application) error {
		a.Operation = &v1.Operation{Sync: &v1.SyncOperation{DryRun: true}, InitiatedBy: v1.OperationInitiator{Username: "admin"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a = e.waitApp("hello", 60*time.Second, func(a *v1.Application) bool {
		return a.Operation == nil && a.Status.OperationState != nil && a.Status.OperationState.Phase.Completed()
	})
	if e.job("hello") != nil || len(a.Status.History) != 0 {
		t.Fatal("dry run changed the cluster or the history")
	}

	// A real sync registers and tags the version with the Git revision.
	_, err = e.store.MutateApplication(e.ctx, "hello", func(a *v1.Application) error {
		a.Operation = &v1.Operation{Sync: &v1.SyncOperation{}, InitiatedBy: v1.OperationInitiator{Username: "admin"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a = e.waitApp("hello", 90*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusSynced && a.Status.Health.Status == v1.HealthStatusHealthy
	})
	versions, _, _, err := e.nomad.Jobs().Versions("hello", false, nil)
	if err != nil || len(versions) == 0 || versions[0].VersionTag == nil || !strings.HasPrefix(versions[0].VersionTag.Name, "cystemd-") {
		t.Fatalf("version not tagged: %v %+v", err, versions)
	}
	events, _ := e.store.ListEvents(e.ctx, "hello")
	var reasons []string
	for _, ev := range events {
		reasons = append(reasons, ev.Reason)
	}
	if !strings.Contains(strings.Join(reasons, ","), "OperationCompleted") {
		t.Fatalf("events = %v", reasons)
	}
}

func hookJobHCL(id, hook, extraMeta string, seconds, exit int) string {
	return fmt.Sprintf(`
job %q {
  type        = "batch"
  datacenters = ["*"]
  meta = {
    "cystemd.hook" = %q
%s
  }
  group "run" {
    restart {
      attempts = 0
      mode     = "fail"
    }
    reschedule {
      attempts  = 0
      unlimited = false
    }
    task "main" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "sleep %d; exit %d"]
      }
      resources {
        cpu    = 20
        memory = 16
      }
    }
  }
}
`, id, hook, extraMeta, seconds, exit)
}

func TestControllerHooks(t *testing.T) {
	e := setup(t)
	files := map[string]string{
		"hooks/web.nomad.hcl":     sleepJob("web", 1, ""),
		"hooks/migrate.nomad.hcl": hookJobHCL("migrate", "PreSync", "", 1, 0),
		"hooks/smoke.nomad.hcl":   hookJobHCL("smoke", "PostSync", `"cystemd.hook-delete-policy" = "HookSucceeded"`, 1, 0),
		// Two hooks of the same wave run together.
		"hooks/check-a.nomad.hcl": hookJobHCL("check-a", "PostSync", `"cystemd.sync-wave" = "1"`, 3, 0),
		"hooks/check-b.nomad.hcl": hookJobHCL("check-b", "PostSync", `"cystemd.sync-wave" = "1"`, 3, 0),
		"hooks/notify.nomad.hcl":  hookJobHCL("notify", "SyncFail", "", 1, 0),
	}
	first := e.repo.Commit(t, "web with hooks", files)
	_, err := e.store.CreateApplication(e.ctx, &v1.Application{
		Metadata: v1.ObjectMeta{Name: "hooked"},
		Spec: v1.ApplicationSpec{Project: "default",
			Source: v1.ApplicationSource{RepoURL: e.repo.URL(), Path: "hooks", TargetRevision: "master"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Hooks are not compared: only web is missing.
	a := e.waitApp("hooked", 60*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusOutOfSync && a.Status.Sync.Revision == first
	})
	if len(a.Status.Resources) != 1 || a.Status.Resources[0].Name != "web" {
		t.Fatalf("resources = %+v", a.Status.Resources)
	}
	live := &controller.Live{Clusters: e.clusters, Repos: e.repos}
	if diffs, err := live.ManagedResources(e.ctx, a); err != nil || len(diffs) != 1 || diffs[0].Name != "web" {
		t.Fatalf("managed resources = %+v, %v", diffs, err)
	}

	syncOp := func(dryRun bool) *v1.Application {
		t.Helper()
		started := time.Now().Add(-time.Second)
		_, err := e.store.MutateApplication(e.ctx, "hooked", func(a *v1.Application) error {
			a.Operation = &v1.Operation{Sync: &v1.SyncOperation{DryRun: dryRun}, InitiatedBy: v1.OperationInitiator{Username: "admin"}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return e.waitApp("hooked", 3*time.Minute, func(a *v1.Application) bool {
			st := a.Status.OperationState
			return a.Operation == nil && st != nil && st.Phase.Completed() && st.StartedAt.After(started)
		})
	}
	results := func(a *v1.Application) string {
		var out []string
		for _, r := range a.Status.OperationState.SyncResult.Resources {
			out = append(out, fmt.Sprintf("%s:%s:%s:%s", r.SyncPhase, r.Name, r.Status, r.Message))
		}
		return strings.Join(out, "\n")
	}

	// 1. A dry run only says what would run.
	a = syncOp(true)
	if got, want := results(a), "PreSync:migrate:Synced:PreSync hook would run (dry run)\n"+
		"Sync:web:Synced:would be updated (dry run)\n"+
		"PostSync:smoke:Synced:PostSync hook would run (dry run)\n"+
		"PostSync:check-a:Synced:PostSync hook would run (dry run)\n"+
		"PostSync:check-b:Synced:PostSync hook would run (dry run)"; got != want {
		t.Fatalf("dry run results:\n%s\nwant:\n%s", got, want)
	}
	if e.job("migrate") != nil || e.job("web") != nil {
		t.Fatal("dry run created jobs")
	}

	// 2. A sync runs PreSync, registers web, waits for it to be healthy, runs
	// PostSync and deletes it (HookSucceeded); SyncFail does not run.
	a = syncOp(false)
	if a.Status.OperationState.Phase != v1.OperationSucceeded {
		t.Fatalf("sync: %s\n%s", a.Status.OperationState.Message, results(a))
	}
	if got, want := results(a), "PreSync:migrate:Synced:PreSync hook succeeded\n"+
		"Sync:web:Synced:job registered\n"+
		"PostSync:smoke:Synced:PostSync hook succeeded (job deleted)\n"+
		"PostSync:check-a:Synced:PostSync hook succeeded\n"+
		"PostSync:check-b:Synced:PostSync hook succeeded"; got != want {
		t.Fatalf("results:\n%s\nwant:\n%s", got, want)
	}
	migrate, web := e.job("migrate"), e.job("web")
	if migrate == nil || *migrate.Status != "dead" || !strings.HasPrefix(migrate.Meta[v1.HookRunMetaKey], "PreSync ") {
		t.Fatalf("PreSync hook job = %+v", migrate)
	}
	allocs, _, err := e.nomad.Jobs().Allocations("migrate", false, nil)
	if err != nil || len(allocs) != 1 || allocs[0].ClientStatus != "complete" {
		t.Fatalf("PreSync allocations = %+v, %v", allocs, err)
	}
	finished := allocs[0].TaskStates["main"].FinishedAt
	if web == nil || time.Unix(0, *web.SubmitTime).Before(finished) {
		t.Fatalf("web was registered before the PreSync hook finished (%v < %v)", web, finished)
	}
	if e.job("smoke") != nil {
		t.Fatal("PostSync hook job not deleted (HookSucceeded)")
	}
	window := func(id string) (time.Time, time.Time) {
		allocs, _, err := e.nomad.Jobs().Allocations(id, false, nil)
		if err != nil || len(allocs) != 1 {
			t.Fatalf("%s allocations = %+v, %v", id, allocs, err)
		}
		ts := allocs[0].TaskStates["main"]
		return ts.StartedAt, ts.FinishedAt
	}
	aStart, aEnd := window("check-a")
	bStart, bEnd := window("check-b")
	if !bStart.Before(aEnd) || !aStart.Before(bEnd) {
		t.Fatalf("hooks of a wave ran one after the other: a %v-%v, b %v-%v", aStart, aEnd, bStart, bEnd)
	}
	if e.job("notify") != nil {
		t.Fatal("SyncFail hook ran after a successful sync")
	}
	// The hook shows among the resources without a say in the status.
	a = e.waitApp("hooked", 60*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusSynced && a.Status.Health.Status == v1.HealthStatusHealthy && len(a.Status.Resources) == 4
	})
	for _, r := range a.Status.Resources {
		if (r.Name != "web") != r.Hook || (r.Hook && (r.Status != "" || r.Health == nil || r.Health.Status != v1.HealthStatusHealthy)) {
			t.Fatalf("resources = %+v", a.Status.Resources)
		}
	}
	tree, err := live.Tree(e.ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	var hookInfo string
	for _, n := range tree.Nodes {
		if n.Kind == v1.KindJob && n.Name == "migrate" {
			for _, i := range n.Info {
				if i.Name == "Hook" {
					hookInfo = i.Value
				}
			}
		}
	}
	if hookInfo != "PreSync" {
		t.Fatalf("tree does not show the hook: %+v", tree.Nodes)
	}

	// 3. A failing PreSync hook stops the sync before web is updated, and
	// the SyncFail hook runs. The previous run was deleted first
	// (BeforeHookCreation), so the result is this run's.
	files["hooks/migrate.nomad.hcl"] = hookJobHCL("migrate", "PreSync", "", 1, 3)
	files["hooks/web.nomad.hcl"] = sleepJob("web", 2, "")
	e.repo.Commit(t, "failing migration", files)
	e.refresh("hooked")
	e.waitApp("hooked", 60*time.Second, func(a *v1.Application) bool { return a.Status.Sync.Status == v1.SyncStatusOutOfSync })
	a = syncOp(false)
	if a.Status.OperationState.Phase != v1.OperationFailed {
		t.Fatalf("sync with a failing hook: %s\n%s", a.Status.OperationState.Phase, results(a))
	}
	if got := results(a); !strings.HasPrefix(got, "PreSync:migrate:SyncFailed:PreSync hook failed: allocation migrate.run[0] failed (task main: Exit Code: 3") ||
		!strings.HasSuffix(got, "\nSyncFail:notify:Synced:SyncFail hook succeeded") || strings.Contains(got, ":web:") {
		t.Fatalf("results:\n%s", got)
	}
	if j := e.job("web"); *j.TaskGroups[0].Count != 1 {
		t.Fatal("web was updated although its PreSync hook failed")
	}
	if allocs, _, _ := e.nomad.Jobs().Allocations("migrate", false, nil); len(allocs) != 1 {
		t.Fatalf("PreSync allocations of this run = %+v", allocs)
	}
	if n := e.job("notify"); n == nil || *n.Status != "dead" {
		t.Fatalf("SyncFail hook job = %+v", n)
	}
	if a.Status.Sync.Status != v1.SyncStatusOutOfSync {
		t.Fatalf("sync status after the failed sync = %s", a.Status.Sync.Status)
	}
}
