# 0010. Single sign-on through Nomad's OIDC auth methods

Status: Accepted

## Context

Argo CD offers single sign-on (its login page's "LOG IN VIA SSO"), through
Dex or an OIDC provider configured in Argo CD, with the provider's groups
mapped to RBAC roles. Teams expect the same of CYSTEMD. CYSTEMD already
accepts Nomad ACL tokens, whose policies and roles become RBAC groups, and
Nomad itself has OIDC auth methods: it runs the authorization code flow with
the provider, maps claims to Nomad roles and policies with binding rules,
and issues an ACL token.

## Decision

Delegate single sign-on to Nomad. With `auth { sso {} }`, the login page
offers the cluster's OIDC auth methods. CYSTEMD asks Nomad for the
provider's authorization URL (with a nonce it keeps in a short-lived,
HTTP-only cookie), receives the provider's callback on
`/ui/auth/callback`, has Nomad complete the login, and turns the ACL token
Nomad issues into a session: the user name is the token's name (the auth
method's `token_name_format`), the groups its roles and policies. The
command line does the same with a callback on `localhost`. CYSTEMD holds no
client secret and verifies no ID token itself.

## Consequences

- One identity configuration serves Nomad and CYSTEMD: the provider, claim
  mappings and binding rules are Nomad's, and an RBAC policy line such as
  `g, nomad:role:platform, role:admin` grants a role of the provider's
  groups.
- No OIDC client in CYSTEMD: nothing to keep in step with providers, and no
  new secret to store; Nomad's checks (state, nonce, audience, signing
  algorithms) and options (PKCE, client assertions) apply.
- Each login creates a Nomad ACL token, valid for the auth method's
  `max_token_ttl`; CYSTEMD does not use it after the login, so a short TTL
  suits. The session lasts `auth.session_duration`.
- SSO needs Nomad's ACL system, and the auth method must allow CYSTEMD's
  callback URLs as redirect URIs.
