#!/usr/bin/env bash
# Writes the Vault policies and JWT roles of the migrated applications:
# each job reads its CYSTEMD v4 secret with its Nomad workload identity.
# Run after deploy/vault/setup.sh, with a Vault admin token:
#
#   VAULT_ADDR=... VAULT_TOKEN=... ./apply.sh
set -euo pipefail
cd "$(dirname "$0")"
: "${VAULT_ADDR:?set VAULT_ADDR}"
for p in policies/*.hcl; do
	vault policy write "$(basename "$p" .hcl)" "$p"
done
for r in roles/*.json; do
	vault write "auth/jwt-nomad/role/$(basename "$r" .json)" - <"$r"
done
