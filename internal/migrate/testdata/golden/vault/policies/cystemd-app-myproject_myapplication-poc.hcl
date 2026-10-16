# Application myproject_myapplication-poc (CYSTEMD v4 service myproject_myapplication): the secret v4 rendered into
# /opt/myproject_myapplication/.env, read with the job's workload identity.
path "kubernetes/data/myproject/poc" {
  capabilities = ["read"]
}
