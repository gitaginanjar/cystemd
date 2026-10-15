# Applications

An Application is a set of Nomad jobs and Consul keys whose desired state is
in Git. The format is Argo CD's, with Nomad as the platform; manifests are
YAML or JSON, several per file:

```sh
cystemd app create -f applications.yaml        # or the UI: NEW APP → EDIT AS YAML
```

```yaml
apiVersion: cystemd.io/v1alpha1
kind: Application
metadata:
  name: payments-api              # lowercase letters, digits, '-', '.', '_'
  labels:
    team: payments
spec:
  project: platform               # default "default"
  source:
    repoURL: https://git.example.com/platform/deploy.git
    targetRevision: main          # branch, tag, commit or HEAD (default)
    path: apps/payments-api       # relative to the repository root
    nomad: {…}                    # or systemd: {…}
  destination:
    name: in-cluster              # a cluster from the control plane configuration
    namespace: payments           # default "default"
    region: europe                # optional
  syncPolicy: {…}
  ignoreDifferences: […]
  revisionHistoryLimit: 10
  info:                           # shown in the Application's details
    - name: Runbook
      value: https://wiki.example.com/payments
```

## Sources

### Nomad jobspecs

The default source: every jobspec directly in `path`.

```yaml
source:
  repoURL: https://git.example.com/platform/deploy.git
  path: apps/payments-api
  nomad:
    files: ["*.nomad.hcl"]        # default *.nomad.hcl, *.nomad, *.nomad.json
    varFiles: [prod.vars.hcl]     # HCL2 variable files, later ones win
    vars:                         # variables, winning over the files
      image_tag: "2.4.1"
    consulKV: consul-kv.yaml      # Consul keys the Application owns
```

HCL2 jobspecs are parsed by Nomad (`/v1/jobs/parse`), with the same functions
and variables as `nomad job run`; JSON jobspecs (`*.nomad.json`, the API
format) are read as they are. Each job gets the meta `cystemd.app`, which ties
it to the Application. A job in the `default` namespace moves to the
destination's namespace; a job naming another namespace keeps it, and the
project must allow it.

The Consul keys file lists the keys the Application manages. Changing a value
in Git and syncing writes it; a key removed from the file is deleted when the
Application prunes. Jobs read the keys with templates (`{{ key "…" }}`), so a
change re-renders the template and restarts or signals the task according to
its `change_mode`, without a new deployment.

```yaml
- key: config/payments-api/settings
  value: |
    PAYMENTS_PROVIDER=stripe
    PAYMENTS_TIMEOUT=8s
- key: config/payments-api/routes.json
  file: routes.json               # the content of a file next to it
```

Keys under the control plane's own prefix (`cystemd/`) are refused.

### systemd

The systemd source is CYSTEMD v4's `continuous_deployment` model: a service
installed from a package, configured from a Helm-style values file. CYSTEMD
renders it into a job for the [systemd task driver](DRIVER.md).

```yaml
source:
  repoURL: https://git.example.com/platform/deploy.git
  targetRevision: main
  path: apps/payments-api
  systemd:
    valuesFile: values.yaml       # in path (default values.yaml)
    selector: payments-api        # replaces ${selector} in the keys below
    configKey: .${selector}.services.${selector}.config.[]
    versionKey: .${selector}.global.image.tag
    cpuKey: .${selector}.services.${selector}.deployment.resources.limits.cpu
    memoryKey: .${selector}.services.${selector}.deployment.resources.limits.memory
    unit: payments-api.service    # default: the Application's name
    package: payments-api         # default: the unit without .service
    serviceConfig: target.config.yml   # the configuration file name (default)
    workdir: /opt                 # files go to <workdir>/<unit name>/ (default /opt)
    replicas: 0                   # 0: every eligible node (a system job); default 1
    nodeSelector: payments        # nodes with client meta cystemd_selector_payments = "true"
    nodePool: default
    datacenters: ["*"]
    secrets:
      - vaultPath: kubernetes/payments/payments-api   # logical KV path
        kvVersion: 2              # default 2
        destination: .env         # in the service directory (default .env)
    secretsDelivery: nomad        # or vault-agent
    vaultRole: cystemd-workloads  # the Vault JWT role (default from the control plane)
    configDelivery: consul        # or inline (default consul when Consul is used)
    changeMode: restart           # restart, signal or noop
    changeSignal: SIGHUP          # with changeMode signal
    service:                      # register the unit in Consul (or Nomad)
      port: 8080
      tags: [api]
      check:
        type: http                # http or tcp
        path: /healthz
        interval: 10s
        timeout: 2s
    resources:                    # Nomad's scheduling reservation (MHz, MB)
      cpu: 200
      memoryMB: 512
```

The keys are yq-style paths into the values file, as v4 read them: `.a.b`
descends; `[]` takes the first element of a list, or the only value of a
one-key map (parsed again when it is a YAML string: the Helm
`config: {file.yml: |…}` case), and passes a larger map through.
`versionKey`, `cpuKey` and `memoryKey` must point at scalars; CPU and memory
are Kubernetes quantities (`500m`, `1Gi`) and become the unit's `CPUQuota`
and `MemoryMax`.

The generated job has one task, driven by the systemd driver in unit mode:

- **Package.** With `versionKey`, the driver installs that version of the
  package before starting the unit (`dnf install <package>-<version>`, or
  `apt-get install <package>=<version>`), unless it is installed already.
- **Configuration.** The extracted configuration is delivered through a Consul
  key the Application manages (`cystemd/values/<application>/<file>`, read by
  a template, so a configuration change restarts the unit in place, without a
  new allocation) or inline in the job. The driver keeps the rendered file at
  `<workdir>/<unit>/<serviceConfig>`, where v4 wrote it.
- **Secrets.** Each secret is rendered into an env file (`KEY="value"` lines)
  that the driver keeps at `<workdir>/<unit>/<destination>` and passes to the
  unit as an `EnvironmentFile`. With `secretsDelivery: nomad`, Nomad's
  template runner reads Vault with the task's workload identity. With
  `vault-agent`, an init task and a sidecar Vault Agent (transient units) do
  it, for nodes where Nomad's Vault integration is not configured.
- **Placement.** One instance per node at most (`distinct_hosts`); `replicas:
  0` runs it on every node matching `nodeSelector` (a system job). Updates are
  rolling, one node at a time, healthy after ten seconds (or the service
  check), and revert automatically when a new version fails.

[examples/gitops/apps/payments-api](../examples/gitops/apps/payments-api)
shows a values file and the Application that uses it.

## Destinations

`destination.name` is a cluster of the control plane: `in-cluster` (the Nomad
the control plane runs with) or a `cluster "<name>" {}` block of its
configuration. The namespace must exist unless the sync option
`CreateNamespace=true` is set (which needs a management token).

## Sync policy

```yaml
syncPolicy:
  automated:                      # without it, syncs are manual
    prune: true                   # delete jobs and keys Git no longer has
    selfHeal: true                # undo changes made outside Git
    allowEmpty: false             # allow an automated sync to an empty state
  syncOptions:
    - CreateNamespace=true        # create the destination namespace
    - PruneLast=true              # accepted; pruning always comes last
    - ApplyOutOfSyncOnly=true     # leave synced jobs alone
    - PreserveCounts=true         # keep task group counts changed by scaling
    - Prune=false                 # never prune
  retry:
    limit: 3                      # retry a failed sync
    backoff:
      duration: 5s                # first delay (default 5s)
      factor: 2                   # growth (default 2)
      maxDuration: 3m             # cap (default 3m)
```

As in Argo CD, an automated sync happens when the Application is `OutOfSync`,
not again for a revision whose automated sync failed (a new commit, or a
manual sync, resets it), and never prunes unless `prune` is set. Self-heal
corrects drift without a new commit, at most every `self_heal_timeout`
(5 s by default). An automated sync refuses to sync to an empty desired state
(every resource pruned) unless `allowEmpty` is set.

### Sync waves

Jobs are registered in waves, by their meta `cystemd.sync-wave` (an integer,
default 0); Consul keys come first, in wave -1. A wave starts when the
previous one is registered (not when it is healthy). Pruning comes after the
last wave.

```hcl
job "cache" {
  meta = {
    "cystemd.sync-wave" = "-1"
  }
  …
}
```

Meta keys with dots are quoted, so `meta` is written as a map (`meta = {…}`):
HCL2 does not accept quoted names in a `meta {}` block.

### Resource hooks

As in Argo CD, a sync can run jobs around it: a database migration before the
new version rolls out, a smoke test after, an alert when it fails. A hook is a
batch job of the source whose meta `cystemd.hook` says when it runs:

```hcl
job "migrate-db" {
  type = "batch"

  meta = {
    "cystemd.hook"               = "PreSync"
    "cystemd.hook-delete-policy" = "HookSucceeded"   # optional
    "cystemd.hook-timeout"       = "10m"             # optional; default 30m
  }

  group "migrate" {
    restart {
      attempts = 0          # a failed migration is not retried…
      mode     = "fail"
    }
    reschedule {
      attempts  = 0         # …here or elsewhere
      unlimited = false
    }
    task "migrate" {
      …
    }
  }
}
```

| Hook | Runs |
|---|---|
| `PreSync` | Before anything is synced. If it fails, nothing is. |
| `PostSync` | After the sync, once every job is healthy or suspended, waiting at most the longest timeout of the PostSync hooks. A degraded job fails the sync instead. |
| `SyncFail` | When the sync failed: a hook, a resource, or the wait for health. |

A job can be a hook of several phases (`"PostSync,SyncFail"`). The hooks of a
phase run by sync wave: those of a wave together, the next wave once they all
succeeded. A hook succeeds when its allocations run to completion, after
Nomad's restarts and reschedules (the restart and reschedule policies of the
job apply), and fails when one fails, when its job is stopped, or after its
timeout. The operation's message says which hook is running, and the hook
appears in the tree as soon as it starts, with its allocations and logs.

Every run is a new job, marked with the meta `cystemd.hook-run` (the phase,
the operation's attempt and the revision). An operation that resumes after a
restart of the controller waits for the run it had started rather than
starting another. `cystemd.hook-delete-policy` takes Argo CD's policies,
comma-separated:

| Policy | The hook's job is purged |
|---|---|
| `BeforeHookCreation` (default) | when the hook runs again: the last run stays visible |
| `HookSucceeded` | once it succeeded |
| `HookFailed` | once it failed |

Hooks are not compared and never pruned: they are shown with the health of
their last run, without a sync status, and count neither towards the
Application's sync status nor its health. They run on full syncs only, not on
partial syncs, and not on rollbacks (which revert job versions). A dry run
lists them. Terminating the operation stops the running hooks. A job of
another Application, or one that is not a hook, is never replaced by a hook
of the same name: the hook fails instead.

Argo CD's `Sync` and `Skip` hooks have no counterpart (the Sync phase is the
Application's jobs, ordered by waves), nor have `PreDelete` and `PostDelete`.

### Manual syncs

A manual sync can target another revision once, sync only some resources
(`app sync -resource Job:payments:payments-api`, or the checkboxes of the sync
panel), prune, be a dry run, and take sync options of its own. A running
operation can be terminated.

## Ignoring differences

Fields owned by something else, an autoscaler for example, can be left out of
the comparison and of syncs (the live value is kept):

```yaml
ignoreDifferences:
  - kind: Job                     # the default
    name: storefront              # empty: every job
    namespace: default            # empty: any namespace
    jsonPointers:
      - /TaskGroups/*/Count       # or TaskGroups.*.Count; * matches any key or index
```

Paths are in the API's job structure (`TaskGroups`, `Tasks`, `Config`, …).

## Status

**Sync status** is `Synced` when every resource matches Git, `OutOfSync`
otherwise (a job whose Nomad plan has changes, a key whose value differs, a
resource missing or requiring pruning), `Unknown` when the comparison failed
(see the Application's conditions).

**Health**, the worst of its resources':

| Health | A job is… |
|---|---|
| `Healthy` | running as many healthy allocations as desired (system jobs: on every eligible node); a periodic or parameterized job; a batch job that completed |
| `Progressing` | rolling out (a running deployment), with pending allocations, or placements queued for less than a minute |
| `Degraded` | with failed or unhealthy allocations, critical service checks, a failed deployment, or placements queued longer (no eligible node, not enough resources) |
| `Suspended` | stopped, or with a paused deployment |
| `Missing` | not registered |

Consul keys are `Healthy` when present and `Missing` otherwise. The
Application's page shows the same assessment for every node of its tree, down
to tasks and service checks.

## Resource actions

| Resource | Actions |
|---|---|
| Job | restart (every running allocation, one after the other), stop, start |
| Deployment | promote, pause, resume, fail |
| Allocation | restart, stop (Nomad reschedules it) |
| Task | restart, signal-hup |

From the UI (a node's menu) or `cystemd app actions <app> <action> <Kind>/<name>`.

## Projects

```yaml
apiVersion: cystemd.io/v1alpha1
kind: AppProject
metadata:
  name: platform
spec:
  description: Platform team services
  sourceRepos:                    # globs; * allows any
    - https://git.example.com/platform/*
  destinations:                   # globs; empty fields match anything
    - name: in-cluster
      namespace: platform-*
```

The `default` project allows everything. An Application whose repository, or
one of whose jobs' destinations, its project does not allow shows a
`ComparisonError` condition and is not synced.

### Orphaned resources

A job is orphaned when it runs in an Application's destination namespace
but no Application tracks it (it has no `cystemd.app` meta): started by
hand, left over from a migration, or of an Application deleted without
cascading. As in Argo CD, a project can watch for them:

```yaml
spec:
  orphanedResources:
    warn: true                    # an OrphanedResourceWarning condition
    ignore:                       # jobs that belong there anyway
      - kind: Job
        name: "debug-*"           # a glob
```

With the block present, the Application's tree can show them (**Show
orphaned resources** in its resource bar; the API's resource tree lists
them as `orphanedNodes`); with `warn: true`, the Application also has an
`OrphanedResourceWarning` condition ("Application has 2 orphaned
resources"). Child jobs of periodic and parameterized jobs, and stopped jobs,
are not orphans.

### Sync windows

As in Argo CD, a project's sync windows say when its Applications may sync:

```yaml
spec:
  syncWindows:
    - kind: deny                  # no syncs over the weekend…
      schedule: "0 18 * * 5"      # from Friday 18:00 (a cron expression)
      duration: 62h               # until Monday 08:00
      timeZone: Europe/Amsterdam  # of the schedule; default UTC
      applications: ["*"]         # globs; any of the lists matches…
      manualSync: true            # …but people may still sync
      description: Weekend freeze
    - kind: allow                 # batch jobs only sync at night
      schedule: "0 1 * * *"
      duration: 2h
      namespaces: ["batch-*"]
      clusters: ["in-cluster"]
      andOperator: true           # …or all of them, with andOperator
```

A window is active from each time its schedule fires, for its duration. For
the windows that match an Application:

- an active `deny` window blocks syncs; manual syncs pass when every active
  deny window sets `manualSync`;
- otherwise an active `allow` window lets syncs through;
- otherwise, if there are `allow` windows, syncs are blocked outside them
  (manual syncs pass when one of them sets `manualSync`);
- without windows, syncs pass.

Blocked automated syncs, self-heal included, wait for the window to allow
them (the next comparison after it opens syncs). Blocked manual syncs and
rollbacks are refused (`cannot sync: blocked by sync window`); dry runs are
not, as they change nothing. The Application's page shows a **Sync window**
status (Allowed, Denied or Manual only) and the active windows; the
project's page lists its windows. `cystemd proj windows` manages them, and
`GET /api/v1/applications/<name>/syncwindows` returns an Application's.

## Repositories

Public repositories need nothing. Private ones are connected in the UI
(Settings → Repositories), with `cystemd repo add`, or declared in the control
plane configuration (`repository {}` blocks, read-only in the UI). HTTPS
repositories take a user name and a password or token, SSH repositories a
private key. SSH needs the control plane's `git { known_hosts_file }`: a host
not in it yet is added on first use, so pin your Git servers' keys there
beforehand. Credentials go to Vault when the control plane
uses it, else to the control plane's store.

## Webhooks and refresh

The controller compares every Application at least every
`reconcile_interval` (three minutes by default), and at once when its spec
changes, when Nomad reports events on its jobs, on Refresh (the UI, `app get
-refresh`), and when a Git server reports a push to `<url>/api/webhook`. The
webhook takes push events from GitHub, GitLab, Gitea, Gogs and Bitbucket
(Cloud and Server); configure the same secret on both sides:

```hcl
webhook {
  secret_file = "/etc/cystemd/secrets/webhook-secret"
}
```

A hard refresh also renders the source again, bypassing the manifest cache.

## Deletion

Deleting an Application deletes its jobs (deregistered and purged) and the
Consul keys it wrote, then the Application: Argo CD's cascading deletion,
through the `resources-finalizer.cystemd.io` finalizer. A non-cascading
delete (`app delete -cascade=false`, or *Non-cascading* in the UI's delete
panel) removes only the Application and leaves its jobs and keys running.

## Badges

`<url>/api/badge?name=<application>` is an SVG badge of the Application's
health and sync status, for READMEs and dashboards. It needs no login and
shows only those two statuses.
