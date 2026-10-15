// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/testutil"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// secretJob reads a Vault KV v2 secret with its workload identity and
// prints it.
func secretJob(id, path string) string {
	return fmt.Sprintf(`job %q {
  datacenters = ["*"]
  group "g" {
    task "t" {
      driver = "raw_exec"
      vault {
        role = "cystemd-workloads"
      }
      template {
        destination = "secrets/app.env"
        data        = <<-EOT
          password={{ with secret %q }}{{ .Data.data.password }}{{ end }}
        EOT
      }
      config {
        command = "/bin/sh"
        args    = ["-c", "cat secrets/app.env; sleep 3600"]
      }
      resources {
        cpu    = 20
        memory = 16
      }
    }
  }
}
`, id, path)
}

// TestEndToEndVault checks the Vault integration as deploy/vault/setup.sh
// configures it: the control plane keeps repository credentials in Vault
// through its Vault Agent, and jobs read their own secrets, and only
// theirs, with their Nomad workload identity.
func TestEndToEndVault(t *testing.T) {
	s := newStack(t, stackOptions{vault: true})
	ctx := s.ctx

	// Repository credentials go to Vault, not to the state in Consul.
	const password = "e2e-s3cret-repository-password"
	if _, err := s.api.AddRepository(ctx, &v1.Repository{Repo: "https://127.0.0.1:1/private.git", Username: "deploy", Password: password}, true); err != nil {
		t.Fatal(err)
	}
	repos, err := s.api.ListRepositories(ctx)
	if err != nil || len(repos) != 1 || !repos[0].HasCredentials || repos[0].Password != "" {
		t.Fatalf("repositories: %+v %v", repos, err)
	}
	list, err := s.vaultAPI.Logical().List("secret/metadata/cystemd/repositories")
	if err != nil || list == nil {
		t.Fatalf("no credentials in Vault: %v", err)
	}
	keys, _ := list.Data["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("Vault keys: %v", list.Data)
	}
	sec, err := s.vaultAPI.KVv2("secret").Get(ctx, "cystemd/repositories/"+fmt.Sprint(keys[0]))
	if err != nil || sec.Data["password"] != password || sec.Data["username"] != "deploy" {
		t.Fatalf("credentials in Vault: %+v %v", sec, err)
	}
	pairs, _, err := s.kv.List("cystemd/", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pairs {
		if strings.Contains(string(p.Value), password) {
			t.Fatalf("Consul key %s holds the password", p.Key)
		}
	}
	if err := s.api.RemoveRepository(ctx, "https://127.0.0.1:1/private.git"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.vaultAPI.Logical().List("secret/metadata/cystemd/repositories"); list != nil && len(list.Data["keys"].([]any)) > 0 {
		t.Fatalf("credentials left in Vault: %v", list.Data)
	}

	// Workload identity: each job reads secret/<namespace>/<job>/.
	for path, value := range map[string]string{"default/reader/app": "own-secret", "default/other/app": "not-yours"} {
		if _, err := s.vaultAPI.KVv2("secret").Put(ctx, path, map[string]any{"password": value}); err != nil {
			t.Fatal(err)
		}
	}
	s.repo.Commit(t, "secret readers", map[string]string{
		"apps/vault/reader.nomad.hcl":   secretJob("reader", "secret/data/default/reader/app"),
		"apps/vault/intruder.nomad.hcl": secretJob("intruder", "secret/data/default/other/app"),
	})
	app := &v1.Application{
		Metadata: v1.ObjectMeta{Name: "vault"},
		Spec: v1.ApplicationSpec{
			Source:     v1.ApplicationSource{RepoURL: s.repo.URL(), Path: "apps/vault", TargetRevision: "master"},
			SyncPolicy: &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{}},
		},
	}
	if _, err := s.api.CreateApplication(ctx, app, false); err != nil {
		t.Fatal(err)
	}
	mustOK(t, cli(t, s.password+"\n", "login", "-password-stdin", s.url), "Logged in")
	testutil.Eventually(t, 3*time.Minute, func() (bool, string) {
		r := cli(t, "", "app", "logs", "vault", "-job", "reader", "-tail", "5")
		return strings.Contains(r.out, "password=own-secret"), r.out
	})
	// The intruder's template cannot read the other job's secret: Nomad
	// reports the read as missing, and the task never starts.
	allocs, _, err := s.nomadAPI.Jobs().Allocations("intruder", false, nil)
	if err != nil || len(allocs) == 0 {
		t.Fatalf("intruder allocations: %v", err)
	}
	testutil.Eventually(t, time.Minute, func() (bool, string) {
		alloc, _, err := s.nomadAPI.Allocations().Info(allocs[0].ID, nil)
		if err != nil {
			return false, err.Error()
		}
		if alloc.ClientStatus == "running" {
			t.Fatal("the intruder read another job's secret")
		}
		var events []string
		if ts := alloc.TaskStates["t"]; ts != nil {
			for _, e := range ts.Events {
				events = append(events, e.DisplayMessage)
			}
		}
		all := strings.Join(events, " | ")
		return strings.Contains(all, "Missing: vault.read(secret/data/default/other/app)"), all
	})
	if a := s.app("vault"); a.Status.Health.Status == v1.HealthStatusHealthy {
		t.Errorf("the application is healthy with a task that cannot start: %+v", a.Status.Health)
	}
}
