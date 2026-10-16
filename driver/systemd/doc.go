// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package systemd is the CYSTEMD Nomad task driver: it runs Nomad tasks as
// systemd units, so the daemons CYSTEMD always managed — RPM-installed
// services with their own unit files — become Nomad workloads without being
// repackaged into containers.
//
// The driver has two modes, chosen per task:
//
//   - Unit mode (`unit = "myapp.service"`): manage an existing unit, usually
//     installed by a package. The driver optionally installs the wanted
//     package version first (the rpm/dpkg database is the source of truth,
//     never a breadcrumb), copies rendered files (templates, Vault secrets) to
//     the host paths the daemon reads, and attaches the task environment and
//     resource limits through a runtime drop-in it removes again on destroy.
//
//   - Transient mode (`command = "/usr/bin/x"`): run a command in a transient
//     unit (like `systemd-run`), with systemd as the process supervisor and
//     cgroup owner.
//
// Every host-mutating capability is opt-in per client through the plugin
// configuration (allowed_units, allowed_packages, allowed_file_destinations,
// allow_transient): a job author can never reach a unit the node operator did
// not allow. See docs/DRIVER.md.
//
// The package builds against Nomad's plugin SDK (MPL-2.0, which links Nomad
// BUSL-1.1 packages); the cystemd agent registers it as a built-in driver and
// cmd/cystemd-driver-systemd serves it as an external plugin.
package systemd
