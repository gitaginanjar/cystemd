// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
	"github.com/hashicorp/go-hclog"
)

// maxUint64 is systemd's "infinity" for usec and byte properties.
const maxUint64 = ^uint64(0)

// unitState is a snapshot of the unit properties the driver reads. Per-type
// properties (MainPID, ExecMain*, Memory*, CPU*, Tasks*, NRestarts,
// TimeoutStopUSec) live on the …systemd1.Service interface: read through the
// …systemd1.Unit interface they silently come back zero.
type unitState struct {
	LoadState      string
	ActiveState    string
	SubState       string
	Result         string
	InvocationID   string
	MainPID        uint32
	ExecMainCode   int32
	ExecMainStatus int32
	NRestarts      uint32
	ActiveEnterUS  uint64
	MemoryCurrent  uint64
	CPUUsageNSec   uint64
	TasksCurrent   uint64
	TimeoutStopUS  uint64
	FragmentPath   string
	DropInPaths    []string
}

// running reports whether the unit has a live or starting process.
// "deactivating" counts: the process is still there.
func (s unitState) running() bool {
	switch s.ActiveState {
	case "active", "activating", "reloading", "refreshing", "deactivating":
		return true
	}
	return false
}

// systemdManager is everything the driver asks of systemd: the test seam.
type systemdManager interface {
	// Version returns the systemd version ("252").
	Version(ctx context.Context) (string, error)
	// UnitState reads the unit's properties.
	UnitState(ctx context.Context, unit string) (unitState, error)
	// Start, Restart and Stop wait for the systemd job to finish.
	Start(ctx context.Context, unit string) error
	Restart(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
	StartTransient(ctx context.Context, unit string, props []dbus.Property) error
	Kill(ctx context.Context, unit string, signal syscall.Signal) error
	ResetFailed(ctx context.Context, unit string) error
	DaemonReload(ctx context.Context) error
	Close()
}

// Job and read bounds. systemdJobTimeout sits above systemd's own default job
// timeouts (~90s), so it only fires when systemd itself is wedged.
const (
	dbusConnectTimeout  = 10 * time.Second
	dbusPropertyTimeout = 5 * time.Second
	systemdJobTimeout   = 120 * time.Second

	// Kill-watchdog grace: the unit's own TimeoutStopUSec + margin, capped.
	// systemd SIGKILLs at its own deadline; ours is a backstop for a wedged
	// systemd, never a competing stop policy.
	watchdogGraceMargin  = 2 * time.Second
	watchdogGraceDefault = 3 * time.Second
	watchdogGraceCap     = 60 * time.Second
	postKillDrainTimeout = 5 * time.Second
)

// dbusManager implements systemdManager over systemd's private D-Bus socket
// (no dbus-daemon needed; root only). The connection is opened lazily and
// re-opened after a failure.
type dbusManager struct {
	logger hclog.Logger

	mu   sync.Mutex
	conn *dbus.Conn
}

func newDBusManager(logger hclog.Logger) *dbusManager {
	return &dbusManager{logger: logger}
}

// systemdBooted reports whether systemd is the init system (sd_booted(3)).
func systemdBooted() bool {
	fi, err := os.Lstat("/run/systemd/system")
	return err == nil && fi.IsDir()
}

// connection returns the live connection, dialling when needed. godbus ties
// a connection's lifetime to the dial context, so the dial runs on a
// background context and only the wait for it is bounded.
func (m *dbusManager) connection() (*dbus.Conn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil && m.conn.Connected() {
		return m.conn, nil
	}
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}
	type result struct {
		conn *dbus.Conn
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := dbus.NewSystemdConnectionContext(context.Background())
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("systemd D-Bus connect: %w", r.err)
		}
		m.conn = r.conn
		return r.conn, nil
	case <-time.After(dbusConnectTimeout):
		return nil, fmt.Errorf("systemd D-Bus connect: timed out after %s", dbusConnectTimeout)
	}
}

func (m *dbusManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.conn != nil {
		m.conn.Close()
		m.conn = nil
	}
}

func (m *dbusManager) Version(ctx context.Context) (string, error) {
	conn, err := m.connection()
	if err != nil {
		return "", err
	}
	v, err := conn.GetManagerProperty("Version")
	if err != nil {
		return "", err
	}
	// The variant renders quoted ("\"252.4-1\""); keep the leading number.
	v = strings.Trim(v, `"`)
	if i := strings.IndexAny(v, ". -~"); i > 0 {
		v = v[:i]
	}
	return v, nil
}

func (m *dbusManager) UnitState(ctx context.Context, unit string) (unitState, error) {
	conn, err := m.connection()
	if err != nil {
		return unitState{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, dbusPropertyTimeout)
	defer cancel()
	uprops, err := conn.GetUnitPropertiesContext(rctx, unit)
	if err != nil {
		return unitState{}, fmt.Errorf("read unit %s: %w", unit, err)
	}
	st := unitState{
		LoadState:    asString(uprops["LoadState"]),
		ActiveState:  asString(uprops["ActiveState"]),
		SubState:     asString(uprops["SubState"]),
		FragmentPath: asString(uprops["FragmentPath"]),
		DropInPaths:  asStrings(uprops["DropInPaths"]),
		InvocationID: invocationID(uprops["InvocationID"]),
	}
	st.ActiveEnterUS = asUint64(uprops["ActiveEnterTimestamp"])
	if st.LoadState != "loaded" {
		return st, nil
	}
	sprops, err := conn.GetUnitTypePropertiesContext(rctx, unit, "Service")
	if err != nil {
		return st, fmt.Errorf("read service properties of %s: %w", unit, err)
	}
	st.Result = asString(sprops["Result"])
	st.MainPID = asUint32(sprops["MainPID"])
	st.ExecMainCode = asInt32(sprops["ExecMainCode"])
	st.ExecMainStatus = asInt32(sprops["ExecMainStatus"])
	st.NRestarts = asUint32(sprops["NRestarts"])
	st.MemoryCurrent = asUint64(sprops["MemoryCurrent"])
	st.CPUUsageNSec = asUint64(sprops["CPUUsageNSec"])
	st.TasksCurrent = asUint64(sprops["TasksCurrent"])
	st.TimeoutStopUS = asUint64(sprops["TimeoutStopUSec"])
	return st, nil
}

// awaitJob waits for a job result: nil for "done", an error otherwise.
func awaitJob(ctx context.Context, ch <-chan string, action, unit string, timeout time.Duration) error {
	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("systemd %s %s: job result %q", action, unit, result)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("systemd %s %s: no job result after %s", action, unit, timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *dbusManager) Start(ctx context.Context, unit string) error {
	conn, err := m.connection()
	if err != nil {
		return err
	}
	ch := make(chan string, 1)
	if _, err := conn.StartUnitContext(ctx, unit, "replace", ch); err != nil {
		return fmt.Errorf("systemd start %s: %w", unit, err)
	}
	return awaitJob(ctx, ch, "start", unit, systemdJobTimeout)
}

func (m *dbusManager) Restart(ctx context.Context, unit string) error {
	return m.withKillWatchdog(ctx, "restart", unit)
}

func (m *dbusManager) Stop(ctx context.Context, unit string) error {
	return m.withKillWatchdog(ctx, "stop", unit)
}

// withKillWatchdog runs a stop or restart job and, if the unit is still
// "deactivating" after its own TimeoutStopUSec plus a margin (capped), SIGKILLs
// its main process: the backstop for a wedged systemd. TimeoutStopUSec of
// infinity is operator policy and is never overridden.
func (m *dbusManager) withKillWatchdog(ctx context.Context, action, unit string) error {
	conn, err := m.connection()
	if err != nil {
		return err
	}
	ch := make(chan string, 1)
	switch action {
	case "stop":
		_, err = conn.StopUnitContext(ctx, unit, "replace", ch)
	case "restart":
		_, err = conn.RestartUnitContext(ctx, unit, "replace", ch)
	default:
		return fmt.Errorf("unknown watchdog action %q", action)
	}
	if err != nil {
		return fmt.Errorf("systemd %s %s: %w", action, unit, err)
	}

	st, _ := m.UnitState(ctx, unit)
	grace, mayKill := watchdogGrace(st.TimeoutStopUS)

	select {
	case result := <-ch:
		if result != "done" {
			return fmt.Errorf("systemd %s %s: job result %q", action, unit, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(grace):
	}

	st, _ = m.UnitState(ctx, unit)
	killed := false
	if st.ActiveState == "deactivating" {
		switch {
		case !mayKill:
			m.logger.Info("unit still deactivating but TimeoutStopUSec=infinity; not killing (operator policy)", "unit", unit)
		case st.MainPID > 0:
			m.logger.Warn("kill watchdog: SIGKILL to unit stuck deactivating", "unit", unit, "pid", st.MainPID, "grace", grace)
			_ = syscall.Kill(int(st.MainPID), syscall.SIGKILL)
			killed = true
		}
	}
	timeout := systemdJobTimeout
	if killed {
		timeout = postKillDrainTimeout
	}
	return awaitJob(ctx, ch, action, unit, timeout)
}

// watchdogGrace returns the wait before a SIGKILL is considered and whether a
// kill is permitted: 0 (unreadable) → default; infinity → never.
func watchdogGrace(stopTimeoutUS uint64) (time.Duration, bool) {
	switch stopTimeoutUS {
	case 0:
		return watchdogGraceDefault, true
	case maxUint64:
		return watchdogGraceCap, false
	}
	d := time.Duration(stopTimeoutUS)*time.Microsecond + watchdogGraceMargin
	if d > watchdogGraceCap {
		d = watchdogGraceCap
	}
	return d, true
}

func (m *dbusManager) StartTransient(ctx context.Context, unit string, props []dbus.Property) error {
	conn, err := m.connection()
	if err != nil {
		return err
	}
	ch := make(chan string, 1)
	if _, err := conn.StartTransientUnitContext(ctx, unit, "fail", props, ch); err != nil {
		return fmt.Errorf("systemd start transient %s: %w", unit, err)
	}
	return awaitJob(ctx, ch, "start", unit, systemdJobTimeout)
}

func (m *dbusManager) Kill(ctx context.Context, unit string, signal syscall.Signal) error {
	conn, err := m.connection()
	if err != nil {
		return err
	}
	return conn.KillUnitWithTarget(ctx, unit, dbus.Main, int32(signal))
}

func (m *dbusManager) ResetFailed(ctx context.Context, unit string) error {
	conn, err := m.connection()
	if err != nil {
		return err
	}
	err = conn.ResetFailedUnitContext(ctx, unit)
	if err != nil && strings.Contains(err.Error(), "not loaded") {
		return nil
	}
	return err
}

func (m *dbusManager) DaemonReload(ctx context.Context) error {
	conn, err := m.connection()
	if err != nil {
		return err
	}
	return conn.ReloadContext(ctx)
}

// errNotLoaded marks a unit systemd does not know (not-found, masked, …).
var errNotLoaded = errors.New("unit not loaded")

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, i := range t {
			if s, ok := i.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func asUint32(v any) uint32 {
	u, _ := v.(uint32)
	return u
}

func asInt32(v any) int32 {
	i, _ := v.(int32)
	return i
}

func asUint64(v any) uint64 {
	u, _ := v.(uint64)
	return u
}

// invocationID renders systemd's InvocationID (ay) as hex.
func invocationID(v any) string {
	b, ok := v.([]byte)
	if !ok || len(b) == 0 {
		return ""
	}
	return fmt.Sprintf("%x", b)
}
