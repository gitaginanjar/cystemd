// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

// fileSyncer keeps `file` blocks' sources (rendered by Nomad templates in the
// task directory) in sync with their host destinations. It remembers what it
// wrote, so an unchanged source costs one read per tick and never a write.
type fileSyncer struct {
	taskDir        string
	sharedAllocDir string
	files          []FileConfig
	written        map[string][32]byte // destination → hash of the content last written
}

func newFileSyncer(taskDir, sharedAllocDir string, files []FileConfig) *fileSyncer {
	return &fileSyncer{taskDir: taskDir, sharedAllocDir: sharedAllocDir, files: files, written: map[string][32]byte{}}
}

// syncResult is one file the last sync changed or could not write.
type syncResult struct {
	Destination string
	OnChange    string
	Err         error
}

// sync copies every changed source. strict makes a missing source an error
// (task start: a template that never rendered must fail the start rather than
// run the daemon on a stale file); later ticks skip it.
func (s *fileSyncer) sync(strict bool) (changed []syncResult, err error) {
	var errs []error
	for _, f := range s.files {
		src := taskPath(s.taskDir, s.sharedAllocDir, f.Source)
		content, rerr := os.ReadFile(src)
		if rerr != nil {
			if strict || !errors.Is(rerr, fs.ErrNotExist) {
				errs = append(errs, fmt.Errorf("read %s: %w", f.Source, rerr))
			}
			continue
		}
		sum := sha256.Sum256(content)
		if prev, ok := s.written[f.Destination]; ok && prev == sum {
			continue
		}
		// First sight (or driver restart): skip the write when the host
		// already holds identical bytes, so the mtime stays stable.
		if existing, eerr := os.ReadFile(f.Destination); eerr == nil && bytes.Equal(existing, content) {
			if aerr := applyOwnership(f); aerr != nil {
				errs = append(errs, aerr)
				continue
			}
			s.written[f.Destination] = sum
			continue
		}
		if werr := writeFileAtomic(f, content); werr != nil {
			errs = append(errs, werr)
			changed = append(changed, syncResult{Destination: f.Destination, OnChange: f.OnChange, Err: werr})
			continue
		}
		s.written[f.Destination] = sum
		changed = append(changed, syncResult{Destination: f.Destination, OnChange: f.OnChange})
	}
	return changed, errors.Join(errs...)
}

// writeFileAtomic replaces f.Destination with content via a temporary file
// and rename. The temporary file is created O_EXCL after removing a stale
// one, so a planted symlink fails the write instead of being written through.
func writeFileAtomic(f FileConfig, content []byte) error {
	mode := os.FileMode(0o644)
	if f.Mode != "" {
		m, err := parseMode(f.Mode)
		if err != nil {
			return err
		}
		mode = os.FileMode(m)
	}
	dir := filepath.Dir(f.Destination)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp := f.Destination + ".cystemd-tmp"
	_ = os.Remove(tmp)
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return fmt.Errorf("write %s: %w", f.Destination, err)
	}
	if _, err := fh.Write(content); err != nil {
		fh.Close()
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", f.Destination, err)
	}
	if err := fh.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", f.Destination, err)
	}
	// Mode again: the umask applied at create.
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("chmod %s: %w", f.Destination, err)
	}
	if err := chownTo(tmp, f); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, f.Destination); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s: %w", f.Destination, err)
	}
	return nil
}

// applyOwnership enforces mode and owner on an existing destination.
func applyOwnership(f FileConfig) error {
	if f.Mode != "" {
		m, err := parseMode(f.Mode)
		if err != nil {
			return err
		}
		if fi, err := os.Lstat(f.Destination); err == nil && fi.Mode().Perm() != os.FileMode(m).Perm() {
			if err := os.Chmod(f.Destination, os.FileMode(m)); err != nil {
				return fmt.Errorf("chmod %s: %w", f.Destination, err)
			}
		}
	}
	return chownTo(f.Destination, f)
}

// chownTo applies f.Owner / f.Group (names or numeric IDs) to path; -1 keeps
// the current value.
func chownTo(path string, f FileConfig) error {
	if f.Owner == "" && f.Group == "" {
		return nil
	}
	uid, gid := -1, -1
	if f.Owner != "" {
		id, err := lookupID(f.Owner, func(n string) (string, error) {
			u, err := user.Lookup(n)
			if err != nil {
				return "", err
			}
			return u.Uid, nil
		})
		if err != nil {
			return fmt.Errorf("owner %q: %w", f.Owner, err)
		}
		uid = id
	}
	if f.Group != "" {
		id, err := lookupID(f.Group, func(n string) (string, error) {
			g, err := user.LookupGroup(n)
			if err != nil {
				return "", err
			}
			return g.Gid, nil
		})
		if err != nil {
			return fmt.Errorf("group %q: %w", f.Group, err)
		}
		gid = id
	}
	if err := os.Lchown(path, uid, gid); err != nil {
		return fmt.Errorf("chown %s: %w", path, err)
	}
	return nil
}

func lookupID(name string, lookup func(string) (string, error)) (int, error) {
	if id, err := strconv.Atoi(name); err == nil {
		return id, nil
	}
	s, err := lookup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(s)
}
