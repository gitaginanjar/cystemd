// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	godbus "github.com/godbus/dbus/v5"
)

// envFileEntry is one EnvironmentFiles entry, D-Bus signature (sb): the path
// and whether a missing file is ignored ("-" prefix in unit files).
type envFileEntry struct {
	Path         string
	IgnoreErrors bool
}

func godbusVariant(v any) godbus.Variant {
	return godbus.MakeVariant(v)
}
