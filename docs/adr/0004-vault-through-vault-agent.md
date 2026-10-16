# 0004. Reach Vault only through Vault Agent and workload identity

Status: Accepted

## Context

CYSTEMD v4 embedded a Vault client: it logged in with AppRole credentials from
its `.env`, renewed leases itself, and wrote secrets to files. That made
CYSTEMD responsible for token lifecycles, put long-lived credentials in its
configuration, and duplicated what Vault Agent does better.

## Decision

CYSTEMD never logs in to Vault itself.

- **The control plane** talks to a local Vault Agent's API proxy
  (`use_auto_auth_token`). The agent logs in (AppRole on hosts, the job's
  workload identity when the control plane runs as a Nomad job) and renews
  its token; the control plane holds no token. It needs Vault only for
  repository credentials (KV v2).
- **Jobs** read secrets with their Nomad workload identity, through Vault's
  JWT auth method (`deploy/vault/setup.sh` sets up `auth/jwt-nomad`, its roles
  and a policy confining each job to its own path). Nomad's template runner
  renders them, or, for systemd sources with `secretsDelivery: vault-agent`, a
  Vault Agent sidecar does.
- **Hosts** that rendered v4's `VAULT_TEMPLATES` run a Vault Agent with those
  templates; `cystemd migrate` generates its configuration.

## Consequences

- No Vault token or AppRole secret in CYSTEMD's configuration or memory;
  revoking a host's or a job's access is a Vault operation.
- Vault Agent is one more process per host (a systemd unit, or a Nomad job on
  the systemd driver: `deploy/nomad/vault-agent.nomad.hcl`).
- v4 templates need converting to consul-template syntax, which `cystemd
  migrate` does for the common forms.
