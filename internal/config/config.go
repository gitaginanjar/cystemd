// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package config loads the control plane configuration (HCL). The same file
// serves the standalone cystemd-server and the control plane embedded in a
// cystemd server agent (`cystemd { control_plane { config_file = … } }`).
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Config is the control plane configuration.
type Config struct {
	// BindAddr is the standalone server's listen address (ignored when
	// embedded in an agent, which serves on its own listeners).
	BindAddr string `hcl:"bind_addr,optional"`
	// URL is the external URL (links, badges, login redirects).
	URL      string `hcl:"url,optional"`
	DataDir  string `hcl:"data_dir,optional"`
	LogLevel string `hcl:"log_level,optional"`
	LogJSON  bool   `hcl:"log_json,optional"`
	// TLS for the standalone listener.
	TLSCertFile string `hcl:"tls_cert_file,optional"`
	TLSKeyFile  string `hcl:"tls_key_file,optional"`

	Nomad        *NomadConfig       `hcl:"nomad,block"`
	Clusters     []*ClusterConfig   `hcl:"cluster,block"`
	Consul       *ConsulConfig      `hcl:"consul,block"`
	Vault        *VaultConfig       `hcl:"vault,block"`
	Git          *GitConfig         `hcl:"git,block"`
	Controller   *ControllerConfig  `hcl:"controller,block"`
	Auth         *AuthConfig        `hcl:"auth,block"`
	RBAC         *RBACConfig        `hcl:"rbac,block"`
	Repositories []*RepositoryBlock `hcl:"repository,block"`
}

// NomadConfig connects to the in-cluster Nomad (destination "in-cluster").
type NomadConfig struct {
	Address       string `hcl:"address,optional"`
	Region        string `hcl:"region,optional"`
	Token         string `hcl:"token,optional"`
	TokenFile     string `hcl:"token_file,optional"`
	CACert        string `hcl:"ca_cert,optional"`
	ClientCert    string `hcl:"client_cert,optional"`
	ClientKey     string `hcl:"client_key,optional"`
	TLSServerName string `hcl:"tls_server_name,optional"`
	TLSSkipVerify bool   `hcl:"tls_skip_verify,optional"`
}

// ClusterConfig registers an additional Nomad cluster as a destination.
type ClusterConfig struct {
	Name          string `hcl:"name,label"`
	Address       string `hcl:"address"`
	Region        string `hcl:"region,optional"`
	Token         string `hcl:"token,optional"`
	TokenFile     string `hcl:"token_file,optional"`
	CACert        string `hcl:"ca_cert,optional"`
	ClientCert    string `hcl:"client_cert,optional"`
	ClientKey     string `hcl:"client_key,optional"`
	TLSServerName string `hcl:"tls_server_name,optional"`
	TLSSkipVerify bool   `hcl:"tls_skip_verify,optional"`
}

// ConsulConfig enables the Consul store, leader election and KV resources.
type ConsulConfig struct {
	Enabled    *bool  `hcl:"enabled,optional"`
	Address    string `hcl:"address,optional"`
	Token      string `hcl:"token,optional"`
	TokenFile  string `hcl:"token_file,optional"`
	Datacenter string `hcl:"datacenter,optional"`
	KVPrefix   string `hcl:"kv_prefix,optional"`
}

// VaultConfig is how the control plane reaches Vault. Prefer AgentAddress:
// a Vault Agent's API proxy (use_auto_auth_token) does authentication and
// renewal, so the control plane never holds a long-lived token.
type VaultConfig struct {
	Enabled      *bool  `hcl:"enabled,optional"`
	AgentAddress string `hcl:"agent_address,optional"`
	Address      string `hcl:"address,optional"`
	TokenFile    string `hcl:"token_file,optional"`
	Namespace    string `hcl:"namespace,optional"`
	KVMount      string `hcl:"kv_mount,optional"`
	// RepoCredsPath stores repository credentials (under KVMount).
	RepoCredsPath string `hcl:"repo_creds_path,optional"`
	// WorkloadRole is the default Vault role of generated jobs.
	WorkloadRole string `hcl:"workload_role,optional"`
	// WorkloadAddress and JWTAuthPath configure generated Vault Agents.
	WorkloadAddress string `hcl:"workload_address,optional"`
	JWTAuthPath     string `hcl:"jwt_auth_path,optional"`
}

// GitConfig configures repository access.
type GitConfig struct {
	KnownHostsFile    string `hcl:"known_hosts_file,optional"`
	SSHPrivateKeyFile string `hcl:"ssh_private_key_file,optional"`
	FetchTimeout      string `hcl:"fetch_timeout,optional"`
}

// ControllerConfig tunes the application controller.
type ControllerConfig struct {
	Enabled           *bool  `hcl:"enabled,optional"`
	ReconcileInterval string `hcl:"reconcile_interval,optional"`
	SelfHealTimeout   string `hcl:"self_heal_timeout,optional"`
	Workers           int    `hcl:"workers,optional"`
}

// AuthConfig configures login.
type AuthConfig struct {
	AdminEnabled    *bool  `hcl:"admin_enabled,optional"`
	SessionDuration string `hcl:"session_duration,optional"`
	// NomadTokenLogin lets users log in with a Nomad ACL token.
	NomadTokenLogin *bool `hcl:"nomad_token_login,optional"`
	// AnonymousRole grants unauthenticated users a role (empty: none).
	AnonymousRole string `hcl:"anonymous_role,optional"`
	// LegacyPublicKeyFile accepts CYSTEMD v4 Ed25519 JWTs (role:admin).
	LegacyPublicKeyFile string `hcl:"legacy_public_key_file,optional"`
	// AdminPasswordFile presets the admin password (bcrypt hash or plain).
	AdminPasswordFile string `hcl:"admin_password_file,optional"`
}

// RBACConfig holds the Casbin-style policy.
type RBACConfig struct {
	DefaultRole string `hcl:"default_role,optional"`
	Policy      string `hcl:"policy,optional"`
	PolicyFile  string `hcl:"policy_file,optional"`
}

// RepositoryBlock declares a repository statically.
type RepositoryBlock struct {
	URL               string `hcl:"url"`
	Name              string `hcl:"name,optional"`
	Username          string `hcl:"username,optional"`
	PasswordFile      string `hcl:"password_file,optional"`
	SSHPrivateKeyFile string `hcl:"ssh_private_key_file,optional"`
	Insecure          bool   `hcl:"insecure,optional"`
}

// Default returns the configuration used when no file is given: a
// self-contained development control plane (memory store, local Nomad).
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

// Load parses an HCL configuration file and applies defaults and
// environment overrides (NOMAD_ADDR, NOMAD_TOKEN, CONSUL_HTTP_ADDR,
// CONSUL_HTTP_TOKEN, VAULT_AGENT_ADDR, VAULT_ADDR).
func Load(path string) (*Config, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(src, path)
}

// Parse parses HCL source.
func Parse(src []byte, filename string) (*Config, error) {
	return ParseEmbedded(src, filename, nil)
}

// EmbeddedDefaults are what the cystemd agent derives from its own
// configuration for the control plane it embeds. The control-plane file
// overrides every one of them.
type EmbeddedDefaults struct {
	// DataDir is a directory under the agent's data_dir.
	DataDir string
	// Nomad is how to reach the agent's own HTTP API.
	NomadAddress    string
	NomadRegion     string
	NomadCACert     string
	NomadClientCert string
	NomadClientKey  string
	// ConsulAddress and ConsulToken come from the agent's consul block;
	// they apply when the file enables Consul without setting them.
	ConsulAddress string
	ConsulToken   string
}

// ParseEmbedded parses HCL source (empty for none), filling what it leaves
// unset from the agent first, then from the environment and defaults.
func ParseEmbedded(src []byte, filename string, d *EmbeddedDefaults) (*Config, error) {
	var c Config
	if len(src) > 0 {
		f, diags := hclsyntax.ParseConfig(src, filename, hcl.InitialPos)
		if diags.HasErrors() {
			return nil, errors.New(diags.Error())
		}
		if diags := gohcl.DecodeBody(f.Body, nil, &c); diags.HasErrors() {
			return nil, errors.New(diags.Error())
		}
	}
	if d != nil {
		c.applyEmbedded(d)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyEmbedded(d *EmbeddedDefaults) {
	if c.DataDir == "" {
		c.DataDir = d.DataDir
	}
	if c.Nomad == nil {
		c.Nomad = &NomadConfig{}
	}
	if c.Nomad.Address == "" {
		c.Nomad.Address = d.NomadAddress
		if c.Nomad.CACert == "" {
			c.Nomad.CACert = d.NomadCACert
		}
		if c.Nomad.ClientCert == "" && c.Nomad.ClientKey == "" {
			c.Nomad.ClientCert, c.Nomad.ClientKey = d.NomadClientCert, d.NomadClientKey
		}
	}
	if c.Nomad.Region == "" {
		c.Nomad.Region = d.NomadRegion
	}
	if c.Consul != nil && (c.Consul.Enabled == nil || *c.Consul.Enabled) {
		if c.Consul.Address == "" {
			c.Consul.Address = d.ConsulAddress
		}
		if c.Consul.Token == "" && c.Consul.TokenFile == "" {
			c.Consul.Token = d.ConsulToken
		}
	}
}

func boolPtr(b bool) *bool { return &b }

func (c *Config) applyDefaults() {
	if c.BindAddr == "" {
		c.BindAddr = "0.0.0.0:8080"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/cystemd"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.Nomad == nil {
		c.Nomad = &NomadConfig{}
	}
	if v := os.Getenv("NOMAD_ADDR"); v != "" && c.Nomad.Address == "" {
		c.Nomad.Address = v
	}
	if v := os.Getenv("NOMAD_TOKEN"); v != "" && c.Nomad.Token == "" && c.Nomad.TokenFile == "" {
		c.Nomad.Token = v
	}
	if c.Nomad.Address == "" {
		c.Nomad.Address = "http://127.0.0.1:4646"
	}
	if c.Consul == nil {
		c.Consul = &ConsulConfig{Enabled: boolPtr(false)}
		if v := os.Getenv("CONSUL_HTTP_ADDR"); v != "" {
			c.Consul = &ConsulConfig{Enabled: boolPtr(true), Address: v}
		}
	}
	if c.Consul.Enabled == nil {
		c.Consul.Enabled = boolPtr(true)
	}
	if c.Consul.Address == "" {
		c.Consul.Address = orEnv("CONSUL_HTTP_ADDR", "127.0.0.1:8500")
	}
	if c.Consul.Token == "" && c.Consul.TokenFile == "" {
		c.Consul.Token = os.Getenv("CONSUL_HTTP_TOKEN")
	}
	if c.Consul.KVPrefix == "" {
		c.Consul.KVPrefix = "cystemd"
	}
	if c.Vault == nil {
		c.Vault = &VaultConfig{Enabled: boolPtr(false)}
		if v := os.Getenv("VAULT_AGENT_ADDR"); v != "" {
			c.Vault = &VaultConfig{Enabled: boolPtr(true), AgentAddress: v}
		} else if v := os.Getenv("VAULT_ADDR"); v != "" {
			c.Vault = &VaultConfig{Enabled: boolPtr(true), Address: v}
		}
	}
	if c.Vault.Enabled == nil {
		c.Vault.Enabled = boolPtr(true)
	}
	if c.Vault.KVMount == "" {
		c.Vault.KVMount = "secret"
	}
	if c.Vault.RepoCredsPath == "" {
		c.Vault.RepoCredsPath = "cystemd/repositories"
	}
	if c.Vault.WorkloadRole == "" {
		c.Vault.WorkloadRole = "cystemd-workloads"
	}
	if c.Vault.JWTAuthPath == "" {
		c.Vault.JWTAuthPath = "auth/jwt-nomad"
	}
	if c.Git == nil {
		c.Git = &GitConfig{}
	}
	if c.Controller == nil {
		c.Controller = &ControllerConfig{}
	}
	if c.Controller.Enabled == nil {
		c.Controller.Enabled = boolPtr(true)
	}
	if c.Auth == nil {
		c.Auth = &AuthConfig{}
	}
	if c.Auth.AdminEnabled == nil {
		c.Auth.AdminEnabled = boolPtr(true)
	}
	if c.Auth.NomadTokenLogin == nil {
		c.Auth.NomadTokenLogin = boolPtr(true)
	}
	if c.RBAC == nil {
		c.RBAC = &RBACConfig{}
	}
	if c.RBAC.DefaultRole == "" {
		c.RBAC.DefaultRole = "role:readonly"
	}
}

// Validate checks values that cannot be defaulted.
func (c *Config) Validate() error {
	var errs []error
	for name, d := range map[string]string{
		"controller.reconcile_interval": c.Controller.ReconcileInterval,
		"controller.self_heal_timeout":  c.Controller.SelfHealTimeout,
		"auth.session_duration":         c.Auth.SessionDuration,
		"git.fetch_timeout":             c.Git.FetchTimeout,
	} {
		if d == "" {
			continue
		}
		if v, err := time.ParseDuration(d); err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s: %q is not a positive duration", name, d))
		}
	}
	seen := map[string]bool{"in-cluster": true}
	for _, cl := range c.Clusters {
		if seen[cl.Name] {
			errs = append(errs, fmt.Errorf("cluster %q is defined twice (in-cluster is reserved)", cl.Name))
		}
		seen[cl.Name] = true
	}
	if (c.TLSCertFile == "") != (c.TLSKeyFile == "") {
		errs = append(errs, errors.New("tls_cert_file and tls_key_file go together"))
	}
	if (c.Nomad.ClientCert == "") != (c.Nomad.ClientKey == "") {
		errs = append(errs, errors.New("nomad: client_cert and client_key go together"))
	}
	return errors.Join(errs...)
}

// ConsulEnabled reports whether Consul is used.
func (c *Config) ConsulEnabled() bool {
	return c.Consul != nil && c.Consul.Enabled != nil && *c.Consul.Enabled
}

// VaultEnabled reports whether Vault is used.
func (c *Config) VaultEnabled() bool {
	return c.Vault != nil && c.Vault.Enabled != nil && *c.Vault.Enabled && (c.Vault.AgentAddress != "" || c.Vault.Address != "")
}

// Duration parses d, falling back to def.
func Duration(d string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(d); err == nil && v > 0 {
		return v
	}
	return def
}

// Secret returns value, or the trimmed content of file when value is empty.
func Secret(value, file string) (string, error) {
	if value != "" || file == "" {
		return value, nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func orEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
