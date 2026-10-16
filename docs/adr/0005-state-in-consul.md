# 0005. Keep the state in Consul KV, with a leader-elected controller

Status: Accepted

## Context

Argo CD keeps Applications as Kubernetes custom resources: the API server
gives it storage, watches, optimistic concurrency and access control. Nomad
has no store meant for a third party's objects (Nomad Variables hold
workloads' configuration and secrets, under workload-identity ACLs). The
control plane must run with several replicas for availability without two of
them syncing the same Application.

## Decision

Store CYSTEMD's objects as JSON in Consul KV under a prefix (`cystemd/`):
Applications, projects, repositories, accounts, events, the keys each
Application owns, settings. Update Applications with check-and-set on the
modify index; turn blocking queries into watches. Elect the replica that runs
the controller with a Consul session lock (`cystemd/leader`); every replica
serves the API and the UI. Without Consul, keep the same layout in a local
file and run a single replica.

## Consequences

- No new datastore: Consul is already deployed with Nomad, and its ACLs scope
  the control plane to its prefix (`deploy/consul/policies`).
- The control plane fails over with Consul sessions (seconds); in the agent,
  the three server agents are the three replicas.
- Objects are limited by Consul's 512 KiB value size; status is kept compact
  (bounded history and events) to stay well within it.
- Consul KV is not encrypted at rest: secrets do not go there when Vault is
  configured ([0004](0004-vault-through-vault-agent.md)).
