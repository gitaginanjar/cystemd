# The command line

The same commands ship in two binaries:

- **`cystemd`**, the agent: `cystemd app sync web`, next to every Nomad
  command. The session commands are under `ctl` (`cystemd ctl login`), since
  Nomad has a `login` of its own.
- **`cystemdctl`**, standalone: `cystemdctl app sync web`, `cystemdctl login`.

Every command has `-h`. Flags follow Nomad's style (`-flag=value` or
`-flag value`) and may come before or after the arguments (`app get web -o
yaml`); after `--`, everything is an argument.

## Connecting

A command finds its server and session in this order:

1. `-address`, `-token`, `-ca-cert`, `-tls-skip-verify` flags;
2. `CYSTEMD_ADDR`, `CYSTEMD_TOKEN`, `CYSTEMD_CACERT`, `CYSTEMD_SKIP_VERIFY=true`;
3. the current context (or `-context <name>`), saved by `login`;
4. in the agent only: `NOMAD_ADDR` (with `NOMAD_CACERT`, `NOMAD_SKIP_VERIFY`)
   — the agent serves the CYSTEMD API on its Nomad address — and
   `NOMAD_TOKEN`, exchanged for a session on each invocation;
5. `http://127.0.0.1:8080` (`cystemdctl`) or `http://127.0.0.1:4646`
   (`cystemd`).

`login -sso` opens the identity provider in a browser (or prints its URL) and
receives the answer on `http://localhost:8085/auth/callback`, which the auth
method must allow as a redirect URI (`-sso-port` changes the port,
`-sso-method` picks an auth method other than the default). See
[deploy/README.md](../deploy/README.md#single-sign-on).

`login` saves a context (server, session token, user, TLS settings) in
`cystemd/config.json` under the user configuration directory
(`~/.config/cystemd/config.json` on Linux; mode 0600; `CYSTEMD_CONFIG`
overrides the path) and makes it current:

```sh
cystemdctl login cystemd.example.com                 # https:// unless a scheme is given
cystemdctl login -username ops -password-stdin https://cystemd.example.com < pw
cystemdctl login -nomad-token env cystemd.example.com   # NOMAD_TOKEN's identity
cystemdctl login -sso cystemd.example.com               # single sign-on in a browser
cystemdctl context                                    # list; * is current
cystemdctl context staging                            # switch
cystemdctl logout                                     # forget the session
```

## Commands

| Command | |
|---|---|
| `login [server]` | Log in (local account, or `-nomad-token`) and save a context |
| `logout [context]` | Forget a context's session |
| `context [name]` | List contexts, switch, or `-delete` one |
| `version` | Client and server versions |
| `app list` | List applications (`-project`, `-l k=v`, `-o wide\|name\|json\|yaml`) |
| `app get <name>` | Show an application and its resources (`-refresh`, `-hard-refresh`, `-o json\|yaml\|tree`) |
| `app create <name>` | Create from flags, or `-f <manifest>` (applications and projects; `-upsert`) |
| `app set <name>` | Change the given spec fields |
| `app delete <name>` | Delete, cascading to jobs and keys unless `-cascade=false`; asks for the name unless `-y`; `-wait` |
| `app sync <name>…` | Sync and wait (`-revision`, `-prune`, `-dry-run`, `-resource K:NS:NAME`, `-sync-option`, `-retry-limit`, `-async`, `-timeout`); exits 1 if it fails |
| `app wait <name>…` | Wait for synced, healthy and idle (or `-sync`, `-health`, `-operation`, `-suspended`, `-degraded`) |
| `app diff <name>` | Unified diff from live to Git, and Nomad's plan; exits 1 when there are differences |
| `app history <name>` | Deployments, newest first |
| `app rollback <name> <id>` | Revert to a deployment (`-prune`, `-dry-run`, `-async`) |
| `app terminate-op <name>` | Stop the running operation |
| `app manifests <name>` | The rendered jobs and keys (`-revision`) |
| `app resources <name>` | Managed resources (`-o tree` for the live tree) |
| `app logs <name>` | Task logs of the running allocations (`-job`, `-group`, `-alloc`, `-task`, `-stderr`, `-f`, `-tail`) |
| `app actions <name> <action> <Kind>/<resource>` | Run a resource action, e.g. `restart Job/web`, `stop Allocation/web.web[1]`, `signal-hup Task/web.web[0]/server` |
| `proj list`, `proj get <name>` | Projects |
| `proj create -f <manifest>` | Create the projects of a manifest (`-upsert`) |
| `proj delete <name>` | Delete a project no application uses |
| `app create <name> -applications [-applications-project <p>]…` | An app of apps: its path holds Application manifests |
| `app resources <name> -orphaned` | The orphaned jobs of its destination namespace (when its project watches for them) |
| `proj windows list <proj>` | Its sync windows, with IDs and whether they are active |
| `proj windows add <proj>` | Add one (`-kind allow\|deny`, `-schedule`, `-duration`, `-applications`, `-namespaces`, `-clusters`, `-and-operator`, `-manual-sync`, `-time-zone`) |
| `proj windows delete <proj> <id>`, `enable-manual-sync`, `disable-manual-sync` | Remove one, or let manual syncs through it |
| `proj role list <proj>`, `get <proj> <role>` | A project's roles; a role's policies, groups and token IDs |
| `proj role create <proj> <role>`, `delete` | Add a role (`-description`), or remove it and its tokens |
| `proj role add-policy <proj> <role>`, `remove-policy` | `-action sync -object 'api-*'` (within the project; `-permission deny`, `-resource logs`) |
| `proj role add-group <proj> <role> <group>`, `remove-group` | Give the role to a group (e.g. `nomad:role:payments`) |
| `proj role create-token <proj> <role>` | A token acting as the role, shown once (`-expires-in`, `-id`, `-token-only`) |
| `proj role list-tokens <proj> <role>`, `delete-token <proj> <role> <id\|iat>` | Its tokens; revoke one |
| `repo list`, `repo add <url>`, `repo rm <url>` | Repository connections (`-username`, `-password` (`-` prompts), `-ssh-private-key-path`, `-insecure`, `-force`) |
| `cluster list` | Destination clusters, their version and nodes |
| `account get-user-info` | Who you are, your groups and roles |
| `account update-password` | Change your password (other sessions end), or another account's with `-account` (needs `accounts/update` and your own password) |
| `account list` | Local accounts |
| `account get` | An account and its API token IDs (`-account`, default yours; `-o json\|yaml`) |
| `account generate-token` | Generate an API token and print it, once (`-account`, `-expires-in 12h\|90d`, default never; `-id`) |
| `account delete-token <id>` | Revoke an API token (`-account`) |
| `account can-i <action> <resource> <object>` | `yes` or `no`: whether RBAC lets you, e.g. `can-i sync applications 'default/*'` |
| `migrate -config config.yml [-env .env]` | Convert a CYSTEMD v4 installation ([MIGRATION.md](MIGRATION.md)) |

## In scripts

- `app sync` waits for the result and exits 1 when the operation fails or
  times out; `app wait` exits 1 on timeout; `app diff` exits 1 when there are
  differences (`-exit-code=false` to always exit 0), like `diff(1)`.
- `-o json` and `-o yaml` print the API objects.
- A token in `CYSTEMD_TOKEN` needs no saved context. In CI, use an API
  token of an account with the `apiKey` capability (below), or a Nomad token
  in `NOMAD_TOKEN` with the agent's `cystemd` binary.

## API tokens

Argo CD's local accounts and API tokens. Declare the account in the control
plane configuration, with the `apiKey` capability, and give it permissions
in the RBAC policy:

```hcl
auth {
  account "ci" {
    capabilities = ["apiKey"]
  }
}
rbac {
  policy = <<EOT
p, ci, applications, get, payments/*, allow
p, ci, applications, sync, payments/*, allow
EOT
}
```

Then, as the admin (or anyone with `accounts/update` on it):

```sh
cystemdctl account generate-token -account ci -id gitlab -expires-in 90d   # printed once
cystemdctl account get -account ci                                         # the token IDs
cystemdctl account delete-token -account ci gitlab                         # revoked at once
```

For automation confined to one project, a project role's token does the
same without an account (`proj role create-token`, see
[APPLICATIONS.md](APPLICATIONS.md#roles)).

The token is a bearer token: `CYSTEMD_TOKEN`, `-token`, or an
`Authorization: Bearer` header on the API. It acts as the account, with the
account's RBAC; it works until it is deleted or expires, or the account is
disabled, loses the `apiKey` capability or is removed from the
configuration. A password change does not revoke it. A token can read its own
account but cannot generate or delete tokens, nor change passwords. An
account with both `login` and `apiKey` manages its own tokens when logged in
with its password, from the CLI or from its page in the UI (User info → API
tokens).

```sh
# Deploy a revision and fail the pipeline if it does not become healthy.
cystemdctl app set payments-api -revision "$GIT_TAG"
cystemdctl app sync payments-api -timeout 15m
cystemdctl app wait payments-api -health -timeout 10m
```
