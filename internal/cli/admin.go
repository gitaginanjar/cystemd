// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/gitaginanjar/cystemd/internal/manifest"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

// ─── projects ───────────────────────────────────────────────────────────

type projListCommand struct{ *meta }

func (c *projListCommand) Synopsis() string { return "List projects" }
func (c *projListCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj list [options]

Options:

  -o=<format>   json or yaml.
` + c.connectionHelp()
}

func (c *projListCommand) Run(args []string) int {
	var out string
	fs := c.flagSet("proj list", true)
	fs.StringVar(&out, "o", "", "")
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" proj list"); !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	projects, err := cl.ListProjects(ctx)
	if err != nil {
		return c.fail(err)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Metadata.Name < projects[j].Metadata.Name })
	if done, err := output(c.out, out, projects); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	t := newTable(c.out, "NAME", "DESCRIPTION", "SOURCES", "DESTINATIONS")
	for _, p := range projects {
		var dests []string
		for _, d := range p.Spec.Destinations {
			dests = append(dests, firstNonEmpty(d.Name, "*")+"/"+firstNonEmpty(d.Namespace, "*"))
		}
		t.row(p.Metadata.Name, orDash(p.Spec.Description), orDash(strings.Join(p.Spec.SourceRepos, ",")), orDash(strings.Join(dests, ",")))
	}
	t.flush()
	return 0
}

type projGetCommand struct{ *meta }

func (c *projGetCommand) Synopsis() string { return "Show a project" }
func (c *projGetCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj get [options] <name>

Options:

  -o=<format>   json or yaml (default yaml).
` + c.connectionHelp()
}

func (c *projGetCommand) Run(args []string) int {
	out := "yaml"
	fs := c.flagSet("proj get", true)
	fs.StringVar(&out, "o", "yaml", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" proj get [options] <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	p, err := cl.GetProject(ctx, rest[0])
	if err != nil {
		return c.fail(err)
	}
	if _, err := output(c.out, out, p); err != nil {
		return c.fail(err)
	}
	return 0
}

type projCreateCommand struct{ *meta }

func (c *projCreateCommand) Synopsis() string { return "Create projects from a manifest" }
func (c *projCreateCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj create -f <manifest.yaml> [-upsert]

  Creates the AppProjects of a manifest file ("-" reads standard input).

Options:

  -f=<file>   Manifest file.
  -upsert     Replace projects that exist.
` + c.connectionHelp()
}

func (c *projCreateCommand) Run(args []string) int {
	var file string
	var upsert bool
	fs := c.flagSet("proj create", true)
	fs.StringVar(&file, "f", "", "")
	fs.BoolVar(&upsert, "upsert", false, "")
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" proj create -f <manifest.yaml>"); !ok {
		return 1
	}
	if file == "" {
		c.ui.Error("-f is required.")
		return 1
	}
	objs, err := manifest.ReadFile(file)
	if err != nil {
		return c.fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	n := 0
	for _, o := range objs {
		if o.Project == nil {
			continue
		}
		result, err := createProject(ctx, cl, o.Project, upsert)
		if err != nil {
			return c.fail(fmt.Errorf("project %s: %w", o.Project.Metadata.Name, err))
		}
		c.ui.Output(fmt.Sprintf("project '%s' %s", o.Project.Metadata.Name, result))
		n++
	}
	if n == 0 {
		return c.fail(errors.New("the manifest has no AppProject"))
	}
	return 0
}

type projDeleteCommand struct{ *meta }

func (c *projDeleteCommand) Synopsis() string { return "Delete an unused project" }
func (c *projDeleteCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj delete <name>
` + c.connectionHelp()
}

func (c *projDeleteCommand) Run(args []string) int {
	fs := c.flagSet("proj delete", true)
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" proj delete <name>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if err := cl.DeleteProject(ctx, rest[0]); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("project '%s' deleted", rest[0]))
	return 0
}

// errExists matches a 409 from the server: the object exists.
var errExists = errors.New("already exists")

type existsError struct{ error }

func (existsError) Is(target error) bool { return target == errExists }

// createProject creates p, or with upsert replaces an existing one; it
// says which. Without upsert an existing project is errExists.
func createProject(ctx context.Context, cl *client.Client, p *v1.AppProject, upsert bool) (string, error) {
	err := cl.CreateProject(ctx, p)
	var ce *client.Error
	if err != nil && errors.As(err, &ce) && ce.Code == 409 {
		if !upsert {
			return "", existsError{err}
		}
		if err := cl.UpdateProject(ctx, p); err != nil {
			return "", err
		}
		return "updated", nil
	}
	if err != nil {
		return "", err
	}
	return "created", nil
}

// ─── repositories ───────────────────────────────────────────────────────

type repoListCommand struct{ *meta }

func (c *repoListCommand) Synopsis() string { return "List connected repositories" }
func (c *repoListCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` repo list [options]

Options:

  -o=<format>   json or yaml.
` + c.connectionHelp()
}

func (c *repoListCommand) Run(args []string) int {
	var out string
	fs := c.flagSet("repo list", true)
	fs.StringVar(&out, "o", "", "")
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" repo list"); !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	repos, err := cl.ListRepositories(ctx)
	if err != nil {
		return c.fail(err)
	}
	if done, err := output(c.out, out, repos); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	t := newTable(c.out, "REPO", "NAME", "CREDENTIALS", "STATUS", "MESSAGE")
	for _, r := range repos {
		creds := "none"
		if r.HasCredentials {
			creds = "stored"
		}
		t.row(r.Repo, orDash(r.Name), creds, orDash(r.ConnectionState.Status), r.ConnectionState.Message)
	}
	t.flush()
	return 0
}

type repoAddCommand struct{ *meta }

func (c *repoAddCommand) Synopsis() string { return "Connect a repository" }
func (c *repoAddCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` repo add [options] <url>

  Connects a Git repository after testing the connection. Credentials are
  stored in Vault (or in the control plane's store without Vault).

Options:

  -name=<name>                  Display name.
  -username=<user>              HTTPS user.
  -password=<pass>              HTTPS password or token (prompted with -password=-).
  -ssh-private-key-path=<file>  SSH private key.
  -insecure                     Skip TLS and host key verification.
  -force                        Save even if the connection test fails.
` + c.connectionHelp()
}

func (c *repoAddCommand) Run(args []string) int {
	var name, username, password, keyPath string
	var insecure, force bool
	fs := c.flagSet("repo add", true)
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&username, "username", "", "")
	fs.StringVar(&password, "password", "", "")
	fs.StringVar(&keyPath, "ssh-private-key-path", "", "")
	fs.BoolVar(&insecure, "insecure", false, "")
	fs.BoolVar(&force, "force", false, "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" repo add [options] <url>")
	if !ok {
		return 1
	}
	repo := &v1.Repository{Repo: rest[0], Name: name, Type: "git", Username: username, Insecure: insecure}
	if password == "-" {
		var err error
		if password, err = c.ui.AskSecret("Password:"); err != nil {
			return c.fail(err)
		}
	}
	repo.Password = password
	if keyPath != "" {
		key, err := os.ReadFile(keyPath)
		if err != nil {
			return c.fail(err)
		}
		repo.SSHPrivateKey = string(key)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	out, err := cl.AddRepository(ctx, repo, force)
	if err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("repository '%s' added (%s)", out.Repo, out.ConnectionState.Status))
	return 0
}

type repoRemoveCommand struct{ *meta }

func (c *repoRemoveCommand) Synopsis() string { return "Disconnect a repository" }
func (c *repoRemoveCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` repo rm <url>
` + c.connectionHelp()
}

func (c *repoRemoveCommand) Run(args []string) int {
	fs := c.flagSet("repo rm", true)
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" repo rm <url>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if err := cl.RemoveRepository(ctx, rest[0]); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("repository '%s' removed", rest[0]))
	return 0
}

// ─── clusters and accounts ──────────────────────────────────────────────

type clusterListCommand struct{ *meta }

func (c *clusterListCommand) Synopsis() string { return "List destination clusters" }
func (c *clusterListCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` cluster list
` + c.connectionHelp()
}

func (c *clusterListCommand) Run(args []string) int {
	fs := c.flagSet("cluster list", true)
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" cluster list"); !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	clusters, err := cl.ListClusters(ctx)
	if err != nil {
		return c.fail(err)
	}
	t := newTable(c.out, "NAME", "SERVER", "VERSION", "NODES", "APPS", "STATUS", "MESSAGE")
	for _, x := range clusters {
		t.row(x.Name, x.Server, orDash(x.Version), fmt.Sprintf("%d/%d", x.ReadyNodes, x.Nodes), fmt.Sprint(x.Applications),
			x.ConnectionState.Status, x.ConnectionState.Message)
	}
	t.flush()
	return 0
}

type accountInfoCommand struct{ *meta }

func (c *accountInfoCommand) Synopsis() string { return "Show who you are logged in as" }
func (c *accountInfoCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` account get-user-info
` + c.connectionHelp()
}

func (c *accountInfoCommand) Run(args []string) int {
	fs := c.flagSet("account get-user-info", true)
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" account get-user-info"); !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	info, err := cl.UserInfo(ctx)
	if err != nil {
		return c.fail(err)
	}
	t := newTable(c.out)
	t.row("Logged in:", fmt.Sprint(info.LoggedIn))
	t.row("Username:", info.Username)
	t.row("Provider:", info.Provider)
	t.row("Groups:", orDash(strings.Join(info.Groups, ",")))
	t.row("Roles:", orDash(strings.Join(info.Roles, ",")))
	t.flush()
	return 0
}

type accountPasswordCommand struct{ *meta }

func (c *accountPasswordCommand) Synopsis() string { return "Change your password" }
func (c *accountPasswordCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` account update-password

  Changes the password of the local account you are logged in as
  (prompted for). Other sessions of the account are signed out.
` + c.connectionHelp()
}

func (c *accountPasswordCommand) Run(args []string) int {
	fs := c.flagSet("account update-password", true)
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" account update-password"); !ok {
		return 1
	}
	current, err := c.ui.AskSecret("Current password:")
	if err != nil {
		return c.fail(err)
	}
	next, err := c.ui.AskSecret("New password:")
	if err != nil {
		return c.fail(err)
	}
	again, err := c.ui.AskSecret("Confirm the new password:")
	if err != nil {
		return c.fail(err)
	}
	if next != again {
		return c.fail(errors.New("the new passwords do not match"))
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if err := cl.UpdatePassword(ctx, current, next); err != nil {
		return c.fail(err)
	}
	c.ui.Output("Password updated. Log in again.")
	return 0
}

type accountListCommand struct{ *meta }

func (c *accountListCommand) Synopsis() string { return "List local accounts" }
func (c *accountListCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` account list
` + c.connectionHelp()
}

func (c *accountListCommand) Run(args []string) int {
	fs := c.flagSet("account list", true)
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" account list"); !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	accts, err := cl.ListAccounts(ctx)
	if err != nil {
		return c.fail(err)
	}
	t := newTable(c.out, "NAME", "ENABLED", "CAPABILITIES", "GROUPS", "PASSWORD CHANGED")
	for _, a := range accts {
		t.row(a.Name, fmt.Sprint(a.Enabled), orDash(strings.Join(a.Capabilities, ",")), orDash(strings.Join(a.Groups, ",")), orDash(agoPtr(a.PasswordMtime)))
	}
	t.flush()
	return 0
}
