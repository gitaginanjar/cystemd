// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Command cystemd is the CYSTEMD agent: the CYSTEMD fork of HashiCorp Nomad
// (see fork/nomad) with the systemd task driver built in, the CYSTEMD web UI
// served under /ui/, and the GitOps control plane embedded in server agents.
//
// Every Nomad command is available unchanged (`cystemd agent`, `cystemd job
// run`, …), so the binary is a drop-in replacement for `nomad`.
//
// The resulting binary links the BUSL-1.1 licensed Nomad tree; see NOTICE.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	// Packages whose init() checks os.Args and runs a helper subprocess
	// (executor, logmon, artifact getter, template renderer, docker
	// logger), exactly as Nomad's own main package imports them.
	_ "github.com/hashicorp/nomad/client/allocrunner/taskrunner/getter"
	_ "github.com/hashicorp/nomad/client/allocrunner/taskrunner/template/renderer"
	_ "github.com/hashicorp/nomad/client/logmon"
	_ "github.com/hashicorp/nomad/drivers/docker/docklog"
	_ "github.com/hashicorp/nomad/drivers/shared/executor"

	"github.com/hashicorp/cli"
	"github.com/hashicorp/nomad/command"
	"github.com/hashicorp/nomad/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	configure()

	meta := new(command.Meta)
	meta.SetupUi(args)
	agentUI := &cli.BasicUi{Reader: os.Stdin, Writer: os.Stdout, ErrorWriter: os.Stderr}

	commands := command.Commands(meta, agentUI)
	for name, factory := range cystemdCommands(agentUI) {
		commands[name] = factory
	}

	c := &cli.CLI{
		Name:                       "cystemd",
		Version:                    version.GetVersion().FullVersionNumber(true),
		Args:                       args,
		Commands:                   commands,
		HiddenCommands:             hiddenCommands,
		Autocomplete:               true,
		AutocompleteNoDefaultFlags: true,
		HelpFunc:                   helpFunc,
		HelpWriter:                 os.Stdout,
	}
	code, err := c.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing CLI: %s\n", err)
		return 1
	}
	return code
}

// hiddenCommands are internal or deprecated Nomad commands kept out of help.
var hiddenCommands = []string{
	"alloc-status", "check", "client-config", "debug", "eval-status",
	"executor", "logmon", "node-drain", "node-status", "server-force-leave",
	"server-join", "server-members", "syslog", "docker_logger",
	"operator raft _info", "operator raft _logs", "operator raft _state",
	"operator snapshot _state", "template-render",
}

// helpFunc lists CYSTEMD's own commands first, then the Nomad commands.
func helpFunc(commands map[string]cli.CommandFactory) string {
	own := cystemdCommandNames()
	hidden := map[string]bool{}
	for _, h := range hiddenCommands {
		hidden[h] = true
	}
	var b strings.Builder
	b.WriteString("Usage: cystemd [-version] [-help] <command> [args]\n\n")
	b.WriteString("CYSTEMD commands:\n")
	writeCommands(&b, commands, own)
	var nomad []string
	for name := range commands {
		if !hidden[name] && !containsString(own, name) && !strings.Contains(name, " ") {
			nomad = append(nomad, name)
		}
	}
	sort.Strings(nomad)
	b.WriteString("\nNomad commands:\n")
	writeCommands(&b, commands, nomad)
	return strings.TrimRight(b.String(), "\n")
}

func writeCommands(b *strings.Builder, commands map[string]cli.CommandFactory, names []string) {
	width := 0
	for _, n := range names {
		width = max(width, len(n))
	}
	for _, n := range names {
		factory, ok := commands[n]
		if !ok {
			continue
		}
		cmd, err := factory()
		if err != nil {
			continue
		}
		fmt.Fprintf(b, "    %-*s    %s\n", width, n, cmd.Synopsis())
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
