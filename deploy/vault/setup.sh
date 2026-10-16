#!/usr/bin/env bash
# Copyright 2026 G.G (https://github.com/gitaginanjar)
# SPDX-License-Identifier: Apache-2.0
#
# Configures Vault for CYSTEMD. Idempotent; run with an admin token:
#
#   VAULT_ADDR=https://vault.example.com:8200 VAULT_TOKEN=... \
#   NOMAD_ADDR=https://nomad.example.com:4646 deploy/vault/setup.sh
#
#  1. KV v2 at secret/ (repository credentials, workload secrets).
#  2. JWT auth at jwt-nomad/, trusting the Nomad cluster's workload
#     identities (its JWKS endpoint), with the roles:
#       cystemd-workloads      jobs CYSTEMD deploys
#       cystemd-control-plane  the cystemd-server job (Nomad deployment)
#  3. AppRole auth with the roles of host Vault Agents:
#       cystemd-host           any host (templates migrated from v4)
#       cystemd-server-host    server hosts running the embedded control plane
#  4. The policies in deploy/vault/policies.
set -euo pipefail

cd "$(dirname "$0")"
: "${VAULT_ADDR:?set VAULT_ADDR}"
: "${NOMAD_ADDR:?set NOMAD_ADDR (the Nomad API serving /.well-known/jwks.json)}"
NOMAD_CACERT=${NOMAD_CACERT:-}

has_mount() { vault "$1" list -format=json | grep -q "\"$2/\""; }

has_mount secrets secret || vault secrets enable -path=secret kv-v2

has_mount auth jwt-nomad || vault auth enable -path=jwt-nomad jwt
jwt_args=(jwks_url="$NOMAD_ADDR/.well-known/jwks.json" jwt_supported_algs=RS256 jwt_supported_algs=EdDSA default_role=cystemd-workloads)
if [ -n "$NOMAD_CACERT" ]; then
	jwt_args+=(jwks_ca_pem=@"$NOMAD_CACERT")
fi
vault write auth/jwt-nomad/config "${jwt_args[@]}"
accessor=$(vault auth list -format=json | sed -n '/"jwt-nomad\/"/,/accessor/ s/.*"accessor": "\([^"]*\)".*/\1/p' | head -n1)
if [ -z "$accessor" ]; then
	echo "setup: cannot find the accessor of auth/jwt-nomad" >&2
	exit 1
fi

vault policy write cystemd-control-plane policies/cystemd-control-plane.hcl
sed "s/AUTH_ACCESSOR/$accessor/g" policies/cystemd-workloads.hcl | vault policy write cystemd-workloads -
vault policy write cystemd-host policies/cystemd-host.hcl

# JWT roles (JSON: bound_claims and claim_mappings are maps). Every role
# maps the job's claims to token metadata, which the templated
# cystemd-workloads policy reads.
jwt_role() {
	local name=$1 policies=$2 bound=$3
	vault write "auth/jwt-nomad/role/$name" - <<JSON
{
  "role_type": "jwt",
  "bound_audiences": ["vault.io"],
  "user_claim": "/nomad_job_id",
  "user_claim_json_pointer": true,
  "bound_claims": $bound,
  "claim_mappings": {
    "nomad_namespace": "nomad_namespace",
    "nomad_job_id": "nomad_job_id",
    "nomad_task": "nomad_task"
  },
  "token_type": "service",
  "token_policies": ["$policies"],
  "token_period": "30m"
}
JSON
}
jwt_role cystemd-workloads cystemd-workloads '{}'
jwt_role cystemd-control-plane cystemd-control-plane '{"nomad_job_id": "cystemd-server"}'

has_mount auth approle || vault auth enable approle
vault write auth/approle/role/cystemd-host token_policies=cystemd-host \
	token_ttl=1h token_max_ttl=24h secret_id_num_uses=0
vault write auth/approle/role/cystemd-server-host token_policies=cystemd-host,cystemd-control-plane \
	token_ttl=1h token_max_ttl=24h secret_id_num_uses=0

cat <<MSG
Vault is configured for CYSTEMD. For each host Vault Agent:

  vault read -field=role_id auth/approle/role/cystemd-host/role-id > /etc/vault-agent/role-id
  vault write -f -field=secret_id auth/approle/role/cystemd-host/secret-id > /etc/vault-agent/secret-id

(use cystemd-server-host on server agents), then install
deploy/vault-agent/vault-agent.hcl and deploy/systemd/vault-agent.service.
MSG
