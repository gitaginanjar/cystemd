// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Command cystemdctl is the CYSTEMD command line client, the counterpart of
// Argo CD's argocd CLI: it logs in to a control plane (cystemd-server or a
// cystemd server agent) and manages applications, projects, repositories
// and accounts through the CYSTEMD API. `cystemdctl migrate` converts a
// CYSTEMD v4 installation.
//
// The cystemd agent binary carries the same commands (`cystemd app sync`).
// This one links no Nomad code, only the API client.
package main

import (
	"fmt"
	"os"

	"github.com/hashicorp/cli"

	"github.com/gitaginanjar/cystemd/internal/buildinfo"
	cycli "github.com/gitaginanjar/cystemd/internal/cli"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	ui := &cli.BasicUi{Reader: os.Stdin, Writer: os.Stdout, ErrorWriter: os.Stderr}
	c := &cli.CLI{
		Name:     "cystemdctl",
		Version:  buildinfo.Version() + " (" + buildinfo.Commit() + ")",
		Args:     args,
		Commands: cycli.Commands(ui, cycli.Options{Name: "cystemdctl", Version: buildinfo.Version()}),
		HelpFunc: cli.BasicHelpFunc("cystemdctl"),
		// Help goes to stdout, like `cystemdctl app -h`.
		HelpWriter: os.Stdout,
	}
	code, err := c.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}
	return code
}
