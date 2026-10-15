# The CYSTEMD control plane as a Nomad job, for clusters running upstream
# Nomad (the cystemd agent embeds the same control plane instead).
#
# Vault Agent does all the Vault work, in two tasks logged in with the
# job's workload identity (JWT auth role cystemd-control-plane, see
# deploy/vault/setup.sh):
#   vault-agent-init  renders the control plane's Nomad and Consul tokens
#                     (from secret/cystemd/control-plane) and exits;
#   vault-agent       keeps them fresh and serves the Vault API proxy the
#                     control plane uses for repository credentials.
#
#   vault kv put secret/cystemd/control-plane nomad_token=... consul_token=...
#   nomad job run -var version=v5.0.0 deploy/nomad/cystemd-server.nomad.hcl
#
# With Consul (as here) several replicas can run: one holds the leader lock
# and reconciles applications, all serve the API and the UI.

variable "version" {
  type        = string
  description = "CYSTEMD release to run"
  default     = "v5.0.0"
}

variable "vault_version" {
  type    = string
  default = "2.1.2"
}

variable "vault_address" {
  type    = string
  default = "https://vault.service.consul:8200"
}

variable "nomad_address" {
  type    = string
  default = "https://nomad.service.consul:4646"
}

job "cystemd-server" {
  datacenters = ["*"]
  type        = "service"

  group "control-plane" {
    count = 2

    network {
      port "http" {}
      port "vault" {}
    }

    service {
      name     = "cystemd-server"
      port     = "http"
      provider = "consul"
      tags     = ["cystemd"]

      check {
        type     = "http"
        path     = "/healthz"
        interval = "10s"
        timeout  = "2s"
      }
    }

    update {
      max_parallel     = 1
      min_healthy_time = "10s"
      auto_revert      = true
    }

    # Renders the tokens before the control plane starts.
    task "vault-agent-init" {
      driver = "exec"

      lifecycle {
        hook    = "prestart"
        sidecar = false
      }

      identity {
        name = "vault_agent"
        aud  = ["vault.io"]
        file = true
        ttl  = "1h"
      }

      artifact {
        source = "https://releases.hashicorp.com/vault/${var.vault_version}/vault_${var.vault_version}_linux_amd64.zip"
      }

      config {
        command = "local/vault"
        args    = ["agent", "-config=local/agent.hcl"]
      }

      template {
        destination     = "local/agent.hcl"
        left_delimiter  = "[["
        right_delimiter = "]]"
        data            = <<-EOT
          exit_after_auth = true

          vault {
            address = "${var.vault_address}"
          }

          auto_auth {
            method "jwt" {
              mount_path = "auth/jwt-nomad"
              config = {
                path                     = "[[ env "NOMAD_SECRETS_DIR" ]]/nomad_vault_agent.jwt"
                role                     = "cystemd-control-plane"
                remove_jwt_after_reading = false
              }
            }
          }

          template {
            destination = "[[ env "NOMAD_ALLOC_DIR" ]]/data/nomad.token"
            perms       = "0600"
            contents    = "{{ with secret \"secret/data/cystemd/control-plane\" }}{{ .Data.data.nomad_token }}{{ end }}"
          }

          template {
            destination = "[[ env "NOMAD_ALLOC_DIR" ]]/data/consul.token"
            perms       = "0600"
            contents    = "{{ with secret \"secret/data/cystemd/control-plane\" }}{{ .Data.data.consul_token }}{{ end }}"
          }
        EOT
      }

      resources {
        cpu    = 50
        memory = 64
      }
    }

    # The Vault API proxy (and token refresh) for the control plane.
    task "vault-agent" {
      driver = "exec"

      lifecycle {
        hook    = "prestart"
        sidecar = true
      }

      identity {
        name = "vault_agent"
        aud  = ["vault.io"]
        file = true
        ttl  = "1h"
      }

      artifact {
        source = "https://releases.hashicorp.com/vault/${var.vault_version}/vault_${var.vault_version}_linux_amd64.zip"
      }

      config {
        command = "local/vault"
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
                role                     = "cystemd-control-plane"
                remove_jwt_after_reading = false
              }
            }
          }

          api_proxy {
            use_auto_auth_token = true
          }

          listener "tcp" {
            address     = "127.0.0.1:[[ env "NOMAD_PORT_vault" ]]"
            tls_disable = true
          }

          template_config {
            static_secret_render_interval = "5m"
          }

          template {
            destination = "[[ env "NOMAD_ALLOC_DIR" ]]/data/nomad.token"
            perms       = "0600"
            contents    = "{{ with secret \"secret/data/cystemd/control-plane\" }}{{ .Data.data.nomad_token }}{{ end }}"
          }

          template {
            destination = "[[ env "NOMAD_ALLOC_DIR" ]]/data/consul.token"
            perms       = "0600"
            contents    = "{{ with secret \"secret/data/cystemd/control-plane\" }}{{ .Data.data.consul_token }}{{ end }}"
          }
        EOT
      }

      resources {
        cpu    = 50
        memory = 64
      }
    }

    task "cystemd-server" {
      driver = "exec"

      artifact {
        source = "https://github.com/gitaginanjar/cystemd/releases/download/${var.version}/cystemd-server_${var.version}_linux_amd64.tar.gz"
      }

      config {
        command = "local/cystemd-server"
        args    = ["-config", "local/control-plane.hcl"]
      }

      template {
        destination = "local/control-plane.hcl"
        data        = <<-EOT
          bind_addr = "{{ env "NOMAD_ADDR_http" }}"
          data_dir  = "{{ env "NOMAD_TASK_DIR" }}/data"

          nomad {
            address    = "${var.nomad_address}"
            token_file = "{{ env "NOMAD_ALLOC_DIR" }}/data/nomad.token"
          }

          consul {
            enabled    = true
            address    = "{{ env "attr.unique.network.ip-address" }}:8500"
            token_file = "{{ env "NOMAD_ALLOC_DIR" }}/data/consul.token"
          }

          vault {
            enabled       = true
            agent_address = "http://127.0.0.1:{{ env "NOMAD_PORT_vault" }}"
            address       = "${var.vault_address}"
          }
        EOT
      }

      resources {
        cpu    = 500
        memory = 256
      }
    }
  }
}
