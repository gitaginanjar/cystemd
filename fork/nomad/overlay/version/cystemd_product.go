// Copyright IBM Corp. 2015, 2026
// Copyright 2026 G.G (https://github.com/gitaginanjar) — CYSTEMD fork changes
// SPDX-License-Identifier: BUSL-1.1

// CYSTEMD fork: product branding for downstream distributions.
//
// Version stays the upstream Nomad version on purpose: servers and clients
// gate features on it (minimum-version checks), so a distribution must never
// change it. The product line is printed above it instead.

package version

var (
	// ProductName is the distribution name printed by `version` (empty for
	// upstream Nomad).
	ProductName = ""

	// ProductVersion is the distribution version printed next to ProductName.
	ProductVersion = ""
)
