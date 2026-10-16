// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package apitest runs the CYSTEMD API in-process for tests of its
// clients: a real server on an in-memory store, with an unreachable Nomad
// (enough for everything but syncs, diffs, live trees and logs).
package apitest

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/git"
	"github.com/gitaginanjar/cystemd/internal/metrics"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/rbac"
	"github.com/gitaginanjar/cystemd/internal/server"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Server is a running API.
type Server struct {
	URL string
	// Password is the admin's.
	Password string
	Store    *store.Store
}

// New starts an API server, stopped when the test ends.
func New(t testing.TB) *Server { return NewWith(t, nil) }

// NewWith lets configure change the configuration first: its accounts
// (with their capabilities given) are declared, its RBAC policy applies.
func NewWith(t testing.TB, configure func(*config.Config)) *Server {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Nomad.Address = "http://127.0.0.1:1"
	if configure != nil {
		configure(cfg)
	}
	st := store.New(store.NewMemoryKV(), "cystemd")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	am, err := auth.NewManager(ctx, auth.Options{Store: st, AdminEnabled: true, DataDir: cfg.DataDir, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	var declared []auth.DeclaredAccount
	for _, a := range cfg.Auth.Accounts {
		declared = append(declared, auth.DeclaredAccount{Name: a.Name, Capabilities: a.Capabilities, Enabled: a.Enabled == nil || *a.Enabled})
	}
	if err := am.SyncAccounts(ctx, declared); err != nil {
		t.Fatal(err)
	}
	enforcer, err := rbac.New(cfg.RBAC.Policy, rbac.RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	clusters := nomad.NewClusters()
	nc, err := nomad.New(nomad.Config{Address: cfg.Nomad.Address})
	if err != nil {
		t.Fatal(err)
	}
	clusters.Add(v1.InClusterName, nc)
	gitClient := git.NewClient(git.Options{CacheDir: t.TempDir()})
	repos := &controller.RepoService{Git: gitClient, Clusters: clusters}
	s := server.New(server.Options{
		Config: cfg, Store: st, Auth: am, RBAC: enforcer, Clusters: clusters, Git: gitClient, Repos: repos,
		Live: &controller.Live{Clusters: clusters, Repos: repos}, Metrics: metrics.New(), Logger: logger,
		Build: server.BuildInfo{Version: "test", Mode: "standalone"},
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	pw, err := os.ReadFile(filepath.Join(cfg.DataDir, "initial-admin-password"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{URL: srv.URL, Password: strings.TrimSpace(string(pw)), Store: st}
}
