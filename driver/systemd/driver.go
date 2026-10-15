// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/hashicorp/consul-template/signals"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/drivers/shared/eventer"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/plugins/drivers"
	"github.com/hashicorp/nomad/plugins/drivers/fsisolation"
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
	pstructs "github.com/hashicorp/nomad/plugins/shared/structs"
)

// Version is the driver plugin version reported to Nomad.
var Version = "0.1.0"

const fingerprintPeriod = 30 * time.Second

var (
	pluginInfo = &base.PluginInfoResponse{
		Type:              base.PluginTypeDriver,
		PluginApiVersions: []string{drivers.ApiVersion010},
		PluginVersion:     Version,
		Name:              PluginName,
	}

	capabilities = &drivers.Capabilities{
		SendSignals:       true,
		Exec:              false,
		FSIsolation:       fsisolation.None,
		NetIsolationModes: []drivers.NetIsolationMode{drivers.NetIsolationModeHost},
		MountConfigs:      drivers.MountConfigSupportNone,
	}
)

// Driver runs Nomad tasks as systemd units. See the package documentation.
type Driver struct {
	eventer *eventer.Eventer
	config  *Config
	tasks   *taskStore
	ctx     context.Context
	logger  hclog.Logger

	mgr     systemdManager
	pkgs    packageManager
	run     commandRunner
	booted  func() bool
	poll    time.Duration
	perCore float64 // MHz of one core, for CPU ticks in stats
}

// NewDriver returns the systemd driver plugin.
func NewDriver(ctx context.Context, logger hclog.Logger) drivers.DriverPlugin {
	logger = logger.Named(PluginName)
	return &Driver{
		eventer: eventer.NewEventer(ctx, logger),
		config:  &Config{Enabled: true},
		tasks:   newTaskStore(),
		ctx:     ctx,
		logger:  logger,
		mgr:     newDBusManager(logger),
		run:     execRunner,
		booted:  systemdBooted,
		poll:    defaultPollInterval,
	}
}

func (d *Driver) PluginInfo() (*base.PluginInfoResponse, error) { return pluginInfo, nil }

func (d *Driver) ConfigSchema() (*hclspec.Spec, error) { return configSpec, nil }

func (d *Driver) TaskConfigSchema() (*hclspec.Spec, error) { return taskConfigSpec, nil }

func (d *Driver) Capabilities() (*drivers.Capabilities, error) { return capabilities, nil }

func (d *Driver) SetConfig(cfg *base.Config) error {
	config := Config{Enabled: true}
	if len(cfg.PluginConfig) != 0 {
		if err := base.MsgPackDecode(cfg.PluginConfig, &config); err != nil {
			return err
		}
	}
	if err := config.validate(); err != nil {
		return fmt.Errorf("invalid systemd driver configuration: %w", err)
	}
	d.config = &config
	d.pkgs = detectPackageManager(config.PackageManager, d.run, d.logger)
	if cfg.AgentConfig != nil {
		compute := cfg.AgentConfig.Compute()
		if compute.NumCores > 0 {
			d.perCore = float64(compute.TotalCompute) / float64(compute.NumCores)
		}
	}
	return nil
}

func (d *Driver) Fingerprint(ctx context.Context) (<-chan *drivers.Fingerprint, error) {
	ch := make(chan *drivers.Fingerprint)
	go func() {
		defer close(ch)
		timer := time.NewTimer(0)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.ctx.Done():
				return
			case <-timer.C:
				timer.Reset(fingerprintPeriod)
				select {
				case ch <- d.buildFingerprint(ctx):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return ch, nil
}

func (d *Driver) buildFingerprint(ctx context.Context) *drivers.Fingerprint {
	fp := &drivers.Fingerprint{Attributes: map[string]*pstructs.Attribute{}}
	switch {
	case !d.config.Enabled:
		fp.Health, fp.HealthDescription = drivers.HealthStateUndetected, "disabled"
		return fp
	case !d.booted():
		fp.Health, fp.HealthDescription = drivers.HealthStateUndetected, "systemd is not the init system"
		return fp
	case os.Geteuid() != 0:
		fp.Health, fp.HealthDescription = drivers.HealthStateUndetected, "the systemd driver must run as root"
		return fp
	}
	version, err := d.mgr.Version(ctx)
	if err != nil {
		fp.Health, fp.HealthDescription = drivers.HealthStateUnhealthy, fmt.Sprintf("systemd unreachable: %v", err)
		return fp
	}
	fp.Health, fp.HealthDescription = drivers.HealthStateHealthy, drivers.DriverHealthy
	fp.Attributes["driver.systemd"] = pstructs.NewBoolAttribute(true)
	fp.Attributes["driver.systemd.version"] = pstructs.NewStringAttribute(version)
	fp.Attributes["driver.systemd.transient"] = pstructs.NewBoolAttribute(d.config.AllowTransient)
	if d.pkgs != nil {
		fp.Attributes["driver.systemd.package_manager"] = pstructs.NewStringAttribute(d.pkgs.Name())
	}
	return fp
}

func (d *Driver) emit(cfg *drivers.TaskConfig) func(string, map[string]string) {
	return func(msg string, annotations map[string]string) {
		_ = d.eventer.EmitEvent(&drivers.TaskEvent{
			TaskID:      cfg.ID,
			TaskName:    cfg.Name,
			AllocID:     cfg.AllocID,
			Timestamp:   time.Now(),
			Message:     msg,
			Annotations: annotations,
		})
	}
}

func (d *Driver) StartTask(cfg *drivers.TaskConfig) (*drivers.TaskHandle, *drivers.DriverNetwork, error) {
	if !d.config.Enabled {
		return nil, nil, errors.New("the systemd driver is disabled on this node")
	}
	if _, ok := d.tasks.Get(cfg.ID); ok {
		return nil, nil, fmt.Errorf("task with ID %q already started", cfg.ID)
	}
	var tc TaskConfig
	if err := cfg.DecodeDriverConfig(&tc); err != nil {
		return nil, nil, fmt.Errorf("failed to decode driver config: %w", err)
	}
	if err := tc.validate(d.config); err != nil {
		return nil, nil, fmt.Errorf("invalid systemd task config: %w", err)
	}

	logger := d.logger.With("alloc_id", cfg.AllocID, "task", cfg.Name)
	emit := d.emit(cfg)
	ctx, cancel := context.WithTimeout(d.ctx, d.config.installTimeout()+2*systemdJobTimeout)
	defer cancel()

	state := &TaskState{TaskConfig: cfg, Mode: string(tc.mode()), StartedAt: time.Now().Round(time.Millisecond), Files: tc.Files}
	taskDir := cfg.TaskDir().Dir

	// Files first: the unit must start on the rendered config, never on a
	// stale copy from the previous allocation.
	var syncer *fileSyncer
	if len(tc.Files) > 0 {
		syncer = newFileSyncer(taskDir, tc.Files)
		if _, err := syncer.sync(true); err != nil {
			return nil, nil, fmt.Errorf("install files: %w", err)
		}
	}

	var err error
	switch tc.mode() {
	case modeUnit:
		state.Unit = normalizeUnit(tc.Unit)
		err = d.startUnit(ctx, cfg, &tc, state, logger, emit)
	case modeTransient:
		state.Unit = transientUnitName(cfg)
		err = d.startTransient(ctx, cfg, &tc, state)
	}
	if err != nil {
		return nil, nil, err
	}

	h := d.newHandle(cfg, state, syncer, logger)
	handle := drivers.NewTaskHandle(taskHandleVersion)
	handle.Config = cfg
	if err := handle.SetDriverState(state); err != nil {
		h.cancel()
		return nil, nil, fmt.Errorf("failed to set driver state: %w", err)
	}
	d.tasks.Set(cfg.ID, h)
	go h.run()
	return handle, nil, nil
}

// startUnit runs unit mode: claim the unit, install the package, write the
// task environment and drop-in, then (re)start the unit.
func (d *Driver) startUnit(ctx context.Context, cfg *drivers.TaskConfig, tc *TaskConfig, state *TaskState, logger hclog.Logger, emit func(string, map[string]string)) error {
	unit := state.Unit
	if err := claimUnit(unit, cfg.AllocID, d.tasks.liveAlloc); err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			releaseUnit(unit, cfg.AllocID)
		}
	}()

	reload := false
	if tc.Package != nil {
		installed, err := ensurePackage(ctx, d.pkgs, d.config, tc.packageName(), tc.Package.Version, logger,
			func(msg string) { emit(msg, map[string]string{"package": tc.packageName()}) })
		if err != nil {
			return err
		}
		// A package may add or replace the unit file; not every package
		// daemon-reloads in its scriptlets.
		reload = reload || installed
	}

	secretsDir := cfg.TaskDir().SecretsDir
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		return err
	}
	state.EnvFile = filepath.Join(secretsDir, taskEnvFileName)
	envContent, skipped := renderEnvFile(cfg.Env)
	if len(skipped) > 0 {
		logger.Warn("environment variables with newlines are not passed to the unit", "keys", skipped)
	}
	if _, err := writeOwned(state.EnvFile, envContent, 0o600); err != nil {
		return fmt.Errorf("write task environment: %w", err)
	}

	dropIn, err := renderDropIn(cfg, tc, state.EnvFile)
	if err != nil {
		return err
	}
	state.DropIn = dropInPath(unit)
	changed, err := writeOwned(state.DropIn, dropIn, 0o644)
	if err != nil {
		return fmt.Errorf("write drop-in: %w", err)
	}
	if changed || reload {
		if err := d.mgr.DaemonReload(ctx); err != nil {
			return fmt.Errorf("daemon-reload: %w", err)
		}
	}

	st, err := d.mgr.UnitState(ctx, unit)
	if err != nil {
		return err
	}
	if st.LoadState != "loaded" {
		return fmt.Errorf("unit %s: %w (%s)", unit, errNotLoaded, st.LoadState)
	}
	switch {
	case st.running() && tc.AdoptRunning:
		emit(fmt.Sprintf("Adopted running unit %s", unit), map[string]string{"unit": unit})
	case st.running():
		// Already up (started at boot or by the previous allocation): restart
		// so the process picks up this task's environment and limits.
		emit(fmt.Sprintf("Restarting unit %s to apply the task configuration", unit), map[string]string{"unit": unit})
		if err := d.mgr.Restart(ctx, unit); err != nil {
			return err
		}
	default:
		if err := d.mgr.Start(ctx, unit); err != nil {
			return err
		}
	}
	ok = true
	return nil
}

// startTransient runs transient mode: a transient service with
// RemainAfterExit, so the exit status survives until the driver stops it.
func (d *Driver) startTransient(ctx context.Context, cfg *drivers.TaskConfig, tc *TaskConfig, state *TaskState) error {
	unit := state.Unit
	secretsDir := cfg.TaskDir().SecretsDir
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		return err
	}
	state.EnvFile = filepath.Join(secretsDir, taskEnvFileName)
	envContent, _ := renderEnvFile(cfg.Env)
	if _, err := writeOwned(state.EnvFile, envContent, 0o600); err != nil {
		return fmt.Errorf("write task environment: %w", err)
	}
	serviceType := "simple"
	if v, err := d.mgr.Version(ctx); err == nil {
		if n, err := strconv.Atoi(v); err == nil && n >= 240 {
			serviceType = "exec"
		}
	}
	props, err := transientProperties(cfg, tc, d.config, state.EnvFile, serviceType)
	if err != nil {
		return err
	}
	// A crashed client may have left the previous unit behind.
	if st, err := d.mgr.UnitState(ctx, unit); err == nil && st.LoadState == "loaded" {
		_ = d.mgr.Stop(ctx, unit)
		_ = d.mgr.ResetFailed(ctx, unit)
	}
	return d.mgr.StartTransient(ctx, unit, props)
}

// transientProperties builds the properties of a transient task unit.
// serviceType is "exec" (start fails when the command cannot be executed) on
// systemd >= 240, else "simple".
func transientProperties(cfg *drivers.TaskConfig, tc *TaskConfig, pc *Config, envFile, serviceType string) ([]dbus.Property, error) {
	command := tc.Command
	if !filepath.IsAbs(command) {
		command = filepath.Join(cfg.TaskDir().Dir, command)
	}
	workDir := tc.WorkDir
	if workDir == "" {
		workDir = cfg.TaskDir().Dir
	}
	envFiles := []envFileEntry{{Path: envFile, IgnoreErrors: false}}
	for _, e := range tc.EnvironmentFiles {
		envFiles = append(envFiles, envFileEntry{Path: filepath.Join(cfg.TaskDir().Dir, filepath.Clean(e)), IgnoreErrors: true})
	}
	props := []dbus.Property{
		dbus.PropDescription(fmt.Sprintf("Nomad task %s/%s/%s (allocation %s)", cfg.JobID, cfg.TaskGroupName, cfg.Name, cfg.AllocID)),
		dbus.PropExecStart(append([]string{command}, tc.Args...), false),
		dbus.PropType(serviceType),
		dbus.PropRemainAfterExit(true),
		dbus.PropSlice(pc.transientSlice()),
		{Name: "WorkingDirectory", Value: godbusVariant(workDir)},
		{Name: "EnvironmentFiles", Value: godbusVariant(envFiles)},
		// The final SIGKILL grace after StopTask's own signal and kill_timeout.
		{Name: "TimeoutStopUSec", Value: godbusVariant(uint64(5 * time.Second / time.Microsecond))},
	}
	if cfg.User != "" {
		props = append(props, dbus.Property{Name: "User", Value: godbusVariant(cfg.User)})
	}
	if cfg.StdoutPath != "" {
		props = append(props, dbus.Property{Name: "StandardOutputFile", Value: godbusVariant(cfg.StdoutPath)})
	}
	if cfg.StderrPath != "" {
		props = append(props, dbus.Property{Name: "StandardErrorFile", Value: godbusVariant(cfg.StderrPath)})
	}
	if tc.CPUQuota != "" {
		q, err := cpuQuotaPerSecUSec(tc.CPUQuota)
		if err != nil {
			return nil, err
		}
		props = append(props, dbus.Property{Name: "CPUQuotaPerSecUSec", Value: godbusVariant(q)})
	}
	memMax := uint64(0)
	if tc.MemoryMax != "" {
		m, err := memoryBytes(tc.MemoryMax)
		if err != nil {
			return nil, err
		}
		memMax = m
	} else if lr := linuxResources(cfg); lr != nil && lr.MemoryLimitBytes > 0 {
		memMax = uint64(lr.MemoryLimitBytes)
	}
	if memMax > 0 {
		props = append(props, dbus.Property{Name: "MemoryMax", Value: godbusVariant(memMax)})
	}
	if lr := linuxResources(cfg); lr != nil && lr.CPUShares > 0 {
		props = append(props, dbus.Property{Name: "CPUWeight", Value: godbusVariant(cpuWeight(lr.CPUShares))})
	}
	return props, nil
}

// unitNameUnsafe matches characters not valid in a unit name.
var unitNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9:_.\\-]`)

// transientUnitName is "cystemd-<job>-<task>-<alloc8>.service", sanitised and
// bounded so it is a valid, unique unit name.
func transientUnitName(cfg *drivers.TaskConfig) string {
	alloc := cfg.AllocID
	if len(alloc) > 8 {
		alloc = alloc[:8]
	}
	base := unitNameUnsafe.ReplaceAllString(fmt.Sprintf("%s-%s", cfg.JobID, cfg.Name), "_")
	if len(base) > 200 {
		base = base[:200]
	}
	return fmt.Sprintf("cystemd-%s-%s.service", base, alloc)
}

func (d *Driver) newHandle(cfg *drivers.TaskConfig, state *TaskState, syncer *fileSyncer, logger hclog.Logger) *taskHandle {
	ctx, cancel := context.WithCancel(d.ctx)
	h := &taskHandle{
		logger:     logger,
		mgr:        d.mgr,
		taskConfig: cfg,
		mode:       taskMode(state.Mode),
		unit:       state.Unit,
		startedAt:  state.StartedAt,
		files:      syncer,
		emit:       d.emit(cfg),
		poll:       d.poll,
		procState:  drivers.TaskStateRunning,
		ctx:        ctx,
		cancel:     cancel,
		doneCh:     make(chan struct{}),
	}
	if h.mode == modeUnit {
		h.journal = followJournal(state.Unit, cfg.StdoutPath, state.StartedAt, logger)
	}
	return h
}

func (d *Driver) RecoverTask(handle *drivers.TaskHandle) error {
	if handle == nil {
		return errors.New("handle cannot be nil")
	}
	if _, ok := d.tasks.Get(handle.Config.ID); ok {
		return nil
	}
	var state TaskState
	if err := handle.GetDriverState(&state); err != nil {
		return fmt.Errorf("failed to decode task state from handle: %w", err)
	}
	if state.TaskConfig == nil || state.Unit == "" {
		return errors.New("task state carries no unit")
	}
	logger := d.logger.With("alloc_id", state.TaskConfig.AllocID, "task", state.TaskConfig.Name)
	var syncer *fileSyncer
	if len(state.Files) > 0 {
		syncer = newFileSyncer(state.TaskConfig.TaskDir().Dir, state.Files)
	}
	// Logs resume from now: the client was down, re-reading would duplicate.
	recovered := state
	recovered.StartedAt = time.Now()
	h := d.newHandle(state.TaskConfig, &recovered, syncer, logger)
	h.startedAt = state.StartedAt
	d.tasks.Set(state.TaskConfig.ID, h)
	go h.run()
	logger.Info("recovered task", "unit", state.Unit, "mode", state.Mode)
	return nil
}

func (d *Driver) WaitTask(ctx context.Context, taskID string) (<-chan *drivers.ExitResult, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}
	ch := make(chan *drivers.ExitResult)
	go func() {
		defer close(ch)
		select {
		case <-h.doneCh:
		case <-ctx.Done():
			return
		case <-d.ctx.Done():
			return
		}
		h.mu.RLock()
		res := h.exitResult
		h.mu.RUnlock()
		if res == nil {
			// Handle cancelled (destroy) before the unit ended.
			res = &drivers.ExitResult{ExitCode: -1, Err: errors.New("task destroyed")}
		}
		select {
		case ch <- res:
		case <-ctx.Done():
		case <-d.ctx.Done():
		}
	}()
	return ch, nil
}

// StopTask stops the unit. Unit mode follows the unit's own stop policy
// (KillSignal, TimeoutStopSec) — the daemon's packaging knows how it must be
// stopped. Transient mode sends signal (the job's kill_signal) first, waits
// up to timeout (kill_timeout), then stops the unit.
func (d *Driver) StopTask(taskID string, timeout time.Duration, signal string) error {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}
	ctx := d.ctx
	if h.mode == modeTransient && h.IsRunning() {
		sig := syscall.SIGTERM
		if s, ok := signals.SignalLookup[signal]; ok {
			if ss, ok := s.(syscall.Signal); ok {
				sig = ss
			}
		}
		if err := d.mgr.Kill(ctx, h.unit, sig); err == nil {
			select {
			case <-h.doneCh:
			case <-time.After(timeout):
			}
		}
	}
	if err := d.mgr.Stop(ctx, h.unit); err != nil && h.IsRunning() {
		return fmt.Errorf("stop %s: %w", h.unit, err)
	}
	select {
	case <-h.doneCh:
	case <-time.After(timeout + 10*time.Second):
		h.logger.Warn("unit did not report its exit after stop", "unit", h.unit)
	}
	return nil
}

// DestroyTask releases everything the task created on the host: the drop-in
// (then daemon-reload), the env file, the unit claim, and for transient mode
// the unit itself. Copied files are left in place: they are the daemon's
// configuration and removing them could break a later manual start.
func (d *Driver) DestroyTask(taskID string, force bool) error {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}
	if h.IsRunning() && !force {
		return errors.New("cannot destroy running task")
	}
	ctx := d.ctx
	if h.IsRunning() {
		_ = d.mgr.Stop(ctx, h.unit)
	}
	h.cancel()
	<-h.doneCh

	if h.mode == modeTransient {
		_ = d.mgr.Stop(ctx, h.unit) // RemainAfterExit keeps it active until stopped
		_ = d.mgr.ResetFailed(ctx, h.unit)
	} else {
		if p := dropInPath(h.unit); fileExists(p) {
			if err := os.Remove(p); err == nil {
				_ = os.Remove(filepath.Dir(p)) // only succeeds when empty
				if err := d.mgr.DaemonReload(ctx); err != nil {
					h.logger.Warn("daemon-reload after removing drop-in failed", "error", err)
				}
			}
		}
		releaseUnit(h.unit, h.taskConfig.AllocID)
	}
	_ = os.Remove(filepath.Join(h.taskConfig.TaskDir().SecretsDir, taskEnvFileName))
	d.tasks.Delete(taskID)
	return nil
}

func (d *Driver) InspectTask(taskID string) (*drivers.TaskStatus, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}
	return h.TaskStatus(), nil
}

func (d *Driver) TaskStats(ctx context.Context, taskID string, interval time.Duration) (<-chan *drivers.TaskResourceUsage, error) {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return nil, drivers.ErrTaskNotFound
	}
	ch := make(chan *drivers.TaskResourceUsage)
	go h.stats(ctx, interval, d.perCore, ch)
	return ch, nil
}

func (d *Driver) TaskEvents(ctx context.Context) (<-chan *drivers.TaskEvent, error) {
	return d.eventer.TaskEvents(ctx)
}

func (d *Driver) SignalTask(taskID string, signal string) error {
	h, ok := d.tasks.Get(taskID)
	if !ok {
		return drivers.ErrTaskNotFound
	}
	s, ok := signals.SignalLookup[strings.ToUpper(signal)]
	if !ok {
		return fmt.Errorf("unknown signal %q", signal)
	}
	sig, ok := s.(syscall.Signal)
	if !ok {
		return fmt.Errorf("unsupported signal %q", signal)
	}
	return d.mgr.Kill(d.ctx, h.unit, sig)
}

func (d *Driver) ExecTask(taskID string, cmd []string, timeout time.Duration) (*drivers.ExecTaskResult, error) {
	return nil, errors.New("the systemd driver does not support exec")
}

func fileExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
