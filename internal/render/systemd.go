// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/values"
	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Inline config templates must never be evaluated: these delimiters cannot
// occur in a sane configuration file.
const (
	inertLeftDelim  = "[[cystemd-inert:"
	inertRightDelim = ":cystemd-inert]]"
)

// selectorUnsafe matches characters not allowed in a node meta key.
var selectorUnsafe = regexp.MustCompile(`[^A-Za-z0-9_]`)

// SelectorMetaKey is the client meta key marking membership of a CYSTEMD v4
// node pool selector (a host may carry several).
func SelectorMetaKey(selector string) string {
	return "cystemd_selector_" + selectorUnsafe.ReplaceAllString(selector, "_")
}

// renderSystemd generates the job of a systemd source: one task driving the
// unit with the systemd driver, the package version, CPU and memory limits
// from the values file, the service config delivered through Consul KV (or
// inline), and Vault secrets rendered into env files.
func (r *Renderer) renderSystemd(app *v1alpha1.Application, dir string) (*Manifests, error) {
	src := app.Spec.Source.Systemd
	valuesFile := orDefault(src.ValuesFile, "values.yaml")
	raw, err := readInside(dir, valuesFile)
	if err != nil {
		return nil, fmt.Errorf("values file: %w", err)
	}

	unit := orDefault(src.Unit, app.Metadata.Name)
	if !strings.Contains(unit, ".") {
		unit += ".service"
	}
	base := strings.TrimSuffix(unit, ".service")
	pkg := orDefault(src.Package, base)
	hostDir := path.Join(orDefault(src.Workdir, "/opt"), base)
	serviceConfig := orDefault(src.ServiceConfig, "target.config.yml")
	if strings.Contains(serviceConfig, "/") {
		return nil, fmt.Errorf("serviceConfig %q must be a file name", serviceConfig)
	}
	sel := src.Selector

	config := raw
	if src.ConfigKey != "" {
		if config, err = values.ExtractYAMLKey(raw, src.ConfigKey, sel); err != nil {
			return nil, fmt.Errorf("configKey %q: %w", values.ExpandSelector(src.ConfigKey, sel), err)
		}
	}
	scalar := func(key, what string) (string, error) {
		if key == "" {
			return "", nil
		}
		v, err := values.ExtractScalar(raw, key, sel)
		if err != nil {
			return "", fmt.Errorf("%s %q: %w", what, values.ExpandSelector(key, sel), err)
		}
		return v, nil
	}
	version, err := scalar(src.VersionKey, "versionKey")
	if err != nil {
		return nil, err
	}
	cpu, err := scalar(src.CPUKey, "cpuKey")
	if err != nil {
		return nil, err
	}
	if cpu != "" {
		if err := values.ValidateCPU(cpu); err != nil {
			return nil, err
		}
	}
	mem, err := scalar(src.MemoryKey, "memoryKey")
	if err != nil {
		return nil, err
	}
	if mem != "" {
		if err := values.ValidateMemory(mem); err != nil {
			return nil, err
		}
	}

	m := &Manifests{SourceType: v1alpha1.SourceTypeSystemd}
	changeMode := orDefault(src.ChangeMode, "restart")
	delivery := src.ConfigDelivery
	if delivery == "" {
		delivery = "inline"
		if r.Opts.ConsulEnabled {
			delivery = "consul"
		}
	}

	driverCfg := map[string]any{"unit": unit}
	if version != "" {
		driverCfg["package"] = []map[string]any{{"name": pkg, "version": version}}
	}
	if cpu != "" {
		driverCfg["cpu_quota"] = cpu
	}
	if mem != "" {
		driverCfg["memory_max"] = mem
	}

	// The service config: the template renders it into the task, the
	// driver keeps the host copy where the daemon reads it.
	cfgTmpl := &api.Template{
		DestPath:   ptr("local/" + serviceConfig),
		ChangeMode: ptr(changeMode),
		Perms:      ptr("0644"),
	}
	switch delivery {
	case "consul":
		key := ValuesKey(r.Opts.kvPrefix(), app.Metadata.Name, serviceConfig)
		m.KV = append(m.KV, KVPair{Key: key, Value: config})
		cfgTmpl.EmbeddedTmpl = ptr(fmt.Sprintf("{{ key %q }}", key))
	default:
		cfgTmpl.EmbeddedTmpl = ptr(string(config))
		cfgTmpl.LeftDelim = ptr(inertLeftDelim)
		cfgTmpl.RightDelim = ptr(inertRightDelim)
	}
	if changeMode == "signal" {
		cfgTmpl.ChangeSignal = ptr(src.ChangeSignal)
	}
	templates := []*api.Template{cfgTmpl}
	files := []map[string]any{{
		"source": "local/" + serviceConfig, "destination": path.Join(hostDir, serviceConfig), "mode": "0644",
	}}

	task := &api.Task{
		Name:   base,
		Driver: "systemd",
		Config: driverCfg,
		Resources: &api.Resources{
			CPU:      ptr(100),
			MemoryMB: ptr(128),
		},
	}
	if src.Resources != nil {
		if src.Resources.CPU > 0 {
			task.Resources.CPU = ptr(src.Resources.CPU)
		}
		if src.Resources.MemoryMB > 0 {
			task.Resources.MemoryMB = ptr(src.Resources.MemoryMB)
		}
	}

	var sidecars []*api.Task
	var envFiles []string
	if len(src.Secrets) > 0 {
		role := orDefault(src.VaultRole, r.Opts.DefaultVaultRole)
		if src.SecretsDelivery == "vault-agent" {
			init, sidecar, secretFiles, secretEnv, err := r.vaultAgentTasks(src.Secrets, role, hostDir, changeMode)
			if err != nil {
				return nil, err
			}
			sidecars = append(sidecars, init, sidecar)
			files = append(files, secretFiles...)
			envFiles = append(envFiles, secretEnv...)
		} else {
			task.Vault = &api.Vault{Role: role, ChangeMode: ptr("noop")}
			for _, sec := range src.Secrets {
				dest := orDefault(sec.Destination, ".env")
				data := vaultEnvTemplate(sec, "{{", "}}")
				t := &api.Template{
					EmbeddedTmpl: ptr(data),
					DestPath:     ptr("secrets/" + dest),
					Perms:        ptr("0600"),
					ChangeMode:   ptr(changeMode),
				}
				if changeMode == "signal" {
					t.ChangeSignal = ptr(src.ChangeSignal)
				}
				templates = append(templates, t)
				files = append(files, map[string]any{
					"source": "secrets/" + dest, "destination": path.Join(hostDir, dest), "mode": "0600",
				})
				envFiles = append(envFiles, "secrets/"+dest)
			}
		}
	}
	driverCfg["file"] = files
	if len(envFiles) > 0 {
		driverCfg["environment_files"] = envFiles
	}
	task.Templates = templates

	replicas := 1
	if src.Replicas != nil {
		replicas = *src.Replicas
	}
	jobType := "service"
	if replicas == 0 {
		jobType = "system"
	}
	tg := &api.TaskGroup{
		Name:  ptr(base),
		Tasks: append(sidecars, task),
		Update: &api.UpdateStrategy{
			MaxParallel:      ptr(1),
			HealthCheck:      ptr("task_states"),
			MinHealthyTime:   ptr(10 * time.Second),
			HealthyDeadline:  ptr(5 * time.Minute),
			ProgressDeadline: ptr(10 * time.Minute),
			AutoRevert:       ptr(true),
		},
	}
	if jobType == "service" {
		tg.Count = ptr(replicas)
		// A unit is a node-wide singleton.
		if replicas > 1 {
			tg.Constraints = append(tg.Constraints, &api.Constraint{Operand: "distinct_hosts", RTarget: "true"})
		}
	}
	if src.NodeSelector != "" {
		tg.Constraints = append(tg.Constraints, &api.Constraint{
			LTarget: "${meta." + SelectorMetaKey(src.NodeSelector) + "}", Operand: "=", RTarget: "true",
		})
	}
	if s := src.Service; s != nil {
		provider := "nomad"
		if r.Opts.ConsulEnabled {
			provider = "consul"
		}
		tg.Networks = []*api.NetworkResource{{Mode: "host", ReservedPorts: []api.Port{{Label: "svc", Value: s.Port}}}}
		svc := &api.Service{Name: orDefault(s.Name, base), PortLabel: "svc", Provider: provider, Tags: s.Tags}
		if c := s.Check; c != nil {
			check := api.ServiceCheck{Name: base + " health", Type: orDefault(c.Type, "tcp"), Path: c.Path}
			if check.Type == "http" && check.Path == "" {
				check.Path = "/"
			}
			check.Interval = parseDuration(c.Interval, 10*time.Second)
			check.Timeout = parseDuration(c.Timeout, 2*time.Second)
			svc.Checks = []api.ServiceCheck{check}
			tg.Update.HealthCheck = ptr("checks")
		}
		tg.Services = []*api.Service{svc}
	}

	datacenters := src.Datacenters
	if len(datacenters) == 0 {
		datacenters = []string{"*"}
	}
	job := &api.Job{
		ID:          ptr(app.Metadata.Name),
		Name:        ptr(app.Metadata.Name),
		Type:        ptr(jobType),
		Datacenters: datacenters,
		TaskGroups:  []*api.TaskGroup{tg},
		Meta: map[string]string{
			"cystemd.source": "systemd",
			"cystemd.unit":   unit,
		},
	}
	if src.NodePool != "" {
		job.NodePool = ptr(src.NodePool)
	}
	if app.Spec.Destination.Namespace != "" {
		job.Namespace = ptr(app.Spec.Destination.Namespace)
	}
	if app.Spec.Destination.Region != "" {
		job.Region = ptr(app.Spec.Destination.Region)
	}
	job.Canonicalize()
	m.Jobs = []*api.Job{job}
	return m, nil
}

// ValuesKey is the Consul key of an Application's rendered config file.
func ValuesKey(prefix, app, file string) string {
	return path.Join(prefix, "values", app, file)
}

// kvDataPath turns a logical KV path into the API path of its engine
// version: KV v2 reads go through "<mount>/data/<path>".
func kvDataPath(sec v1alpha1.SystemdSecret) string {
	p := strings.Trim(sec.VaultPath, "/")
	if sec.KVVersion == 1 {
		return p
	}
	mount, rest, ok := strings.Cut(p, "/")
	if !ok {
		return p + "/data"
	}
	return mount + "/data/" + rest
}

// vaultEnvTemplate renders a KV secret as KEY="value" lines (JSON-quoted,
// so quotes and newlines are escaped) with the given template delimiters.
func vaultEnvTemplate(sec v1alpha1.SystemdSecret, l, r string) string {
	data := ".Data.data"
	if sec.KVVersion == 1 {
		data = ".Data"
	}
	return fmt.Sprintf("%s- with secret %q %s%s range $k, $v := %s %s%s $k %s=%s $v | toJSON %s\n%s end %s%s end %s",
		l, kvDataPath(sec), r, l, data, r, l, r, l, r, l, r, l, r)
}

// vaultAgentTasks builds the Vault Agent init and sidecar tasks of the
// "vault-agent" secrets delivery — the Kubernetes injector pattern on Nomad:
// the init task authenticates with the task's workload identity (JWT auth),
// renders the env files once and exits so the daemon starts with them; the
// sidecar keeps them fresh. Both write to the shared allocation directory,
// where the systemd driver syncs them to the host and restarts the unit on
// change (on_change).
func (r *Renderer) vaultAgentTasks(secrets []v1alpha1.SystemdSecret, role, hostDir, changeMode string) (init, sidecar *api.Task, files []map[string]any, envFiles []string, err error) {
	if r.Opts.VaultAddress == "" {
		return nil, nil, nil, nil, fmt.Errorf("secretsDelivery vault-agent needs the control plane's vault.address")
	}
	var tmpls strings.Builder
	for _, sec := range secrets {
		dest := orDefault(sec.Destination, ".env")
		fmt.Fprintf(&tmpls, `
template {
  destination = "[[ env "NOMAD_ALLOC_DIR" ]]/data/vault/%s"
  perms       = "0600"
  contents    = <<EOT
%s
EOT
}
`, dest, vaultEnvTemplate(sec, "{{", "}}"))
		onChange := "none"
		if changeMode == "restart" {
			onChange = "restart"
		}
		files = append(files, map[string]any{
			"source": "alloc/data/vault/" + dest, "destination": path.Join(hostDir, dest), "mode": "0600", "on_change": onChange,
		})
		envFiles = append(envFiles, "alloc/data/vault/"+dest)
	}
	authPath := orDefault(r.Opts.VaultJWTAuthPath, "auth/jwt-nomad")
	agentConfig := func(exitAfterAuth bool) string {
		return fmt.Sprintf(`# Rendered by Nomad for the CYSTEMD vault-agent secrets delivery.
exit_after_auth = %t
pid_file        = "[[ env "NOMAD_TASK_DIR" ]]/vault-agent.pid"

vault {
  address = %q
}

auto_auth {
  method "jwt" {
    mount_path = %q
    config = {
      path                     = "[[ env "NOMAD_SECRETS_DIR" ]]/nomad_vault_default.jwt"
      role                     = %q
      remove_jwt_after_reading = false
    }
  }
}

template_config {
  exit_on_retry_failure = true
}
%s`, exitAfterAuth, r.Opts.VaultAddress, authPath, role, tmpls.String())
	}
	command := orDefault(r.Opts.VaultAgentCommand, "/usr/bin/vault")
	mk := func(name string, sidecarTask bool) *api.Task {
		return &api.Task{
			Name:      name,
			Driver:    "systemd",
			Lifecycle: &api.TaskLifecycle{Hook: "prestart", Sidecar: sidecarTask},
			Config: map[string]any{
				"command": command,
				"args":    []string{"agent", "-config=local/vault-agent.hcl"},
			},
			Identities: []*api.WorkloadIdentity{{
				Name: "vault_default", Audience: []string{"vault.io"}, File: true, TTL: time.Hour,
			}},
			Templates: []*api.Template{{
				EmbeddedTmpl: ptr(agentConfig(!sidecarTask)),
				DestPath:     ptr("local/vault-agent.hcl"),
				LeftDelim:    ptr("[["),
				RightDelim:   ptr("]]"),
				ChangeMode:   ptr("restart"),
			}},
			Resources: &api.Resources{CPU: ptr(50), MemoryMB: ptr(64)},
		}
	}
	return mk("vault-agent-init", false), mk("vault-agent", true), files, envFiles, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func parseDuration(s string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return d
	}
	return def
}
