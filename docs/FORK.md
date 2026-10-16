# The Nomad fork

The `cystemd` agent is HashiCorp Nomad with a few hook points added, so that
one binary is a complete CYSTEMD host: a Nomad server or client, the systemd
task driver, and on servers the control plane and web UI on Nomad's own HTTP
address. [ADR 0001](adr/0001-build-on-a-nomad-fork.md) explains why a fork
rather than a plugin and a sidecar.

The fork is deliberately thin: everything CYSTEMD does lives outside the Nomad
tree and plugs into it from [agent/](../agent).

## What the fork changes

Base: **Nomad 2.0.7** (commit `9dcbdc5e64ec`), pinned in
[fork/nomad/UPSTREAM](../fork/nomad/UPSTREAM).

New files ([fork/nomad/overlay](../fork/nomad/overlay); an overlay file may
never replace an upstream one):

| File | What it adds |
|---|---|
| `command/agent/cystemd_hooks.go` | The hook points: `HTTPExtension` (mount handlers on every public HTTP listener, shut down with the agent), `UIHandler` (replace the web UI under `/ui/`), and the `cystemd {}` configuration block. |
| `version/cystemd_product.go` | `ProductName` and `ProductVersion`, printed by `cystemd version`. |

Patches ([fork/nomad/patches](../fork/nomad/patches), applied in order with
no fuzz):

| Patch | Size | What it does |
|---|---|---|
| `0001-agent-cystemd-config-block.patch` | +7 | Adds `Config.Cystemd` and merges and copies it like every other agent block. |
| `0002-agent-http-extension-and-ui-hooks.patch` | +5 −1 | Mounts the registered extensions on each public HTTP listener (not the task API socket) and serves `UIHandler` under `/ui/`, wrapped in Nomad's `ui.content_security_policy`. |
| `0003-agent-shutdown-extensions.patch` | +2 | Shuts the extensions down with the agent. |
| `0004-version-product-branding.patch` | +4 | Prints the product line first in `version`. |

What the fork does **not** change: Nomad's version number (servers and
clients gate features on it, so a `cystemd` agent stays wire-compatible with
upstream Nomad agents of the same version), its RPC, state store, scheduler,
API, ACLs, or any command. A cluster can mix `cystemd` and `nomad` agents:
upstream clients simply have no systemd driver.

## What plugs in

[agent/configure.go](../agent/configure.go), run before Nomad's CLI:

- registers the systemd driver in Nomad's built-in plugin catalog, next to
  `docker` and `raw_exec`;
- registers the control plane as an `HTTPExtension`: on an agent with
  `cystemd { control_plane { enabled = true } }` (the server agents) or in
  `-dev` mode, it starts once, with defaults taken from the agent's own
  configuration (its address and TLS files for Nomad, its Consul address and
  token, its region and data directory), and serves `/api/`; other agents
  answer `/api/` and `/ui/` with a pointer to the servers;
- sets `UIHandler` to the CYSTEMD web UI;
- adds the CYSTEMD commands to Nomad's CLI (`cystemd app …`, `cystemd ctl …`);
- sets the product line (`CYSTEMD <version> (<commit>)`).

```hcl
# Server agent: deploy/agent/server.hcl
cystemd {
  control_plane {
    enabled     = true
    config_file = "/etc/cystemd/control-plane.hcl"
  }
}
```

## Building

```sh
make agent        # scripts/fork-assemble.sh, then go build in agent/
```

[scripts/fork-assemble.sh](../scripts/fork-assemble.sh) assembles the fork
into `build/nomad` (a build artefact, not committed):

1. downloads Nomad and its nested `api` and `jobspec2` modules at the pinned
   versions through the Go module proxy; `go mod download` checks them against
   the Go checksum database, and the script checks them against the `h1:`
   sums pinned in `UPSTREAM`, so the tree is byte for byte the release;
2. copies the overlay, refusing a file that exists upstream;
3. applies the patches with `--fuzz=0`, so a change of context upstream fails
   instead of patching the wrong place.

[agent/go.mod](../agent/go.mod) and [driver/go.mod](../driver/go.mod) build
against that tree through `replace` directives.

## Upgrading Nomad

1. Update `fork/nomad/UPSTREAM`: the version, the release commit, and the
   module pseudo-versions and `h1:` sums (Nomad 2 tags are not valid Go
   module versions, so the commit's pseudo-version is used; `go mod download
   -json github.com/hashicorp/nomad@<commit>` prints both).
2. `scripts/fork-assemble.sh`. A patch that no longer applies says so; fix it
   against the new tree (`diff -u`) and keep it minimal.
3. In `agent/` and `driver/`: `go mod tidy`, then `go build`.
4. Pin `github.com/hashicorp/nomad/api` to the same commit in the root
   `go.mod` (`go get github.com/hashicorp/nomad/api@<pseudo-version>`).
5. `make test test-integration test-e2e`. The agent's end-to-end test
   (`TestEndToEndAgent`) runs the new binary as a dev agent and deploys
   through it.

## Browsing the fork as Git history

[scripts/fork-export.sh](../scripts/fork-export.sh) produces the fork as a
branch of the upstream repository, for review or for a fork of
`hashicorp/nomad` on GitHub:

```sh
scripts/fork-export.sh                      # build/nomad-fork, branch cystemd/v2.0.7
git -C build/nomad-fork log --oneline -6    # the release, the overlay, one commit per patch
git -C build/nomad-fork push <your-fork> cystemd/v2.0.7
```

The branch has the same tree as `build/nomad`. Builds do not use it.

## Licensing

Nomad is licensed under the Business Source License 1.1. The `cystemd` agent
binary, built from it, is subject to that license; so are the overlay files
and patches, which modify Nomad. CYSTEMD's own code is Apache-2.0, and the
Apache-2.0 binaries, `cystemd-server` and `cystemdctl`, never link Nomad:
[internal/licensecheck](../internal/licensecheck) fails the build's tests if
the root module ever depends on a Nomad package other than the MPL-2.0 API
client. See [NOTICE](../NOTICE).
