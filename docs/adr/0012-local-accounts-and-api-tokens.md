# 0012. Local accounts and API tokens declared in the configuration

Status: Accepted

## Context

Pipelines and scripts need credentials for the CYSTEMD API. Sessions are
meant for people: they expire after `auth.session_duration`, and a password
change ends them. Nomad ACL tokens work, but they grant Nomad access too and
only the agent's `cystemd` binary turns them into a session. CYSTEMD v4 gave
CI long-lived Ed25519 JWTs that could not be revoked.

Argo CD answers this with local accounts declared in `argocd-cm`
(`accounts.<name>: apiKey, login`) and API tokens generated for them
(`argocd account generate-token`), whose IDs are kept in `argocd-secret`;
deleting the ID revokes the token.

## Decision

Do the same. Local accounts besides the admin are declared in the control
plane configuration, `auth { account "<name>" { capabilities, enabled } }`,
with Argo CD's two capabilities: `login` (a password) and `apiKey`. The
control plane reconciles the stored accounts with the declarations when it
starts: missing ones are created without a password, capabilities and
`enabled` updated, undeclared ones deleted with their tokens; the admin is
only ever given its declared capabilities (`login` when undeclared). Password
login requires `login`.

An API token is a JWT signed with the sessions' key, marked as an API token,
with the account as subject and the token ID as `jti`. The account's record
lists each token's ID, issue time and expiry, never the token. A token is
valid while the account is enabled, has `apiKey`, and lists the token's ID
with the same issue time, so deleting the ID revokes it and a token generated
again under that ID does not revive the old one. Verification caches the
account for ten seconds, which bounds how long a revoked token lingers on the
other replicas.

An account manages its own tokens when signed in with its password; others
need `accounts/update` on it. A token can read its own account, but cannot
generate or delete tokens, nor change passwords: a leaked token cannot
outlive its expiry by minting another. The admin sets other accounts'
passwords by confirming its own (`account update-password -account`).
`account can-i` asks the RBAC enforcer for the caller.

## Consequences

- Credentials for automation are scoped by RBAC (the account name is the
  subject), expire when asked to, and are revoked by deleting their ID, in
  the CLI or in Settings → Accounts.
- Declaring accounts in the configuration keeps them reviewable and the same
  on every replica; there is no API to create accounts. Removing a
  declaration deletes the account, its password and tokens included: adding
  it back starts afresh.
- A password change ends sessions but leaves API tokens alone, as in Argo
  CD; disabling the account stops both.
- Token checks read the account from the store at most every ten seconds per
  replica; a revocation takes effect at once on the replica that handled it.
