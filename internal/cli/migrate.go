// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gitaginanjar/cystemd/internal/migrate"
)

type migrateCommand struct{ *meta }

func (c *migrateCommand) Synopsis() string { return "Convert a CYSTEMD v4 config.yml and .env" }
func (c *migrateCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` migrate [options] -config=<config.yml>

  Converts a CYSTEMD v4 installation into v5 configuration, written to a
  directory with a report (MIGRATION.md) of what to install where:

    applications.yaml       an Application per continuous_deployment entry
    hosts/<host>.hcl        each host's node pools and driver allowlists
    control-plane.hcl       the repository, Git, reconcile and auth settings
    vault/                  Vault policies and JWT roles for the secrets
    vault-agent/            a host Vault Agent for VAULT_TEMPLATES

  Credentials are never copied. Nothing is changed on any server unless
  -apply is given.

Options:

  -config=<path>           The v4 config.yml (required).
  -env=<path>              The v4 .env (GIT_*, VAULT_*, AUTH_*, CYSTEMD_DNF_*).
  -out=<dir>               Output directory (default cystemd-migration).
  -force                   Write into a directory that is not empty.
  -repo=<url>              The GitOps repository (default GIT_REPOSITORY).
  -project=<name>          Project of the applications (default "default").
  -dest-name=<cluster>     Destination cluster (default in-cluster).
  -dest-namespace=<ns>     Nomad namespace (default "default").
  -kv-version=<1|2>        KV version of the secrets' mounts (default 2).
  -secrets-delivery=<how>  nomad (default) or vault-agent.
  -vault-ca-cert=<path>    CA certificate the host Vault Agent trusts.
  -templates-dir=<dir>     Where to find VAULT_TEMPLATES sources copied
                           from a host (matched by file name).
  -apply                   Also create the applications on the server
                           (with -upsert, replace existing ones).
  -upsert                  With -apply: replace existing applications.
` + c.connectionHelp()
}

func (c *migrateCommand) Run(args []string) int {
	var configPath, envPath, out, templatesDir string
	var force, apply, upsert bool
	var o migrate.Options
	fs := c.flagSet("migrate", true)
	fs.StringVar(&configPath, "config", "", "")
	fs.StringVar(&envPath, "env", "", "")
	fs.StringVar(&out, "out", "cystemd-migration", "")
	fs.BoolVar(&force, "force", false, "")
	fs.StringVar(&o.RepoURL, "repo", "", "")
	fs.StringVar(&o.Project, "project", "", "")
	fs.StringVar(&o.DestName, "dest-name", "", "")
	fs.StringVar(&o.DestNamespace, "dest-namespace", "", "")
	fs.IntVar(&o.KVVersion, "kv-version", 2, "")
	fs.StringVar(&o.SecretsDelivery, "secrets-delivery", "", "")
	fs.StringVar(&o.VaultCACert, "vault-ca-cert", "", "")
	fs.StringVar(&templatesDir, "templates-dir", "", "")
	fs.BoolVar(&apply, "apply", false, "")
	fs.BoolVar(&upsert, "upsert", false, "")
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" migrate [options] -config=<config.yml>"); !ok {
		return 1
	}
	if configPath == "" {
		c.ui.Error("-config is required. Usage: " + c.opts.Name + " migrate [options] -config=<config.yml>")
		return 1
	}
	o.TemplatesDir = templatesDir

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return c.fail(err)
	}
	legacy, warnings, err := migrate.ParseConfig(raw)
	if err != nil {
		return c.fail(err)
	}
	env := map[string]string{}
	if envPath != "" {
		b, err := os.ReadFile(envPath)
		if err != nil {
			return c.fail(err)
		}
		env = migrate.ParseEnv(b)
	}
	res, err := migrate.Convert(legacy, env, o)
	if err != nil {
		return c.fail(err)
	}
	res.Warnings = append(warnings, res.Warnings...)

	if err := writeTree(out, res.Files, force); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Wrote %d applications, %d hosts and %d files to %s; read %s.",
		len(res.Applications), len(res.Hosts), len(res.Files), out, filepath.Join(out, "MIGRATION.md")))
	for _, w := range res.Warnings {
		c.ui.Warn("Warning: " + w)
	}
	if !apply {
		return 0
	}

	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	code := 0
	for _, a := range res.Applications {
		if _, err := cl.CreateApplication(ctx, a, upsert); err != nil {
			code = c.fail(fmt.Errorf("application %s: %w", a.Metadata.Name, err))
			continue
		}
		c.ui.Output(fmt.Sprintf("application '%s' created", a.Metadata.Name))
	}
	return code
}

// writeTree writes files under dir, refusing a non-empty directory unless
// force is set. Shell scripts are executable; everything else is 0644
// (nothing written is secret).
func writeTree(dir string, files map[string][]byte, force bool) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return err
	case len(entries) > 0 && !force:
		return fmt.Errorf("%s is not empty (use -force to write into it)", dir)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, filepath.FromSlash(n))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(n, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(p, files[n], mode); err != nil {
			return err
		}
	}
	return nil
}
