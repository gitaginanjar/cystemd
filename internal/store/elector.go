// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	consulapi "github.com/hashicorp/consul/api"
)

// Elector runs a function while this replica is the leader. Only the leader
// reconciles and executes operations; every replica serves the API.
type Elector interface {
	// Run calls lead with a context cancelled when leadership is lost, and
	// campaigns again until ctx ends.
	Run(ctx context.Context, lead func(ctx context.Context))
	// IsLeader reports whether this replica leads right now.
	IsLeader() bool
}

// SingleElector is always the leader (one replica, memory store).
type SingleElector struct{ leading chan struct{} }

func NewSingleElector() *SingleElector { return &SingleElector{leading: make(chan struct{})} }

func (s *SingleElector) Run(ctx context.Context, lead func(ctx context.Context)) {
	close(s.leading)
	lead(ctx)
}

func (s *SingleElector) IsLeader() bool {
	select {
	case <-s.leading:
		return true
	default:
		return false
	}
}

// ConsulElector elects a leader with a Consul session lock on key.
type ConsulElector struct {
	client *consulapi.Client
	key    string
	logger *slog.Logger
	leader atomic.Bool
}

// NewConsulElector returns an elector locking key (e.g. cystemd/leader).
func NewConsulElector(client *consulapi.Client, key string, logger *slog.Logger) *ConsulElector {
	return &ConsulElector{client: client, key: key, logger: logger}
}

func (c *ConsulElector) IsLeader() bool { return c.leader.Load() }

func (c *ConsulElector) Run(ctx context.Context, lead func(ctx context.Context)) {
	host, _ := os.Hostname()
	for ctx.Err() == nil {
		lock, err := c.client.LockOpts(&consulapi.LockOptions{
			Key:              c.key,
			Value:            []byte(host),
			SessionName:      "cystemd-controller",
			SessionTTL:       "15s",
			MonitorRetries:   5,
			MonitorRetryTime: 2 * time.Second,
			LockWaitTime:     15 * time.Second,
		})
		if err != nil {
			c.logger.Error("leader election: cannot create lock", "key", c.key, "error", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		lost, err := lock.Lock(ctx.Done())
		if err != nil {
			c.logger.Warn("leader election: lock attempt failed", "key", c.key, "error", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if lost == nil { // ctx ended while waiting
			return
		}
		c.logger.Info("leader election: acquired leadership", "key", c.key, "host", host)
		c.leader.Store(true)
		lctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			lead(lctx)
		}()
		select {
		case <-lost:
			c.logger.Warn("leader election: leadership lost", "key", c.key)
		case <-ctx.Done():
		}
		cancel()
		<-done
		c.leader.Store(false)
		_ = lock.Unlock()
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
