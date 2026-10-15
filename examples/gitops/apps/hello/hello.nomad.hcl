# A Nomad application: a service whose settings live in a Consul key the
# same application manages (consul-kv.yaml). Changing the key in Git and
# syncing re-renders the template and restarts the task in place.
variable "version" {
  type    = string
  default = "v1.10.4"
}

variable "count" {
  type    = number
  default = 1
}

job "hello" {
  datacenters = ["*"]
  type        = "service"

  group "web" {
    count = var.count

    network {
      port "http" {
        to = 80
      }
    }

    service {
      name     = "hello"
      port     = "http"
      provider = "consul"

      check {
        type     = "http"
        path     = "/health"
        interval = "10s"
        timeout  = "2s"
      }
    }

    task "whoami" {
      driver = "docker"

      config {
        image = "traefik/whoami:${var.version}"
        ports = ["http"]
      }

      template {
        destination = "local/settings.env"
        env         = true
        change_mode = "restart"
        data        = <<-EOT
          {{ key "config/hello/settings" }}
        EOT
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
