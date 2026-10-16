// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"net/http"

	"github.com/hashicorp/cli"
	"github.com/hashicorp/go-hclog"
	nomadagent "github.com/hashicorp/nomad/command/agent"
	"github.com/hashicorp/nomad/helper/pluginutils/catalog"
	"github.com/hashicorp/nomad/helper/pluginutils/loader"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/version"

	"github.com/gitaginanjar/cystemd/driver/systemd"
	cycli "github.com/gitaginanjar/cystemd/internal/cli"
)

// Build metadata, injected with -ldflags -X main.<name>=… (see Makefile).
var (
	cystemdVersion = "dev"
	cystemdCommit  = "unknown"
)

// configure wires CYSTEMD into the Nomad fork through its hook points.
func configure() {
	version.ProductName = "CYSTEMD"
	version.ProductVersion = cystemdVersion
	if cystemdCommit != "unknown" {
		version.ProductVersion += " (" + cystemdCommit + ")"
	}

	// The control plane: API under /api/, web UI as the agent's /ui/.
	nomadagent.RegisterHTTPExtension(controlPlaneExt)
	nomadagent.UIHandler = http.HandlerFunc(controlPlaneExt.serveUI)

	// The systemd task driver is built in, like raw_exec and docker.
	catalog.Register(loader.PluginID{Name: systemd.PluginName, PluginType: base.PluginTypeDriver},
		&loader.InternalPluginConfig{
			Config: map[string]any{},
			Factory: func(ctx context.Context, l hclog.Logger) any {
				return systemd.NewDriver(ctx, l)
			},
		})
}

// cliOptions mount the CYSTEMD commands in the Nomad CLI: the session
// commands go under "ctl" (Nomad has its own login and version), and
// without a CYSTEMD context the commands talk to NOMAD_ADDR with
// NOMAD_TOKEN, where the agent serves the CYSTEMD API.
func cliOptions() cycli.Options {
	return cycli.Options{
		Name:           "cystemd",
		NomadFallback:  true,
		DefaultAddress: "http://127.0.0.1:4646",
		Version:        cystemdVersion,
		SessionPrefix:  "ctl ",
	}
}

// cystemdCommands are the CYSTEMD commands added to the Nomad CLI.
func cystemdCommands(ui cli.Ui) map[string]cli.CommandFactory {
	return cycli.Commands(ui, cliOptions())
}

func cystemdCommandNames() []string {
	return cycli.TopLevel(cliOptions())
}
