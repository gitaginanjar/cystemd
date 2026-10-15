// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package controlplane assembles the CYSTEMD control plane from its
// configuration: store, leader election, Nomad clusters, Consul, Vault,
// repository service, controller and API server. cystemd-server runs it
// standalone; the cystemd agent embeds it in server agents.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	consulapi "github.com/hashicorp/consul/api"
	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/git"
	"github.com/gitaginanjar/cystemd/internal/metrics"
	"github.com/gitaginanjar/cystemd/internal/nomad"
	"github.com/gitaginanjar/cystemd/internal/rbac"
	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/internal/server"
	"github.com/gitaginanjar/cystemd/internal/store"
	"github.com/gitaginanjar/cystemd/internal/vault"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Options are the parts the host process provides.
type Options struct {
	Logger *slog.Logger
	Build  server.BuildInfo
}

// ControlPlane is a running control plane.
type ControlPlane struct {
	Config     *config.Config
	Store      *store.Store
	Server     *server.Server
	Controller *controller.Controller
	Elector    store.Elector
	Logger     *slog.Logger

	closers []func()
}

// nomadRolePolicy maps Nomad token logins onto CYSTEMD roles.
const nomadRolePolicy = `
g, nomad:management, role:admin
`

// New builds a control plane from cfg.
func New(ctx context.Context, cfg *config.Config, o Options) (*ControlPlane, error) {
	logger := o.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cp := &ControlPlane{Config: cfg, Logger: logger}
	m := metrics.New()

	// Nomad clusters.
	clusters := nomad.NewClusters()
	inToken, err := config.Secret(cfg.Nomad.Token, cfg.Nomad.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("nomad token: %w", err)
	}
	in, err := nomad.New(nomad.Config{Address: cfg.Nomad.Address, Region: cfg.Nomad.Region, Token: inToken,
		CACert: cfg.Nomad.CACert, TLSSkip: cfg.Nomad.TLSSkipVerify})
	if err != nil {
		return nil, err
	}
	clusters.Add(v1.InClusterName, in)
	for _, cl := range cfg.Clusters {
		tok, err := config.Secret(cl.Token, cl.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("cluster %s token: %w", cl.Name, err)
		}
		c, err := nomad.New(nomad.Config{Address: cl.Address, Region: cl.Region, Token: tok, CACert: cl.CACert, TLSSkip: cl.TLSSkipVerify})
		if err != nil {
			return nil, fmt.Errorf("cluster %s: %w", cl.Name, err)
		}
		clusters.Add(cl.Name, c)
	}

	// Store and leader election: Consul when enabled, else a file in
	// data_dir (single replica).
	var consulClient *consulapi.Client
	var kv store.KV
	if cfg.ConsulEnabled() {
		ccfg := consulapi.DefaultConfig()
		ccfg.Address = cfg.Consul.Address
		ccfg.Datacenter = cfg.Consul.Datacenter
		if tok, err := config.Secret(cfg.Consul.Token, cfg.Consul.TokenFile); err != nil {
			return nil, fmt.Errorf("consul token: %w", err)
		} else if tok != "" {
			ccfg.Token = tok
		}
		if consulClient, err = consulapi.NewClient(ccfg); err != nil {
			return nil, err
		}
		kv = store.NewConsulKV(consulClient)
		cp.Elector = store.NewConsulElector(consulClient, strings.Trim(cfg.Consul.KVPrefix, "/")+"/leader", logger)
	} else {
		fkv, err := store.NewFileKV(filepath.Join(cfg.DataDir, "state.json"), logger)
		if err != nil {
			return nil, fmt.Errorf("state file: %w", err)
		}
		cp.closers = append(cp.closers, fkv.Close)
		kv = fkv
		cp.Elector = store.NewSingleElector()
		logger.Warn("Consul is not configured: state is kept in a local file and only one replica may run", "file", filepath.Join(cfg.DataDir, "state.json"))
	}
	cp.Store = store.New(kv, cfg.Consul.KVPrefix)

	// Vault (through Vault Agent).
	var vc *vault.Client
	if cfg.VaultEnabled() {
		vc, err = vault.New(vault.Config{AgentAddress: cfg.Vault.AgentAddress, Address: cfg.Vault.Address,
			TokenFile: cfg.Vault.TokenFile, Namespace: cfg.Vault.Namespace, KVMount: cfg.Vault.KVMount})
		if err != nil {
			return nil, fmt.Errorf("vault: %w", err)
		}
	}

	// Repositories.
	creds := &server.Credentials{Store: cp.Store, Vault: vc, Config: cfg}
	gitClient := git.NewClient(git.Options{
		CacheDir:       filepath.Join(cfg.DataDir, "repos"),
		Credentials:    creds.Resolve,
		KnownHostsFile: cfg.Git.KnownHostsFile,
		FetchTimeout:   config.Duration(cfg.Git.FetchTimeout, 0),
		Logger:         logger.With("component", "git"),
	})
	repos := &controller.RepoService{Git: gitClient, Clusters: clusters, Options: render.Options{
		ConsulEnabled:    cfg.ConsulEnabled(),
		KVPrefix:         cfg.Consul.KVPrefix,
		DefaultVaultRole: cfg.Vault.WorkloadRole,
		VaultAddress:     cfg.Vault.WorkloadAddress,
		VaultJWTAuthPath: cfg.Vault.JWTAuthPath,
	}}

	var consulKV controller.ConsulKV
	var checks controller.ConsulChecks
	if consulClient != nil {
		c := &controller.Consul{Client: consulClient}
		consulKV, checks = c, c
	}

	// Authentication and RBAC.
	legacyKey := ""
	if f := cfg.Auth.LegacyPublicKeyFile; f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("legacy public key: %w", err)
		}
		legacyKey = string(b)
	}
	var nomadLogin auth.NomadTokenResolver
	if *cfg.Auth.NomadTokenLogin {
		nomadLogin = nomadTokenResolver(cfg)
	}
	am, err := auth.NewManager(ctx, auth.Options{
		Store: cp.Store, SessionTTL: config.Duration(cfg.Auth.SessionDuration, 0),
		AdminEnabled: *cfg.Auth.AdminEnabled, AnonymousRole: cfg.Auth.AnonymousRole,
		DataDir: cfg.DataDir, AdminPasswordFile: cfg.Auth.AdminPasswordFile,
		LegacyPublicKey: legacyKey, NomadToken: nomadLogin, Logger: logger.With("component", "auth"),
	})
	if err != nil {
		return nil, err
	}
	policy := cfg.RBAC.Policy
	if cfg.RBAC.PolicyFile != "" {
		b, err := os.ReadFile(cfg.RBAC.PolicyFile)
		if err != nil {
			return nil, fmt.Errorf("rbac policy file: %w", err)
		}
		policy += "\n" + string(b)
	}
	enforcer, err := rbac.New(nomadRolePolicy+policy, cfg.RBAC.DefaultRole)
	if err != nil {
		return nil, fmt.Errorf("rbac: %w", err)
	}

	live := &controller.Live{Clusters: clusters, Consul: consulKV, Checks: checks, Repos: repos}
	if *cfg.Controller.Enabled {
		cp.Controller = controller.New(controller.Options{
			Store: cp.Store, Clusters: clusters, Repos: repos, Consul: consulKV, Checks: checks,
			Logger: logger, Metrics: m,
			Config: controller.Config{
				ReconcileInterval: config.Duration(cfg.Controller.ReconcileInterval, 0),
				SelfHealTimeout:   config.Duration(cfg.Controller.SelfHealTimeout, 0),
				Workers:           cfg.Controller.Workers,
			},
		})
	}
	cp.Server = server.New(server.Options{
		Config: cfg, Store: cp.Store, Auth: am, RBAC: enforcer, Live: live, Repos: repos, Git: gitClient,
		Clusters: clusters, Vault: vc, Consul: consulClient, Metrics: m, Elector: cp.Elector,
		Logger: logger, Build: o.Build,
	})
	return cp, nil
}

// Run campaigns for leadership and runs the controller while leading; it
// returns when ctx ends.
func (cp *ControlPlane) Run(ctx context.Context) {
	if cp.Controller == nil {
		<-ctx.Done()
		return
	}
	cp.Elector.Run(ctx, cp.Controller.Run)
}

// Close releases resources (flushes the state file).
func (cp *ControlPlane) Close() {
	for _, c := range cp.closers {
		c()
	}
}

// nomadTokenResolver validates a Nomad ACL token against the in-cluster
// Nomad and maps it to groups: nomad:management for management tokens,
// nomad:policy:<name> and nomad:role:<name> otherwise.
func nomadTokenResolver(cfg *config.Config) auth.NomadTokenResolver {
	return func(ctx context.Context, token string) (*auth.Identity, error) {
		c := nomadapi.DefaultConfig()
		c.Address = cfg.Nomad.Address
		c.Region = cfg.Nomad.Region
		c.SecretID = token
		if cfg.Nomad.CACert != "" || cfg.Nomad.TLSSkipVerify {
			c.TLSConfig = &nomadapi.TLSConfig{CACert: cfg.Nomad.CACert, Insecure: cfg.Nomad.TLSSkipVerify}
		}
		client, err := nomadapi.NewClient(c)
		if err != nil {
			return nil, err
		}
		tok, _, err := client.ACLTokens().Self((&nomadapi.QueryOptions{}).WithContext(ctx))
		if err != nil {
			return nil, err
		}
		if tok == nil {
			return nil, errors.New("token not found")
		}
		name := tok.Name
		if name == "" {
			name = tok.AccessorID[:8]
		}
		id := &auth.Identity{Username: "nomad:" + name}
		if tok.Type == "management" {
			id.Groups = append(id.Groups, "nomad:management")
		}
		for _, p := range tok.Policies {
			id.Groups = append(id.Groups, "nomad:policy:"+p)
		}
		for _, r := range tok.Roles {
			if r != nil {
				id.Groups = append(id.Groups, "nomad:role:"+r.Name)
			}
		}
		return id, nil
	}
}
