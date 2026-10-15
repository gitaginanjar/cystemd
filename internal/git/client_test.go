// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestRepo is a local repository for tests: commit files, branch, tag.
type TestRepo struct {
	Dir  string
	repo *gogit.Repository
}

func newTestRepo(t *testing.T) *TestRepo {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	return &TestRepo{Dir: dir, repo: repo}
}

func (r *TestRepo) URL() string { return "file://" + r.Dir }

func (r *TestRepo) commit(t *testing.T, files map[string]string, msg string) string {
	t.Helper()
	wt, _ := r.repo.Worktree()
	for name, content := range files {
		p := filepath.Join(r.Dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if content == "" {
			wt.Remove(name)
			continue
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{Author: &object.Signature{Name: "Dev", Email: "dev@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}

func TestLsRemoteCheckoutAndMetadata(t *testing.T) {
	ctx := context.Background()
	src := newTestRepo(t)
	first := src.commit(t, map[string]string{"apps/web/web.nomad.hcl": "job \"web\" {}\n"}, "first")
	if _, err := src.repo.CreateTag("v1.0.0", plumbing.NewHash(first), &gogit.CreateTagOptions{
		Message: "release", Tagger: &object.Signature{Name: "Dev", Email: "dev@example.com", When: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	second := src.commit(t, map[string]string{"apps/web/web.nomad.hcl": "job \"web\" { type = \"batch\" }\n", "README.md": "x"}, "second")

	c := NewClient(Options{CacheDir: t.TempDir()})

	head, err := c.LsRemote(ctx, src.URL(), "HEAD")
	if err != nil || head != second {
		t.Fatalf("HEAD = %s, %v (want %s)", head, err, second)
	}
	branch, _ := c.LsRemote(ctx, src.URL(), "master")
	if branch != second {
		t.Fatalf("master = %s", branch)
	}
	tag, err := c.LsRemote(ctx, src.URL(), "v1.0.0")
	if err != nil || tag != first {
		t.Fatalf("annotated tag must peel to its commit: %s, %v", tag, err)
	}
	if _, err := c.LsRemote(ctx, src.URL(), "nope"); err == nil {
		t.Fatal("unknown revision resolved")
	}

	var content string
	err = c.Checkout(ctx, src.URL(), first, func(dir string) error {
		b, err := os.ReadFile(filepath.Join(dir, "apps/web/web.nomad.hcl"))
		content = string(b)
		if _, err := os.Stat(filepath.Join(dir, "README.md")); err == nil {
			t.Error("file of a later commit present in an older checkout")
		}
		return err
	})
	if err != nil || content != "job \"web\" {}\n" {
		t.Fatalf("checkout first: %q, %v", content, err)
	}

	// A commit pushed after the clone is fetched on demand.
	third := src.commit(t, map[string]string{"apps/web/web.nomad.hcl": "job \"web\" { type = \"system\" }\n"}, "third")
	err = c.Checkout(ctx, src.URL(), third, func(dir string) error {
		b, err := os.ReadFile(filepath.Join(dir, "apps/web/web.nomad.hcl"))
		content = string(b)
		return err
	})
	if err != nil || content != "job \"web\" { type = \"system\" }\n" {
		t.Fatalf("checkout third: %q, %v", content, err)
	}

	md, err := c.Commit(ctx, src.URL(), first)
	if err != nil {
		t.Fatal(err)
	}
	if md.Message != "first" || md.Author != "Dev <dev@example.com>" || len(md.Tags) != 1 || md.Tags[0] != "v1.0.0" {
		t.Fatalf("metadata = %+v", md)
	}

	branches, tags, err := c.Refs(ctx, src.URL())
	if err != nil || len(branches) != 1 || len(tags) != 1 {
		t.Fatalf("refs: %v %v %v", branches, tags, err)
	}
}

func TestIsSSH(t *testing.T) {
	for url, want := range map[string]bool{
		"git@bitbucket.org:org/repo.git":  true,
		"ssh://git@host:22/org/repo.git":  true,
		"https://github.com/org/repo.git": false,
		"file:///tmp/repo":                false,
		"/local/path":                     false,
	} {
		if got := isSSH(url); got != want {
			t.Errorf("isSSH(%q) = %v", url, got)
		}
	}
}

func TestSSHRequiresHostKeyVerification(t *testing.T) {
	_, _, err := authMethod("git@host:org/repo.git", &Credentials{SSHPrivateKey: []byte("x")}, "")
	if err == nil {
		t.Fatal("invalid key accepted")
	}
	if _, _, err := authMethod("git@host:org/repo.git", nil, ""); err == nil {
		t.Fatal("ssh without key accepted")
	}
}
