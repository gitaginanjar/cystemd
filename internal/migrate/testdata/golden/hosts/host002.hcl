# CYSTEMD v4 host host002 (node pools: myapplication, myapplication2), converted by `cystemd migrate`.
# Install next to client.hcl as /etc/cystemd/agent.d/90-migrated.hcl. Nomad merges
# client meta, but a plugin block replaces an earlier one: this file's
# plugin "systemd" settings win over client.hcl's.

client {
  meta {
    cystemd_selector_myapplication  = "true"
    cystemd_selector_myapplication2 = "true"
  }
}

plugin "systemd" {
  config {
    allowed_units             = ["Payments@API.service", "myproject_myapplication.service", "myproject_myapplication2.service"]
    allowed_packages          = ["Payments@API", "myproject_myapplication", "myproject_myapplication2"]
    allowed_file_destinations = ["/opt/Payments@API/**", "/opt/myproject_myapplication/**", "/srv/myproject_myapplication2/**"]
    package_manager           = "dnf"
    makecache_repos           = ["internal-rpms"]
    makecache_timeout         = "2m"
  }
}
