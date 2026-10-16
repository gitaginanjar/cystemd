// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// GitRepo is a local Git repository tests commit to.
type GitRepo struct {
	Dir  string
	repo *gogit.Repository
}

// NewGitRepo creates an empty repository.
func NewGitRepo(t testing.TB) *GitRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &GitRepo{Dir: dir, repo: repo}
}

// URL is the file:// URL of the repository.
func (r *GitRepo) URL() string { return "file://" + r.Dir }

// Commit writes files (an empty content deletes the file) and commits them,
// returning the commit hash.
func (r *GitRepo) Commit(t testing.TB, message string, files map[string]string) string {
	t.Helper()
	wt, err := r.repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		p := filepath.Join(r.Dir, name)
		if content == "" {
			if _, err := wt.Remove(name); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	h, err := wt.Commit(message, &gogit.CommitOptions{
		Author:            &object.Signature{Name: "CYSTEMD Test", Email: "test@cystemd.io", When: time.Now()},
		AllowEmptyCommits: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}
