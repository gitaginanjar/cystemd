# 0011. Applications declared by an Application (app of apps)

Status: Accepted

## Context

In Argo CD, Applications are Kubernetes resources, so an Application can
deploy other Applications: the "app of apps" pattern, the usual way to keep
the Applications themselves in Git and bootstrap a cluster from one. CYSTEMD
stores Applications in Consul, not as Nomad objects, so nothing of the kind
comes for free. CYSTEMD v4 users had their whole deployment, which services
on which pools, in one `config.yml` in Git; creating Applications by hand
or from scripts would be a step back.

## Decision

Add an `applications` source: its path holds Application manifests (the
format `app create -f` reads). The declaring Application treats them as its
resources: it compares their spec, and the labels, annotations and
finalizers they set, with the stored Applications; a sync creates and
updates them; pruning, and a cascading delete of the declaring Application,
delete them, their own `resources-finalizer.cystemd.io` deciding whether
their jobs go too. They are tracked by the label `cystemd.app`, the job
meta key, naming the declaring Application. Their health is shown in the
tree but not added to the declaring Application's, as in Argo CD since 1.8.

Unlike Argo CD, which lets an app of apps create Applications in any
project, the declared Applications must belong to the projects
`source.applications.projects` lists (default: the declaring Application's
own), and listing a project takes the right to create applications in it.
An Application cannot declare itself, an Application another one declares,
or its own ancestor.

## Consequences

- The Applications can live in Git, reviewed and versioned like the jobs:
  `cystemd migrate`'s `applications.yaml`, committed, keeps v4's
  "everything in Git" workflow.
- Write access to the declaring Application's path is the power to create
  Applications, bounded by the projects it may use, whose rules on
  repositories and destinations still apply to every declared Application.
- Edits made to a declared Application outside Git show as drift of its
  parent, and self-heal undoes them.
