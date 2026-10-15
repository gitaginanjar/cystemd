# Jobs CYSTEMD deploys, logged in with their Nomad workload identity (JWT
# auth at jwt-nomad, role cystemd-workloads). Each job reads the secrets
# under secret/<namespace>/<job>/. setup.sh fills in the accessor of the
# jwt-nomad mount in the paths below.
path "secret/data/{{identity.entity.aliases.AUTH_ACCESSOR.metadata.nomad_namespace}}/{{identity.entity.aliases.AUTH_ACCESSOR.metadata.nomad_job_id}}/*" {
  capabilities = ["read"]
}

path "secret/data/{{identity.entity.aliases.AUTH_ACCESSOR.metadata.nomad_namespace}}/{{identity.entity.aliases.AUTH_ACCESSOR.metadata.nomad_job_id}}" {
  capabilities = ["read"]
}

# Secrets that keep their CYSTEMD v4 paths (spec.source.systemd.secrets):
# grant them explicitly, for example:
#
# path "kubernetes/data/myproject/*" {
#   capabilities = ["read"]
# }

