// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package store persists CYSTEMD objects (Applications, projects,
// repositories, accounts, events) in a key/value backend: Consul KV in
// production, memory for development and tests. Every object is one JSON
// document written with check-and-set on its modify index, so concurrent
// writers (API replicas, the controller) never lose updates.
package store

import (
	"context"
	"errors"
)

var (
	// ErrNotFound is returned when a key does not exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict is returned when a check-and-set lost the race.
	ErrConflict = errors.New("the object has been modified; please apply your changes to the latest version and try again")
	// ErrExists is returned when creating an object that exists.
	ErrExists = errors.New("already exists")
)

// Entry is one key with its value and modify index.
type Entry struct {
	Key         string
	Value       []byte
	ModifyIndex uint64
}

// KV is the raw backend.
type KV interface {
	// Get returns the entry or ErrNotFound.
	Get(ctx context.Context, key string) (*Entry, error)
	// List returns every entry under prefix and the backend index.
	List(ctx context.Context, prefix string) ([]*Entry, uint64, error)
	// CAS writes value when the key's modify index equals index (0: the key
	// must not exist) and returns ErrConflict otherwise.
	CAS(ctx context.Context, key string, value []byte, index uint64) error
	// Put writes value unconditionally.
	Put(ctx context.Context, key string, value []byte) error
	// Delete removes key (no error when absent).
	Delete(ctx context.Context, key string) error
	// Watch blocks until something under prefix changes after waitIndex (or
	// ctx ends, or the backend's wait time elapses) and returns the entries
	// and the new index.
	Watch(ctx context.Context, prefix string, waitIndex uint64) ([]*Entry, uint64, error)
}
