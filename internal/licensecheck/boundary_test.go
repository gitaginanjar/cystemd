// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package licensecheck guards the licensing boundary of the repository: the
// root module (cystemd-server, the CLI, the control plane) is Apache-2.0 and
// may link HashiCorp's MPL-2.0 API clients, but never Nomad itself, whose
// source is BUSL-1.1. Only the agent/ and driver/ modules build against the
// Nomad tree (see NOTICE and docs/FORK.md).
package licensecheck

import (
	"os/exec"
	"strings"
	"testing"
)

func TestRootModuleNeverLinksBUSLNomad(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "-f", "{{.ImportPath}}", "github.com/gitaginanjar/cystemd/...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, out)
	}
	for _, pkg := range strings.Split(string(out), "\n") {
		if pkg == "github.com/hashicorp/nomad" || strings.HasPrefix(pkg, "github.com/hashicorp/nomad/") {
			if !strings.HasPrefix(pkg, "github.com/hashicorp/nomad/api") {
				t.Errorf("root module depends on %s (BUSL-1.1); only github.com/hashicorp/nomad/api (MPL-2.0) is allowed", pkg)
			}
		}
	}
}
