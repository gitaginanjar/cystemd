// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/internal/vaultagent"
)

// Paths on the hosts that generated files reference.
const (
	gitKeyPath       = "/etc/cystemd/secrets/git_ed25519"
	knownHostsPath   = "/etc/cystemd/git_known_hosts"
	agentRoleIDPath  = "/etc/vault-agent/role-id"
	agentSecretPath  = "/etc/vault-agent/secret-id"
	agentTokenPath   = "/etc/vault-agent/token"
	agentTemplateDir = "/etc/vault-agent/templates"
)

func comment(b *hclwrite.Body, lines ...string) {
	for _, l := range lines {
		b.AppendUnstructuredTokens(hclwrite.Tokens{{Type: hclsyntax.TokenComment, Bytes: []byte("# " + l + "\n")}})
	}
}

func stringList(vals []string) cty.Value {
	if len(vals) == 0 {
		return cty.ListValEmpty(cty.String)
	}
	out := make([]cty.Value, len(vals))
	for i, v := range vals {
		out[i] = cty.StringVal(v)
	}
	return cty.ListVal(out)
}

// hostConfig is a host's Nomad client configuration: its pools as node
// meta and the systemd driver's allowlists for its Applications.
func hostConfig(h Host, apps []*app, d dnf, o Options) ([]byte, error) {
	f := hclwrite.NewEmptyFile()
	root := f.Body()
	comment(root,
		fmt.Sprintf("CYSTEMD v4 host %s (node pools: %s), converted by `cystemd migrate`.", h.Name, strings.Join(h.Selectors, ", ")),
		"Install next to client.hcl as /etc/cystemd/agent.d/90-migrated.hcl. Nomad merges",
		"client meta, but a plugin block replaces an earlier one: this file's",
		"plugin \"systemd\" settings win over client.hcl's.")
	root.AppendNewline()
	meta := root.AppendNewBlock("client", nil).Body().AppendNewBlock("meta", nil).Body()
	for _, s := range h.Selectors {
		meta.SetAttributeValue(render.SelectorMetaKey(s), cty.StringVal("true"))
	}

	var units, pkgs, dirs []string
	seen := map[string]bool{}
	add := func(list *[]string, v string) {
		if !seen[v] {
			seen[v] = true
			*list = append(*list, v)
		}
	}
	for _, a := range apps {
		if !contains(h.Apps, a.app.Metadata.Name) {
			continue
		}
		u := unitOf(a.app)
		add(&units, u)
		add(&pkgs, strings.TrimSuffix(u, ".service"))
		add(&dirs, hostDir(a.app)+"/**")
	}
	sort.Strings(units)
	sort.Strings(pkgs)
	sort.Strings(dirs)

	root.AppendNewline()
	cfg := root.AppendNewBlock("plugin", []string{"systemd"}).Body().AppendNewBlock("config", nil).Body()
	cfg.SetAttributeValue("allowed_units", stringList(units))
	cfg.SetAttributeValue("allowed_packages", stringList(pkgs))
	cfg.SetAttributeValue("allowed_file_destinations", stringList(dirs))
	cfg.SetAttributeValue("package_manager", cty.StringVal("dnf"))
	if len(d.repos) > 0 {
		cfg.SetAttributeValue("makecache_repos", stringList(d.repos))
	}
	if d.timeout != "" {
		cfg.SetAttributeValue("makecache_timeout", cty.StringVal(d.timeout))
	}
	if o.SecretsDelivery == "vault-agent" {
		// The Vault Agent sidecars run as transient units.
		cfg.SetAttributeValue("allow_transient", cty.True)
		cfg.SetAttributeValue("transient_slice", cty.StringVal("cystemd.slice"))
	}
	return hclwrite.Format(f.Bytes()), nil
}

// vaultRoles writes, per Application with secrets, a Vault policy reading
// its v4 secret path and a JWT role binding it to the job's workload
// identity, and the script applying them.
func vaultRoles(r *Result, apps []*app, o Options) error {
	n := 0
	for _, a := range apps {
		if a.role == "" {
			continue
		}
		n++
		sd := a.app.Spec.Source.Systemd
		var p strings.Builder
		fmt.Fprintf(&p, "# Application %s (CYSTEMD v4 service %s): the secret v4 rendered into\n# %s/.env, read with the job's workload identity.\n",
			a.app.Metadata.Name, a.entry.Service, hostDir(a.app))
		for _, sec := range sd.Secrets {
			kv := sec.KVVersion
			if kv == 0 {
				kv = 2
			}
			fmt.Fprintf(&p, "path %q {\n  capabilities = [\"read\"]\n}\n", vaultagent.KVDataPath(sec.VaultPath, kv))
		}
		r.Files["vault/policies/"+a.role+".hcl"] = []byte(p.String())

		ns := a.app.Spec.Destination.NamespaceOrDefault()
		role := map[string]any{
			"role_type":               "jwt",
			"bound_audiences":         []string{"vault.io"},
			"user_claim":              "/nomad_job_id",
			"user_claim_json_pointer": true,
			"bound_claims":            map[string]string{"nomad_namespace": ns, "nomad_job_id": a.app.Metadata.Name},
			"claim_mappings":          map[string]string{"nomad_namespace": "nomad_namespace", "nomad_job_id": "nomad_job_id", "nomad_task": "nomad_task"},
			"token_type":              "service",
			"token_policies":          []string{a.role},
			"token_period":            "30m",
		}
		b, err := json.MarshalIndent(role, "", "  ")
		if err != nil {
			return err
		}
		r.Files["vault/roles/"+a.role+".json"] = append(b, '\n')
	}
	if n == 0 {
		return nil
	}
	r.Files["vault/apply.sh"] = []byte(`#!/usr/bin/env bash
# Writes the Vault policies and JWT roles of the migrated applications:
# each job reads its CYSTEMD v4 secret with its Nomad workload identity.
# Run after deploy/vault/setup.sh, with a Vault admin token:
#
#   VAULT_ADDR=... VAULT_TOKEN=... ./apply.sh
set -euo pipefail
cd "$(dirname "$0")"
: "${VAULT_ADDR:?set VAULT_ADDR}"
for p in policies/*.hcl; do
	vault policy write "$(basename "$p" .hcl)" "$p"
done
for r in roles/*.json; do
	vault write "auth/` + o.JWTAuthMount + `/role/$(basename "$r" .json)" - <"$r"
done
`)
	return nil
}

// controlPlane converts the v4 Git and auth settings; it returns the
// configuration fragment and notes about credentials to install.
func controlPlane(cfg *Legacy, env map[string]string, repo string) ([]byte, []string) {
	var notes []string
	f := hclwrite.NewEmptyFile()
	root := f.Body()
	comment(root,
		"The CYSTEMD v4 Git and auth settings, converted by `cystemd migrate`.",
		"Merge into the control plane configuration (deploy/agent/control-plane.hcl).")
	if cfg.LogLevel != "" {
		root.AppendNewline()
		root.SetAttributeValue("log_level", cty.StringVal(strings.ToLower(cfg.LogLevel)))
	}

	key := strings.TrimSpace(env["GIT_SSH_KEY_PRIVATE"])
	keyFile := ""
	switch {
	case strings.Contains(key, "-----BEGIN"):
		keyFile = gitKeyPath
		notes = append(notes, "GIT_SSH_KEY_PRIVATE holds the key inline: save it to "+gitKeyPath+
			" on the control plane hosts (mode 0600), or store it with the repository: cystemd repo add "+repo+" -ssh-private-key-path=<file>")
	case key != "":
		keyFile = key
		notes = append(notes, "copy the Git SSH key "+key+" (GIT_SSH_KEY_PRIVATE) to the control plane hosts")
	}
	known := strings.TrimSpace(env["GIT_SSH_KNOWN_HOSTS"])
	if known == "" {
		known = knownHostsPath
		notes = append(notes, "v4 did not verify SSH host keys without GIT_SSH_KNOWN_HOSTS; v5 does: "+
			"pin the Git server's keys in "+knownHostsPath+" (ssh-keyscan <git host> >> "+knownHostsPath+")")
	}
	root.AppendNewline()
	git := root.AppendNewBlock("git", nil).Body()
	if keyFile != "" {
		git.SetAttributeValue("ssh_private_key_file", cty.StringVal(keyFile))
	}
	git.SetAttributeValue("known_hosts_file", cty.StringVal(known))

	if iv := strings.TrimSpace(env["GIT_INTERVAL"]); iv != "" {
		if d, err := time.ParseDuration(iv); err == nil && d > 0 {
			root.AppendNewline()
			root.AppendNewBlock("controller", nil).Body().SetAttributeValue("reconcile_interval", cty.StringVal(d.String()))
		} else {
			notes = append(notes, fmt.Sprintf("GIT_INTERVAL %q is not a positive duration: the default reconcile interval applies", iv))
		}
	}

	auth := cfg.Auth.Enabled
	if v := strings.ToLower(strings.TrimSpace(env["AUTH_ENABLED"])); v != "" {
		auth = v == "true" || v == "yes" || v == "1"
	}
	if pk := firstNonEmpty(strings.TrimSpace(env["AUTH_PUBLIC_KEY_FILE"]), cfg.Auth.PublicKeyFile); auth && pk != "" {
		root.AppendNewline()
		a := root.AppendNewBlock("auth", nil).Body()
		comment(a, "Accept v4 Ed25519 JWTs (as role:admin) while clients move to v5 sessions.")
		a.SetAttributeValue("legacy_public_key_file", cty.StringVal(pk))
		notes = append(notes, "copy the v4 JWT public key "+pk+" to the control plane hosts")
	} else if auth && (env["AUTH_PUBLIC_KEY"] != "" || env["PUBLIC_KEY"] != "") {
		notes = append(notes, "the v4 JWT public key is inline in the .env (AUTH_PUBLIC_KEY): save it to a file and set auth.legacy_public_key_file")
	}

	if repo != "" {
		root.AppendNewline()
		rb := root.AppendNewBlock("repository", nil).Body()
		rb.SetAttributeValue("url", cty.StringVal(repo))
		rb.SetAttributeValue("name", cty.StringVal("cystemd-v4"))
		if keyFile != "" {
			rb.SetAttributeValue("ssh_private_key_file", cty.StringVal(keyFile))
		}
	}
	return hclwrite.Format(f.Bytes()), notes
}

// hostVaultAgent converts VAULT_TEMPLATES into a host Vault Agent
// configuration, with the templates rewritten for Vault Agent.
func hostVaultAgent(r *Result, cfg *Legacy, env map[string]string, o Options) error {
	specs := ParseTemplateSpecs(env["VAULT_TEMPLATES"])
	if len(specs) == 0 {
		return nil
	}
	addr := strings.TrimSpace(env["VAULT_ADDRESS"])
	if addr == "" {
		addr = "https://vault.service.consul:8200"
		r.Warnings = append(r.Warnings, "VAULT_TEMPLATES is set but VAULT_ADDRESS is not: set vault.address in vault-agent/vault-agent.hcl")
	}
	c := vaultagent.Config{Vault: vaultagent.Vault{Address: addr, CACert: o.VaultCACert}}
	switch {
	case env["VAULT_ROLE_ID"] != "" || env["VAULT_SECRET_ID"] != "" || env["VAULT_WRAPPING_TOKEN"] != "":
		c.AutoAuth = vaultagent.AutoAuth{Method: vaultagent.MethodAppRole, MountPath: "auth/approle",
			RoleIDFile: agentRoleIDPath, SecretIDFile: agentSecretPath}
	default:
		c.AutoAuth = vaultagent.AutoAuth{Method: vaultagent.MethodTokenFile, TokenFilePath: agentTokenPath}
		r.Warnings = append(r.Warnings, "the host Vault Agent logs in with a token file ("+agentTokenPath+
			") because the .env has no AppRole credentials; prefer AppRole (deploy/vault/setup.sh creates cystemd-host)")
	}
	if strings.EqualFold(strings.TrimSpace(env["VAULT_SKIP_VERIFY"]), "true") || strings.TrimSpace(env["VAULT_SKIP_VERIFY"]) == "" {
		if o.VaultCACert == "" {
			r.Warnings = append(r.Warnings, "v4 skipped Vault TLS verification by default; the Vault Agent verifies it: "+
				"set vault.ca_cert in vault-agent/vault-agent.hcl if Vault's certificate is not issued by a system CA")
		}
	}
	interval := firstNonEmpty(strings.TrimSpace(env["VAULT_REFRESH_INTERVAL"]), cfg.Vault.RefreshInterval)
	if d, err := time.ParseDuration(interval); err == nil && d > 0 {
		c.StaticSecretRenderInterval = d.String()
	}

	used := map[string]bool{}
	for _, s := range specs {
		name := strings.TrimSuffix(filepath.Base(s.Source), filepath.Ext(s.Source))
		file := name + ".ctmpl"
		for n := 2; used[file]; n++ {
			file = fmt.Sprintf("%s-%d.ctmpl", name, n)
		}
		used[file] = true
		c.Templates = append(c.Templates, vaultagent.Template{
			Source:            path.Join(agentTemplateDir, file),
			Destination:       s.Dest,
			Command:           s.Reload,
			ErrorOnMissingKey: true,
		})
		src, err := o.ReadFile(s.Source)
		if err != nil && o.TemplatesDir != "" {
			src, err = o.ReadFile(filepath.Join(o.TemplatesDir, filepath.Base(s.Source)))
		}
		if err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("template %s: %v; convert it by hand into vault-agent/templates/%s (see MIGRATION.md)", s.Source, err, file))
			continue
		}
		out, notes := ConvertTemplate(string(src), o.KVVersion)
		for _, n := range notes {
			r.Warnings = append(r.Warnings, "template "+s.Source+": "+n)
		}
		r.Files["vault-agent/templates/"+file] = []byte(out)
	}
	b, err := c.HCL()
	if err != nil {
		return err
	}
	header := "# The host Vault Agent rendering CYSTEMD v4's VAULT_TEMPLATES, converted by\n" +
		"# `cystemd migrate`. Install as /etc/vault-agent/vault-agent.hcl with the\n" +
		"# templates in " + agentTemplateDir + " (deploy/systemd/vault-agent.service).\n\n"
	r.Files["vault-agent/vault-agent.hcl"] = append([]byte(header), b...)
	return nil
}

// TemplateSpec is a VAULT_TEMPLATES entry.
type TemplateSpec struct {
	Source, Dest, Reload string
}

// ParseTemplateSpecs parses VAULT_TEMPLATES as v4 did: entries split on ';'
// or newlines, each "source:destination[:reload]" split on its first two
// colons; malformed entries are skipped.
func ParseTemplateSpecs(raw string) []TemplateSpec {
	var out []TemplateSpec
	for _, e := range strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' }) {
		src, rest, ok := strings.Cut(strings.TrimSpace(e), ":")
		if !ok {
			continue
		}
		dest, reload, _ := strings.Cut(rest, ":")
		s := TemplateSpec{Source: strings.TrimSpace(src), Dest: strings.TrimSpace(dest), Reload: strings.TrimSpace(reload)}
		if s.Source != "" && s.Dest != "" {
			out = append(out, s)
		}
	}
	return out
}
