// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package vaultagent generates HashiCorp Vault Agent configurations.
//
// CYSTEMD delegates every Vault concern to a real Vault Agent instead of
// embedding one (CYSTEMD v4 ran its own AppRole login, renewal and
// templating loop in-process): auto-auth and token renewal, an API proxy the
// control plane calls without holding a token, and templates that render
// secrets to files and run a command when they change. This package writes
// those configurations (host agents, `cystemd migrate`, the deployment
// examples) with correct HCL quoting, so template text is never mangled.
package vaultagent

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// Config is a Vault Agent configuration: the subset CYSTEMD uses.
type Config struct {
	Vault    Vault
	AutoAuth AutoAuth
	// APIProxy serves the Vault API on a local listener, adding the
	// auto-auth token to requests that carry none (use_auto_auth_token).
	APIProxy *APIProxy
	// Sinks write the auto-auth token to files (for tools that need it).
	Sinks     []FileSink
	Templates []Template
	// ExitAfterAuth makes a one-shot agent: authenticate, render, exit.
	ExitAfterAuth bool
	// ExitOnRetryFailure stops the agent when a template keeps failing,
	// so a supervisor notices (template_config).
	ExitOnRetryFailure bool
	// StaticSecretRenderInterval re-reads KV secrets that have no lease.
	StaticSecretRenderInterval string
	PIDFile                    string
}

// Vault is the Vault server the agent talks to.
type Vault struct {
	Address       string
	CACert        string
	ClientCert    string
	ClientKey     string
	TLSServerName string
	Namespace     string
}

// Auto-auth methods.
const (
	MethodAppRole   = "approle"
	MethodJWT       = "jwt"
	MethodCert      = "cert"
	MethodTokenFile = "token_file"
)

// AutoAuth is how the agent logs in.
type AutoAuth struct {
	Method    string
	MountPath string // e.g. auth/approle, auth/jwt-nomad

	// AppRole: credentials read from files.
	RoleIDFile                     string
	SecretIDFile                   string
	RemoveSecretIDFileAfterReading bool

	// JWT: a token file (a Nomad workload identity) and the role.
	JWTPath string
	Role    string

	// Cert: the role name of the cert auth method (TLS client cert from
	// Vault.ClientCert).
	CertName string

	// TokenFile: a token read from a file (development).
	TokenFilePath string
}

// APIProxy is the agent's local Vault API listener.
type APIProxy struct {
	// Address is host:port, e.g. 127.0.0.1:8100.
	Address string
	// TLSCertFile and TLSKeyFile serve the listener over TLS; without them
	// it is plain HTTP (bind it to loopback).
	TLSCertFile string
	TLSKeyFile  string
}

// FileSink writes the auto-auth token to a file.
type FileSink struct {
	Path string
	Mode int // octal permission bits, default 0600
}

// Template renders a secret to a file.
type Template struct {
	// Source is a template file; Contents an inline template (one of).
	Source   string
	Contents string
	// Destination is the rendered file.
	Destination string
	// Perms is the file mode, default "0600".
	Perms string
	// Command runs when the rendered file changes (e.g. a reload).
	Command string
	// ErrorOnMissingKey fails the render when a key is absent, instead of
	// writing "<no value>" (the CYSTEMD v4 fail-closed behaviour).
	ErrorOnMissingKey bool
}

// Validate checks the configuration is complete.
func (c *Config) Validate() error {
	var errs []error
	if c.Vault.Address == "" {
		errs = append(errs, errors.New("vault address is required"))
	}
	if (c.Vault.ClientCert == "") != (c.Vault.ClientKey == "") {
		errs = append(errs, errors.New("vault client_cert and client_key go together"))
	}
	a := c.AutoAuth
	switch a.Method {
	case MethodAppRole:
		if a.RoleIDFile == "" || a.SecretIDFile == "" {
			errs = append(errs, errors.New("approle auto-auth needs role_id_file_path and secret_id_file_path"))
		}
	case MethodJWT:
		if a.JWTPath == "" || a.Role == "" {
			errs = append(errs, errors.New("jwt auto-auth needs the token path and the role"))
		}
	case MethodCert:
		if c.Vault.ClientCert == "" {
			errs = append(errs, errors.New("cert auto-auth needs the vault client certificate"))
		}
	case MethodTokenFile:
		if a.TokenFilePath == "" {
			errs = append(errs, errors.New("token_file auto-auth needs the token file path"))
		}
	default:
		errs = append(errs, fmt.Errorf("unknown auto-auth method %q", a.Method))
	}
	if p := c.APIProxy; p != nil {
		if p.Address == "" {
			errs = append(errs, errors.New("api proxy listener address is required"))
		}
		if (p.TLSCertFile == "") != (p.TLSKeyFile == "") {
			errs = append(errs, errors.New("api proxy tls cert and key go together"))
		}
	}
	for i, s := range c.Sinks {
		if s.Path == "" {
			errs = append(errs, fmt.Errorf("sink %d: path is required", i))
		}
	}
	for i, t := range c.Templates {
		if (t.Source == "") == (t.Contents == "") {
			errs = append(errs, fmt.Errorf("template %d: set exactly one of source and contents", i))
		}
		if t.Destination == "" {
			errs = append(errs, fmt.Errorf("template %d: destination is required", i))
		}
	}
	return errors.Join(errs...)
}

// HCL renders the configuration.
func (c *Config) HCL() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	f := hclwrite.NewEmptyFile()
	root := f.Body()
	if c.PIDFile != "" {
		root.SetAttributeValue("pid_file", cty.StringVal(c.PIDFile))
	}
	if c.ExitAfterAuth {
		root.SetAttributeValue("exit_after_auth", cty.True)
	}

	v := root.AppendNewBlock("vault", nil).Body()
	setStrings(v, "address", c.Vault.Address, "ca_cert", c.Vault.CACert, "client_cert", c.Vault.ClientCert,
		"client_key", c.Vault.ClientKey, "tls_server_name", c.Vault.TLSServerName, "namespace", c.Vault.Namespace)

	root.AppendNewline()
	aa := root.AppendNewBlock("auto_auth", nil).Body()
	m := aa.AppendNewBlock("method", []string{c.AutoAuth.Method}).Body()
	if c.AutoAuth.MountPath != "" {
		m.SetAttributeValue("mount_path", cty.StringVal(c.AutoAuth.MountPath))
	}
	if cfg := c.AutoAuth.methodConfig(); len(cfg) > 0 {
		m.SetAttributeValue("config", cty.ObjectVal(cfg))
	}
	for _, s := range c.Sinks {
		mode := s.Mode
		if mode == 0 {
			mode = 0o600
		}
		sb := aa.AppendNewBlock("sink", []string{"file"}).Body()
		sb.SetAttributeValue("config", cty.ObjectVal(map[string]cty.Value{
			"path": cty.StringVal(s.Path),
			"mode": cty.NumberIntVal(int64(mode)),
		}))
	}

	if p := c.APIProxy; p != nil {
		root.AppendNewline()
		root.AppendNewBlock("api_proxy", nil).Body().SetAttributeValue("use_auto_auth_token", cty.True)
		root.AppendNewline()
		l := root.AppendNewBlock("listener", []string{"tcp"}).Body()
		l.SetAttributeValue("address", cty.StringVal(p.Address))
		if p.TLSCertFile == "" {
			l.SetAttributeValue("tls_disable", cty.True)
		} else {
			setStrings(l, "tls_cert_file", p.TLSCertFile, "tls_key_file", p.TLSKeyFile)
		}
	}

	if c.ExitOnRetryFailure || c.StaticSecretRenderInterval != "" {
		root.AppendNewline()
		tc := root.AppendNewBlock("template_config", nil).Body()
		if c.ExitOnRetryFailure {
			tc.SetAttributeValue("exit_on_retry_failure", cty.True)
		}
		if c.StaticSecretRenderInterval != "" {
			tc.SetAttributeValue("static_secret_render_interval", cty.StringVal(c.StaticSecretRenderInterval))
		}
	}
	for _, t := range c.Templates {
		root.AppendNewline()
		tb := root.AppendNewBlock("template", nil).Body()
		setStrings(tb, "source", t.Source, "contents", t.Contents, "destination", t.Destination)
		perms := t.Perms
		if perms == "" {
			perms = "0600"
		}
		tb.SetAttributeValue("perms", cty.StringVal(perms))
		if t.Command != "" {
			tb.SetAttributeValue("command", cty.StringVal(t.Command))
		}
		if t.ErrorOnMissingKey {
			tb.SetAttributeValue("error_on_missing_key", cty.True)
		}
	}
	return hclwrite.Format(f.Bytes()), nil
}

func (a AutoAuth) methodConfig() map[string]cty.Value {
	cfg := map[string]cty.Value{}
	switch a.Method {
	case MethodAppRole:
		cfg["role_id_file_path"] = cty.StringVal(a.RoleIDFile)
		cfg["secret_id_file_path"] = cty.StringVal(a.SecretIDFile)
		cfg["remove_secret_id_file_after_reading"] = cty.BoolVal(a.RemoveSecretIDFileAfterReading)
	case MethodJWT:
		cfg["path"] = cty.StringVal(a.JWTPath)
		cfg["role"] = cty.StringVal(a.Role)
		cfg["remove_jwt_after_reading"] = cty.False
	case MethodCert:
		if a.CertName != "" {
			cfg["name"] = cty.StringVal(a.CertName)
		}
	case MethodTokenFile:
		cfg["token_file_path"] = cty.StringVal(a.TokenFilePath)
	}
	return cfg
}

// setStrings sets string attributes, skipping empty values. kv alternates
// names and values.
func setStrings(b *hclwrite.Body, kv ...string) {
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			b.SetAttributeValue(kv[i], cty.StringVal(kv[i+1]))
		}
	}
}

// KVDataPath turns a logical KV path ("secret/app") into the API path the
// secret function reads: KV v2 inserts "data/" after the mount.
func KVDataPath(logical string, kvVersion int) string {
	logical = strings.Trim(logical, "/")
	if kvVersion == 1 {
		return logical
	}
	mount, rest, ok := strings.Cut(logical, "/")
	if !ok {
		return logical
	}
	if strings.HasPrefix(rest, "data/") {
		return logical
	}
	return mount + "/data/" + rest
}

// EnvTemplate renders every key of a KV secret as KEY="value" lines
// (JSON-quoted values), the format of systemd EnvironmentFile= and of the
// CYSTEMD v4 .env files.
func EnvTemplate(logical string, kvVersion int) string {
	data := ".Data.data"
	if kvVersion == 1 {
		data = ".Data"
	}
	return fmt.Sprintf("{{- with secret %q }}{{ range $k, $v := %s }}{{ $k }}={{ $v | toJSON }}\n{{ end }}{{ end }}",
		KVDataPath(logical, kvVersion), data)
}

// SortTemplates orders templates by destination (stable output).
func SortTemplates(ts []Template) {
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].Destination < ts[j].Destination })
}
