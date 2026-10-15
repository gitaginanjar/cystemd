# CYSTEMD control plane configuration, for the control plane embedded in
# the server agents (deploy/agent/server.hcl → cystemd.control_plane.config_file)
# or for the standalone cystemd-server (-config). Every value shown has a
# default; embedded, the Nomad address, TLS files, region, data dir and
# Consul address default to the agent's own.

# The URL users reach the UI at (links in notifications and badges).
url       = "https://cystemd.example.com"
log_level = "info"

nomad {
  # A Nomad ACL token allowed to manage the jobs of every namespace
  # applications deploy to (deploy/nomad/acl/cystemd-control-plane.hcl).
  token_file = "/etc/cystemd/secrets/nomad.token"
}

# Consul holds the CYSTEMD state (applications, projects, repositories,
# accounts) under kv_prefix, the leader lock that lets several control
# planes run with one reconciling, the Consul keys applications manage,
# and the health of the services jobs register.
consul {
  enabled    = true
  kv_prefix  = "cystemd"
  token_file = "/etc/cystemd/secrets/consul.token"
}

vault {
  enabled = true

  # The local Vault Agent's API proxy (deploy/vault-agent): it logs in and
  # renews the token; the control plane never holds one.
  agent_address = "http://127.0.0.1:8100"

  # Repository credentials are stored here (KV v2).
  kv_mount        = "secret"
  repo_creds_path = "cystemd/repositories"

  # For the jobs CYSTEMD generates (systemd sources with secrets): the
  # Vault address their Vault Agent sidecars use, and the JWT auth role and
  # mount their workload identities log in with.
  address       = "https://vault.service.consul:8200"
  workload_role = "cystemd-workloads"
  jwt_auth_path = "auth/jwt-nomad"
}

git {
  # The default SSH key for repositories without stored credentials (the
  # CYSTEMD v4 GIT_SSH_KEY_PRIVATE), and pinned host keys.
  ssh_private_key_file = "/etc/cystemd/secrets/git_ed25519"
  known_hosts_file     = "/etc/cystemd/git_known_hosts"
  fetch_timeout        = "2m"
}

# Git servers report pushes to <url>/api/webhook (GitHub, GitLab, Gitea,
# Gogs, Bitbucket Cloud and Server): the applications that track a pushed
# branch or tag are compared at once. Requests must be signed with this
# secret (GitLab: sent as its token); without a secret any request is
# accepted, as a push only makes a comparison happen sooner.
webhook {
  secret_file = "/etc/cystemd/secrets/webhook-secret"
}

controller {
  # Applications are compared with Git and Nomad at least this often (Git
  # webhooks and the UI's Refresh trigger it sooner).
  reconcile_interval = "3m"
  self_heal_timeout  = "5s"
  workers            = 10
}

auth {
  admin_enabled     = true
  nomad_token_login = true
  session_duration  = "24h"
  # Accept CYSTEMD v4 Ed25519 JWTs (as role:admin) during the migration.
  # legacy_public_key_file = "/etc/cystemd/v4-jwt.pub"

  # Single sign-on through Nomad's OIDC auth methods (deploy/README.md).
  # sso {
  #   auth_methods = ["example-idp"]
  # }
}

# Argo CD's RBAC policy format. Built-in roles: role:admin, role:readonly.
# A Nomad management token logs in as role:admin.
rbac {
  default_role = "role:readonly"
  policy       = <<-EOT
    p, role:deployer, applications, get, */*, allow
    p, role:deployer, applications, sync, */*, allow
    p, role:deployer, applications, action, */*, allow
    p, role:deployer, logs, get, */*, allow
    g, payments-team, role:deployer
  EOT
}

# Repositories can also be connected in the UI (Settings → Repositories);
# these are declared here and read-only there.
repository {
  url                  = "git@git.example.com:platform/deploy.git"
  name                 = "deploy"
  ssh_private_key_file = "/etc/cystemd/secrets/git_ed25519"
}
