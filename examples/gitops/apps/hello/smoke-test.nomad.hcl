# A PostSync hook: after every sync, once hello is healthy, check that its
# instances answer. A failed check fails the sync, with the reason in the
# sync result; a passed one is deleted (HookSucceeded).
job "hello-smoke-test" {
  datacenters = ["*"]
  type        = "batch"

  meta = {
    "cystemd.hook"               = "PostSync"
    "cystemd.hook-delete-policy" = "HookSucceeded"
    "cystemd.hook-timeout"       = "5m"
  }

  group "check" {
    restart {
      attempts = 2
      delay    = "5s"
      mode     = "fail"
    }

    reschedule {
      attempts  = 0
      unlimited = false
    }

    task "curl" {
      driver = "docker"

      config {
        image   = "curlimages/curl:8.10.1"
        command = "/bin/sh"
        args    = ["/local/check.sh"]
      }

      template {
        destination = "local/check.sh"
        data        = <<-EOT
          set -eu
          {{ range service "hello" -}}
          curl -fsS --max-time 5 "http://{{ .Address }}:{{ .Port }}/health"
          {{ else -}}
          echo "no healthy instance of hello" >&2
          exit 1
          {{ end -}}
        EOT
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
