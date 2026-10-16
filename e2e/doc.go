// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package e2e holds CYSTEMD's end-to-end tests: the control plane, the
// command line and the web UI against real Consul, Nomad and Vault dev
// agents, and the cystemd agent (the Nomad fork) itself.
//
//	make test-e2e
//
// They need consul, nomad and vault on PATH (each test skips what is
// missing), bin/cystemd for the agent test (make agent) and a Chromium
// for the UI test (CHROME_PATH, or chromium/google-chrome on PATH).
// CYSTEMD_E2E_SCREENSHOTS=<dir> keeps the UI test's screenshots.
//
// It is a module of its own so that the browser driver stays out of the
// main module's dependencies.
package e2e
