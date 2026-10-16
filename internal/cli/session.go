// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/gitaginanjar/cystemd/pkg/client"
)

type loginCommand struct{ *meta }

func (c *loginCommand) Synopsis() string { return "Log in to a CYSTEMD server" }
func (c *loginCommand) Help() string {
	return `Usage: ` + c.session("login") + ` [options] [server]

  Logs in to a CYSTEMD server and saves the session as a context (the
  current one from then on). The server is a cystemd-server URL or a
  cystemd agent's address. The password is prompted for unless given.

Options:

  -username=<name>    Local account (default admin).
  -password=<pass>    Its password. Prefer the prompt or -password-stdin.
  -password-stdin     Read the password from standard input.
  -nomad-token=<id>   Log in with a Nomad ACL token instead (or NOMAD_TOKEN
                      with -nomad-token=env).
  -sso                Log in through single sign-on (a Nomad OIDC auth
                      method): opens the identity provider in a browser and
                      receives its answer on http://localhost:<sso-port>.
  -sso-method=<name>  Auth method (default: the server's default one).
  -sso-port=<port>    Port of the local callback (default 8085); the auth
                      method must allow http://localhost:<port>/auth/callback
                      as a redirect URI.
  -name=<context>     Context name (default: the server's host).
  -ca-cert=<path>     CA certificate to verify the server with.
  -tls-skip-verify    Do not verify the server's certificate.
`
}

func (c *loginCommand) Run(args []string) int {
	var username, password, nomadToken, name, ssoMethod string
	var passwordStdin, sso bool
	var ssoPort int
	fs := c.flagSet("login", false)
	fs.StringVar(&username, "username", "admin", "")
	fs.StringVar(&password, "password", "", "")
	fs.BoolVar(&passwordStdin, "password-stdin", false, "")
	fs.StringVar(&nomadToken, "nomad-token", "", "")
	fs.BoolVar(&sso, "sso", false, "")
	fs.StringVar(&ssoMethod, "sso-method", "", "")
	fs.IntVar(&ssoPort, "sso-port", 8085, "")
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&c.caCert, "ca-cert", "", "")
	fs.BoolVar(&c.insecure, "tls-skip-verify", false, "")
	rest, ok := c.parse(fs, args, -1, c.session("login")+" [options] [server]")
	if !ok {
		return 1
	}
	cfg, err := loadConfig()
	if err != nil {
		return c.fail(err)
	}
	server := ""
	if len(rest) > 0 {
		server = rest[0]
	}
	server = firstNonEmpty(server, os.Getenv("CYSTEMD_ADDR"), cfg.context("").Server)
	if server == "" && c.opts.NomadFallback {
		server = os.Getenv("NOMAD_ADDR")
	}
	if server == "" {
		c.ui.Error("Which server? Usage: " + c.session("login") + " <server>")
		return 1
	}
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	cl, err := client.New(server, client.Options{CACert: c.caCert, Insecure: c.insecure, UserAgent: c.opts.Name + "/" + c.opts.Version})
	if err != nil {
		return c.fail(err)
	}
	ctx, cancel := signalContext()
	defer cancel()
	var token string
	switch {
	case sso:
		token, username, err = c.ssoLogin(ctx, cl, ssoMethod, ssoPort)
	case nomadToken != "":
		if nomadToken == "env" {
			nomadToken = os.Getenv("NOMAD_TOKEN")
		}
		username = "nomad token"
		token, err = cl.LoginNomadToken(ctx, nomadToken)
	default:
		switch {
		case passwordStdin:
			line, rerr := bufio.NewReader(c.opts.Stdin).ReadString('\n')
			if rerr != nil && line == "" {
				return c.fail(fmt.Errorf("read password: %w", rerr))
			}
			password = strings.TrimRight(line, "\r\n")
		case password == "":
			if password, err = c.ui.AskSecret(fmt.Sprintf("Password for %s:", username)); err != nil {
				return c.fail(err)
			}
		}
		token, err = cl.Login(ctx, username, password)
	}
	if err != nil {
		return c.fail(err)
	}
	if name == "" {
		u, _ := url.Parse(server)
		name = u.Host
	}
	cfg.Contexts[name] = &cliContext{Server: server, Token: token, User: username, CACert: c.caCert, Insecure: c.insecure}
	cfg.Current = name
	if err := cfg.save(); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Logged in to %s as %s (context %q).", server, username, name))
	return 0
}

type logoutCommand struct{ *meta }

func (c *logoutCommand) Synopsis() string { return "Forget the session of a context" }
func (c *logoutCommand) Help() string {
	return `Usage: ` + c.session("logout") + ` [context]

  Removes the session token of a context (the current one by default); the
  server is kept for the next login.
`
}

func (c *logoutCommand) Run(args []string) int {
	fs := c.flagSet("logout", false)
	rest, ok := c.parse(fs, args, -1, c.session("logout")+" [context]")
	if !ok {
		return 1
	}
	cfg, err := loadConfig()
	if err != nil {
		return c.fail(err)
	}
	name := cfg.Current
	if len(rest) > 0 {
		name = rest[0]
	}
	ctx, found := cfg.Contexts[name]
	if !found {
		c.ui.Error(fmt.Sprintf("No context %q.", name))
		return 1
	}
	ctx.Token = ""
	if err := cfg.save(); err != nil {
		return c.fail(err)
	}
	c.ui.Output(fmt.Sprintf("Logged out of %s.", name))
	return 0
}

type contextCommand struct{ *meta }

func (c *contextCommand) Synopsis() string { return "List contexts or switch to one" }
func (c *contextCommand) Help() string {
	return `Usage: ` + c.session("context") + ` [name]

  Without a name, lists the saved contexts (* is the current one). With a
  name, makes it the current context.

Options:

  -delete    Remove the named context.
`
}

func (c *contextCommand) Run(args []string) int {
	var del bool
	fs := c.flagSet("context", false)
	fs.BoolVar(&del, "delete", false, "")
	rest, ok := c.parse(fs, args, -1, c.session("context")+" [name]")
	if !ok {
		return 1
	}
	cfg, err := loadConfig()
	if err != nil {
		return c.fail(err)
	}
	if len(rest) == 0 {
		t := newTable(c.out, "CURRENT", "NAME", "SERVER", "USER")
		for _, n := range cfg.names() {
			cur := ""
			if n == cfg.Current {
				cur = "*"
			}
			x := cfg.Contexts[n]
			user := x.User
			if x.Token == "" {
				user = "(logged out)"
			}
			t.row(cur, n, x.Server, user)
		}
		t.flush()
		return 0
	}
	name := rest[0]
	if _, found := cfg.Contexts[name]; !found {
		c.ui.Error(fmt.Sprintf("No context %q.", name))
		return 1
	}
	if del {
		delete(cfg.Contexts, name)
		if cfg.Current == name {
			cfg.Current = ""
		}
	} else {
		cfg.Current = name
	}
	if err := cfg.save(); err != nil {
		return c.fail(err)
	}
	if del {
		c.ui.Output(fmt.Sprintf("Deleted context %q.", name))
	} else {
		c.ui.Output(fmt.Sprintf("Switched to context %q.", name))
	}
	return 0
}

type versionCommand struct{ *meta }

func (c *versionCommand) Synopsis() string { return "Print the client and server versions" }
func (c *versionCommand) Help() string {
	return `Usage: ` + c.session("version") + ` [options]

  Prints this client's version and, when reachable, the server's.
` + c.connectionHelp()
}

func (c *versionCommand) Run(args []string) int {
	fs := c.flagSet("version", true)
	if _, ok := c.parse(fs, args, 0, c.session("version")); !ok {
		return 1
	}
	c.ui.Output(fmt.Sprintf("%s: %s", c.opts.Name, c.opts.Version))
	ctx, cancel := signalContext()
	defer cancel()
	cl, err := c.connect(ctx)
	if err != nil {
		c.ui.Output("server: " + err.Error())
		return 0
	}
	v, err := cl.Version(ctx)
	if err != nil {
		c.ui.Output(fmt.Sprintf("server: unreachable (%v)", err))
		return 0
	}
	c.ui.Output(fmt.Sprintf("server: %s %s (%s, %s, %s mode)", cl.BaseURL, v.Version, v.GitCommit, v.Platform, v.Mode))
	return 0
}
