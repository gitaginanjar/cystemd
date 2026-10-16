# The quick start's application (README.md): two instances of a task that
# says hello, runnable on `cystemd agent -dev` (raw_exec is enabled in dev
# mode). Change the count or the message, commit, and watch CYSTEMD notice.
job "hello" {
  datacenters = ["*"]

  group "hello" {
    count = 2

    task "greeter" {
      driver = "raw_exec"

      config {
        command = "/bin/sh"
        args    = ["-c", "while true; do echo \"hello from instance $NOMAD_ALLOC_INDEX\"; sleep 5; done"]
      }

      resources {
        cpu    = 20
        memory = 32
      }
    }
  }
}
