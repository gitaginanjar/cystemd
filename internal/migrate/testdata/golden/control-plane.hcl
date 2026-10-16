# The CYSTEMD v4 Git and auth settings, converted by `cystemd migrate`.
# Merge into the control plane configuration (deploy/agent/control-plane.hcl).

log_level = "info"

git {
  ssh_private_key_file = "/opt/cystemd/.ssh/id_ed25519"
  known_hosts_file     = "/opt/cystemd/.ssh/known_hosts"
}

controller {
  reconcile_interval = "30s"
}

auth {
  # Accept v4 Ed25519 JWTs (as role:admin) while clients move to v5 sessions.
  legacy_public_key_file = "/opt/cystemd/jwt.pub"
}

repository {
  url                  = "git@git.example.com:platform/deploy.git"
  name                 = "cystemd-v4"
  ssh_private_key_file = "/opt/cystemd/.ssh/id_ed25519"
}
