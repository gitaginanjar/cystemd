# 0006. Run units, not containers: a systemd task driver

Status: Accepted

## Context

The services CYSTEMD manages are RPM-installed daemons with their own unit
files, users, directories and configuration paths, many of them stateful.
Repackaging them into containers is a project per service. Nomad's `exec` and
`raw_exec` drivers run a command, not an existing unit with its packaging's
stop policy, and know nothing of packages.

## Decision

Write a Nomad task driver that runs tasks as systemd units, with two modes:

- **unit mode** manages an existing unit: installs the wanted package version
  (the package database is the source of truth), keeps the files Nomad
  renders (configuration, secrets) in place on the host, attaches the task
  environment and resource limits through a runtime drop-in it removes on
  destroy, follows the unit's journal into the task's logs, and stops the unit
  by its own policy, with a kill watchdog for a wedged systemd;
- **transient mode** runs a command in a transient unit, with systemd as
  supervisor.

Make every host-mutating capability opt-in per node: the units, packages,
file destinations and unit properties a job may touch, and transient mode.
Ship the driver built into the agent and as an external plugin.

## Consequences

- Existing services move to Nomad unchanged, and keep running if the agent
  stops (they are systemd's).
- A job author can only reach what the node operator allowed; the allowlists
  are per node, and `cystemd migrate` writes them for exactly what each v4
  host ran.
- A unit is a node-wide singleton: the driver lets one allocation own a unit
  per node, and generated jobs use `distinct_hosts`.
- The driver needs root and systemd as the init system; it reports itself
  undetected elsewhere.
