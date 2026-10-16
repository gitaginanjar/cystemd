# Example GitOps repository

Two applications in the layout CYSTEMD expects:

| Path | Source type | What it shows |
|---|---|---|
| `apps/hello` | Nomad jobspecs | HCL2 variables from a var file, a Consul key the app manages and reads in a template, a PostSync hook that checks the new version answers |
| `apps/payments-api` | systemd (values file) | The CYSTEMD v4 model: a package version, CPU and memory limits and a config file from Helm-style values, run on a node pool by the systemd driver, secrets from Vault |

`applications/` and `projects/` hold the manifests that create them, either
one by one or, with `bootstrap.yaml`, through one Application that declares
the others (app of apps):

```sh
cystemd proj create -f projects/platform.yaml
cystemd app create -f applications/hello.yaml
cystemd app create -f applications/payments-api.yaml
# or, the Applications following Git from then on:
cystemd app create -f bootstrap.yaml
```

The same manifests can be pasted into the UI (NEW APP → EDIT AS YAML).
