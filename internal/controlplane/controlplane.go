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
	"slices"
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
		CACert: cfg.Nomad.CACert, ClientCert: cfg.Nomad.ClientCert, ClientKey: cfg.Nomad.ClientKey,
		ServerName: cfg.Nomad.TLSServerName, TLSSkip: cfg.Nomad.TLSSkipVerify})
	if err != nil {
		return nil, err
	}
	clusters.Add(v1.InClusterName, in)
	for _, cl := range cfg.Clusters {
		tok, err := config.Secret(cl.Token, cl.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("cluster %s token: %w", cl.Name, err)
		}
		c, err := nomad.New(nomad.Config{Address: cl.Address, Region: cl.Region, Token: tok, CACert: cl.CACert,
			ClientCert: cl.ClientCert, ClientKey: cl.ClientKey, ServerName: cl.TLSServerName, TLSSkip: cl.TLSSkipVerify})
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
	declared := make([]auth.DeclaredAccount, 0, len(cfg.Auth.Accounts))
	for _, a := range cfg.Auth.Accounts {
		declared = append(declared, auth.DeclaredAccount{Name: a.Name, Capabilities: a.Capabilities, Enabled: *a.Enabled})
	}
	if err := am.SyncAccounts(ctx, declared); err != nil {
		return nil, fmt.Errorf("accounts: %w", err)
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

	live := &controller.Live{Clusters: clusters, Consul: consulKV, Checks: checks, Repos: repos, Apps: cp.Store}
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
	var sso server.SSO
	if cfg.SSOEnabled() {
		client, err := inClusterNomad(cfg, "")
		if err != nil {
			return nil, fmt.Errorf("sso: %w", err)
		}
		sso = &nomadSSO{client: client, only: cfg.Auth.SSO.AuthMethods}
	}
	cp.Server = server.New(server.Options{
		Config: cfg, Store: cp.Store, Auth: am, RBAC: enforcer, Live: live, Repos: repos, Git: gitClient,
		Clusters: clusters, Vault: vc, Consul: consulClient, Metrics: m, Elector: cp.Elector,
		Logger: logger, Build: o.Build, SSO: sso,
	})
	return cp, nil
}

// nomadSSO is single sign-on through the in-cluster Nomad's OIDC auth
// methods. Their endpoints are public: no token is sent.
type nomadSSO struct {
	client *nomadapi.Client
	// only restricts and orders the auth methods offered.
	only []string
}

func (n *nomadSSO) Methods(ctx context.Context) ([]string, error) {
	list, _, err := n.client.ACLAuthMethods().List((&nomadapi.QueryOptions{}).WithContext(ctx))
	if err != nil {
		return nil, err
	}
	var oidc []string
	for _, m := range list {
		if m.Type != "OIDC" {
			continue
		}
		if m.Default {
			oidc = append([]string{m.Name}, oidc...)
		} else {
			oidc = append(oidc, m.Name)
		}
	}
	if len(n.only) == 0 {
		return oidc, nil
	}
	var out []string
	for _, name := range n.only {
		if slices.Contains(oidc, name) {
			out = append(out, name)
		}
	}
	return out, nil
}

func (n *nomadSSO) AuthURL(ctx context.Context, method, redirectURI, nonce string) (string, error) {
	resp, _, err := n.client.ACLAuth().GetAuthURL(&nomadapi.ACLOIDCAuthURLRequest{
		AuthMethodName: method, RedirectURI: redirectURI, ClientNonce: nonce,
	}, (&nomadapi.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return "", err
	}
	return resp.AuthURL, nil
}

func (n *nomadSSO) Complete(ctx context.Context, cb server.SSOCallback) (*auth.NomadToken, error) {
	tok, _, err := n.client.ACLAuth().CompleteAuth(&nomadapi.ACLOIDCCompleteAuthRequest{
		AuthMethodName: cb.AuthMethod, ClientNonce: cb.ClientNonce, State: cb.State, Code: cb.Code,
		Iss: cb.Iss, RedirectURI: cb.RedirectURI,
	}, (&nomadapi.WriteOptions{}).WithContext(ctx))
	if err != nil {
		return nil, err
	}
	return nomadTokenOf(tok), nil
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

// inClusterNomad is a client of the in-cluster Nomad with the given token
// (none for the public endpoints).
func inClusterNomad(cfg *config.Config, token string) (*nomadapi.Client, error) {
	c := nomadapi.DefaultConfig()
	c.Address = cfg.Nomad.Address
	c.Region = cfg.Nomad.Region
	c.SecretID = token
	n := cfg.Nomad
	if n.CACert != "" || n.ClientCert != "" || n.TLSServerName != "" || n.TLSSkipVerify {
		c.TLSConfig = &nomadapi.TLSConfig{CACert: n.CACert, ClientCert: n.ClientCert, ClientKey: n.ClientKey,
			TLSServerName: n.TLSServerName, Insecure: n.TLSSkipVerify}
	}
	return nomadapi.NewClient(c)
}

// nomadTokenResolver validates a Nomad ACL token against the in-cluster
// Nomad.
func nomadTokenResolver(cfg *config.Config) auth.NomadTokenResolver {
	return func(ctx context.Context, token string) (*auth.NomadToken, error) {
		client, err := inClusterNomad(cfg, token)
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
		return nomadTokenOf(tok), nil
	}
}

func nomadTokenOf(tok *nomadapi.ACLToken) *auth.NomadToken {
	nt := &auth.NomadToken{Name: tok.Name, AccessorID: tok.AccessorID, Management: tok.Type == "management", Policies: tok.Policies}
	for _, r := range tok.Roles {
		if r != nil {
			nt.Roles = append(nt.Roles, r.Name)
		}
	}
	return nt
}
