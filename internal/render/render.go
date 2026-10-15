// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package render turns an Application's source tree into the desired state:
// Nomad jobs and Consul keys. Two renderers exist: Nomad (jobspecs in Git,
// parsed server-side by Nomad itself) and Systemd (the CYSTEMD v4 values-file
// model, generated into systemd-driver jobs).
package render

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// KVPair is a desired Consul key.
type KVPair struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
}

// Manifests is the rendered desired state of an Application.
type Manifests struct {
	Jobs       []*api.Job          `json:"jobs"`
	KV         []KVPair            `json:"kv,omitempty"`
	SourceType v1alpha1.SourceType `json:"sourceType"`
	// Images summarises what the jobs run (UI).
	Images []string `json:"images,omitempty"`
	// Sources maps job IDs to their original jobspec, stored as the Nomad
	// job submission so `nomad job inspect`/the UI can show it.
	Sources map[string]*api.JobSubmission `json:"-"`
}

// ParseFunc parses an HCL2 jobspec with variables (Nomad's /v1/jobs/parse).
type ParseFunc func(ctx context.Context, hcl, variables string) (*api.Job, error)

// Options configures rendering.
type Options struct {
	// ConsulEnabled selects Consul KV config delivery and Consul service
	// registration in generated jobs.
	ConsulEnabled bool
	// KVPrefix is the control plane's Consul prefix (default "cystemd").
	KVPrefix string
	// DefaultVaultRole is the Vault JWT role of generated workloads.
	DefaultVaultRole string
	// VaultAddress and VaultJWTAuthPath configure Vault Agent sidecars.
	VaultAddress     string
	VaultJWTAuthPath string
	// VaultAgentCommand is the vault binary on the nodes (default
	// /usr/bin/vault).
	VaultAgentCommand string
}

func (o Options) kvPrefix() string {
	if o.KVPrefix == "" {
		return "cystemd"
	}
	return strings.Trim(o.KVPrefix, "/")
}

// Renderer renders Applications.
type Renderer struct {
	Parse ParseFunc
	Opts  Options
}

// Render renders app from dir, the checked-out source path.
func (r *Renderer) Render(ctx context.Context, app *v1alpha1.Application, dir string) (*Manifests, error) {
	var m *Manifests
	var err error
	switch app.Spec.Source.Type() {
	case v1alpha1.SourceTypeSystemd:
		m, err = r.renderSystemd(app, dir)
	default:
		m, err = r.renderNomad(ctx, app, dir)
	}
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, job := range m.Jobs {
		instrument(job, app)
		key := deref(job.Namespace) + "/" + deref(job.ID)
		if seen[key] {
			return nil, fmt.Errorf("job %s is defined more than once", key)
		}
		seen[key] = true
	}
	sort.Slice(m.Jobs, func(i, j int) bool { return jobKey(m.Jobs[i]) < jobKey(m.Jobs[j]) })
	sort.Slice(m.KV, func(i, j int) bool { return m.KV[i].Key < m.KV[j].Key })
	m.Images = summarizeImages(m.Jobs)
	return m, nil
}

// instrument stamps the tracking meta and applies the destination: a job
// left in the default namespace/region moves to the Application's; a job
// naming another namespace keeps it (and is checked against the project).
func instrument(job *api.Job, app *v1alpha1.Application) {
	if job.Meta == nil {
		job.Meta = map[string]string{}
	}
	job.Meta[v1alpha1.AppMetaKey] = app.Metadata.Name
	dest := app.Spec.Destination
	if dest.Namespace != "" && (job.Namespace == nil || *job.Namespace == "" || *job.Namespace == "default") {
		job.Namespace = ptr(dest.Namespace)
	}
	if job.Namespace == nil || *job.Namespace == "" {
		job.Namespace = ptr("default")
	}
	if dest.Region != "" && (job.Region == nil || *job.Region == "" || *job.Region == "global") {
		job.Region = ptr(dest.Region)
	}
}

func jobKey(j *api.Job) string { return deref(j.Namespace) + "/" + deref(j.ID) }

// JobKey is "<namespace>/<id>".
func JobKey(j *api.Job) string { return jobKey(j) }

// summarizeImages lists container images, packages and commands.
func summarizeImages(jobs []*api.Job) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, j := range jobs {
		for _, tg := range j.TaskGroups {
			for _, t := range tg.Tasks {
				switch t.Driver {
				case "docker", "podman":
					add(fmt.Sprint(t.Config["image"]))
				case "systemd":
					if pkgs, ok := t.Config["package"].([]any); ok && len(pkgs) > 0 {
						if p, ok := pkgs[0].(map[string]any); ok {
							add(fmt.Sprintf("pkg:%v@%v", p["name"], p["version"]))
							continue
						}
					}
					if pkgs, ok := t.Config["package"].([]map[string]any); ok && len(pkgs) > 0 {
						add(fmt.Sprintf("pkg:%v@%v", pkgs[0]["name"], pkgs[0]["version"]))
						continue
					}
					if u, ok := t.Config["unit"].(string); ok {
						add("unit:" + u)
					} else if c, ok := t.Config["command"].(string); ok {
						add(c)
					}
				default:
					if c, ok := t.Config["command"].(string); ok {
						add(c)
					}
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

func ptr[T any](v T) *T { return &v }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
