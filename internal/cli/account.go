// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gitaginanjar/cystemd/pkg/client"
)

// accountName is the account a command acts on: -account, else the
// caller's.
func accountName(ctx context.Context, cl *client.Client, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	info, err := cl.UserInfo(ctx)
	if err != nil {
		return "", err
	}
	return info.Username, nil
}

func expiry(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.Local().Format(time.RFC3339)
}

type accountGetCommand struct{ *meta }

func (c *accountGetCommand) Synopsis() string { return "Show a local account and its API tokens" }
func (c *accountGetCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` account get [options]

  Shows a local account: its capabilities and the IDs of its API tokens.

Options:

  -account=<name>  The account (default: yours). Another account needs
                   the accounts/get permission.
  -o=<format>      json or yaml (default: a summary).
` + c.connectionHelp()
}

func (c *accountGetCommand) Run(args []string) int {
	var name, out string
	fs := c.flagSet("account get", true)
	fs.StringVar(&name, "account", "", "")
	fs.StringVar(&out, "o", "", "")
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" account get [options]"); !ok {
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
	if name, err = accountName(ctx, cl, name); err != nil {
		return c.fail(err)
	}
	a, err := cl.GetAccount(ctx, name)
	if err != nil {
		return c.fail(err)
	}
	if done, err := output(c.out, out, a); done {
		if err != nil {
			return c.fail(err)
		}
		return 0
	}
	t := newTable(c.out)
	t.row("Name:", a.Name)
	t.row("Enabled:", fmt.Sprint(a.Enabled))
	t.row("Capabilities:", orDash(strings.Join(a.Capabilities, ", ")))
	t.row("Groups:", orDash(strings.Join(a.Groups, ", ")))
	t.row("Password changed:", orDash(agoPtr(a.PasswordMtime)))
	t.flush()
	fmt.Fprintln(c.out)
	if len(a.Tokens) == 0 {
		fmt.Fprintln(c.out, "No API tokens.")
		return 0
	}
	t = newTable(c.out, "TOKEN ID", "ISSUED AT", "EXPIRES AT")
	for _, tok := range a.Tokens {
		t.row(tok.ID, tok.IssuedAt.Local().Format(time.RFC3339), expiry(tok.ExpiresAt))
	}
	t.flush()
	return 0
}

type accountGenerateTokenCommand struct{ *meta }

func (c *accountGenerateTokenCommand) Synopsis() string { return "Generate an API token" }
func (c *accountGenerateTokenCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` account generate-token [options]

  Generates an API token for an account with the apiKey capability and
  prints it: it is shown this once. Use it as CYSTEMD_TOKEN, -token or an
  "Authorization: Bearer" header. It stops working when deleted, when it
  expires, or when the account is disabled or loses the capability.

Options:

  -account=<name>     The account (default: yours). Another account needs
                      the accounts/update permission.
  -expires-in=<dur>   Lifetime, such as 12h or 90d (default 0: never
                      expires).
  -id=<id>            The token's ID (default: random).
` + c.connectionHelp()
}

func (c *accountGenerateTokenCommand) Run(args []string) int {
	var name, expiresIn, id string
	fs := c.flagSet("account generate-token", true)
	fs.StringVar(&name, "account", "", "")
	fs.StringVar(&expiresIn, "expires-in", "0", "")
	fs.StringVar(&id, "id", "", "")
	if _, ok := c.parse(fs, args, 0, c.opts.Name+" account generate-token [options]"); !ok {
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
	if name, err = accountName(ctx, cl, name); err != nil {
		return c.fail(err)
	}
	token, _, err := cl.GenerateToken(ctx, name, id, ttl)
	if err != nil {
		return c.fail(err)
	}
	fmt.Fprintln(c.out, token)
	return 0
}

type accountDeleteTokenCommand struct{ *meta }

func (c *accountDeleteTokenCommand) Synopsis() string { return "Delete (revoke) an API token" }
func (c *accountDeleteTokenCommand) Help() string {
	return `Usage: ` + c.opts.Name + ` account delete-token [options] <token id>

  Deletes an API token: it stops working at once (within seconds on the
  other control plane replicas). "account get" lists the token IDs.

Options:

  -account=<name>  The account (default: yours). Another account needs
                   the accounts/update permission.
` + c.connectionHelp()
}

func (c *accountDeleteTokenCommand) Run(args []string) int {
	var name string
	fs := c.flagSet("account delete-token", true)
	fs.StringVar(&name, "account", "", "")
	rest, ok := c.parse(fs, args, 1, c.opts.Name+" account delete-token [options] <token id>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	if name, err = accountName(ctx, cl, name); err != nil {
		return c.fail(err)
	}
	if err := cl.DeleteToken(ctx, name, rest[0]); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Token %s of account %s deleted.", rest[0], name))
	return 0
}

type accountCanICommand struct{ *meta }

func (c *accountCanICommand) Synopsis() string { return "Check whether you may perform an action" }
func (c *accountCanICommand) Help() string {
	return `Usage: ` + c.opts.Name + ` account can-i <action> <resource> <object>

  Prints yes or no: whether RBAC lets you perform an action. Objects are
  <project>/<application> for applications and logs, a name otherwise;
  * stands for any.

  Examples:

    ` + c.opts.Name + ` account can-i sync applications 'default/guestbook'
    ` + c.opts.Name + ` account can-i update projects '*'
    ` + c.opts.Name + ` account can-i get logs 'platform/*'
` + c.connectionHelp()
}

func (c *accountCanICommand) Run(args []string) int {
	fs := c.flagSet("account can-i", true)
	rest, ok := c.parse(fs, args, 3, c.opts.Name+" account can-i <action> <resource> <object>")
	if !ok {
		return 1
	}
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		return c.fail(err)
	}
	yes, err := cl.CanI(ctx, rest[0], rest[1], cmp.Or(rest[2], "*"))
	if err != nil {
		return c.fail(err)
	}
	if yes {
		fmt.Fprintln(c.out, "yes")
	} else {
		fmt.Fprintln(c.out, "no")
	}
	return 0
}
