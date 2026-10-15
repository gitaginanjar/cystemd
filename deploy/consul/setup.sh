#!/usr/bin/env bash
# Copyright 2026 G.G (https://github.com/gitaginanjar)
# SPDX-License-Identifier: Apache-2.0
#
# Creates the CYSTEMD control plane's Consul ACL policy and a token for it.
# Idempotent; run with a token allowed to manage ACLs:
#
#   CONSUL_HTTP_ADDR=127.0.0.1:8500 CONSUL_HTTP_TOKEN=... deploy/consul/setup.sh
#
# Prints the token's SecretID once (store it in Vault, see deploy/README.md);
# an existing token is kept and not printed again.
set -euo pipefail

cd "$(dirname "$0")"
name=cystemd-control-plane

if consul acl policy read -name "$name" >/dev/null 2>&1; then
	consul acl policy update -name "$name" -rules @policies/$name.hcl >/dev/null
else
	consul acl policy create -name "$name" -description "CYSTEMD control plane" -rules @policies/$name.hcl >/dev/null
fi
echo "setup: policy $name is up to date"

if consul acl token list -format=json | grep -q "\"Description\": \"CYSTEMD control plane\""; then
	echo "setup: a token for the CYSTEMD control plane exists; not creating another"
	exit 0
fi
secret=$(consul acl token create -description "CYSTEMD control plane" -policy-name "$name" -format=json |
	sed -n 's/.*"SecretID": "\([^"]*\)".*/\1/p')
echo "setup: created the control plane's token. Store it where the control plane reads it, e.g."
echo "  vault kv put secret/cystemd/control-plane consul_token=$secret"
