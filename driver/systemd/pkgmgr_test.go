// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/hashicorp/go-hclog"
)

// exitErr returns a real *exec.ExitError with the given code.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+string(rune('0'+code))).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("no exit error: %v", err)
	}
	return err
}

func TestRPMInstalled(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name      string
		stdout    string
		stderr    string
		err       error
		wantVers  []string
		wantKnown bool
	}{
		{name: "installed", stdout: "1.2.3\n", wantVers: []string{"1.2.3"}, wantKnown: true},
		{name: "duplicates", stdout: "1.2.3\n1.2.4\n", wantVers: []string{"1.2.3", "1.2.4"}, wantKnown: true},
		{name: "silent exit 1 = not installed", err: exitErr(t, 1), wantKnown: true},
		{name: "rpmdb error = inconclusive", stderr: "error: rpmdb open failed", err: exitErr(t, 1)},
		{name: "exit 0 with no version = inconclusive"},
		{name: "missing rpm binary = inconclusive", err: exec.ErrNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotArgs []string
			m := &rpmManager{tool: "dnf", logger: hclog.NewNullLogger(), run: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
				gotArgs = append([]string{name}, args...)
				return []byte(c.stdout), []byte(c.stderr), c.err
			}}
			vers, known := m.Installed(ctx, "myapp")
			if known != c.wantKnown || strings.Join(vers, ",") != strings.Join(c.wantVers, ",") {
				t.Fatalf("got (%v, %v), want (%v, %v)", vers, known, c.wantVers, c.wantKnown)
			}
			if gotArgs[len(gotArgs)-2] != "--" {
				t.Fatalf("package name not separated from options: %v", gotArgs)
			}
		})
	}
}

func TestAPTInstalled(t *testing.T) {
	m := &aptManager{logger: hclog.NewNullLogger(), run: func(context.Context, string, ...string) ([]byte, []byte, error) {
		return []byte("install ok installed\t2.0.1-1\ndeinstall ok config-files\t1.0\n"), nil, nil
	}}
	vers, known := m.Installed(context.Background(), "myapp")
	if !known || len(vers) != 1 || vers[0] != "2.0.1-1" {
		t.Fatalf("got (%v, %v)", vers, known)
	}
}

func TestEnsurePackage(t *testing.T) {
	ctx := context.Background()
	cfg := &Config{MakecacheRepos: []string{"internal"}}
	log := hclog.NewNullLogger()

	t.Run("already installed: no install, no refresh", func(t *testing.T) {
		pm := &fakePackages{known: true, installed: map[string][]string{"myapp": {"1.0", "1.1"}}}
		installed, err := ensurePackage(ctx, pm, cfg, "myapp", "1.1", log, func(string) {})
		if err != nil || installed || len(pm.installs) != 0 || len(pm.refreshed) != 0 {
			t.Fatalf("installed=%v err=%v installs=%v refreshed=%v", installed, err, pm.installs, pm.refreshed)
		}
	})
	t.Run("absent: refresh scoped repos then install", func(t *testing.T) {
		pm := &fakePackages{known: true, installed: map[string][]string{}}
		var events []string
		installed, err := ensurePackage(ctx, pm, cfg, "myapp", "1.2", log, func(m string) { events = append(events, m) })
		if err != nil || !installed {
			t.Fatalf("installed=%v err=%v", installed, err)
		}
		if strings.Join(pm.refreshed, ",") != "internal" || strings.Join(pm.installs, ",") != "myapp-1.2" {
			t.Fatalf("refreshed=%v installs=%v", pm.refreshed, pm.installs)
		}
		if len(events) != 2 || !strings.HasPrefix(events[0], "Installing package myapp 1.2") {
			t.Fatalf("events=%v", events)
		}
	})
	t.Run("inconclusive database: refuse to decide", func(t *testing.T) {
		pm := &fakePackages{known: false}
		if _, err := ensurePackage(ctx, pm, cfg, "myapp", "1.2", log, func(string) {}); err == nil || len(pm.installs) != 0 {
			t.Fatalf("err=%v installs=%v", err, pm.installs)
		}
	})
	t.Run("install failure surfaces", func(t *testing.T) {
		pm := &fakePackages{known: true, installed: map[string][]string{}, installErr: errors.New("no match")}
		if _, err := ensurePackage(ctx, pm, cfg, "myapp", "9", log, func(string) {}); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("no package manager", func(t *testing.T) {
		if _, err := ensurePackage(ctx, nil, cfg, "myapp", "9", log, func(string) {}); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestRPMRefreshIsScopedPerRepo(t *testing.T) {
	var calls []string
	m := &rpmManager{tool: "dnf", logger: hclog.NewNullLogger(), run: func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil, nil
	}}
	m.Refresh(context.Background(), []string{"a", "b"}, defaultMakecacheTimeout)
	want := []string{
		"dnf makecache --disablerepo=* --enablerepo=a",
		"dnf makecache --disablerepo=* --enablerepo=b",
	}
	if strings.Join(calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls=%v", calls)
	}
}
