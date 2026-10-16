// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

// Project roles (Argo CD's proj role): policies on a project's
// applications, for the groups the role lists and its tokens' holders.

func roleOf(p *v1.AppProject, name string) (*v1.ProjectRole, error) {
	if r := p.Role(name); r != nil {
		return r, nil
	}
	return nil, fmt.Errorf("project %s has no role %q (see proj role list)", p.Metadata.Name, name)
}

func unixTime(sec int64) string {
	if sec == 0 {
		return "never"
	}
	return time.Unix(sec, 0).Local().Format(time.RFC3339)
}

// rolePolicy builds a role's policy line from the add-policy and
// remove-policy flags; the object is within the project.
func rolePolicy(project, role, resource, action, object, permission string) (string, error) {
	if action == "" || object == "" {
		return "", fmt.Errorf("-action and -object are required")
	}
	if permission != "allow" && permission != "deny" {
		return "", fmt.Errorf("-permission is allow or deny, not %q", permission)
	}
	if !slices.Contains(v1.RoleResources, resource) {
		return "", fmt.Errorf("-resource is %s, not %q", strings.Join(v1.RoleResources, " or "), resource)
	}
	return v1.NewRolePolicy(project, role, resource, action, object, permission == "allow").String(), nil
}

type projRoleListCommand struct{ *meta }

func (c *projRoleListCommand) Synopsis() string { return "List a project's roles" }
func (c *projRoleListCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role list [options] <project>

Options:

  -o=<format>   wide (default), json or yaml.
` + c.connectionHelp()
}

func (c *projRoleListCommand) Run(args []string) int {
	out := "wide"
	fs := c.flagSet("proj role list", true)
	fs.StringVar(&out, "o", "wide", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" proj role list [options] <project>")
	if !ok {
		return 1
	}
	if err := validOutput(out, "wide"); err != nil {
		return c.fail(err)
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
	if done, err := output(c.out, out, p.Spec.Roles); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	t := newTable(c.out, "ROLE", "DESCRIPTION", "POLICIES", "GROUPS", "TOKENS")
	for _, r := range p.Spec.Roles {
		t.row(r.Name, orDash(r.Description), fmt.Sprint(len(r.Policies)), orDash(strings.Join(r.Groups, ",")), fmt.Sprint(len(p.RoleTokens(r.Name))))
	}
	t.flush()
	return 0
}

type projRoleGetCommand struct{ *meta }

func (c *projRoleGetCommand) Synopsis() string { return "Show a project role" }
func (c *projRoleGetCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role get [options] <project> <role>

  Shows a role: its policies, groups and token IDs.

Options:

  -o=<format>   json or yaml (default: a summary).
` + c.connectionHelp()
}

func (c *projRoleGetCommand) Run(args []string) int {
	var out string
	fs := c.flagSet("proj role get", true)
	fs.StringVar(&out, "o", "", "")
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" proj role get [options] <project> <role>")
	if !ok {
		return 1
	}
	if err := validOutput(out); err != nil {
		return c.fail(err)
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
	r, err := roleOf(p, rest[1])
	if err != nil {
		return c.fail(err)
	}
	tokens := p.RoleTokens(r.Name)
	if done, err := output(c.out, out, struct {
		v1.ProjectRole `yaml:",inline"`
		Tokens         []v1.JWTToken `json:"tokens,omitempty" yaml:"tokens,omitempty"`
	}{*r, tokens}); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	t := newTable(c.out)
	t.row("Role:", r.Name)
	t.row("Subject:", v1.RoleSubject(p.Metadata.Name, r.Name))
	t.row("Description:", orDash(r.Description))
	t.row("Groups:", orDash(strings.Join(r.Groups, ", ")))
	t.flush()
	fmt.Fprintln(c.out, "Policies:")
	if len(r.Policies) == 0 {
		fmt.Fprintln(c.out, "  (none)")
	}
	for _, line := range r.Policies {
		fmt.Fprintln(c.out, "  "+line)
	}
	fmt.Fprintln(c.out)
	if len(tokens) == 0 {
		fmt.Fprintln(c.out, "No tokens.")
		return 0
	}
	t = newTable(c.out, "TOKEN ID", "ISSUED AT", "EXPIRES AT")
	for _, tok := range tokens {
		t.row(tok.ID, unixTime(tok.IssuedAt), unixTime(tok.ExpiresAt))
	}
	t.flush()
	return 0
}

type projRoleCreateCommand struct{ *meta }

func (c *projRoleCreateCommand) Synopsis() string { return "Create a project role" }
func (c *projRoleCreateCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role create [options] <project> <role>

  Creates a role, without policies: add them with proj role add-policy.

Options:

  -description=<text>  What the role is for.
` + c.connectionHelp()
}

func (c *projRoleCreateCommand) Run(args []string) int {
	var description string
	fs := c.flagSet("proj role create", true)
	fs.StringVar(&description, "description", "", "")
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" proj role create [options] <project> <role>")
	if !ok {
		return 1
	}
	if err := v1.ValidateRoleName(rest[1]); err != nil {
		return c.fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	err = editProject(ctx, cl, rest[0], func(p *v1.AppProject) error {
		if p.Role(rest[1]) != nil {
			return fmt.Errorf("project %s already has a role %q", rest[0], rest[1])
		}
		p.Spec.Roles = append(p.Spec.Roles, v1.ProjectRole{Name: rest[1], Description: description})
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Role %s created in project %s.", rest[1], rest[0]))
	return 0
}

type projRoleDeleteCommand struct{ *meta }

func (c *projRoleDeleteCommand) Synopsis() string { return "Delete a project role" }
func (c *projRoleDeleteCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role delete [options] <project> <role>

  Deletes a role; its tokens stop working.
` + c.connectionHelp()
}

func (c *projRoleDeleteCommand) Run(args []string) int {
	fs := c.flagSet("proj role delete", true)
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" proj role delete [options] <project> <role>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	err = editProject(ctx, cl, rest[0], func(p *v1.AppProject) error {
		if _, err := roleOf(p, rest[1]); err != nil {
			return err
		}
		p.Spec.Roles = slices.DeleteFunc(p.Spec.Roles, func(r v1.ProjectRole) bool { return r.Name == rest[1] })
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Role %s deleted from project %s.", rest[1], rest[0]))
	return 0
}

// projRolePolicyCommand adds or removes a policy.
type projRolePolicyCommand struct {
	*meta
	remove bool
}

func (c *projRolePolicyCommand) name() string {
	if c.remove {
		return "remove-policy"
	}
	return "add-policy"
}

func (c *projRolePolicyCommand) Synopsis() string {
	if c.remove {
		return "Remove a policy from a project role"
	}
	return "Add a policy to a project role"
}

func (c *projRolePolicyCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role ` + c.name() + ` [options] <project> <role>

  A policy lets (or forbids) the role an action on applications of the
  project, or on their logs:

    ` + c.opts.Name + ` proj role ` + c.name() + ` payments ci -action sync -object 'api-*'

  is "p, proj:payments:ci, applications, sync, payments/api-*, allow".

Options:

  -action=<action>          get, sync, update, delete, action, override, or *.
  -object=<glob>            Applications of the project ("*" for all); the
                            "<project>/" prefix is added.
  -permission=<allow|deny>  Default allow.
  -resource=<resource>      applications (default) or logs.
` + c.connectionHelp()
}

func (c *projRolePolicyCommand) Run(args []string) int {
	var action, object, permission, resource string
	fs := c.flagSet("proj role "+c.name(), true)
	fs.StringVar(&action, "action", "", "")
	fs.StringVar(&object, "object", "", "")
	fs.StringVar(&permission, "permission", "allow", "")
	fs.StringVar(&resource, "resource", "applications", "")
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" proj role "+c.name()+" [options] <project> <role>")
	if !ok {
		return 1
	}
	line, err := rolePolicy(rest[0], rest[1], resource, action, object, permission)
	if err != nil {
		return c.fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	err = editProject(ctx, cl, rest[0], func(p *v1.AppProject) error {
		r, err := roleOf(p, rest[1])
		if err != nil {
			return err
		}
		i := slices.IndexFunc(r.Policies, func(l string) bool { return v1.SamePolicy(l, line) })
		switch {
		case c.remove && i < 0:
			return fmt.Errorf("role %s has no policy %q", rest[1], line)
		case c.remove:
			r.Policies = slices.Delete(r.Policies, i, i+1)
		case i < 0:
			r.Policies = append(r.Policies, line)
		}
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	if c.remove {
		c.ui.Output("Policy removed: " + line)
	} else {
		c.ui.Output("Policy added: " + line)
	}
	return 0
}

// projRoleGroupCommand adds or removes a group.
type projRoleGroupCommand struct {
	*meta
	remove bool
}

func (c *projRoleGroupCommand) name() string {
	if c.remove {
		return "remove-group"
	}
	return "add-group"
}

func (c *projRoleGroupCommand) Synopsis() string {
	if c.remove {
		return "Take a project role from a group"
	}
	return "Give a project role to a group"
}

func (c *projRoleGroupCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role ` + c.name() + ` [options] <project> <role> <group>

  The members of a group have the role: an SSO or Nomad token group
  (nomad:role:<name>, nomad:policy:<name>), or a local account's group.
` + c.connectionHelp()
}

func (c *projRoleGroupCommand) Run(args []string) int {
	fs := c.flagSet("proj role "+c.name(), true)
	rest, ok := c.parse(fs, args, 3, c.opts.Name+" proj role "+c.name()+" [options] <project> <role> <group>")
	if !ok {
		return 1
	}
	group := strings.TrimSpace(rest[2])
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	err = editProject(ctx, cl, rest[0], func(p *v1.AppProject) error {
		r, err := roleOf(p, rest[1])
		if err != nil {
			return err
		}
		i := slices.Index(r.Groups, group)
		switch {
		case c.remove && i < 0:
			return fmt.Errorf("role %s does not list group %q", rest[1], group)
		case c.remove:
			r.Groups = slices.Delete(r.Groups, i, i+1)
		case i < 0:
			r.Groups = append(r.Groups, group)
		}
		return nil
	})
	if err != nil {
		return c.fail(err)
	}
	if c.remove {
		c.ui.Output(fmt.Sprintf("Group %s no longer has role %s.", group, rest[1]))
	} else {
		c.ui.Output(fmt.Sprintf("Group %s has role %s.", group, rest[1]))
	}
	return 0
}

type projRoleCreateTokenCommand struct{ *meta }

func (c *projRoleCreateTokenCommand) Synopsis() string { return "Create a project role token" }
func (c *projRoleCreateTokenCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role create-token [options] <project> <role>

  Creates a token whose holder acts as the role (proj:<project>:<role>),
  for automation confined to the project. It is shown this once. It stops
  working when deleted, when it expires, or with its role.

Options:

  -expires-in=<dur>  Lifetime, such as 12h or 90d (default 0: never
                     expires).
  -id=<id>           The token's ID (default: random).
  -token-only        Print the token alone.
` + c.connectionHelp()
}

func (c *projRoleCreateTokenCommand) Run(args []string) int {
	var expiresIn, id string
	var tokenOnly bool
	fs := c.flagSet("proj role create-token", true)
	fs.StringVar(&expiresIn, "expires-in", "0", "")
	fs.StringVar(&id, "id", "", "")
	fs.BoolVar(&tokenOnly, "token-only", false, "")
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" proj role create-token [options] <project> <role>")
	if !ok {
		return 1
	}
	ttl, err := client.ParseLifetime(expiresIn)
	if err != nil {
		return c.fail(fmt.Errorf("-expires-in: %w", err))
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	token, rec, err := cl.CreateRoleToken(ctx, rest[0], rest[1], id, ttl)
	if err != nil {
		return c.fail(err)
	}
	if tokenOnly {
		fmt.Fprintln(c.out, token)
		return 0
	}
	fmt.Fprintf(c.out, "Token created for %s.\n", v1.RoleSubject(rest[0], rest[1]))
	t := newTable(c.out)
	t.row("  ID:", rec.ID)
	t.row("  Issued at:", unixTime(rec.IssuedAt))
	t.row("  Expires at:", unixTime(rec.ExpiresAt))
	t.row("  Token:", token)
	t.flush()
	return 0
}

type projRoleDeleteTokenCommand struct{ *meta }

func (c *projRoleDeleteTokenCommand) Synopsis() string { return "Delete a project role token" }
func (c *projRoleDeleteTokenCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role delete-token [options] <project> <role> <token id or issued-at>

  Deletes (revokes) a role token, named by its ID or by its issue time in
  Unix seconds, as proj role list-tokens shows them.
` + c.connectionHelp()
}

func (c *projRoleDeleteTokenCommand) Run(args []string) int {
	fs := c.flagSet("proj role delete-token", true)
	rest, ok := c.parse(fs, args, 3, c.opts.Name+" proj role delete-token [options] <project> <role> <token id or issued-at>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if err := cl.DeleteRoleToken(ctx, rest[0], rest[1], rest[2]); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Token %s of role %s deleted.", rest[2], v1.RoleSubject(rest[0], rest[1])))
	return 0
}

type projRoleListTokensCommand struct{ *meta }

func (c *projRoleListTokensCommand) Synopsis() string { return "List a project role's tokens" }
func (c *projRoleListTokensCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` proj role list-tokens [options] <project> <role>
` + c.connectionHelp()
}

func (c *projRoleListTokensCommand) Run(args []string) int {
	fs := c.flagSet("proj role list-tokens", true)
	rest, ok := c.parse(fs, args, 2, c.opts.Name+" proj role list-tokens [options] <project> <role>")
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
	if _, err := roleOf(p, rest[1]); err != nil {
		return c.fail(err)
	}
	t := newTable(c.out, "TOKEN ID", "ISSUED AT", "ISSUED AT (UNIX)", "EXPIRES AT")
	for _, tok := range p.RoleTokens(rest[1]) {
		t.row(tok.ID, unixTime(tok.IssuedAt), strconv.FormatInt(tok.IssuedAt, 10), unixTime(tok.ExpiresAt))
	}
	t.flush()
	return 0
}
