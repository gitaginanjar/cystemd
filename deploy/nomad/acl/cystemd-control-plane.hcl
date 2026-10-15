# The CYSTEMD control plane's Nomad ACL token:
#
#   nomad acl policy apply -description "CYSTEMD control plane" cystemd-control-plane deploy/nomad/acl/cystemd-control-plane.hcl
#   nomad acl token create -name cystemd-control-plane -policy cystemd-control-plane
#
# The sync option CreateNamespace=true needs a management token instead:
# creating namespaces is reserved to it. Create namespaces ahead otherwise.

namespace "*" {
  # Parse, plan, register, revert, tag and deregister jobs; read their
  # versions, allocations, deployments, evaluations, checks and logs;
  # restart, stop and signal allocations; promote, pause and fail
  # deployments.
  policy = "write"
}

# Settings → Clusters: client nodes and their drivers.
node {
  policy = "read"
}

node_pool "*" {
  policy = "read"
}

# Cluster version and health.
agent {
  policy = "read"
}
