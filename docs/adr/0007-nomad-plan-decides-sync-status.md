# 0007. Nomad's plan decides whether a job is out of sync

Status: Accepted

## Context

Argo CD compares desired and live manifests field by field, with
normalizations for defaults that Kubernetes fills in. A Nomad job read back
from the API differs from the job parsed from HCL in many ways that are not
changes: defaults the server fills in or zeroes, canonicalized durations,
server-side fields, the job-level `update` block folded into task groups.
A field diff would report drift that is not there, or need a normalization
rule for every such case and every Nomad version.

## Decision

Ask Nomad. A job is `OutOfSync` when `Job.Plan` for the desired job reports
changes; the plan's diff is shown with the YAML diff. The YAML diff, for
people, compares both jobs after normalization (server-side fields dropped,
the desired job canonicalized by Nomad's parser, the job-level update block
reduced to what the server keeps) and only covers the fields the desired job
sets. `ignoreDifferences` paths are copied from the live job into the desired
one before planning, so ignored fields never cause a plan change or get
overwritten by a sync.

## Consequences

- "Out of sync" means exactly "a sync would change something", by Nomad's
  own definition, across Nomad versions.
- Each comparison of a job costs a plan request to the Nomad servers (cheap,
  no scheduling side effects); comparisons are triggered by events and a
  periodic interval, not continuously.
- Without a reachable Nomad, the comparison of jobs fails (`Unknown`) rather
  than guessing.
