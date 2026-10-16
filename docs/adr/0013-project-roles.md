# 0013. Project roles and project tokens

Status: Accepted

## Context

The RBAC policy is global and lives in the control plane configuration:
granting a team the right to sync its own project's Applications means
changing that configuration. Pipelines confined to one project need a
credential that cannot reach further, without a local account per pipeline.

Argo CD answers both with project roles: an AppProject lists roles with
policies whose subject is `proj:<project>:<role>` and whose objects are the
project's own Applications, the groups that have the role, and JWT tokens
issued for it (`argocd proj role create-token`), recorded in the project's
status so that applying the project again does not revoke them.

## Decision

Do the same. `spec.roles[]` holds a role's name, description, policy lines
and groups. Validation keeps a role's policies to its own subject, to the
`applications` and `logs` resources, and to objects of the project
(`<project>/...`): a project's roles cannot grant on anything else.

Every permission check adds to the policy a grant made of the roles the
caller holds: the role of the project token it presents, and the roles that
list one of its groups. The enforcer evaluates them with the policy as one:
the default role still applies, and a deny anywhere wins. The roles are
indexed from the store and reloaded every five seconds, and at once on the
replica that changes a project.

A project token is a JWT signed with the sessions' key, with
`proj:<project>:<role>` as subject and its ID as `jti`. The project's
status lists each role's tokens (ID, issue and expiry times, as Unix
seconds, as Argo CD does); a token is valid while the project lists it with
the same issue time under a role it still has. Project writes keep the
status, which only the control plane changes, and drop the tokens of the
roles a write removes. Creating and deleting tokens needs `projects/update`
on the project; a token, which cannot hold that, cannot create more.

## Consequences

- A team's permissions can be delegated with its project, in its manifest,
  to an SSO group (`nomad:role:<name>`), without touching the global policy.
- Project tokens give CI the narrowest credential: one project's
  Applications, revocable one by one, with an expiry when asked.
- Every authorization consults the role index; with few projects that is a
  short scan, and a store outage leaves the last index in force.
- A role removed on one replica stops granting on the others within five
  seconds, and its tokens within ten (the token cache).
