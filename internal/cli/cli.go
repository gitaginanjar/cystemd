// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package cli implements the CYSTEMD command line: the commands of the
// standalone `cystemdctl` and of the cystemd agent binary (`cystemd app
// sync web`), modelled on Argo CD's argocd CLI with Nomad's flag style.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/hashicorp/cli"

	"github.com/gitaginanjar/cystemd/pkg/client"
)

// Options describe the binary the commands are mounted in.
type Options struct {
	// Name is the binary name used in help ("cystemd" or "cystemdctl").
	Name string
	// NomadFallback lets the commands use NOMAD_ADDR and NOMAD_TOKEN when
	// no CYSTEMD server or token is configured: a cystemd agent serves the
	// CYSTEMD API on its Nomad address.
	NomadFallback bool
	// DefaultAddress is used when nothing else sets the server.
	DefaultAddress string
	// Version is the client's version.
	Version string
	// SessionPrefix prefixes the session commands (login, logout, context,
	// version), e.g. "ctl " where the binary has its own (Nomad's login).
	SessionPrefix string
	// Stdout receives command output (default os.Stdout); messages and
	// errors go to the Ui. Stdin is read by login -password-stdin.
	Stdout io.Writer
	Stdin  io.Reader
	// OpenBrowser opens a URL for login -sso (default: the desktop's
	// browser, best effort; the URL is printed either way).
	OpenBrowser func(url string) error
}

// Commands returns the command factories (keys such as "app sync").
func Commands(ui cli.Ui, o Options) map[string]cli.CommandFactory {
	if o.Name == "" {
		o.Name = "cystemdctl"
	}
	if o.DefaultAddress == "" {
		o.DefaultAddress = "http://127.0.0.1:8080"
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stdin == nil {
		o.Stdin = os.Stdin
	}
	m := func() *meta { return &meta{ui: ui, opts: o, out: o.Stdout} }
	sp := o.SessionPrefix
	cmds := map[string]cli.CommandFactory{
		sp + "login":   func() (cli.Command, error) { return &loginCommand{meta: m()}, nil },
		sp + "logout":  func() (cli.Command, error) { return &logoutCommand{meta: m()}, nil },
		sp + "context": func() (cli.Command, error) { return &contextCommand{meta: m()}, nil },
		sp + "version": func() (cli.Command, error) { return &versionCommand{meta: m()}, nil },

		"app": func() (cli.Command, error) {
			return &groupCommand{meta: m(), name: "app", synopsis: "Manage applications"}, nil
		},
		"app list":         func() (cli.Command, error) { return &appListCommand{meta: m()}, nil },
		"app get":          func() (cli.Command, error) { return &appGetCommand{meta: m()}, nil },
		"app create":       func() (cli.Command, error) { return &appCreateCommand{meta: m()}, nil },
		"app set":          func() (cli.Command, error) { return &appSetCommand{meta: m()}, nil },
		"app delete":       func() (cli.Command, error) { return &appDeleteCommand{meta: m()}, nil },
		"app sync":         func() (cli.Command, error) { return &appSyncCommand{meta: m()}, nil },
		"app wait":         func() (cli.Command, error) { return &appWaitCommand{meta: m()}, nil },
		"app diff":         func() (cli.Command, error) { return &appDiffCommand{meta: m()}, nil },
		"app history":      func() (cli.Command, error) { return &appHistoryCommand{meta: m()}, nil },
		"app rollback":     func() (cli.Command, error) { return &appRollbackCommand{meta: m()}, nil },
		"app terminate-op": func() (cli.Command, error) { return &appTerminateCommand{meta: m()}, nil },
		"app manifests":    func() (cli.Command, error) { return &appManifestsCommand{meta: m()}, nil },
		"app resources":    func() (cli.Command, error) { return &appResourcesCommand{meta: m()}, nil },
		"app logs":         func() (cli.Command, error) { return &appLogsCommand{meta: m()}, nil },
		"app actions":      func() (cli.Command, error) { return &appActionsCommand{meta: m()}, nil },

		"proj": func() (cli.Command, error) {
			return &groupCommand{meta: m(), name: "proj", synopsis: "Manage projects"}, nil
		},
		"proj list":   func() (cli.Command, error) { return &projListCommand{meta: m()}, nil },
		"proj get":    func() (cli.Command, error) { return &projGetCommand{meta: m()}, nil },
		"proj create": func() (cli.Command, error) { return &projCreateCommand{meta: m()}, nil },
		"proj delete": func() (cli.Command, error) { return &projDeleteCommand{meta: m()}, nil },
		"proj windows": func() (cli.Command, error) {
			return &groupCommand{meta: m(), name: "proj windows", synopsis: "Manage a project's sync windows"}, nil
		},
		"proj windows list":   func() (cli.Command, error) { return &projWindowsListCommand{meta: m()}, nil },
		"proj windows add":    func() (cli.Command, error) { return &projWindowsAddCommand{meta: m()}, nil },
		"proj windows delete": func() (cli.Command, error) { return &projWindowsDeleteCommand{meta: m()}, nil },
		"proj windows enable-manual-sync": func() (cli.Command, error) {
			return &projWindowsManualCommand{meta: m(), enable: true}, nil
		},
		"proj windows disable-manual-sync": func() (cli.Command, error) { return &projWindowsManualCommand{meta: m()}, nil },

		"repo": func() (cli.Command, error) {
			return &groupCommand{meta: m(), name: "repo", synopsis: "Manage repository connections"}, nil
		},
		"repo list": func() (cli.Command, error) { return &repoListCommand{meta: m()}, nil },
		"repo add":  func() (cli.Command, error) { return &repoAddCommand{meta: m()}, nil },
		"repo rm":   func() (cli.Command, error) { return &repoRemoveCommand{meta: m()}, nil },

		"cluster": func() (cli.Command, error) {
			return &groupCommand{meta: m(), name: "cluster", synopsis: "Inspect destination Nomad clusters"}, nil
		},
		"cluster list": func() (cli.Command, error) { return &clusterListCommand{meta: m()}, nil },

		"account": func() (cli.Command, error) {
			return &groupCommand{meta: m(), name: "account", synopsis: "Manage accounts and API tokens"}, nil
		},
		"account get-user-info":   func() (cli.Command, error) { return &accountInfoCommand{meta: m()}, nil },
		"account update-password": func() (cli.Command, error) { return &accountPasswordCommand{meta: m()}, nil },
		"account list":            func() (cli.Command, error) { return &accountListCommand{meta: m()}, nil },
		"account get":             func() (cli.Command, error) { return &accountGetCommand{meta: m()}, nil },
		"account generate-token":  func() (cli.Command, error) { return &accountGenerateTokenCommand{meta: m()}, nil },
		"account delete-token":    func() (cli.Command, error) { return &accountDeleteTokenCommand{meta: m()}, nil },
		"account can-i":           func() (cli.Command, error) { return &accountCanICommand{meta: m()}, nil },

		"migrate": func() (cli.Command, error) { return &migrateCommand{meta: m()}, nil },
	}
	if group := strings.TrimSpace(sp); group != "" {
		cmds[group] = func() (cli.Command, error) {
			return &groupCommand{meta: m(), name: group, synopsis: "Log in to CYSTEMD servers and manage contexts"}, nil
		}
	}
	return cmds
}

// TopLevel lists the top-level command names (for a binary's help).
func TopLevel(o Options) []string {
	var out []string
	for name := range Commands(nil, o) {
		if !strings.Contains(name, " ") {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// meta carries what every command shares: output and connection flags.
type meta struct {
	ui   cli.Ui
	opts Options
	out  io.Writer

	address  string
	token    string
	caCert   string
	insecure bool
	ctxName  string

	client *client.Client
}

// flagSet returns a flag set with the connection flags.
func (m *meta) flagSet(name string, conn bool) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if conn {
		fs.StringVar(&m.address, "address", "", "")
		fs.StringVar(&m.token, "token", "", "")
		fs.StringVar(&m.caCert, "ca-cert", "", "")
		fs.BoolVar(&m.insecure, "tls-skip-verify", false, "")
		fs.StringVar(&m.ctxName, "context", "", "")
	}
	return fs
}

// session names a session command ("cystemd ctl login").
func (m *meta) session(name string) string {
	return m.opts.Name + " " + m.opts.SessionPrefix + name
}

// connectionHelp documents the shared flags.
func (m *meta) connectionHelp() string {
	fallback := ""
	if m.opts.NomadFallback {
		fallback = "\n    Without a CYSTEMD server or token, NOMAD_ADDR and NOMAD_TOKEN are used:\n    the cystemd agent serves the CYSTEMD API on its Nomad address."
	}
	return `
Connection options:

  -address=<url>
    The CYSTEMD server. Overrides CYSTEMD_ADDR and the current context.

  -token=<token>
    A session or API token. Overrides CYSTEMD_TOKEN and the current
    context.

  -ca-cert=<path>
    A CA certificate to verify the server with (CYSTEMD_CACERT).

  -tls-skip-verify
    Do not verify the server's certificate (CYSTEMD_SKIP_VERIFY).

  -context=<name>
    Use a saved context (see '` + m.session("context") + `').` + fallback + `
`
}

// connect builds the API client from flags, environment and contexts.
func (m *meta) connect(ctx context.Context) (*client.Client, error) {
	if m.client != nil {
		return m.client, nil
	}
	cfg, _ := loadConfig()
	cur := cfg.context(m.ctxName)
	address := firstNonEmpty(m.address, os.Getenv("CYSTEMD_ADDR"), cur.Server)
	token := firstNonEmpty(m.token, os.Getenv("CYSTEMD_TOKEN"), cur.Token)
	caCert := firstNonEmpty(m.caCert, os.Getenv("CYSTEMD_CACERT"), cur.CACert)
	insecure := m.insecure || cur.Insecure || os.Getenv("CYSTEMD_SKIP_VERIFY") == "true"
	nomadToken := ""
	if m.opts.NomadFallback {
		if address == "" {
			address = os.Getenv("NOMAD_ADDR")
			caCert = firstNonEmpty(caCert, os.Getenv("NOMAD_CACERT"))
			insecure = insecure || os.Getenv("NOMAD_SKIP_VERIFY") == "true"
		}
		if token == "" {
			nomadToken = os.Getenv("NOMAD_TOKEN")
		}
	}
	if address == "" {
		address = m.opts.DefaultAddress
	}
	c, err := client.New(address, client.Options{Token: token, CACert: caCert, Insecure: insecure,
		UserAgent: m.opts.Name + "/" + m.opts.Version, Timeout: 2 * time.Minute})
	if err != nil {
		return nil, err
	}
	if nomadToken != "" {
		// A Nomad ACL token logs in for this invocation only.
		if c.Token, err = c.LoginNomadToken(ctx, nomadToken); err != nil {
			return nil, fmt.Errorf("log in with NOMAD_TOKEN: %w", err)
		}
	}
	m.client = c
	return c, nil
}

// fail prints err and returns the exit code; a missing session hints at
// logging in.
func (m *meta) fail(err error) int {
	if client.IsUnauthorized(err) {
		m.ui.Error(fmt.Sprintf("Error: %v\n\nLog in with '%s <server>' or set CYSTEMD_TOKEN.", err, m.session("login")))
		return 1
	}
	m.ui.Error("Error: " + err.Error())
	return 1
}

// parse parses args, reporting usage errors (exit code 1). Flags may
// follow the arguments ("app get web -o yaml"); after "--" everything is
// an argument.
func (m *meta) parse(fs *flag.FlagSet, args []string, wantArgs int, usage string) ([]string, bool) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			m.ui.Error(err.Error())
			m.ui.Error("Usage: " + usage)
			return nil, false
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	pos = append(pos, tail...)
	if wantArgs >= 0 && len(pos) != wantArgs {
		m.ui.Error(fmt.Sprintf("This command takes %d argument(s).", wantArgs))
		m.ui.Error("Usage: " + usage)
		return nil, false
	}
	return pos, true
}

// signalContext is cancelled on Ctrl-C.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// groupCommand prints its subcommands ("cystemd app").
type groupCommand struct {
	*meta
	name     string
	synopsis string
}

func (c *groupCommand) Help() string {
	return fmt.Sprintf("Usage: %s %s <subcommand> [options] [args]\n\n  %s. Run '%s %s <subcommand> -h' for help on a subcommand.",
		c.opts.Name, c.name, c.synopsis, c.opts.Name, c.name)
}
func (c *groupCommand) Synopsis() string   { return c.synopsis }
func (c *groupCommand) Run(_ []string) int { return cli.RunResultHelp }

// stringList is a repeatable flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// keyValues parses repeated k=v flags.
func keyValues(list []string, what string) (map[string]string, error) {
	if len(list) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, kv := range list {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("%s %q: expected key=value", what, kv)
		}
		out[k] = v
	}
	return out, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
