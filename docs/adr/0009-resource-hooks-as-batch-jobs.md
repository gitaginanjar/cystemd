# 0009. Resource hooks as batch jobs

Status: Accepted. Amends the consequence of [0002](0002-argo-cd-application-model.md)
that left hooks out.

## Context

Argo CD runs hooks around a sync: Kubernetes resources, usually Jobs,
annotated with the phase they run in (`PreSync`, `Sync`, `PostSync`,
`SyncFail`, `Skip`) and a delete policy. Teams use them for what has to
happen around a rollout: migrate a database before the new version starts,
run a smoke test after it, alert or clean up when it fails. 0002 left them out
as Kubernetes-specific, but the needs are the same on Nomad, and Nomad has
the building block: a batch job runs to completion and reports whether it
succeeded.

## Decision

A batch job of the source whose meta `cystemd.hook` lists phases is a hook.
Hooks are taken out of the comparison, the sync and pruning, and run on full
syncs: `PreSync` before anything is synced, `PostSync` once every synced job
is healthy, `SyncFail` when the sync failed. The hooks of a phase run by
sync wave, those of a wave together, and each must complete for the
operation to go on. Every run is a new job version, marked with the meta
`cystemd.hook-run`; Argo CD's delete policies decide when the job is purged
(`BeforeHookCreation` by default, `HookSucceeded`, `HookFailed`), and
`cystemd.hook-timeout` bounds a run (30 minutes by default). Hook jobs appear
among the Application's resources without a sync status and without a say in
its health. Terminating the operation stops the running hooks.

Not reproduced: `Sync` and `Skip` hooks (the Sync phase is the Application's
jobs, already ordered by waves), `PreDelete` and `PostDelete`, and hooks on
rollback, which reverts Nomad job versions instead of syncing a past
revision.

## Consequences

- Migrations, smoke tests and failure notifications live in Git next to the
  jobs they guard, and a failed `PreSync` hook stops a rollout before any job
  changes.
- An operation lasts as long as its hooks (bounded by their timeouts) and
  occupies one of the controller's workers meanwhile; other Applications go
  on being reconciled.
- A hook's job ID belongs to the hook: the controller refuses to replace a
  job of another Application, or a job that is not a hook, with it.
- Nomad's restart and reschedule policies apply to hooks; a hook that must
  not be retried says so in its job.
