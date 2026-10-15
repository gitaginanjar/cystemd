# Vault Agent on a host (AppRole cystemd-host): the templates migrated
# from CYSTEMD v4 (VAULT_TEMPLATES, per-service .env files). List the paths
# your services read; v4 typically used one KV mount per platform:
path "kubernetes/data/myproject/*" {
  capabilities = ["read"]
}

# Server hosts also proxy the embedded control plane's requests: give them
# the control plane's policy too (setup.sh does so for cystemd-server-host).

