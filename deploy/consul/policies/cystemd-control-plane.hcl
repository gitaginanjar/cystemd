# The CYSTEMD control plane's Consul ACL policy (deploy/consul/setup.sh).

# State store and leader lock, under the control plane's kv_prefix.
key_prefix "cystemd/" {
  policy = "write"
}

# Consul keys applications manage (consul-kv files in Git): list the
# prefixes your applications use.
key_prefix "config/" {
  policy = "write"
}

# The leader lock is a session on the local Consul agent's node.
session_prefix "" {
  policy = "write"
}

node_prefix "" {
  policy = "read"
}

# Health of the services the deployed jobs register.
service_prefix "" {
  policy = "read"
}
