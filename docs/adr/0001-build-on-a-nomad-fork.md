# 0001. Ship CYSTEMD as a thin fork of Nomad

Status: Accepted

## Context

CYSTEMD v4 was its own agent on every host: it pulled configuration from Git,
installed packages, wrote files and restarted units, with no scheduler, no
rollout strategy, no placement and no view of the fleet. Rewriting it means
choosing a substrate for scheduling, supervision and rollout. Nomad does all
of it, runs non-container workloads through task drivers, and integrates with
Consul and Vault, which the organisation already runs.

Nomad can be extended with external task driver plugins, but not with HTTP
endpoints or a replacement UI: a GitOps control plane would be a separate
service with its own address, TLS, authentication and deployment, and the
operator would install two products.

## Decision

Build CYSTEMD on a fork of Nomad that adds only hook points — HTTP extensions
on the agent's listeners, a replaceable `/ui/` handler, a `cystemd {}`
configuration block, shutdown of extensions and a product line in `version` —
as an overlay of new files and four small patches applied with no fuzz to a
checksum-pinned upstream release. Everything CYSTEMD does lives outside the
Nomad tree and plugs in from the `cystemd` binary's main package: the systemd
driver in the built-in plugin catalog, the control plane on the server agents'
HTTP address, the CYSTEMD UI under `/ui/`, the CYSTEMD commands in Nomad's
CLI.

Keep the same control plane available as `cystemd-server`, a standalone
process for clusters that run upstream Nomad.

## Consequences

- One binary and one address per host; the control plane gets Nomad's TLS,
  and its high availability comes with the server agents.
- Nomad's version, RPC, API and behaviour are unchanged, so `cystemd` and
  `nomad` agents of the same version interoperate and Nomad's documentation,
  tooling and upgrades apply.
- Each Nomad upgrade means re-applying the patches (`scripts/fork-assemble.sh`
  fails loudly when one does not apply) and re-running the end-to-end tests.
  The fork's surface is a few dozen lines to keep that cheap.
- The agent binary is BUSL-1.1 licensed, as Nomad is ([0008](0008-licensing-boundary.md)).
