# 0002. Adopt Argo CD's application model and UX

Status: Accepted. Hooks: amended by [0009](0009-resource-hooks-as-batch-jobs.md).

## Context

The people who will operate CYSTEMD know Argo CD from Kubernetes: its
Applications, projects, sync and health statuses, diffs, sync options, history
and rollback, its UI's resource tree and sliding panels, and its CLI. A GitOps
tool for Nomad could invent its own vocabulary, but every difference is
something to learn and a reason to doubt what the tool will do.

## Decision

Model CYSTEMD on Argo CD wherever Nomad allows: the `Application` and
`AppProject` objects with the same field names (`source`, `destination`,
`syncPolicy.automated.{prune,selfHeal,allowEmpty}`, `syncOptions`,
`ignoreDifferences`, `revisionHistoryLimit`), the same sync and health
statuses and auto-sync rules, sync waves, partial and dry-run syncs, rollback
to a history entry, the cascade finalizer, RBAC in Argo CD's policy format,
an API shaped like Argo CD's, a UI with the same pages and interactions
(application tiles, the tree, the sync and diff panels, history), and a CLI
with the same verbs (`app sync`, `app diff`, `app wait`, …).

Translate what is Kubernetes-specific: resources are Nomad jobs and Consul
keys, tracked by the job meta `cystemd.app`; the tree shows deployments, task
groups, services, allocations and tasks; health is computed from Nomad's
objects; resource actions are Nomad's (restart, stop, signal, promote).

## Consequences

- Argo CD users find their way without reading documentation; Argo CD's
  documentation explains most behaviours.
- Where Nomad differs, CYSTEMD follows Nomad and says so (for example, an
  out-of-sync job is the one Nomad's plan would change; see [0007](0007-nomad-plan-decides-sync-status.md)).
- Argo CD features with no Nomad counterpart (Helm, Kustomize, hooks as
  Kubernetes resources) are not reproduced; the systemd source covers the
  values-file use case CYSTEMD v4 had.
