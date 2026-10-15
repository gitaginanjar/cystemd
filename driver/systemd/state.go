// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"sync"
	"time"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// taskHandleVersion is the version of TaskState this driver writes and reads.
const taskHandleVersion = 1

// TaskState is persisted in the Nomad task handle so RecoverTask can
// re-attach after a client restart: everything needed to find the unit again
// and to clean up after it.
type TaskState struct {
	TaskConfig *drivers.TaskConfig
	Mode       string
	Unit       string
	StartedAt  time.Time
	Files      []FileConfig
	DropIn     string
	EnvFile    string
}

// taskStore is the in-memory map of task ID → handle.
type taskStore struct {
	mu    sync.RWMutex
	store map[string]*taskHandle
}

func newTaskStore() *taskStore {
	return &taskStore{store: map[string]*taskHandle{}}
}

func (ts *taskStore) Set(id string, h *taskHandle) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.store[id] = h
}

func (ts *taskStore) Get(id string) (*taskHandle, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	h, ok := ts.store[id]
	return h, ok
}

func (ts *taskStore) Delete(id string) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	delete(ts.store, id)
}

// liveAlloc reports whether any running task of this driver belongs to
// allocID (unit ownership checks).
func (ts *taskStore) liveAlloc(allocID string) bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	for _, h := range ts.store {
		if h.taskConfig.AllocID == allocID && h.IsRunning() {
			return true
		}
	}
	return false
}
