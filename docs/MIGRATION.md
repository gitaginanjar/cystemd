# Migrating from CYSTEMD v4

CYSTEMD v4 was a daemon on every host. It pulled `config.yml` from Git,
matched its host name against `node_pools`, and for each
`continuous_deployment` entry of its pools installed the package version from
the values file, wrote the service's configuration and `.env`, applied CPU and
memory limits and restarted the unit. It exposed `systemctl` over HTTP.

v5 keeps the model — services installed from packages, Helm-style values
files, secrets from Vault, `node_pools` — and moves the mechanics to Nomad: a
`continuous_deployment` entry becomes an **Application** with a **systemd
source**, the per-host loop becomes Nomad scheduling a job onto the hosts of
the pool, and the HTTP API becomes the CYSTEMD API, UI and CLI.

## What becomes what

| v4 | v5 |
|---|---|
| `continuous_deployment[]` entry | An Application with a systemd source ([APPLICATIONS.md](APPLICATIONS.md#systemd)) |
| `service` | `systemd.unit` (and the package name) |
| `git_values`, `git_branch` | `source.path` + `systemd.valuesFile`, `source.targetRevision` |
| `git_values_config_key`, `…_version_key`, `…_cpu_key`, `…_memory_key` | `systemd.configKey`, `versionKey`, `cpuKey`, `memoryKey` (same syntax, same `${selector}`) |
| `selector` / `selectorkey` | `systemd.nodeSelector` (the pool) / `systemd.selector` (the `${selector}` value) |
| `node_pools[]` | Client meta `cystemd_selector_<pool> = "true"` on each host of the pool |
| `replica` (informational in v4) | `systemd.replicas: 0`: one instance on every node of the pool, as v4 did |
| `service_config`, `workdir` | `systemd.serviceConfig`, `systemd.workdir` — files land where v4 wrote them |
| `secrets` (→ `${workdir}/${service}/.env`) | `systemd.secrets`, read by the job's Nomad workload identity |
| `autosync: true` | `syncPolicy.automated` with `selfHeal: true` (v4 never deleted units: no `prune`) |
| `autosync: false` (observe only) | No automated sync: the Application shows its diff, a person syncs |
| `/sync` | `cystemd app sync <app>` (or the UI) |
| `/diff`, `/lastdiff` | `cystemd app diff <app>` (or the UI's diff) |
| `/restart`, `/start`, `/stop` | `cystemd app actions <app> restart Job/<app>` (or an allocation), Nomad's `job stop`/`job run` |
| `/status` | `cystemd app get <app>`, `cystemd app resources <app> -o tree` |
| `/config`, `/env` | The live manifest and files in the UI (`nomad alloc fs`) |
| `/version` | `cystemd version` |
| `/metrics` | The control plane's `/metrics`, and Nomad's |
| `dnf.makecache_repos`, `makecache_timeout` | The driver's `makecache_repos`, `makecache_timeout` |
| `GIT_REPOSITORY`, `GIT_SSH_KEY_PRIVATE`, `GIT_SSH_KNOWN_HOSTS` | A `repository {}` block and `git {}` in the control plane configuration |
| `GIT_INTERVAL` | `controller { reconcile_interval }`, plus webhooks |
| `auth`, `AUTH_PUBLIC_KEY_FILE` (Ed25519 JWTs) | Accounts, API tokens, Nomad-token login and RBAC; `auth { legacy_public_key_file }` keeps v4 JWTs working meanwhile |
| `VAULT_ROLE_ID`, `VAULT_SECRET_ID`, `VAULT_TOKEN` | A Vault Agent per host (AppRole), never in CYSTEMD |
| `VAULT_TEMPLATES` | The host Vault Agent's templates |
| `vault.renewal_*`, the in-process agent | Vault Agent and Nomad renew tokens and leases |
| `version` (self-update) | Upgrade the `cystemd` agent like any Nomad agent |

## 1. Convert

Run `migrate` with a copy of `config.yml` and one host's `.env` (the hosts'
`.env` files usually differ only in credentials, which are never copied):

```sh
cystemdctl migrate -config config.yml -env .env -out migration
```

| Option | |
|---|---|
| `-repo <url>` | The GitOps repository, if not `GIT_REPOSITORY` |
| `-project`, `-dest-name`, `-dest-namespace` | Where the Applications go (default project, in-cluster, `default`) |
| `-kv-version 1` | The secrets' KV mounts are version 1 (v4 tried v2, then v1; v5 needs to know) |
| `-secrets-delivery vault-agent` | Use Vault Agent sidecars instead of Nomad's Vault integration |
| `-vault-ca-cert <path>` | The CA the host Vault Agents trust (v4 skipped verification by default) |
| `-templates-dir <dir>` | Where `VAULT_TEMPLATES` sources are, if not at their paths on this machine |
| `-apply` | Also create the Applications (`-upsert` to replace) |

`migration/` then holds:

| File | Install |
|---|---|
| `MIGRATION.md` | The report: every application and host, what to install where, what was not carried over, warnings |
| `applications.yaml` | `cystemd app create -f` |
| `hosts/<host>.hcl` | `/etc/cystemd/agent.d/90-migrated.hcl` on that host: its pools as client meta, and the driver's allowlists for exactly its units, packages and directories |
| `control-plane.hcl` | Merged into the control plane configuration: repository, Git key and known hosts, reconcile interval, v4 JWT key |
| `vault/` | A Vault policy and JWT role per application with secrets (each job may read its v4 secret path, and only that), and `apply.sh` |
| `vault-agent/` | A host Vault Agent configuration and the converted `VAULT_TEMPLATES` |

Read the report's warnings. They point at what needs a decision: entries
without `git_values` (skipped), a service declared in two pools a host belongs
to (v4 managed the first; v5 would run both), templates that cannot be
converted mechanically, missing SSH host keys (v4 skipped verification without
`GIT_SSH_KNOWN_HOSTS`; v5 does not).

Application names are the service names, lowercased, other characters
replaced by `-` (`Payments@API` → `payments-api`); a service declared in
several entries becomes `<service>-<pool>` per entry.

## 2. Set up Consul, Vault and the servers

Follow [deploy/README.md](../deploy/README.md): Consul, Vault
(`deploy/vault/setup.sh`), the Nomad ACLs, then the server agents with the
control plane, merging `control-plane.hcl` into their configuration. Copy the
Git SSH key and, while v4 clients exist, the v4 JWT public key to the paths
the configuration names.

Then the applications' Vault roles:

```sh
VAULT_ADDR=… VAULT_TOKEN=… migration/vault/apply.sh
```

## 3. Move the hosts, one at a time

On each host:

```sh
systemctl disable --now cystemd                 # the v4 daemon; units keep running
install -D deploy/agent/client.hcl /etc/cystemd/agent.d/client.hcl
install -D migration/hosts/HOST001.hcl /etc/cystemd/agent.d/90-migrated.hcl   # this host's file
install -D deploy/systemd/cystemd.service /etc/systemd/system/cystemd.service
systemctl enable --now cystemd                  # the agent: a Nomad client
```

A plugin block replaces an earlier one, so `90-migrated.hcl`'s
`plugin "systemd"` settings are the host's. If the host used
`VAULT_TEMPLATES`, install `migration/vault-agent/vault-agent.hcl` and the
templates under `/etc/vault-agent/` with
[deploy/systemd/vault-agent.service](../deploy/systemd/vault-agent.service),
and give it AppRole credentials (`deploy/vault/setup.sh` creates the
`cystemd-host` role; list the paths your templates read in its policy).

Nothing touches the units until their Application syncs.

## 4. Create the applications and sync

```sh
cystemd ctl login https://cystemd.example.com        # or cystemdctl login
cystemd app create -f migration/applications.yaml
```

Or keep them in Git, as v4 kept `config.yml`: commit `applications.yaml`
under a directory of the GitOps repository and create one Application that
declares them ([app of apps](APPLICATIONS.md#applications-app-of-apps));
adding a service is then a commit, as it was in v4.

Applications that were `autosync: true` in v4 are automated and sync at once.
To look first, remove their `syncPolicy` from `applications.yaml` before
creating them, then:

```sh
cystemd app diff payments-api          # what would change
cystemd app sync payments-api          # deploy, wait, report
cystemd app set payments-api -sync-policy automated -self-heal
```

The first sync restarts each unit once, so that it runs with the task's
environment, secrets and limits (v4 restarted on every change too). Units
whose package version and configuration already match are otherwise
untouched: the driver checks the package database before installing, and
files are only written when they differ.

## 5. Move the clients of the v4 API

v4's endpoints are gone. Scripts that called them move to the CLI or the API
(see the table above); a CI job typically becomes:

```sh
cystemdctl app sync payments-api -revision "$GIT_COMMIT" -timeout 15m
```

with an API token in `CYSTEMD_TOKEN`, or, with the agent's `cystemd` binary,
a Nomad ACL token in `NOMAD_TOKEN`. The v4 JWTs CI held become API tokens of
an account declared for it, with only the permissions it needs:

```hcl
auth {
  account "ci" {
    capabilities = ["apiKey"]
  }
}
rbac {
  policy = "p, ci, applications, sync, default/payments-*, allow"
}
```

```sh
cystemdctl account generate-token -account ci -id gitlab -expires-in 365d
```

Unlike a v4 JWT, such a token can be revoked
(`cystemdctl account delete-token -account ci gitlab`). A pipeline that
deploys one project's applications only can instead hold a project role's
token, which cannot reach beyond that project
(`cystemdctl proj role create-token <project> <role>`, see
[APPLICATIONS.md](APPLICATIONS.md#roles)). While the clients
move, `auth { legacy_public_key_file }` accepts v4 JWTs (as `role:admin`) on
the API. Remove it when they are done.

## Templates

v4 rendered `VAULT_TEMPLATES` with Go templates and two functions: `secret
PATH` returned the KV data map and `secretField PATH FIELD` one field, failing
when it is missing. Vault Agent uses consul-template, whose `secret` returns
the whole response (the map is `.Data.data` on KV v2, `.Data` on v1) and is
nil during a first pass in which it only discovers what the template reads.
`migrate` rewrites each action that reads a secret into a guarded one:

```
db_password = {{ secretField "kubernetes/myproject/app" "db_password" }}
```

becomes

```
db_password = {{ $cystemd_s1 := secret "kubernetes/data/myproject/app" }}{{ if $cystemd_s1 }}{{ $cystemd_s1.Data.data.db_password }}{{ end }}
```

and a block reading a secret (`{{ with secret "…" }}`, `{{ range … := secret
"…" }}`) is enclosed the same way up to its `{{ end }}`. With
`error_on_missing_key`, a missing field still fails the render, as in v4. A
field name that is not a template identifier (`api-key`) is read with `index`,
which renders an empty value instead; the report lists those. A secret read in
an `{{ else if }}`, in a variable assignment or with a computed path cannot be
guarded mechanically; the report gives its line, and the rewrite is:

```
{{ with secret "kubernetes/data/myproject/app" }}{{ $user := .Data.data.db_user }}…{{ end }}
```

## Going back

The units are systemd's, not the agent's: stopping the agent leaves them
running. To return a host to v4:

1. `systemctl disable --now cystemd` — Nomad can no longer act on the host,
   and marks its allocations lost;
2. remove the driver's runtime drop-ins, so the units start as v4 knew them:
   `rm -f /run/systemd/system/*.d/90-cystemd.conf && systemctl daemon-reload`;
3. start the v4 daemon again (keep its package installed until you are done).

Once no host of a pool runs the agent any more, delete the pool's
Applications without cascading (`cystemd app delete <app> -cascade=false`)
and purge their jobs (`cystemd job stop -purge <app>`): with no client left on
those hosts, that stops nothing.
