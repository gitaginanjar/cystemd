// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryKV is an in-process KV for development and tests. It mirrors Consul's
// semantics: a global, monotonically increasing index and blocking watches.
type MemoryKV struct {
	mu      sync.Mutex
	index   uint64
	entries map[string]*Entry
	changed chan struct{} // closed and replaced on every write
}

// NewMemoryKV returns an empty MemoryKV.
func NewMemoryKV() *MemoryKV {
	return &MemoryKV{entries: map[string]*Entry{}, changed: make(chan struct{}), index: 1}
}

func (m *MemoryKV) notify() {
	close(m.changed)
	m.changed = make(chan struct{})
}

func copyEntry(e *Entry) *Entry {
	return &Entry{Key: e.Key, Value: append([]byte(nil), e.Value...), ModifyIndex: e.ModifyIndex}
}

func (m *MemoryKV) Get(_ context.Context, key string) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok {
		return nil, ErrNotFound
	}
	return copyEntry(e), nil
}

func (m *MemoryKV) listLocked(prefix string) []*Entry {
	var out []*Entry
	for k, e := range m.entries {
		if strings.HasPrefix(k, prefix) {
			out = append(out, copyEntry(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (m *MemoryKV) List(_ context.Context, prefix string) ([]*Entry, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listLocked(prefix), m.index, nil
}

func (m *MemoryKV) CAS(_ context.Context, key string, value []byte, index uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	switch {
	case index == 0 && ok:
		return ErrConflict
	case index != 0 && (!ok || e.ModifyIndex != index):
		return ErrConflict
	}
	m.index++
	m.entries[key] = &Entry{Key: key, Value: append([]byte(nil), value...), ModifyIndex: m.index}
	m.notify()
	return nil
}

func (m *MemoryKV) Put(_ context.Context, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.index++
	m.entries[key] = &Entry{Key: key, Value: append([]byte(nil), value...), ModifyIndex: m.index}
	m.notify()
	return nil
}

func (m *MemoryKV) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[key]; ok {
		delete(m.entries, key)
		m.index++
		m.notify()
	}
	return nil
}

// memoryMaxWait bounds a watch like Consul's default blocking-query wait.
const memoryMaxWait = 5 * time.Minute

func (m *MemoryKV) Watch(ctx context.Context, prefix string, waitIndex uint64) ([]*Entry, uint64, error) {
	timeout := time.NewTimer(memoryMaxWait)
	defer timeout.Stop()
	for {
		m.mu.Lock()
		if m.index > waitIndex {
			out, idx := m.listLocked(prefix), m.index
			m.mu.Unlock()
			return out, idx, nil
		}
		ch := m.changed
		m.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, waitIndex, ctx.Err()
		case <-timeout.C:
			m.mu.Lock()
			out, idx := m.listLocked(prefix), m.index
			m.mu.Unlock()
			return out, idx, nil
		}
	}
}
