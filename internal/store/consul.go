// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// ConsulKV stores entries in Consul's KV store.
type ConsulKV struct {
	kv      *consulapi.KV
	maxWait time.Duration
}

// NewConsulKV wraps a Consul client.
func NewConsulKV(client *consulapi.Client) *ConsulKV {
	return &ConsulKV{kv: client.KV(), maxWait: 5 * time.Minute}
}

func (c *ConsulKV) Get(ctx context.Context, key string) (*Entry, error) {
	p, _, err := c.kv.Get(key, (&consulapi.QueryOptions{RequireConsistent: true}).WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("consul kv get %s: %w", key, err)
	}
	if p == nil {
		return nil, ErrNotFound
	}
	return &Entry{Key: p.Key, Value: p.Value, ModifyIndex: p.ModifyIndex}, nil
}

func fromPairs(pairs consulapi.KVPairs) []*Entry {
	out := make([]*Entry, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, &Entry{Key: p.Key, Value: p.Value, ModifyIndex: p.ModifyIndex})
	}
	return out
}

func (c *ConsulKV) List(ctx context.Context, prefix string) ([]*Entry, uint64, error) {
	pairs, meta, err := c.kv.List(prefix, (&consulapi.QueryOptions{RequireConsistent: true}).WithContext(ctx))
	if err != nil {
		return nil, 0, fmt.Errorf("consul kv list %s: %w", prefix, err)
	}
	return fromPairs(pairs), meta.LastIndex, nil
}

func (c *ConsulKV) CAS(ctx context.Context, key string, value []byte, index uint64) error {
	ok, _, err := c.kv.CAS(&consulapi.KVPair{Key: key, Value: value, ModifyIndex: index},
		(&consulapi.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return fmt.Errorf("consul kv cas %s: %w", key, err)
	}
	if !ok {
		return ErrConflict
	}
	return nil
}

func (c *ConsulKV) Put(ctx context.Context, key string, value []byte) error {
	if _, err := c.kv.Put(&consulapi.KVPair{Key: key, Value: value}, (&consulapi.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("consul kv put %s: %w", key, err)
	}
	return nil
}

func (c *ConsulKV) Delete(ctx context.Context, key string) error {
	if _, err := c.kv.Delete(key, (&consulapi.WriteOptions{}).WithContext(ctx)); err != nil {
		return fmt.Errorf("consul kv delete %s: %w", key, err)
	}
	return nil
}

// Watch is a Consul blocking query on prefix.
func (c *ConsulKV) Watch(ctx context.Context, prefix string, waitIndex uint64) ([]*Entry, uint64, error) {
	q := (&consulapi.QueryOptions{WaitIndex: waitIndex, WaitTime: c.maxWait}).WithContext(ctx)
	pairs, meta, err := c.kv.List(prefix, q)
	if err != nil {
		return nil, waitIndex, fmt.Errorf("consul kv watch %s: %w", prefix, err)
	}
	idx := meta.LastIndex
	// Consul may return an index lower than the one waited on (after a
	// snapshot restore); restart from zero then, as its docs advise.
	if idx < waitIndex {
		idx = 0
	}
	return fromPairs(pairs), idx, nil
}
