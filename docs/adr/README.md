# Architecture decision records

Each record states a decision, the context that led to it and what follows
from it. A decision is changed by a new record that supersedes the old one,
never by editing it.

| # | Decision | Status |
|---|---|---|
| [0001](0001-build-on-a-nomad-fork.md) | Ship CYSTEMD as a thin fork of Nomad | Accepted |
| [0002](0002-argo-cd-application-model.md) | Adopt Argo CD's application model and UX | Accepted, amended by 0009 |
| [0003](0003-server-rendered-ui.md) | A server-rendered UI: Go, templ and htmx, no JavaScript build | Accepted |
| [0004](0004-vault-through-vault-agent.md) | Reach Vault only through Vault Agent and workload identity | Accepted |
| [0005](0005-state-in-consul.md) | Keep the state in Consul KV, with a leader-elected controller | Accepted |
| [0006](0006-systemd-task-driver.md) | Run units, not containers: a systemd task driver | Accepted |
| [0007](0007-nomad-plan-decides-sync-status.md) | Nomad's plan decides whether a job is out of sync | Accepted |
| [0008](0008-licensing-boundary.md) | Keep CYSTEMD's own binaries free of BUSL code | Accepted |
| [0009](0009-resource-hooks-as-batch-jobs.md) | Resource hooks as batch jobs | Accepted |
| [0010](0010-sso-through-nomad-oidc.md) | Single sign-on through Nomad's OIDC auth methods | Accepted |
| [0011](0011-applications-declared-by-applications.md) | Applications declared by an Application (app of apps) | Accepted |
| [0012](0012-local-accounts-and-api-tokens.md) | Local accounts and API tokens declared in the configuration | Accepted |
| [0013](0013-project-roles.md) | Project roles and project tokens | Accepted |
