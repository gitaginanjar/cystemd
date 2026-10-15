// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"github.com/hashicorp/cli"
	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/helper/pluginutils/catalog"
	"github.com/hashicorp/nomad/helper/pluginutils/loader"
	"github.com/hashicorp/nomad/plugins/base"
	"github.com/hashicorp/nomad/version"

	"github.com/gitaginanjar/cystemd/driver/systemd"
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

	// The systemd task driver is built in, like raw_exec and docker.
	catalog.Register(loader.PluginID{Name: systemd.PluginName, PluginType: base.PluginTypeDriver},
		&loader.InternalPluginConfig{
			Config: map[string]any{},
			Factory: func(ctx context.Context, l hclog.Logger) any {
				return systemd.NewDriver(ctx, l)
			},
		})
}

// cystemdCommands are the CYSTEMD commands added to the Nomad CLI.
func cystemdCommands(ui cli.Ui) map[string]cli.CommandFactory {
	return map[string]cli.CommandFactory{}
}

func cystemdCommandNames() []string {
	return nil
}
