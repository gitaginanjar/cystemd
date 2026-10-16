# The systemd task driver

The `systemd` driver runs Nomad tasks as systemd units. It exists for the
daemons CYSTEMD always managed: services installed from RPM or Debian
packages, with their own unit files, configured by files on the host. They
become Nomad workloads (scheduled, rolled out, restarted, health-checked,
logged) without being repackaged into containers.

It is built into the `cystemd` agent and also ships as an external plugin
(`make driver` → `bin/cystemd-driver-systemd`) for upstream Nomad clients.
It needs systemd as the init system and runs as root. A systemd source
([APPLICATIONS.md](APPLICATIONS.md#systemd)) generates jobs for it; you can
also write them yourself.

## Two modes

**Unit mode** manages an existing unit:

```hcl
task "payments-api" {
  driver = "systemd"

  config {
    unit = "payments-api.service"

    package {                       # optional: install this version first
      name    = "payments-api"      # default: the unit without .service
      version = "2.4.1"
    }

    file {                          # keep a rendered file in place on the host
      source      = "local/config.yml"
      destination = "/opt/payments-api/config.yml"
      mode        = "0640"
      owner       = "payments"
      group       = "payments"
      on_change   = "restart"       # restart, reload or none (default)
    }

    environment_files = ["secrets/.env"]   # passed to the unit
    cpu_quota         = "500m"             # CPUQuota
    memory_max        = "512Mi"            # MemoryMax
    properties = [{ LimitNOFILE = "65536" }]
  }

  template {
    data        = "{{ key \"cystemd/values/payments-api/config.yml\" }}"
    destination = "local/config.yml"
  }
}
```

Starting a unit-mode task:

1. **Claims the unit** on the node: one allocation at a time owns a unit (in
   `/run/cystemd/units`); a second one fails rather than fight over it.
2. **Installs the package version** when `package` is set, unless the package
   database (rpm or dpkg, the source of truth) says it is installed. With
   `makecache_repos`, those repositories' metadata is refreshed first, one at
   a time, so a build published a minute ago is found.
3. **Writes a runtime drop-in**, `/run/systemd/system/<unit>.d/90-cystemd.conf`:
   the task environment (as an `EnvironmentFile` in the task's secrets
   directory, mode 0600 — it can carry tokens), the `environment_files`,
   `CPUQuota`, `MemoryMax`, `CPUWeight` from the task's CPU, and the allowed
   `properties`. Runtime drop-ins vanish at reboot, exactly when Nomad starts
   the task again.
4. **Starts the unit**, or restarts it when it was already running, so that it
   runs with the drop-in. With `adopt_running = true` a running unit is
   adopted as it is instead.

While the task runs, the driver polls the unit: it reports a restart made
outside Nomad as a task event, keeps each `file` destination identical to its
source (Nomad re-renders templates when Consul keys or Vault secrets change)
and restarts or reloads the unit when a file with `on_change` changes, and
follows the unit's journal into the task's stdout, so `nomad alloc logs` and
the UI show the daemon's output. The task exits when the unit stops or fails
on its own; Nomad's restart policy applies. After a client restart, the
driver re-attaches to its units.

Stopping follows the unit's own stop policy (`KillSignal`, `TimeoutStopSec`):
its packaging knows how it must be stopped. If systemd itself wedges and the
unit stays `deactivating` past its `TimeoutStopSec` plus a margin (at most a
minute), the driver kills its main process; `TimeoutStopSec=infinity` is
respected. Destroying the task removes the drop-in, the environment file and
the claim; synced files stay, as they are the daemon's configuration.

Memory is not limited unless the job says so (`memory_max`, or
`enforce_resources = true` to apply Nomad's memory reservation): an existing
daemon never had a limit, and adopting it must not get it OOM-killed.

**Transient mode** runs a command in a transient unit, like `systemd-run`, with
systemd as supervisor and cgroup owner:

```hcl
config {
  command  = "/usr/bin/vault"
  args     = ["agent", "-config", "local/agent.hcl"]
  work_dir = "/var/lib/app"        # default: the task directory
}
```

The unit is `cystemd-<job>-<task>-<allocation>.service` in `transient_slice`,
runs as the task's `user`, writes to Nomad's log pipes, and is always limited
to the task's resources. Stopping sends the task's `kill_signal`, waits
`kill_timeout`, then stops the unit. CYSTEMD uses transient mode for the Vault
Agent sidecars of `secretsDelivery: vault-agent`.

Both modes report CPU and memory usage from the unit's cgroup and support
signals (`nomad alloc signal`, the UI's *Send SIGHUP*). `exec` is not
supported.

## Node configuration

Everything that touches the host is off until the node operator allows it:

```hcl
plugin "systemd" {
  config {
    # Units unit-mode tasks may manage (path.Match globs). Empty: no unit mode.
    allowed_units = ["payments-*.service", "billing-api.service"]

    # Packages tasks may install. Empty: no package block.
    allowed_packages = ["payments-*", "billing-api"]

    # Where file blocks may write: globs, or "dir/**" for anything below dir.
    allowed_file_destinations = ["/opt/**", "/etc/payments/**"]

    # The [Service] directives `properties` may set. The default allows
    # resource controls and restart tuning only — never what runs or as whom.
    allowed_properties = ["CPUQuota", "MemoryMax", "TasksMax", "Nice"]

    # Transient mode (arbitrary commands as root, like raw_exec). Default off.
    allow_transient = true
    transient_slice = "cystemd.slice"

    package_manager   = "dnf"         # dnf, yum, apt or none; default: detected
    makecache_repos   = ["internal-rpms"]
    makecache_timeout = "60s"         # default 60s
    install_timeout   = "10m"         # default 10m
  }
}
```

The default `allowed_properties` are `CPUWeight`, `CPUQuota`,
`CPUQuotaPeriodSec`, `AllowedCPUs`, `MemoryMin`, `MemoryLow`, `MemoryHigh`,
`MemoryMax`, `MemorySwapMax`, `TasksMax`, `IOWeight`, `LimitNOFILE`,
`LimitNPROC`, `LimitCORE`, `LimitMEMLOCK`, `Nice`, `OOMScoreAdjust`,
`OOMPolicy`, `Restart`, `RestartSec`, `TimeoutStartSec`, `TimeoutStopSec` and
`UMask`. A job that asks for anything outside the allowlists fails at start,
with the reason in its events.

Package names and versions are validated (no leading `-`, no whitespace) and
passed after `--`, so a job can never inject package manager options.

`cystemd migrate` writes each v4 host's allowlists, for exactly the units,
packages and directories its applications use ([MIGRATION.md](MIGRATION.md)).

## Node attributes

The driver fingerprints `driver.systemd = true`, `driver.systemd.version`
(systemd's), `driver.systemd.transient` and
`driver.systemd.package_manager`, usable in constraints. It reports itself
undetected when systemd is not the init system or the client does not run as
root, and unhealthy when systemd cannot be reached over D-Bus.
