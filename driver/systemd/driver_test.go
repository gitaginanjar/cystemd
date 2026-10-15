// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// testDriver returns a driver wired to fakes, with runtime paths in temp dirs.
func testDriver(t *testing.T, cfg *Config) (*Driver, *fakeManager, *fakePackages) {
	t.Helper()
	runtimeUnitDir = t.TempDir()
	ownerDir = t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	d := NewDriver(ctx, hclog.NewNullLogger()).(*Driver)
	fm := newFakeManager()
	fp := &fakePackages{known: true, installed: map[string][]string{}}
	d.mgr, d.pkgs = fm, fp
	d.booted = func() bool { return true }
	d.poll = 10 * time.Millisecond
	if cfg == nil {
		cfg = &Config{Enabled: true, AllowedUnits: []string{"myapp*"}, AllowedPackages: []string{"myapp*"}, AllowedFileDestinations: []string{"/**"}}
	}
	d.config = cfg
	return d, fm, fp
}

func taskConfig(t *testing.T, tc TaskConfig) *drivers.TaskConfig {
	t.Helper()
	alloc := t.TempDir()
	cfg := &drivers.TaskConfig{
		ID: "alloc1/myapp/0", AllocID: "alloc-1111-2222", JobID: "myapp", TaskGroupName: "myapp", Name: "myapp",
		AllocDir: alloc,
		Env:      map[string]string{"NOMAD_ALLOC_ID": "alloc-1111-2222", "GREETING": "hello"},
	}
	if err := os.MkdirAll(cfg.TaskDir().SecretsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cfg.EncodeConcreteDriverConfig(&tc); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func waitExit(t *testing.T, d *Driver, id string) *drivers.ExitResult {
	t.Helper()
	ch, err := d.WaitTask(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-ch:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("task did not exit")
		return nil
	}
}

func TestUnitModeLifecycle(t *testing.T) {
	d, fm, fp := testDriver(t, nil)
	fm.addUnit("myapp.service")

	cfg := taskConfig(t, TaskConfig{
		Unit:      "myapp",
		Package:   &PackageConfig{Version: "1.2.3"},
		CPUQuota:  "200m",
		MemoryMax: "512Mi",
	})
	handle, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fp.installs, ",") != "myapp-1.2.3" {
		t.Fatalf("installs = %v", fp.installs)
	}
	calls := strings.Join(fm.Calls(), "|")
	if !strings.Contains(calls, "daemon-reload|start myapp.service") {
		t.Fatalf("calls = %s", calls)
	}

	// Drop-in and task env file written, env file private.
	dropIn, err := os.ReadFile(dropInPath("myapp.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(dropIn), "CPUQuota=20%") || !strings.Contains(string(dropIn), "MemoryMax=536870912") {
		t.Fatalf("drop-in:\n%s", dropIn)
	}
	envPath := filepath.Join(cfg.TaskDir().SecretsDir, taskEnvFileName)
	env, _ := os.ReadFile(envPath)
	if !strings.Contains(string(env), `GREETING="hello"`) {
		t.Fatalf("env file: %s", env)
	}
	if fi, _ := os.Stat(envPath); fi.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode %o", fi.Mode().Perm())
	}

	// Handle round-trips the state.
	var st TaskState
	if err := handle.GetDriverState(&st); err != nil || st.Unit != "myapp.service" || st.Mode != "unit" {
		t.Fatalf("state=%+v err=%v", st, err)
	}

	status, err := d.InspectTask(cfg.ID)
	if err != nil || status.State != drivers.TaskStateRunning || status.DriverAttributes["unit"] != "myapp.service" {
		t.Fatalf("status=%+v err=%v", status, err)
	}

	// A second allocation cannot grab the same unit.
	other := taskConfig(t, TaskConfig{Unit: "myapp"})
	other.ID, other.AllocID = "alloc2/myapp/0", "alloc-3333"
	if _, _, err := d.StartTask(other); err == nil || !strings.Contains(err.Error(), "already managed") {
		t.Fatalf("second allocation: %v", err)
	}

	// Stop → exit result from systemd's ExecMain status.
	if err := d.StopTask(cfg.ID, time.Second, "SIGTERM"); err != nil {
		t.Fatal(err)
	}
	res := waitExit(t, d, cfg.ID)
	if res.Signal != 15 {
		t.Fatalf("exit = %+v", res)
	}

	// Destroy removes the drop-in, the env file and the claim, and reloads.
	reloads := fm.reloads
	if err := d.DestroyTask(cfg.ID, false); err != nil {
		t.Fatal(err)
	}
	if fileExists(dropInPath("myapp.service")) || fileExists(envPath) {
		t.Fatal("drop-in or env file left behind")
	}
	if fm.reloads != reloads+1 {
		t.Fatalf("no daemon-reload after removing the drop-in")
	}
	if _, err := d.InspectTask(cfg.ID); err != drivers.ErrTaskNotFound {
		t.Fatalf("task still known: %v", err)
	}
}

func TestUnitAlreadyRunningIsRestartedUnlessAdopted(t *testing.T) {
	for _, adopt := range []bool{false, true} {
		d, fm, _ := testDriver(t, nil)
		fm.addUnit("myapp.service")
		fm.set("myapp.service", func(u *unitState) { u.ActiveState, u.SubState, u.MainPID = "active", "running", 7 })

		cfg := taskConfig(t, TaskConfig{Unit: "myapp", AdoptRunning: adopt})
		if _, _, err := d.StartTask(cfg); err != nil {
			t.Fatal(err)
		}
		calls := strings.Join(fm.Calls(), "|")
		restarted := strings.Contains(calls, "restart myapp.service")
		if restarted == adopt {
			t.Fatalf("adopt=%v calls=%s", adopt, calls)
		}
		if strings.Contains(calls, "start myapp.service") && !strings.Contains(calls, "restart") {
			t.Fatalf("running unit started again: %s", calls)
		}
	}
}

func TestUnitExitDetectedWhenStoppedOutsideNomad(t *testing.T) {
	d, fm, _ := testDriver(t, nil)
	fm.addUnit("myapp.service")
	cfg := taskConfig(t, TaskConfig{Unit: "myapp"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	// The daemon crashes: systemd marks the unit failed with exit status 3.
	fm.set("myapp.service", func(u *unitState) {
		u.ActiveState, u.SubState, u.Result = "failed", "failed", "exit-code"
		u.ExecMainCode, u.ExecMainStatus = 1, 3
	})
	res := waitExit(t, d, cfg.ID)
	if res.ExitCode != 3 || res.Successful() {
		t.Fatalf("exit = %+v", res)
	}
}

func TestOOMKillReported(t *testing.T) {
	d, fm, _ := testDriver(t, nil)
	fm.addUnit("myapp.service")
	cfg := taskConfig(t, TaskConfig{Unit: "myapp"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	fm.set("myapp.service", func(u *unitState) {
		u.ActiveState, u.Result = "failed", "oom-kill"
		u.ExecMainCode, u.ExecMainStatus = 2, 9
	})
	res := waitExit(t, d, cfg.ID)
	if !res.OOMKilled || res.Signal != 9 {
		t.Fatalf("exit = %+v", res)
	}
}

func TestUnitNotFound(t *testing.T) {
	d, _, _ := testDriver(t, nil)
	cfg := taskConfig(t, TaskConfig{Unit: "myapp"})
	_, _, err := d.StartTask(cfg)
	if err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("err = %v", err)
	}
	// The failed start releases the claim.
	if fileExists(filepath.Join(ownerDir, "myapp.service")) {
		t.Fatal("claim kept after a failed start")
	}
}

func TestStartTaskRejectsDisallowedUnit(t *testing.T) {
	d, fm, _ := testDriver(t, nil)
	fm.addUnit("sshd.service")
	cfg := taskConfig(t, TaskConfig{Unit: "sshd"})
	if _, _, err := d.StartTask(cfg); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("err = %v", err)
	}
	if len(fm.Calls()) != 0 {
		t.Fatalf("systemd touched: %v", fm.Calls())
	}
}

func TestFilesInstalledBeforeStart(t *testing.T) {
	d, fm, _ := testDriver(t, nil)
	fm.addUnit("myapp.service")
	dest := filepath.Join(t.TempDir(), "myapp", "target.config.yml")
	cfg := taskConfig(t, TaskConfig{Unit: "myapp", Files: []FileConfig{{Source: "local/target.config.yml", Destination: dest}}})

	// Template not rendered yet: the start fails before systemd is touched.
	if _, _, err := d.StartTask(cfg); err == nil {
		t.Fatal("start with an unrendered file succeeded")
	}
	if len(fm.Calls()) != 0 {
		t.Fatalf("systemd touched: %v", fm.Calls())
	}

	os.MkdirAll(cfg.TaskDir().LocalDir, 0o755)
	os.WriteFile(filepath.Join(cfg.TaskDir().LocalDir, "target.config.yml"), []byte("port: 8080\n"), 0o644)
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "port: 8080\n" {
		t.Fatalf("dest = %q", b)
	}
}

func TestTransientMode(t *testing.T) {
	d, fm, _ := testDriver(t, &Config{Enabled: true, AllowTransient: true})
	cfg := taskConfig(t, TaskConfig{Command: "/usr/bin/sleep", Args: []string{"60"}, MemoryMax: "64Mi"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	unit := transientUnitName(cfg)
	if !strings.HasPrefix(unit, "cystemd-myapp-myapp-alloc-11") || !strings.HasSuffix(unit, ".service") {
		t.Fatalf("unit name %q", unit)
	}
	props := map[string]dbus.Property{}
	for _, p := range fm.transient[unit] {
		props[p.Name] = p
	}
	for _, name := range []string{"ExecStart", "RemainAfterExit", "Slice", "EnvironmentFiles", "MemoryMax", "Type", "WorkingDirectory"} {
		if _, ok := props[name]; !ok {
			t.Errorf("transient unit lacks %s", name)
		}
	}
	if got := props["Type"].Value.Value(); got != "exec" {
		t.Errorf("Type = %v (systemd 252 supports exec)", got)
	}
	if got := props["MemoryMax"].Value.Value(); got != uint64(64<<20) {
		t.Errorf("MemoryMax = %v", got)
	}

	// The command ends: RemainAfterExit keeps the unit "active (exited)".
	fm.set(unit, func(u *unitState) { u.SubState, u.ExecMainCode, u.ExecMainStatus = "exited", 1, 0 })
	res := waitExit(t, d, cfg.ID)
	if !res.Successful() {
		t.Fatalf("exit = %+v", res)
	}
	if err := d.DestroyTask(cfg.ID, false); err != nil {
		t.Fatal(err)
	}
	calls := strings.Join(fm.Calls(), "|")
	if !strings.Contains(calls, "stop "+unit) || !strings.Contains(calls, "reset-failed "+unit) {
		t.Fatalf("transient unit not cleaned up: %s", calls)
	}
}

func TestTransientStopSendsKillSignalFirst(t *testing.T) {
	d, fm, _ := testDriver(t, &Config{Enabled: true, AllowTransient: true})
	cfg := taskConfig(t, TaskConfig{Command: "/usr/bin/sleep"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	if err := d.StopTask(cfg.ID, 50*time.Millisecond, "SIGINT"); err != nil {
		t.Fatal(err)
	}
	calls := fm.Calls()
	unit := transientUnitName(cfg)
	if !containsString(calls, "kill "+unit+" 2") {
		t.Fatalf("kill_signal not sent first: %v", calls)
	}
}

func TestRecoverTask(t *testing.T) {
	d, fm, _ := testDriver(t, nil)
	fm.addUnit("myapp.service")
	cfg := taskConfig(t, TaskConfig{Unit: "myapp"})
	handle, _, err := d.StartTask(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh driver (client restart) re-attaches from the handle.
	d2, _, _ := testDriver(t, nil)
	d2.mgr = fm
	if err := d2.RecoverTask(handle); err != nil {
		t.Fatal(err)
	}
	status, err := d2.InspectTask(cfg.ID)
	if err != nil || status.State != drivers.TaskStateRunning {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	// Recovering twice is a no-op.
	if err := d2.RecoverTask(handle); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprint(t *testing.T) {
	d, _, _ := testDriver(t, nil)
	d.booted = func() bool { return false }
	fp := d.buildFingerprint(context.Background())
	if fp.Health != drivers.HealthStateUndetected {
		t.Fatalf("health = %v", fp.Health)
	}
	d.config.Enabled = false
	if fp := d.buildFingerprint(context.Background()); fp.HealthDescription != "disabled" {
		t.Fatalf("description = %q", fp.HealthDescription)
	}
}

func TestSignalTask(t *testing.T) {
	d, fm, _ := testDriver(t, nil)
	fm.addUnit("myapp.service")
	cfg := taskConfig(t, TaskConfig{Unit: "myapp"})
	if _, _, err := d.StartTask(cfg); err != nil {
		t.Fatal(err)
	}
	if err := d.SignalTask(cfg.ID, "SIGHUP"); err != nil {
		t.Fatal(err)
	}
	if !containsString(fm.Calls(), "kill myapp.service 1") {
		t.Fatalf("calls = %v", fm.Calls())
	}
	if err := d.SignalTask(cfg.ID, "SIGNOPE"); err == nil {
		t.Fatal("unknown signal accepted")
	}
}
