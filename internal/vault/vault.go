// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package vault gives the control plane Vault KV access through Vault
// Agent. The recommended setup runs Vault Agent next to cystemd-server with
// auto_auth and an api_proxy listener (use_auto_auth_token = true): cystemd
// then sends tokenless requests to the agent, which authenticates, renews
// and caches — the control plane never handles a Vault token. A token sink
// file written by the agent, or VAULT_ADDR/VAULT_TOKEN (development), work
// too.
package vault

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	vaultapi "github.com/hashicorp/vault/api"
)

// Config selects how to reach Vault.
type Config struct {
	// AgentAddress is a Vault Agent api_proxy listener.
	AgentAddress string
	// Address is a Vault server (with TokenFile or VAULT_TOKEN).
	Address string
	// TokenFile is a Vault Agent token sink, re-read on change.
	TokenFile string
	Namespace string
	// KVMount is the KV v2 mount (default "secret").
	KVMount string
}

// Client reads and writes KV v2 secrets.
type Client struct {
	c       *vaultapi.Client
	cfg     Config
	mu      sync.Mutex
	tokenAt time.Time
	viaAgnt bool
}

// New returns a client.
func New(cfg Config) (*Client, error) {
	vc := vaultapi.DefaultConfig()
	if vc.Error != nil {
		return nil, vc.Error
	}
	switch {
	case cfg.AgentAddress != "":
		vc.Address = cfg.AgentAddress
	case cfg.Address != "":
		vc.Address = cfg.Address
	}
	c, err := vaultapi.NewClient(vc)
	if err != nil {
		return nil, err
	}
	if cfg.KVMount == "" {
		cfg.KVMount = "secret"
	}
	if cfg.Namespace != "" {
		c.SetNamespace(cfg.Namespace)
	}
	cl := &Client{c: c, cfg: cfg, viaAgnt: cfg.AgentAddress != ""}
	if cl.viaAgnt {
		// The agent injects its auto-auth token; never send one ourselves.
		c.ClearToken()
	}
	return cl, nil
}

// refreshToken re-reads the token sink when it changed (Vault Agent
// rewrites it on every re-authentication).
func (c *Client) refreshToken() error {
	if c.viaAgnt || c.cfg.TokenFile == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fi, err := os.Stat(c.cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("vault token sink: %w", err)
	}
	if !fi.ModTime().After(c.tokenAt) {
		return nil
	}
	b, err := os.ReadFile(c.cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("vault token sink: %w", err)
	}
	c.c.SetToken(strings.TrimSpace(string(b)))
	c.tokenAt = fi.ModTime()
	return nil
}

func (c *Client) dataPath(p string) string {
	return path.Join(c.cfg.KVMount, "data", strings.Trim(p, "/"))
}

// ErrNotFound reports a missing secret.
var ErrNotFound = errors.New("secret not found")

// Read returns the data of a KV v2 secret.
func (c *Client) Read(ctx context.Context, p string) (map[string]any, error) {
	if err := c.refreshToken(); err != nil {
		return nil, err
	}
	s, err := c.c.Logical().ReadWithContext(ctx, c.dataPath(p))
	if err != nil {
		return nil, err
	}
	if s == nil || s.Data == nil {
		return nil, ErrNotFound
	}
	data, ok := s.Data["data"].(map[string]any)
	if !ok || data == nil {
		return nil, ErrNotFound
	}
	return data, nil
}

// Write stores data as a new version of a KV v2 secret.
func (c *Client) Write(ctx context.Context, p string, data map[string]any) error {
	if err := c.refreshToken(); err != nil {
		return err
	}
	_, err := c.c.Logical().WriteWithContext(ctx, c.dataPath(p), map[string]any{"data": data})
	return err
}

// Delete removes every version of a KV v2 secret.
func (c *Client) Delete(ctx context.Context, p string) error {
	if err := c.refreshToken(); err != nil {
		return err
	}
	_, err := c.c.Logical().DeleteWithContext(ctx, path.Join(c.cfg.KVMount, "metadata", strings.Trim(p, "/")))
	return err
}

// Health reports whether Vault (through the agent) is reachable and
// unsealed, and how it is reached.
func (c *Client) Health(ctx context.Context) (Status, error) {
	st := Status{Address: c.c.Address(), ViaAgent: c.viaAgnt}
	h, err := c.c.Sys().HealthWithContext(ctx)
	if err != nil {
		return st, err
	}
	st.Initialized, st.Sealed, st.Version = h.Initialized, h.Sealed, h.Version
	return st, nil
}

// Status is shown on the settings page.
type Status struct {
	Address     string `json:"address"`
	ViaAgent    bool   `json:"viaAgent"`
	Initialized bool   `json:"initialized"`
	Sealed      bool   `json:"sealed"`
	Version     string `json:"version"`
}
