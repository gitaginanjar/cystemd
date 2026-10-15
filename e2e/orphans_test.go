// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// TestEndToEndOrphans watches a project for jobs nobody tracks: a job
// started by hand next to an Application's shows in its tree and raises a
// warning, from the command line.
func TestEndToEndOrphans(t *testing.T) {
	s := newStack(t, stackOptions{})
	s.repo.Commit(t, "web", map[string]string{"apps/web/web.nomad.hcl": tickJob("web", 1)})
	mustOK(t, cli(t, s.password+"\n", "login", "-password-stdin", s.url), "Logged in")

	manifest := filepath.Join(t.TempDir(), "project.yaml")
	if err := os.WriteFile(manifest, []byte(`
apiVersion: cystemd.io/v1alpha1
kind: AppProject
metadata:
  name: watched
spec:
  sourceRepos: ["*"]
  destinations: [{name: "*", namespace: "*"}]
  orphanedResources:
    warn: true
`), 0o600); err != nil {
		t.Fatal(err)
	}
	mustOK(t, cli(t, "", "proj", "create", "-f", manifest), "watched")
	mustOK(t, cli(t, "", "app", "create", "web", "-project", "watched", "-repo", s.repo.URL(), "-path", "apps/web",
		"-revision", "master", "-sync-policy", "automated"), "created")

	job, err := s.nomadAPI.Jobs().ParseHCL(tickJob("by-hand", 1), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.nomadAPI.Jobs().Register(job, nil); err != nil {
		t.Fatal(err)
	}
	s.waitApp("web", 2*time.Minute, func(a *v1.Application) bool {
		for _, c := range a.Status.Conditions {
			if c.Type == v1.ConditionOrphanedWarning {
				return a.Status.Sync.Status == v1.SyncStatusSynced
			}
		}
		return false
	})
	mustOK(t, cli(t, "", "app", "get", "web"), "CONDITION OrphanedResourceWarning: Application has 1 orphaned resources")
	mustOK(t, cli(t, "", "app", "resources", "web", "-orphaned"), "Job   default    by-hand")
	mustOK(t, cli(t, "", "app", "resources", "web"), "web")
}
