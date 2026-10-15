// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins/drivers"
)

// fileSyncEvery is how many poll ticks pass between file syncs.
const fileSyncEvery = 5

// taskHandle tracks one task's unit from start (or recovery) to exit.
type taskHandle struct {
	logger     hclog.Logger
	mgr        systemdManager
	taskConfig *drivers.TaskConfig
	mode       taskMode
	unit       string
	startedAt  time.Time
	files      *fileSyncer
	emit       func(msg string, annotations map[string]string)
	journal    *journalFollower
	poll       time.Duration

	mu          sync.RWMutex
	procState   drivers.TaskState
	completedAt time.Time
	exitResult  *drivers.ExitResult
	lastState   unitState

	ctx    context.Context
	cancel context.CancelFunc
	doneCh chan struct{}
}

func (h *taskHandle) IsRunning() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.mu.RLock()
	defer h.mu.RUnlock()
	attrs := map[string]string{
		"unit":  h.unit,
		"mode":  string(h.mode),
		"state": h.lastState.ActiveState + "/" + h.lastState.SubState,
	}
	if h.lastState.MainPID > 0 {
		attrs["main_pid"] = strconv.FormatUint(uint64(h.lastState.MainPID), 10)
	}
	if h.lastState.InvocationID != "" {
		attrs["invocation_id"] = h.lastState.InvocationID
	}
	attrs["n_restarts"] = strconv.FormatUint(uint64(h.lastState.NRestarts), 10)
	return &drivers.TaskStatus{
		ID:               h.taskConfig.ID,
		Name:             h.taskConfig.Name,
		State:            h.procState,
		StartedAt:        h.startedAt,
		CompletedAt:      h.completedAt,
		ExitResult:       h.exitResult,
		DriverAttributes: attrs,
	}
}

// exited decides from a unit snapshot whether the task has ended, and how.
// Unit mode: the unit stopped (inactive/failed). Transient mode units run
// with RemainAfterExit, so "active (exited)" also means the command ended —
// and keeps the exit status readable until the driver stops the unit.
func (h *taskHandle) exited(st unitState) (*drivers.ExitResult, bool) {
	if st.LoadState != "" && st.LoadState != "loaded" {
		return &drivers.ExitResult{ExitCode: 1, Err: fmt.Errorf("unit %s is %s", h.unit, st.LoadState)}, true
	}
	done := st.ActiveState == "inactive" || st.ActiveState == "failed"
	if h.mode == modeTransient && st.ActiveState == "active" && st.SubState == "exited" {
		done = true
	}
	if !done {
		return nil, false
	}
	res := &drivers.ExitResult{}
	switch st.ExecMainCode {
	case 1: // CLD_EXITED
		res.ExitCode = int(st.ExecMainStatus)
	case 2, 3: // CLD_KILLED, CLD_DUMPED
		res.Signal = int(st.ExecMainStatus)
		res.ExitCode = 128 + int(st.ExecMainStatus)
	}
	if st.Result == "oom-kill" {
		res.OOMKilled = true
	}
	if st.ActiveState == "failed" && res.ExitCode == 0 && res.Signal == 0 {
		res.ExitCode = 1
		res.Err = fmt.Errorf("unit %s failed (result %s)", h.unit, st.Result)
	}
	return res, true
}

// run polls the unit until it ends or the handle is cancelled, syncing the
// task's files to their host destinations every fileSyncEvery ticks.
func (h *taskHandle) run() {
	defer close(h.doneCh)
	defer h.journal.stop()

	ticker := time.NewTicker(h.poll)
	defer ticker.Stop()
	failures := 0
	for tick := 1; ; tick++ {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
		}

		st, err := h.mgr.UnitState(h.ctx, h.unit)
		if err != nil {
			// D-Bus hiccup or systemd re-exec: keep polling, the unit is not
			// gone just because one read failed.
			failures++
			if failures == 3 {
				h.logger.Warn("cannot read unit state; still retrying", "unit", h.unit, "error", err)
			}
			continue
		}
		failures = 0

		h.mu.Lock()
		prev := h.lastState
		h.lastState = st
		h.mu.Unlock()

		if prev.InvocationID != "" && st.InvocationID != "" && prev.InvocationID != st.InvocationID && st.running() {
			h.emit(fmt.Sprintf("Unit %s was restarted outside Nomad", h.unit), map[string]string{"unit": h.unit})
		}

		if res, done := h.exited(st); done {
			h.mu.Lock()
			h.procState = drivers.TaskStateExited
			h.exitResult = res
			h.completedAt = time.Now()
			h.mu.Unlock()
			h.logger.Info("unit exited", "unit", h.unit, "active_state", st.ActiveState,
				"result", st.Result, "exit_code", res.ExitCode, "signal", res.Signal)
			return
		}

		if h.files != nil && tick%fileSyncEvery == 0 {
			changed, err := h.files.sync(false)
			for _, c := range changed {
				if c.Err == nil {
					h.emit("Updated "+c.Destination, map[string]string{"file": c.Destination})
				}
			}
			if err != nil {
				h.logger.Warn("file sync failed", "unit", h.unit, "error", err)
			}
		}
	}
}

// stats emits resource usage every interval until ctx is done.
func (h *taskHandle) stats(ctx context.Context, interval time.Duration, perCoreMHz float64, ch chan<- *drivers.TaskResourceUsage) {
	defer close(ch)
	var prevCPU uint64
	var prevAt time.Time
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(interval)
		}
		st, err := h.mgr.UnitState(ctx, h.unit)
		if err != nil {
			continue
		}
		now := time.Now()
		cpu := &drivers.CpuStats{Measured: []string{"Percent", "Total Ticks"}}
		if !prevAt.IsZero() && st.CPUUsageNSec >= prevCPU && st.CPUUsageNSec != maxUint64 {
			wall := now.Sub(prevAt).Nanoseconds()
			if wall > 0 {
				cpu.Percent = float64(st.CPUUsageNSec-prevCPU) / float64(wall) * 100
				cpu.TotalTicks = cpu.Percent / 100 * perCoreMHz
			}
		}
		prevCPU, prevAt = st.CPUUsageNSec, now
		mem := &drivers.MemoryStats{Measured: []string{"Usage"}}
		if st.MemoryCurrent != maxUint64 {
			mem.Usage = st.MemoryCurrent
			mem.RSS = st.MemoryCurrent
		}
		usage := &drivers.TaskResourceUsage{
			ResourceUsage: &drivers.ResourceUsage{MemoryStats: mem, CpuStats: cpu},
			Timestamp:     now.UnixNano(),
		}
		select {
		case ch <- usage:
		case <-ctx.Done():
			return
		}
	}
}
