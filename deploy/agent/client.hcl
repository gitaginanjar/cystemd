# CYSTEMD client agent: a Nomad client on a host whose systemd services
# CYSTEMD manages. The systemd task driver is built in; everything it may
# touch is opt-in through the allowlists below.
#
# Install: /etc/cystemd/agent.d/client.hcl, started by deploy/systemd/cystemd.service
# (as root: the driver talks to systemd over D-Bus and installs packages).

datacenter = "dc1"
region     = "global"
data_dir   = "/var/lib/cystemd"

client {
  enabled = true

  server_join {
    retry_join = ["provider=consul service=nomad"]
  }

  # Node pools of CYSTEMD v4 (config.yml node_pools): a host may belong to
  # several. Applications select hosts with spec.source.systemd.nodeSelector,
  # which matches cystemd_selector_<name> = "true".
  meta {
    cystemd_selector_myapplication  = "true"
    cystemd_selector_myapplication4 = "true"
  }
}

plugin "systemd" {
  config {
    # Units the driver may manage (globs). A unit not listed is refused.
    allowed_units = ["myapplication*.service", "myapplication4*.service"]

    # Packages it may install, and where task files may be synced to.
    allowed_packages          = ["myapplication*"]
    allowed_file_destinations = ["/opt/**", "/etc/myapplication/**"]

    # systemd properties a job may set beyond the CPU and memory limits.
    allowed_properties = ["CPUQuota", "MemoryMax", "MemoryHigh", "TasksMax", "Nice"]

    # Transient units run plain commands (Vault Agent sidecars, one-shot
    # tasks) under systemd, in this slice.
    allow_transient = true
    transient_slice = "cystemd.slice"

    # Package installs: dnf by default; refresh only these repositories
    # before installing (scoped makecache).
    package_manager   = "dnf"
    makecache_repos   = ["internal-rpms"]
    makecache_timeout = "2m"
    install_timeout   = "10m"
  }
}

tls {
  http                   = true
  rpc                    = true
  ca_file                = "/etc/cystemd/tls/ca.pem"
  cert_file              = "/etc/cystemd/tls/client.pem"
  key_file               = "/etc/cystemd/tls/client-key.pem"
  verify_server_hostname = true
}

consul {
  address = "127.0.0.1:8500"
}

# Tasks log in to Vault with their workload identity (JWT auth at
# jwt-nomad): Nomad's template runner, or the Vault Agent sidecars CYSTEMD
# generates for secretsDelivery = "vault-agent".
vault {
  enabled               = true
  address               = "https://vault.service.consul:8200"
  jwt_auth_backend_path = "jwt-nomad"
}

# Clients do not run the control plane.
cystemd {
  control_plane {
    enabled = false
  }
}
