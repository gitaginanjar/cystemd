// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

// fakeManager is an in-memory systemd: units have a state the tests script,
// and every call is recorded.
type fakeManager struct {
	mu        sync.Mutex
	version   string
	units     map[string]*unitState
	calls     []string
	reloads   int
	transient map[string][]dbus.Property
	startErr  error
}

func newFakeManager() *fakeManager {
	return &fakeManager{
		version:   "252",
		units:     map[string]*unitState{},
		transient: map[string][]dbus.Property{},
	}
}

// addUnit registers an installed (loaded), inactive unit.
func (f *fakeManager) addUnit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.units[name] = &unitState{LoadState: "loaded", ActiveState: "inactive", SubState: "dead", TimeoutStopUS: 90_000_000}
}

func (f *fakeManager) set(name string, mut func(*unitState)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mut(f.units[name])
}

func (f *fakeManager) record(format string, args ...any) {
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeManager) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeManager) Version(context.Context) (string, error) { return f.version, nil }

func (f *fakeManager) UnitState(_ context.Context, unit string) (unitState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.units[unit]
	if !ok {
		return unitState{LoadState: "not-found", ActiveState: "inactive", SubState: "dead"}, nil
	}
	return *u, nil
}

func (f *fakeManager) activate(unit string) {
	u := f.units[unit]
	u.ActiveState, u.SubState, u.Result = "active", "running", "success"
	u.MainPID = 4242
	u.InvocationID = fmt.Sprintf("inv-%d", len(f.calls))
	u.ExecMainCode, u.ExecMainStatus = 0, 0
}

func (f *fakeManager) Start(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start %s", unit)
	if f.startErr != nil {
		return f.startErr
	}
	if _, ok := f.units[unit]; !ok {
		return fmt.Errorf("unit %s not found", unit)
	}
	f.activate(unit)
	return nil
}

func (f *fakeManager) Restart(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("restart %s", unit)
	f.activate(unit)
	return nil
}

func (f *fakeManager) Stop(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("stop %s", unit)
	if u, ok := f.units[unit]; ok && u.ActiveState != "inactive" {
		u.ActiveState, u.SubState = "inactive", "dead"
		u.ExecMainCode, u.ExecMainStatus = 2, int32(syscall.SIGTERM)
		u.MainPID = 0
	}
	return nil
}

func (f *fakeManager) StartTransient(_ context.Context, unit string, props []dbus.Property) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start-transient %s", unit)
	f.transient[unit] = props
	f.units[unit] = &unitState{LoadState: "loaded", TimeoutStopUS: 5_000_000}
	f.activate(unit)
	return nil
}

func (f *fakeManager) Kill(_ context.Context, unit string, signal syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("kill %s %d", unit, signal)
	return nil
}

func (f *fakeManager) ResetFailed(_ context.Context, unit string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("reset-failed %s", unit)
	return nil
}

func (f *fakeManager) DaemonReload(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reloads++
	f.record("daemon-reload")
	return nil
}

func (f *fakeManager) Close() {}

var _ systemdManager = (*fakeManager)(nil)

// fakePackages is an in-memory package database.
type fakePackages struct {
	mu         sync.Mutex
	installed  map[string][]string
	known      bool
	installs   []string
	refreshed  []string
	installErr error
}

func (p *fakePackages) Name() string { return "dnf" }

func (p *fakePackages) Installed(_ context.Context, pkg string) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.known {
		return nil, false
	}
	return p.installed[pkg], true
}

func (p *fakePackages) Install(_ context.Context, pkg, version string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.installs = append(p.installs, pkg+"-"+version)
	if p.installErr != nil {
		return "boom", p.installErr
	}
	p.installed[pkg] = []string{version}
	return "Complete!", nil
}

func (p *fakePackages) Refresh(_ context.Context, repos []string, _ time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshed = append(p.refreshed, repos...)
}

var _ packageManager = (*fakePackages)(nil)
