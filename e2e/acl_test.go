// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/server"
	"github.com/gitaginanjar/cystemd/internal/testutil"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

// TestEndToEndLeastPrivilege runs the control plane with only the tokens
// the policies in deploy/ grant, on a default-deny Consul and an ACL
// Nomad: state, leader lock, sync, prune, Consul keys, health, logs and
// actions must all work with them. It also logs in with Nomad tokens.
func TestEndToEndLeastPrivilege(t *testing.T) {
	s := newStack(t, stackOptions{acl: true})

	// The leader lock and both dependencies work with the tokens.
	var health server.HealthDetail
	code, body := httpGet(t, s.url+"/api/v1/health", s.api.Token)
	if err := json.Unmarshal([]byte(body), &health); code != 200 || err != nil {
		t.Fatalf("health: %d %s", code, body)
	}
	if !health.Leader || health.Consul == nil || !health.Consul.Healthy || len(health.Nomad) != 1 || !health.Nomad[0].Healthy {
		t.Fatalf("health: %s", body)
	}

	rev := s.repo.Commit(t, "stack", map[string]string{
		"apps/stack/api.nomad.hcl":    tickJob("api", 1),
		"apps/stack/worker.nomad.hcl": tickJob("worker", 1),
		"apps/stack/consul-kv.yaml":   "- key: config/stack/flags\n  value: checkout=on\n- key: config/stack/limits\n  value: rps=10\n",
	})
	app := &v1.Application{
		Metadata: v1.ObjectMeta{Name: "stack"},
		Spec: v1.ApplicationSpec{
			Source: v1.ApplicationSource{RepoURL: s.repo.URL(), Path: "apps/stack", TargetRevision: "master",
				Nomad: &v1.NomadSource{ConsulKV: "consul-kv.yaml"}},
			SyncPolicy: &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{Prune: true, SelfHeal: true}},
		},
	}
	if _, err := s.api.CreateApplication(s.ctx, app, false); err != nil {
		t.Fatal(err)
	}
	s.waitApp("stack", 3*time.Minute, func(a *v1.Application) bool {
		return a.Status.Sync.Status == v1.SyncStatusSynced && a.Status.Health.Status == v1.HealthStatusHealthy && a.Status.Sync.Revision == rev
	})
	if s.job("api") == nil || s.job("worker") == nil {
		t.Fatal("jobs not registered")
	}
	if v, ok := s.key("config/stack/flags"); !ok || v != "checkout=on" {
		t.Fatalf("config/stack/flags = %q", v)
	}

	// Self-heal: a change made outside Git is reverted.
	if _, err := s.kv.Put(&consulapi.KVPair{Key: "config/stack/flags", Value: []byte("checkout=off")}, nil); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, time.Minute, func() (bool, string) {
		v, _ := s.key("config/stack/flags")
		return v == "checkout=on", v
	})

	// Prune: what Git drops is deleted.
	s.repo.Commit(t, "drop the worker and a key", map[string]string{
		"apps/stack/worker.nomad.hcl": "",
		"apps/stack/consul-kv.yaml":   "- key: config/stack/flags\n  value: checkout=on\n",
	})
	if _, err := s.api.GetApplication(s.ctx, "stack", "hard"); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, 2*time.Minute, func() (bool, string) {
		w := s.job("worker")
		_, keyLeft := s.key("config/stack/limits")
		return (w == nil || (w.Stop != nil && *w.Stop)) && !keyLeft, "worker or config/stack/limits still there"
	})

	// Logs and actions go through the control plane's token too.
	allocs, _, err := s.nomadAPI.Jobs().Allocations("api", false, nil)
	if err != nil || len(allocs) == 0 {
		t.Fatalf("allocations: %v", err)
	}
	var lines []string
	testutil.Eventually(t, time.Minute, func() (bool, string) {
		lines = nil
		err := s.api.Logs(s.ctx, "stack", allocs[0].ID, client.LogOptions{TailLines: 3}, func(l client.LogLine) error {
			lines = append(lines, l.Content)
			return nil
		})
		return err == nil && len(lines) == 3, strings.Join(lines, "|")
	})
	tree, err := s.api.ResourceTree(s.ctx, "stack")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range tree.Nodes {
		if n.Kind == v1.KindAllocation {
			if err := s.api.RunAction(s.ctx, "stack", n.ResourceRef, "restart"); err != nil {
				t.Fatalf("restart %s: %v", n.Name, err)
			}
			break
		}
	}

	// Nomad tokens log in: a management token as role:admin, a token of
	// the control plane's own policy as the default role (read-only).
	mustOK(t, cli(t, "", "login", "-nomad-token", s.nomad.Token, "-name", "mgmt", s.url), "Logged in")
	mustOK(t, cli(t, "", "account", "get-user-info"), "Provider:   nomad", "role:admin")
	mustOK(t, cli(t, "", "app", "sync", "stack", "-async"), "sync of 'stack' requested")

	reader, _, err := s.nomadAPI.ACLTokens().Create(&nomadapi.ACLToken{Name: "reader", Type: "client", Policies: []string{"cystemd-control-plane"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustOK(t, cli(t, "", "login", "-nomad-token", reader.SecretID, "-name", "reader", s.url), "Logged in")
	mustOK(t, cli(t, "", "app", "list"), "stack")
	mustExit(t, cli(t, "", "app", "sync", "stack"), 1, "permission denied")
	mustExit(t, cli(t, "", "login", "-nomad-token", "00000000-0000-0000-0000-000000000000", s.url), 1, "Nomad rejected the token")

	// The agent's command line finds the API at NOMAD_ADDR and logs in
	// with NOMAD_TOKEN on each invocation.
	isolate(t)
	t.Setenv("NOMAD_ADDR", s.url)
	t.Setenv("NOMAD_TOKEN", s.nomad.Token)
	agentCLI := agentOptions()
	out := runCLI(t, agentCLI, "", "app", "get", "stack")
	if out.code != 0 || !strings.Contains(out.out, "Sync status:") {
		t.Fatalf("agent CLI with NOMAD_TOKEN: %d\n%s", out.code, out.out)
	}
}
