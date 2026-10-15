# The CYSTEMD control plane (through its Vault Agent's API proxy).

# Repository credentials connected in the UI (KV v2).
path "secret/data/cystemd/repositories/*" {
  capabilities = ["create", "read", "update", "delete"]
}

path "secret/metadata/cystemd/repositories/*" {
  capabilities = ["read", "list", "delete"]
}

# Its own Nomad and Consul tokens, rendered to files by its Vault Agent
# (deploy/nomad/cystemd-server.nomad.hcl).
path "secret/data/cystemd/control-plane" {
  capabilities = ["read"]
}
