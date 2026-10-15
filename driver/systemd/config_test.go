// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"strings"
	"testing"
)

func policy() *Config {
	return &Config{
		Enabled:                 true,
		AllowedUnits:            []string{"myapp*.service"},
		AllowedPackages:         []string{"myapp*"},
		AllowedFileDestinations: []string{"/opt/**"},
	}
}

func TestTaskConfigValidate(t *testing.T) {
	cases := []struct {
		name    string
		tc      TaskConfig
		cfg     *Config
		wantErr string
	}{
		{name: "unit ok", tc: TaskConfig{Unit: "myapp"}},
		{name: "unit with suffix ok", tc: TaskConfig{Unit: "myapp-api.service"}},
		{name: "neither mode", tc: TaskConfig{}, wantErr: "one of unit or command"},
		{name: "both modes", tc: TaskConfig{Unit: "myapp", Command: "/bin/true"}, wantErr: "not both"},
		{name: "unit not allowed", tc: TaskConfig{Unit: "sshd"}, wantErr: "not allowed on this node"},
		{name: "non-service unit", tc: TaskConfig{Unit: "myapp.socket"}, wantErr: "only .service"},
		{name: "invalid unit chars", tc: TaskConfig{Unit: "my app"}, wantErr: "invalid unit name"},
		{name: "path traversal unit", tc: TaskConfig{Unit: "../myapp.service"}, wantErr: "invalid unit name"},
		{name: "args in unit mode", tc: TaskConfig{Unit: "myapp", Args: []string{"x"}}, wantErr: "transient mode only"},
		{name: "transient disabled", tc: TaskConfig{Command: "/bin/sleep"}, wantErr: "transient mode is disabled"},
		{
			name: "transient enabled",
			tc:   TaskConfig{Command: "/bin/sleep", Args: []string{"1"}},
			cfg:  &Config{Enabled: true, AllowTransient: true},
		},
		{
			name:    "transient relative work dir",
			tc:      TaskConfig{Command: "/bin/sleep", WorkDir: "tmp"},
			cfg:     &Config{Enabled: true, AllowTransient: true},
			wantErr: "work_dir",
		},
		{name: "package ok, default name", tc: TaskConfig{Unit: "myapp", Package: &PackageConfig{Version: "1.2.3"}}},
		{name: "package not allowed", tc: TaskConfig{Unit: "myapp", Package: &PackageConfig{Name: "kernel", Version: "1"}}, wantErr: "not allowed on this node"},
		{name: "package option injection", tc: TaskConfig{Unit: "myapp", Package: &PackageConfig{Name: "-y", Version: "1"}}, wantErr: "invalid"},
		{name: "package version injection", tc: TaskConfig{Unit: "myapp", Package: &PackageConfig{Version: "--setopt=x"}}, wantErr: "version"},
		{
			name: "file ok",
			tc:   TaskConfig{Unit: "myapp", Files: []FileConfig{{Source: "local/app.yml", Destination: "/opt/myapp/app.yml", Mode: "0640"}}},
		},
		{
			name:    "file escapes task dir",
			tc:      TaskConfig{Unit: "myapp", Files: []FileConfig{{Source: "../../etc/shadow", Destination: "/opt/x"}}},
			wantErr: "inside the task directory",
		},
		{
			name:    "file destination not allowed",
			tc:      TaskConfig{Unit: "myapp", Files: []FileConfig{{Source: "local/x", Destination: "/etc/passwd"}}},
			wantErr: "not allowed on this node",
		},
		{
			name:    "file destination not clean",
			tc:      TaskConfig{Unit: "myapp", Files: []FileConfig{{Source: "local/x", Destination: "/opt/../etc/passwd"}}},
			wantErr: "clean path",
		},
		{
			name:    "file bad mode",
			tc:      TaskConfig{Unit: "myapp", Files: []FileConfig{{Source: "local/x", Destination: "/opt/x", Mode: "rw"}}},
			wantErr: "octal",
		},
		{name: "cpu quota", tc: TaskConfig{Unit: "myapp", CPUQuota: "200m", MemoryMax: "1Gi"}},
		{name: "bad cpu quota", tc: TaskConfig{Unit: "myapp", CPUQuota: "-1"}, wantErr: "cpu_quota"},
		{name: "bad memory", tc: TaskConfig{Unit: "myapp", MemoryMax: "lots"}, wantErr: "memory_max"},
		{name: "allowed property", tc: TaskConfig{Unit: "myapp", Properties: []map[string]string{{"LimitNOFILE": "65536"}}}},
		{name: "forbidden property", tc: TaskConfig{Unit: "myapp", Properties: []map[string]string{{"ExecStart": "/bin/sh"}}}, wantErr: "not allowed"},
		{name: "multiline property", tc: TaskConfig{Unit: "myapp", Properties: []map[string]string{{"Restart": "always\nExecStart=/bin/sh"}}}, wantErr: "single line"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := c.cfg
			if cfg == nil {
				cfg = policy()
			}
			err := c.tc.validate(cfg)
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("expected error containing %q", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

func TestEmptyPolicyDeniesEverything(t *testing.T) {
	empty := &Config{Enabled: true}
	for _, tc := range []TaskConfig{
		{Unit: "anything.service"},
		{Command: "/bin/true"},
	} {
		if err := tc.validate(empty); err == nil {
			t.Fatalf("%+v allowed under an empty policy", tc)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	ok := Config{PackageManager: "dnf", InstallTimeout: "5m", AllowedFileDestinations: []string{"/opt/**"}}
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []Config{
		{PackageManager: "pacman"},
		{InstallTimeout: "soon"},
		{MakecacheTimeout: "-1s"},
		{AllowedFileDestinations: []string{"opt/**"}},
		{AllowedUnits: []string{"[bad"}},
		{TransientSlice: "tasks"},
	} {
		if err := bad.validate(); err == nil {
			t.Fatalf("%+v: expected an error", bad)
		}
	}
}

func TestNormalizeUnit(t *testing.T) {
	for in, want := range map[string]string{
		"myapp":          "myapp.service",
		"myapp.service":  "myapp.service",
		"nginx@80":       "nginx@80.service",
		"my.app.service": "my.app.service",
	} {
		if got := normalizeUnit(in); got != want {
			t.Errorf("normalizeUnit(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMatchAnyPath(t *testing.T) {
	pats := []string{"/opt/**", "/etc/myapp/*.conf"}
	for p, want := range map[string]bool{
		"/opt/myapp/app.yml":    true,
		"/opt/a/b/c":            true,
		"/opt":                  false,
		"/optx/a":               false,
		"/etc/myapp/x.conf":     true,
		"/etc/myapp/sub/x.conf": false,
		"/etc/passwd":           false,
	} {
		if got := matchAnyPath(pats, p); got != want {
			t.Errorf("matchAnyPath(%q) = %v, want %v", p, got, want)
		}
	}
}
