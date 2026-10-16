// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"strings"

	consulapi "github.com/hashicorp/consul/api"

	"github.com/gitaginanjar/cystemd/internal/health"
)

// ConsulKV reads and writes the Consul keys Applications manage.
type ConsulKV interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Put(ctx context.Context, key string, value []byte) error
	Delete(ctx context.Context, key string) error
}

// ConsulChecks reports the health checks of Consul-registered services.
type ConsulChecks interface {
	// ServiceChecks returns, per allocation ID, the worst check state of
	// the service's instances registered by Nomad.
	ServiceChecks(ctx context.Context, service string) (map[string]string, error)
}

// Consul implements ConsulKV and ConsulChecks over the Consul API.
type Consul struct {
	Client *consulapi.Client
}

func (c *Consul) Get(ctx context.Context, key string) ([]byte, bool, error) {
	p, _, err := c.Client.KV().Get(key, (&consulapi.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return nil, false, err
	}
	if p == nil {
		return nil, false, nil
	}
	return p.Value, true, nil
}

func (c *Consul) Put(ctx context.Context, key string, value []byte) error {
	_, err := c.Client.KV().Put(&consulapi.KVPair{Key: key, Value: value}, (&consulapi.WriteOptions{}).WithContext(ctx))
	return err
}

func (c *Consul) Delete(ctx context.Context, key string) error {
	_, err := c.Client.KV().Delete(key, (&consulapi.WriteOptions{}).WithContext(ctx))
	return err
}

// nomadServicePrefix starts the Consul service ID of every service Nomad
// registers: _nomad-task-<allocID>-…
const nomadServicePrefix = "_nomad-task-"

func (c *Consul) ServiceChecks(ctx context.Context, service string) (map[string]string, error) {
	entries, _, err := c.Client.Health().Service(service, "", false, (&consulapi.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("consul health of %s: %w", service, err)
	}
	out := map[string]string{}
	for _, e := range entries {
		id := e.Service.ID
		if !strings.HasPrefix(id, nomadServicePrefix) || len(id) < len(nomadServicePrefix)+36 {
			continue
		}
		alloc := id[len(nomadServicePrefix) : len(nomadServicePrefix)+36]
		states := []string{out[alloc]}
		for _, chk := range e.Checks {
			if chk.ServiceID == id {
				states = append(states, chk.Status)
			}
		}
		out[alloc] = health.WorstCheck(states...)
	}
	return out, nil
}
