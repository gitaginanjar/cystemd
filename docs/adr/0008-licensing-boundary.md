# 0008. Keep CYSTEMD's own binaries free of BUSL code

Status: Accepted

## Context

CYSTEMD's code is Apache-2.0. Nomad is licensed under the Business Source
License 1.1; its API client (`github.com/hashicorp/nomad/api`) and the
plugin SDK's MPL-2.0 packages are separate, but the SDK links Nomad
packages. The agent binary is necessarily built from Nomad
([0001](0001-build-on-a-nomad-fork.md)).

## Decision

Split the repository into modules along the license boundary:

- the root module (`cystemd-server`, `cystemdctl`, the control plane, the UI,
  the CLI) depends on Nomad only through the MPL-2.0 API client;
- `agent/` and `driver/` build against the assembled Nomad tree and produce
  the BUSL-1.1 licensed binaries (`cystemd`, `cystemd-driver-systemd`);
- `fork/` holds the overlay and patches, which modify Nomad and are BUSL-1.1
  licensed like it.

A test (`internal/licensecheck`) fails if the root module ever depends on a
Nomad package other than the API client.

## Consequences

- Organisations can use `cystemd-server` and `cystemdctl` under Apache-2.0
  terms alone, with upstream Nomad.
- The agent is subject to the BUSL-1.1 terms that apply to Nomad itself; NOTICE
  says so.
- The `agent` module imports the root module's internal packages (allowed by
  Go's path rule, as `agent/` is under the root path); the reverse never
  happens.
