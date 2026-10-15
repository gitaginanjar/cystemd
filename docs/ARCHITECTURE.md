# Architecture

CYSTEMD is a GitOps control plane for HashiCorp Nomad, shaped after Argo CD,
and a small fork of Nomad that ships it. This document describes the moving
parts and how a change in Git becomes a running unit.

## The pieces

| Piece | Where | What it is |
|---|---|---|
| `cystemd` | [agent/](../agent), [fork/](../fork) | The agent: Nomad 2.0.7 with four small patches (see [FORK.md](FORK.md)), the systemd task driver built in, and in server agents the control plane and web UI, served on Nomad's HTTP address. |
| `cystemd-server` | [cmd/cystemd-server](../cmd/cystemd-server) | The same control plane as a standalone process, for upstream Nomad. It links no Nomad code, only the MPL-2.0 API clients. |
| `cystemdctl` | [cmd/cystemdctl](../cmd/cystemdctl) | The command line, also built into the agent (`cystemd app …`). |
| systemd driver | [driver/](../driver) | A Nomad task driver running tasks as systemd units (see [DRIVER.md](DRIVER.md)), built in or as an external plugin. |
| Consul | — | State store, leader election, the keys applications manage, service health. |
| Vault + Vault Agent | — | Repository credentials (KV v2) for the control plane, and secrets for jobs through Nomad workload identity. Nothing in CYSTEMD holds a long-lived Vault token. |

Inside the control plane:

| Package | Role (Argo CD counterpart) |
|---|---|
| [internal/controller](../internal/controller) | The application controller: watches, compares, syncs, assesses health, records history (`argocd-application-controller`). `RepoService` resolves revisions and renders manifests (`argocd-repo-server`). |
| [internal/render](../internal/render) | Turns a source into Nomad jobs and Consul keys: Nomad jobspecs, parsed by Nomad itself, or a systemd source (a values file) rendered into a systemd-driver job. |
| [internal/server](../internal/server) | The HTTP API (`/api/v1/…`, shaped like Argo CD's), the web UI (`/ui/`), event streams, the Git webhook, `/metrics` and `/healthz` (`argocd-server`). |
| [internal/store](../internal/store) | Typed objects on a key/value store with compare-and-set updates and watches: Consul KV, or a local file for a single replica. Leader election. |
| [internal/auth](../internal/auth), [internal/rbac](../internal/rbac) | Sessions, local accounts, Nomad-token and v4-JWT login; Argo CD's RBAC policy format. |
| [internal/git](../internal/git) | Git over HTTPS and SSH (go-git): `ls-remote`, a clone cache per repository, commit metadata. |
| [internal/health](../internal/health), [internal/diff](../internal/diff) | Health assessment of Nomad objects; normalization and diffs of jobs. |
| [internal/vault](../internal/vault), [internal/vaultagent](../internal/vaultagent) | KV access through a Vault Agent's API proxy; generation of Vault Agent configurations. |
| [ui/](../ui) | The web UI: templ views, CSS, a little JavaScript and vendored htmx. |
| [pkg/apis/v1alpha1](../pkg/apis/v1alpha1), [pkg/client](../pkg/client) | The `Application` and `AppProject` types; the Go API client. |

## The object model

The model is Argo CD's, with Nomad as the platform (see
[APPLICATIONS.md](APPLICATIONS.md)):

- An **Application** has a *source* (a repository, revision and path, rendered
  as Nomad jobspecs or as a systemd source), a *destination* (a Nomad cluster
  and namespace) and a *sync policy*. Its status holds the compared revision,
  the sync and health status of each resource, conditions, the last operation
  and the deployment history. A requested sync is its `operation`.
- An **AppProject** limits the repositories and destinations of its
  Applications.
- A **Repository** is a connection to Git; its credentials are stored apart.
- Resources are Nomad **jobs** and **Consul keys**. A job belongs to an
  Application through its meta `cystemd.app = <application>`, the counterpart
  of Argo CD's tracking label. Under a job, the live tree shows its
  deployment, task groups, services, allocations and tasks.

State lives in Consul under a prefix (`cystemd/` by default):

```
cystemd/applications/<name>     Application: metadata, spec, status, operation
cystemd/projects/<name>         AppProject
cystemd/repositories/<id>       Repository (no secret material)
cystemd/repo-creds/<id>         repository credentials, when Vault is not used
cystemd/accounts/<name>         local account (bcrypt hash)
cystemd/events/<application>    the last 100 events of an Application
cystemd/owned-kv/<application>  the Consul keys an Application wrote
cystemd/settings/<name>         server settings (the session signing key)
cystemd/values/<app>/<file>     systemd sources' rendered configuration files
cystemd/leader                  the leader lock
```

Application updates are compare-and-set on the key's modify index, so
replicas and the controller never overwrite each other's changes; watches
turn changes into events.
Without Consul, the same layout lives in a file in the data directory and
only one replica may run.

## From a commit to a running unit

```
 Git ──ls-remote──▶ RepoService ──render──▶ desired jobs and keys
                                                  │
 Nomad ◀──────── plan / register / revert ──── controller ◀── store watch
   │                                              ▲              ▲
   └──── event stream (jobs, allocations, ────────┘          API, UI, CLI,
         deployments)                                         webhooks
```

The controller runs on one replica, the leader, with a work queue and a pool
of workers. An Application is queued when it changes in the store (a new
spec, a requested operation or refresh, a deletion), when Nomad reports an
event on one of its jobs, allocations or deployments, and every
`reconcile_interval` (three minutes by default). A Git webhook queues the
Applications of the pushed branch at once.

**Compare.** The worker resolves the target revision to a commit
(`git ls-remote`, cached for ten seconds; a refresh bypasses the cache) and
renders the source at that commit (cached by commit and spec: commits do not
change). Nomad jobspecs go through Nomad's own parser (`/v1/jobs/parse`), so
HCL2 functions and variables behave as in `nomad job run`. For each desired
job, the live job is fetched and Nomad is asked for a plan: the job is
`OutOfSync` when the plan has changes. The diff shown to people is computed
from both jobs, normalized (server-side fields dropped, defaults filled) and
with the Application's `ignoreDifferences` applied to the desired side.
Consul keys are compared byte for byte. Jobs that carry the Application's
meta but are no longer rendered, and keys the Application wrote that are no
longer rendered, *require pruning*.

**Health.** Each job's health comes from its type and state: its latest
deployment, the allocations of its current version (client status,
deployment health, Consul or Nomad service checks), queued placements, or
for batch jobs the outcome of their runs ([internal/health](../internal/health)).
The Application takes the worst health of its resources.

**Sync.** An operation runs resources in waves: Consul keys first (wave -1),
then jobs by their meta `cystemd.sync-wave` (default 0), and, with the
`PruneLast=true` option, pruning last. Jobs are registered with
`EnforceIndex`, so a job changed in between fails the operation rather than
being overwritten blindly. Options follow Argo CD: `CreateNamespace`,
`ApplyOutOfSyncOnly`, `PreserveCounts` (keep scaled counts), `Prune=false`
(never prune), dry runs, partial syncs, retries with backoff, termination. When it
succeeds, the operation records a history entry with the job versions Nomad
gave the registered jobs.

**Automation.** With `syncPolicy.automated`, an `OutOfSync` Application is
synced unless the last automated sync of the same revision failed (Argo CD's
"skipping auto-sync: failed previous sync attempt"), and pruning happens only
with `prune: true`. With `selfHeal: true`, drift without a new commit (a job
stopped or edited by hand, a key changed) is corrected after
`self_heal_timeout`.

**Rollback** reverts each job to the version a history entry recorded (Nomad
keeps job versions), restores that revision's Consul keys, and is refused
while automated sync is on.

**Deletion** with cascade (the `resources-finalizer.cystemd.io` finalizer)
deregisters and purges the Application's jobs and deletes the keys it wrote
before removing it.

## Leader election and high availability

Every control plane replica serves the API and the UI; one, holding the
`cystemd/leader` lock (a Consul session), runs the controller. In the agent,
the three server agents each run a replica, so the control plane fails over
as the servers do. The UI reads live data (trees, logs, diffs) from Nomad
directly on whichever replica serves the request.

## The API and the web UI

The API follows Argo CD's (`/api/v1/applications`, `/sync`, `/rollback`,
`/resource-tree`, `/managed-resources`, `/resource/actions`, logs and watch
streams as server-sent events); the CLI uses it through
[pkg/client](../pkg/client). `/api/webhook` takes Git push events.

The web UI is rendered on the server with [templ](https://templ.guide) and
made interactive with [htmx](https://htmx.org): each state of a page has a
URL (a sliding panel is `?panel=diff`, a resource is `?node=…`), links and
forms swap fragments, and live updates arrive on an event stream that only
says *something changed*, after which the affected regions re-render. No
JavaScript build is involved; the whole UI works under a strict
Content-Security-Policy (`script-src 'self'`, no inline scripts). In the
agent, the UI replaces Nomad's under `/ui/` and Nomad's own CSP applies.
[ADR 0003](adr/0003-server-rendered-ui.md) explains the choice.

## Security

- **Identity.** Users log in with a local account (bcrypt, throttled) or a
  Nomad ACL token, which the control plane validates against Nomad and maps to
  RBAC groups (`nomad:management`, `nomad:policy:<name>`). Sessions are
  HS256 tokens signed with a key kept in the store; changing a password
  revokes the account's sessions. During a v4 migration, v4's Ed25519 JWTs can
  be accepted as `role:admin`.
- **Authorization** is an Argo CD RBAC policy: `role:admin`, `role:readonly`
  and your own roles, matched against `<project>/<application>`.
- **The browser.** Session cookies are `HttpOnly` and `SameSite=Lax`;
  state-changing UI requests must be htmx requests from the same origin, and
  API writes with a cookie must be JSON. A strict CSP applies.
- **The control plane's own credentials** are least-privilege: the Nomad and
  Consul policies in [deploy/](../deploy) grant exactly what it uses (the
  end-to-end suite runs it with only those), and Vault is reached through a
  Vault Agent that logs in with AppRole and renews its token.
- **Repository credentials** are stored in Vault (KV v2) when Vault is
  configured, never in Consul; without Vault they are kept under
  `cystemd/repo-creds/`, which the Consul ACL policy should restrict to the
  control plane.
- **Jobs** read secrets with their Nomad workload identity; the policies in
  [deploy/vault](../deploy/vault) confine each job to its own path.
- **Hosts.** The systemd driver can only touch the units, packages, paths and
  properties its node configuration allows ([DRIVER.md](DRIVER.md)).
- **Audit.** Every change and every denial is logged as an `[AUDIT]` line:
  who, from where, what, on what, and the result.

## Observability

`/metrics` exposes Prometheus metrics: each Application's sync and health
status (`cystemd_app_info`), sync operations (`cystemd_app_sync_total`),
reconcile durations, HTTP requests and latencies, audited actions, Git
requests, and whether the replica leads (`cystemd_controller_leader`). `/healthz` is the liveness probe; `/api/v1/health` reports
the leader, Consul, Vault and each Nomad cluster.
