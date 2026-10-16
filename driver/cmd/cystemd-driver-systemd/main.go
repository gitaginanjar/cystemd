// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Command cystemd-driver-systemd serves the CYSTEMD systemd task driver as an
// external Nomad plugin, for clusters running upstream Nomad instead of the
// cystemd agent (where the driver is built in). Install it into the client's
// plugin_dir and enable it with a `plugin "systemd" {}` block.
package main

import (
	"context"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/plugins"

	"github.com/gitaginanjar/cystemd/driver/systemd"
)

func main() {
	plugins.ServeCtx(func(ctx context.Context, logger hclog.Logger) any {
		return systemd.NewDriver(ctx, logger)
	})
}
