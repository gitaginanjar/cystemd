# CYSTEMD server agent: a Nomad server (Raft) that also runs the CYSTEMD
# control plane — the API on /api/ and the web UI on /ui/ of every agent
# HTTP listener. Run three for quorum; with Consul enabled in the control
# plane, one of them reconciles applications (leader lock) and all three
# serve the API and the UI.
#
# Install: /etc/cystemd/agent.d/server.hcl, started by deploy/systemd/cystemd.service.

datacenter = "dc1"
region     = "global"
data_dir   = "/var/lib/cystemd"
bind_addr  = "0.0.0.0"

server {
  enabled          = true
  bootstrap_expect = 3
}

acl {
  enabled = true
}

tls {
  http                   = true
  rpc                    = true
  ca_file                = "/etc/cystemd/tls/ca.pem"
  cert_file              = "/etc/cystemd/tls/server.pem"
  key_file               = "/etc/cystemd/tls/server-key.pem"
  verify_server_hostname = true
}

# Consul: service registration, Consul templates and, through the control
# plane, the CYSTEMD state store, leader lock and managed keys. Workload
# identities let tasks log in to Consul with JWTs Nomad signs.
consul {
  address = "127.0.0.1:8500"

  service_identity {
    aud = ["consul.io"]
    ttl = "1h"
  }

  task_identity {
    aud = ["consul.io"]
    ttl = "1h"
  }
}

# Vault: tasks log in with their workload identity (JWT auth mounted at
# jwt-nomad, see deploy/vault/setup.sh); no Vault token on the servers.
vault {
  enabled = true
  address = "https://vault.service.consul:8200"

  default_identity {
    aud = ["vault.io"]
    ttl = "1h"
  }
}

ui {
  enabled = true
}

# The CYSTEMD control plane runs in this agent. Its configuration file only
# needs what differs from this agent's own settings (address, TLS, region,
# data dir and Consul address are taken from here).
cystemd {
  control_plane {
    enabled     = true
    config_file = "/etc/cystemd/control-plane.hcl"
  }
}
