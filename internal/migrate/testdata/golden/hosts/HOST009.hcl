# CYSTEMD v4 host HOST009 (node pools: poc), converted by `cystemd migrate`.
# Install next to client.hcl as /etc/cystemd/agent.d/90-migrated.hcl. Nomad merges
# client meta, but a plugin block replaces an earlier one: this file's
# plugin "systemd" settings win over client.hcl's.

client {
  meta {
    cystemd_selector_poc = "true"
  }
}

plugin "systemd" {
  config {
    allowed_units             = ["myproject_myapplication.service"]
    allowed_packages          = ["myproject_myapplication"]
    allowed_file_destinations = ["/opt/myproject_myapplication/**"]
    package_manager           = "dnf"
    makecache_repos           = ["internal-rpms"]
    makecache_timeout         = "2m"
  }
}
