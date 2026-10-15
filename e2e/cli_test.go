// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/testutil"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// TestEndToEndCLI drives an application through its life with the
// command line: create, diff, dry run, sync, logs, actions, a Git change
// synced in two steps, rollback, and a cascading delete.
func TestEndToEndCLI(t *testing.T) {
	s := newStack(t, stackOptions{})
	v1rev := s.repo.Commit(t, "web v1", map[string]string{
		"apps/web/web.nomad.hcl":  tickJob("web", 1),
		"apps/web/consul-kv.yaml": "- key: config/web/flags\n  value: a=1\n",
	})

	mustOK(t, cli(t, s.password+"\n", "login", "-password-stdin", "-name", "e2e", s.url), "Logged in")
	mustOK(t, cli(t, "", "app", "create", "web", "-repo", s.repo.URL(), "-path", "apps/web", "-revision", "master",
		"-consul-kv", "consul-kv.yaml", "-label", "team=e2e"), "application 'web' created")

	// Compared, not synced: the job and the key are missing.
	s.waitApp("web", time.Minute, func(a *v1.Application) bool { return a.Status.Sync.Status == v1.SyncStatusOutOfSync })
	mustExit(t, cli(t, "", "app", "diff", "web"), 1,
		"===== Job default/web =====", "+ID: web", "===== ConsulKV config/web/flags =====", "+a=1")
	mustOK(t, cli(t, "", "app", "list", "-l", "team=e2e"), "web   in-cluster  default    default  OutOfSync")

	// A dry run changes nothing.
	mustOK(t, cli(t, "", "app", "sync", "web", "-dry-run"), "Phase:      Succeeded", "Operation:  Dry run")
	if s.job("web") != nil {
		t.Fatal("a dry run registered the job")
	}

	// sync waits for the operation and reports each resource.
	mustOK(t, cli(t, "", "app", "sync", "web", "-timeout", "3m"),
		"Job       default    web", "ConsulKV", "config/web/flags", "Phase:      Succeeded", "Revision:   "+v1rev)
	mustOK(t, cli(t, "", "app", "wait", "web", "-timeout", "3m"), "web: Synced, Healthy")
	if v, ok := s.key("config/web/flags"); !ok || v != "a=1" {
		t.Fatalf("config/web/flags = %q, %v", v, ok)
	}
	mustExit(t, cli(t, "", "app", "diff", "web"), 0, "No differences.")
	mustOK(t, cli(t, "", "app", "get", "web"), "Sync status:     Synced to master ("+v1rev[:7]+")", "Health status:   Healthy")
	mustOK(t, cli(t, "", "app", "resources", "web", "-o", "tree"), "Job web Healthy", "Allocation web.app[0]", "Task tick")
	mustOK(t, cli(t, "", "app", "manifests", "web"), "# Nomad source at "+v1rev, "ID: web", "config/web/flags")

	// The task's output, through the control plane.
	testutil.Eventually(t, time.Minute, func() (bool, string) {
		r := cli(t, "", "app", "logs", "web", "-tail", "2")
		return r.code == 0 && strings.Count(r.out, "tick ") == 2, r.out
	})

	// Resource actions.
	allocs, _, err := s.nomadAPI.Jobs().Allocations("web", false, nil)
	if err != nil || len(allocs) != 1 {
		t.Fatalf("allocations: %v %v", allocs, err)
	}
	mustOK(t, cli(t, "", "app", "actions", "web", "restart", "Allocation/web.app[0]"), "restart: Allocation web.app[0]")
	mustOK(t, cli(t, "", "app", "actions", "web", "restart", "Job/web"), "restart: Job web")
	mustExit(t, cli(t, "", "app", "actions", "web", "promote", "Job/web"), 1, `action "promote" is not available on Job`)

	// Git moves on. Sync only the key first, then the rest.
	v2rev := s.repo.Commit(t, "web v2", map[string]string{
		"apps/web/web.nomad.hcl":  tickJob("web", 2),
		"apps/web/consul-kv.yaml": "- key: config/web/flags\n  value: a=2\n",
	})
	// The Git server reports the push: the application is compared at once.
	payload := fmt.Sprintf(`{"ref":"refs/heads/master","repository":{"clone_url":%q,"default_branch":"master"}}`, s.repo.URL())
	req, _ := http.NewRequest(http.MethodPost, s.url+"/api/webhook", strings.NewReader(payload))
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var hook struct{ Refreshed []string }
	_ = json.NewDecoder(resp.Body).Decode(&hook)
	resp.Body.Close()
	if resp.StatusCode != 200 || strings.Join(hook.Refreshed, ",") != "web" {
		t.Fatalf("webhook: %d %+v", resp.StatusCode, hook)
	}
	s.waitApp("web", 30*time.Second, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusOutOfSync && a.Status.Sync.Revision == v2rev
	})
	mustOK(t, cli(t, "", "app", "get", "web", "-hard-refresh"), "Sync status:     OutOfSync to master ("+v2rev[:7]+")")
	mustExit(t, cli(t, "", "app", "diff", "web"), 1, "-  - Count: 1", "+  - Count: 2", "-a=1", "+a=2", "nomad plan")
	mustOK(t, cli(t, "", "app", "sync", "web", "-resource", "ConsulKV::config/web/flags"), "Phase:      Succeeded")
	if v, _ := s.key("config/web/flags"); v != "a=2" {
		t.Fatalf("after the partial sync, config/web/flags = %q", v)
	}
	if j := s.job("web"); j == nil || *j.TaskGroups[0].Count != 1 {
		t.Fatal("the partial sync changed the job")
	}
	mustOK(t, cli(t, "", "app", "sync", "web"), "Phase:      Succeeded", "Revision:   "+v2rev)
	mustOK(t, cli(t, "", "app", "wait", "web", "-timeout", "3m"), "Synced, Healthy")
	if j := s.job("web"); j == nil || *j.TaskGroups[0].Count != 2 {
		t.Fatalf("job after sync: %+v", j)
	}

	// History, and a rollback to the first deployment.
	var history []v1.RevisionHistory
	out := mustOK(t, cli(t, "", "app", "history", "web", "-o", "json"))
	if err := json.Unmarshal([]byte(out), &history); err != nil || len(history) < 3 {
		t.Fatalf("history: %v\n%s", err, out)
	}
	first := history[0]
	if first.Revision != v1rev || first.JobVersions["default/web"] != 0 {
		t.Fatalf("first deployment: %+v", first)
	}
	mustOK(t, cli(t, "", "app", "history", "web"), "admin", "master ("+v2rev[:7]+")", "default/web@")
	mustExit(t, cli(t, "", "app", "rollback", "web", "99"), 1, "history entry 99 not found")
	mustOK(t, cli(t, "", "app", "rollback", "web", strconv.FormatInt(first.ID, 10), "-timeout", "3m"),
		"reverted to version 0", "key restored", "Phase:      Succeeded")
	if j := s.job("web"); j == nil || *j.TaskGroups[0].Count != 1 {
		t.Fatalf("job after rollback: %+v", j)
	}
	if v, _ := s.key("config/web/flags"); v != "a=1" {
		t.Fatalf("after the rollback, config/web/flags = %q", v)
	}
	// Git still says v2.
	s.waitApp("web", time.Minute, func(a *v1.Application) bool { return a.Status.Sync.Status == v1.SyncStatusOutOfSync })
	// Auto-sync excludes rollbacks.
	mustOK(t, cli(t, "", "app", "set", "web", "-sync-policy", "automated"), "updated")
	mustExit(t, cli(t, "", "app", "rollback", "web", strconv.FormatInt(first.ID, 10)), 1, "auto-sync is enabled")
	// ... and brings Git's state back.
	s.waitApp("web", 3*time.Minute, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusSynced && a.Status.Sync.Revision == v2rev
	})

	// Deleting cascades to the job and the key.
	mustOK(t, cli(t, "web\n", "app", "delete", "web", "-wait"), "application 'web' deleted")
	if s.job("web") != nil {
		t.Fatal("the job survived the application")
	}
	if _, ok := s.key("config/web/flags"); ok {
		t.Fatal("the key survived the application")
	}
	if out := mustOK(t, cli(t, "", "app", "list", "-o", "name")); strings.TrimSpace(out) != "" {
		t.Fatalf("applications left: %q", out)
	}
}
