# A Vault Agent on every CYSTEMD host, run by Nomad through the systemd
# driver (a transient unit, so it renders into the host's file system):
# the API proxy on 127.0.0.1:8100 for host tools, and the templates of
# services migrated from CYSTEMD v4 (VAULT_TEMPLATES). It logs in with the
# job's workload identity (JWT auth role cystemd-workloads) instead of an
# AppRole secret on disk.
#
# The alternative is deploy/systemd/vault-agent.service with an AppRole
# (needed where Nomad is not the host's supervisor, e.g. on server hosts).
#
# The client's systemd plugin must allow transient units (allow_transient).

variable "vault_address" {
  type    = string
  default = "https://vault.service.consul:8200"
}

job "vault-agent" {
  datacenters = ["*"]
  type        = "system"

  group "agent" {
    task "vault-agent" {
      driver = "systemd"

      identity {
        name = "vault_agent"
        aud  = ["vault.io"]
        file = true
        ttl  = "1h"
      }

      config {
        command = "/usr/bin/vault"
        args    = ["agent", "-config=local/agent.hcl"]
      }

      template {
        destination     = "local/agent.hcl"
        left_delimiter  = "[["
        right_delimiter = "]]"
        data            = <<-EOT
          vault {
            address = "${var.vault_address}"
          }

          auto_auth {
            method "jwt" {
              mount_path = "auth/jwt-nomad"
              config = {
                path                     = "[[ env "NOMAD_SECRETS_DIR" ]]/nomad_vault_agent.jwt"
                role                     = "cystemd-workloads"
                remove_jwt_after_reading = false
              }
            }
          }

          api_proxy {
            use_auto_auth_token = true
          }

          listener "tcp" {
            address     = "127.0.0.1:8100"
            tls_disable = true
          }

          template_config {
            exit_on_retry_failure         = true
            static_secret_render_interval = "1m"
          }

          # Templates migrated from CYSTEMD v4 (cystemd migrate writes them):
          #
          # template {
          #   source      = "/etc/cystemd/templates/app.conf.tmpl"
          #   destination = "/opt/app/app.conf"
          #   perms       = "0600"
          #   command     = "systemctl try-reload-or-restart app.service"
          # }
        EOT
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
