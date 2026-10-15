// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileKV is a MemoryKV persisted to a JSON file: durable storage for a
// single control-plane replica without Consul (development, small sites).
// Writes are flushed by a background saver within saveDelay.
type FileKV struct {
	*MemoryKV
	path   string
	logger *slog.Logger

	mu    sync.Mutex
	dirty bool
	stop  chan struct{}
	done  chan struct{}
}

const saveDelay = time.Second

type fileSnapshot struct {
	Index   uint64            `json:"index"`
	Entries map[string][]byte `json:"entries"`
}

// NewFileKV loads path (when it exists) and starts the saver.
func NewFileKV(path string, logger *slog.Logger) (*FileKV, error) {
	if logger == nil {
		logger = slog.Default()
	}
	f := &FileKV{MemoryKV: NewMemoryKV(), path: path, logger: logger, stop: make(chan struct{}), done: make(chan struct{})}
	if b, err := os.ReadFile(path); err == nil {
		var snap fileSnapshot
		if err := json.Unmarshal(b, &snap); err != nil {
			return nil, err
		}
		for k, v := range snap.Entries {
			f.MemoryKV.entries[k] = &Entry{Key: k, Value: v, ModifyIndex: snap.Index}
		}
		if snap.Index > f.MemoryKV.index {
			f.MemoryKV.index = snap.Index
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	go f.saver()
	return f, nil
}

func (f *FileKV) markDirty() {
	f.mu.Lock()
	f.dirty = true
	f.mu.Unlock()
}

func (f *FileKV) CAS(ctx context.Context, key string, value []byte, index uint64) error {
	err := f.MemoryKV.CAS(ctx, key, value, index)
	if err == nil {
		f.markDirty()
	}
	return err
}

func (f *FileKV) Put(ctx context.Context, key string, value []byte) error {
	err := f.MemoryKV.Put(ctx, key, value)
	if err == nil {
		f.markDirty()
	}
	return err
}

func (f *FileKV) Delete(ctx context.Context, key string) error {
	err := f.MemoryKV.Delete(ctx, key)
	if err == nil {
		f.markDirty()
	}
	return err
}

func (f *FileKV) saver() {
	defer close(f.done)
	t := time.NewTicker(saveDelay)
	defer t.Stop()
	for {
		select {
		case <-f.stop:
			f.save()
			return
		case <-t.C:
			f.save()
		}
	}
}

func (f *FileKV) save() {
	f.mu.Lock()
	dirty := f.dirty
	f.dirty = false
	f.mu.Unlock()
	if !dirty {
		return
	}
	f.MemoryKV.mu.Lock()
	snap := fileSnapshot{Index: f.MemoryKV.index, Entries: make(map[string][]byte, len(f.MemoryKV.entries))}
	for k, e := range f.MemoryKV.entries {
		snap.Entries[k] = e.Value
	}
	f.MemoryKV.mu.Unlock()
	b, err := json.Marshal(snap)
	if err == nil {
		err = os.MkdirAll(filepath.Dir(f.path), 0o700)
	}
	if err == nil {
		tmp := f.path + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, f.path)
		}
	}
	if err != nil {
		f.logger.Error("persist state", "file", f.path, "error", err)
		f.markDirty()
	}
}

// Close flushes pending writes.
func (f *FileKV) Close() {
	select {
	case <-f.stop:
	default:
		close(f.stop)
	}
	<-f.done
}
