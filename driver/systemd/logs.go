// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/hashicorp/go-hclog"
)

// journalFollower streams a unit's journal into the task's stdout FIFO, so
// `nomad alloc logs` and the UI show a unit-mode daemon's output: such a
// daemon writes to the journal, not to Nomad's log pipes.
type journalFollower struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// followJournal starts `journalctl -f` for unit from since. It returns nil
// (logs unavailable, logged once) when journalctl is missing.
func followJournal(unit, stdoutPath string, since time.Time, logger hclog.Logger) *journalFollower {
	if stdoutPath == "" {
		return nil
	}
	bin, err := exec.LookPath("journalctl")
	if err != nil {
		logger.Warn("journalctl not found; unit logs are not forwarded to Nomad", "unit", unit)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &journalFollower{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		// Opening a FIFO for writing blocks until logmon opens the reader.
		out, err := os.OpenFile(stdoutPath, os.O_WRONLY, 0)
		if err != nil {
			logger.Warn("cannot open task stdout; unit logs are not forwarded", "unit", unit, "error", err)
			return
		}
		defer out.Close()
		cmd := exec.CommandContext(ctx, bin,
			"--follow", "--no-pager", "--output=cat", "--unit="+unit,
			fmt.Sprintf("--since=@%d", since.Unix()))
		cmd.Stdout = out
		cmd.Stderr = out
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Run(); err != nil && ctx.Err() == nil {
			logger.Debug("journal follower exited", "unit", unit, "error", err)
		}
	}()
	return f
}

// stop terminates the follower and waits briefly for it to exit.
func (f *journalFollower) stop() {
	if f == nil {
		return
	}
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(5 * time.Second):
	}
}
