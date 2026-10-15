// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileSyncer(t *testing.T) {
	taskDir := t.TempDir()
	dest := filepath.Join(t.TempDir(), "myapp", "target.config.yml")
	src := filepath.Join(taskDir, "local", "target.config.yml")
	files := []FileConfig{{Source: "local/target.config.yml", Destination: dest, Mode: "0640"}}
	s := newFileSyncer(taskDir, files)

	// Strict sync fails on a template that has not rendered.
	if _, err := s.sync(true); err == nil {
		t.Fatal("strict sync with a missing source must fail")
	}
	// A later tick skips it silently.
	if changed, err := s.sync(false); err != nil || len(changed) != 0 {
		t.Fatalf("lenient sync: changed=%v err=%v", changed, err)
	}

	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("a: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := s.sync(true)
	if err != nil || len(changed) != 1 {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "a: 1\n" {
		t.Fatalf("dest = %q", got)
	}
	fi, _ := os.Stat(dest)
	if fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", fi.Mode().Perm())
	}

	// Unchanged source: nothing written.
	if changed, _ := s.sync(false); len(changed) != 0 {
		t.Fatalf("unchanged source rewrote: %v", changed)
	}

	// Changed source: propagated.
	if err := os.WriteFile(src, []byte("a: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, _ := s.sync(false); len(changed) != 1 {
		t.Fatalf("changed source not propagated")
	}
	got, _ = os.ReadFile(dest)
	if string(got) != "a: 2\n" {
		t.Fatalf("dest = %q", got)
	}
}

func TestFileSyncerKeepsIdenticalDestination(t *testing.T) {
	taskDir := t.TempDir()
	dest := filepath.Join(t.TempDir(), "app.yml")
	os.MkdirAll(filepath.Join(taskDir, "local"), 0o755)
	os.WriteFile(filepath.Join(taskDir, "local", "app.yml"), []byte("same"), 0o644)
	os.WriteFile(dest, []byte("same"), 0o644)
	before, _ := os.Stat(dest)

	s := newFileSyncer(taskDir, []FileConfig{{Source: "local/app.yml", Destination: dest}})
	changed, err := s.sync(true)
	if err != nil || len(changed) != 0 {
		t.Fatalf("identical destination rewritten: changed=%v err=%v", changed, err)
	}
	after, _ := os.Stat(dest)
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("mtime changed")
	}
}

func TestWriteFileAtomicRefusesPlantedSymlink(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "app.yml")
	victim := filepath.Join(dir, "victim")
	os.WriteFile(victim, []byte("keep"), 0o644)
	// A symlink at the temp path is removed, never written through.
	os.Symlink(victim, dest+".cystemd-tmp")
	if err := writeFileAtomic(FileConfig{Destination: dest}, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "keep" {
		t.Fatalf("victim overwritten: %q", b)
	}
	if b, _ := os.ReadFile(dest); string(b) != "new" {
		t.Fatalf("dest = %q", b)
	}
}
