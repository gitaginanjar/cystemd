// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/ui/views"
)

// Conversions between the UI's forms, the YAML manifest editor and the API
// objects.

func trimmed(v url.Values, key string) string { return strings.TrimSpace(v.Get(key)) }

// appFormFromValues reads the application form.
func appFormFromValues(v url.Values) views.AppForm {
	f := views.AppForm{
		Name:            trimmed(v, "name"),
		Project:         trimmed(v, "project"),
		Labels:          v.Get("labels"),
		RepoURL:         trimmed(v, "repoURL"),
		Revision:        trimmed(v, "targetRevision"),
		Path:            strings.Trim(trimmed(v, "path"), "/"),
		DestName:        trimmed(v, "destName"),
		DestNamespace:   trimmed(v, "destNamespace"),
		DestRegion:      trimmed(v, "destRegion"),
		Automated:       v.Get("syncPolicy") == "automated",
		Prune:           v.Get("prune") == "true",
		SelfHeal:        v.Get("selfHeal") == "true",
		SyncOptions:     map[string]bool{},
		SourceType:      v.Get("sourceType"),
		ResourceVersion: trimmed(v, "resourceVersion"),

		NomadFiles:    trimmed(v, "nomadFiles"),
		NomadVarFiles: trimmed(v, "nomadVarFiles"),
		NomadVars:     v.Get("nomadVars"),
		NomadConsulKV: trimmed(v, "nomadConsulKV"),

		ApplicationsFiles:    trimmed(v, "applicationsFiles"),
		ApplicationsProjects: trimmed(v, "applicationsProjects"),

		SystemdValuesFile:      trimmed(v, "systemdValuesFile"),
		SystemdSelector:        trimmed(v, "systemdSelector"),
		SystemdConfigKey:       trimmed(v, "systemdConfigKey"),
		SystemdVersionKey:      trimmed(v, "systemdVersionKey"),
		SystemdCPUKey:          trimmed(v, "systemdCPUKey"),
		SystemdMemoryKey:       trimmed(v, "systemdMemoryKey"),
		SystemdUnit:            trimmed(v, "systemdUnit"),
		SystemdPackage:         trimmed(v, "systemdPackage"),
		SystemdServiceConfig:   trimmed(v, "systemdServiceConfig"),
		SystemdWorkdir:         trimmed(v, "systemdWorkdir"),
		SystemdReplicas:        trimmed(v, "systemdReplicas"),
		SystemdNodePool:        trimmed(v, "systemdNodePool"),
		SystemdNodeSelector:    trimmed(v, "systemdNodeSelector"),
		SystemdDatacenters:     trimmed(v, "systemdDatacenters"),
		SystemdSecrets:         v.Get("systemdSecrets"),
		SystemdSecretsDelivery: trimmed(v, "systemdSecretsDelivery"),
		SystemdVaultRole:       trimmed(v, "systemdVaultRole"),
		SystemdConfigDelivery:  trimmed(v, "systemdConfigDelivery"),
		SystemdChangeMode:      trimmed(v, "systemdChangeMode"),
		SystemdServicePort:     trimmed(v, "systemdServicePort"),
		SystemdServiceName:     trimmed(v, "systemdServiceName"),
		SystemdCheckType:       trimmed(v, "systemdCheckType"),
		SystemdCheckPath:       trimmed(v, "systemdCheckPath"),
	}
	for _, o := range v["syncOption"] {
		f.SyncOptions[o] = true
	}
	switch f.SourceType {
	case string(v1.SourceTypeSystemd), string(v1.SourceTypeApplications):
	default:
		f.SourceType = string(v1.SourceTypeNomad)
	}
	return f
}

// parseKV reads "k=v" lines (or comma separated pairs).
func parseKV(s, what string) (map[string]string, error) {
	out := map[string]string{}
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == ',' }) {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, badRequest("%s %q: expected key=value", what, line)
		}
		out[k] = strings.TrimSpace(v)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func formatKV(m map[string]string) string {
	var b strings.Builder
	for _, k := range views.SortedKeys(m) {
		b.WriteString(k + "=" + m[k] + "\n")
	}
	return b.String()
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// applyAppForm writes the form's fields onto app, keeping what the form
// does not show (ignoreDifferences, retry, resources, annotations...).
func applyAppForm(app *v1.Application, f views.AppForm) error {
	app.APIVersion, app.Kind = v1.APIVersion, v1.KindApplication
	if app.Metadata.Name == "" {
		app.Metadata.Name = f.Name
	}
	app.Metadata.ResourceVersion = f.ResourceVersion
	labels, err := parseKV(f.Labels, "label")
	if err != nil {
		return err
	}
	app.Metadata.Labels = labels
	app.Spec.Project = f.Project
	if app.Spec.Project == "" {
		app.Spec.Project = v1.DefaultProject
	}
	src := &app.Spec.Source
	src.RepoURL, src.TargetRevision, src.Path = f.RepoURL, f.Revision, f.Path
	if src.TargetRevision == "HEAD" {
		src.TargetRevision = ""
	}
	switch f.SourceType {
	case string(v1.SourceTypeApplications):
		src.Nomad, src.Systemd = nil, nil
		src.Applications = &v1.ApplicationsSource{Files: splitList(f.ApplicationsFiles), Projects: splitList(f.ApplicationsProjects)}
	case string(v1.SourceTypeSystemd):
		src.Nomad, src.Applications = nil, nil
		if src.Systemd == nil {
			src.Systemd = &v1.SystemdSource{}
		}
		if err := applySystemdForm(src.Systemd, f); err != nil {
			return err
		}
	default:
		src.Systemd, src.Applications = nil, nil
		vars, err := parseKV(f.NomadVars, "variable")
		if err != nil {
			return err
		}
		n := &v1.NomadSource{Files: splitList(f.NomadFiles), VarFiles: splitList(f.NomadVarFiles), Vars: vars, ConsulKV: f.NomadConsulKV}
		if len(n.Files) == 0 && len(n.VarFiles) == 0 && len(n.Vars) == 0 && n.ConsulKV == "" {
			n = nil
		}
		src.Nomad = n
	}
	dest := f.DestName
	if dest == v1.InClusterName {
		dest = ""
	}
	app.Spec.Destination = v1.ApplicationDestination{Name: dest, Region: f.DestRegion, Namespace: f.DestNamespace}

	policy := app.Spec.SyncPolicy
	if policy == nil {
		policy = &v1.SyncPolicy{}
	}
	if f.Automated {
		if policy.Automated == nil {
			policy.Automated = &v1.SyncPolicyAutomated{}
		}
		policy.Automated.Prune, policy.Automated.SelfHeal = f.Prune, f.SelfHeal
	} else {
		policy.Automated = nil
	}
	// Options the form shows are replaced; others (Prune=false...) stay.
	shown := map[string]bool{}
	for _, c := range views.SyncOptionChoices {
		shown[c.Option] = true
	}
	var opts []string
	for _, o := range policy.SyncOptions {
		if !shown[o] {
			opts = append(opts, o)
		}
	}
	for _, c := range views.SyncOptionChoices {
		if f.SyncOptions[c.Option] {
			opts = append(opts, c.Option)
		}
	}
	policy.SyncOptions = opts
	if policy.Automated == nil && len(policy.SyncOptions) == 0 && policy.Retry == nil {
		policy = nil
	}
	app.Spec.SyncPolicy = policy
	return nil
}

func applySystemdForm(s *v1.SystemdSource, f views.AppForm) error {
	s.ValuesFile, s.Selector, s.ConfigKey = f.SystemdValuesFile, f.SystemdSelector, f.SystemdConfigKey
	s.VersionKey, s.CPUKey, s.MemoryKey = f.SystemdVersionKey, f.SystemdCPUKey, f.SystemdMemoryKey
	s.Unit, s.Package, s.ServiceConfig, s.Workdir = f.SystemdUnit, f.SystemdPackage, f.SystemdServiceConfig, f.SystemdWorkdir
	s.NodePool, s.NodeSelector, s.Datacenters = f.SystemdNodePool, f.SystemdNodeSelector, splitList(f.SystemdDatacenters)
	s.SecretsDelivery, s.VaultRole, s.ConfigDelivery = f.SystemdSecretsDelivery, f.SystemdVaultRole, f.SystemdConfigDelivery
	if f.SystemdChangeMode == "restart" {
		f.SystemdChangeMode = ""
	}
	s.ChangeMode = f.SystemdChangeMode
	s.Replicas = nil
	if f.SystemdReplicas != "" {
		n, err := strconv.Atoi(f.SystemdReplicas)
		if err != nil || n < 0 {
			return badRequest("replicas %q: expected a number, 0 or more", f.SystemdReplicas)
		}
		s.Replicas = &n
	}
	s.Secrets = nil
	for _, line := range views.TrimLines(f.SystemdSecrets) {
		path, dest, _ := strings.Cut(line, ":")
		s.Secrets = append(s.Secrets, v1.SystemdSecret{VaultPath: strings.TrimSpace(path), Destination: strings.TrimSpace(dest)})
	}
	if f.SystemdServicePort == "" {
		s.Service = nil
		return nil
	}
	port, err := strconv.Atoi(f.SystemdServicePort)
	if err != nil || port <= 0 || port > 65535 {
		return badRequest("service port %q: expected 1-65535", f.SystemdServicePort)
	}
	if s.Service == nil {
		s.Service = &v1.ServiceSpec{}
	}
	s.Service.Port, s.Service.Name = port, f.SystemdServiceName
	switch f.SystemdCheckType {
	case "":
		s.Service.Check = nil
	default:
		if s.Service.Check == nil {
			s.Service.Check = &v1.CheckSpec{}
		}
		s.Service.Check.Type, s.Service.Check.Path = f.SystemdCheckType, f.SystemdCheckPath
	}
	return nil
}

// appForm fills the form from an Application.
func appForm(a *v1.Application) views.AppForm {
	f := views.AppForm{
		Name:            a.Metadata.Name,
		Project:         a.Spec.Project,
		Labels:          formatKV(a.Metadata.Labels),
		RepoURL:         a.Spec.Source.RepoURL,
		Revision:        a.Spec.Source.TargetRevision,
		Path:            a.Spec.Source.Path,
		DestName:        a.Spec.Destination.ClusterName(),
		DestNamespace:   a.Spec.Destination.Namespace,
		DestRegion:      a.Spec.Destination.Region,
		Automated:       a.Spec.SyncPolicy.IsAutomated(),
		SyncOptions:     map[string]bool{},
		SourceType:      string(a.Spec.Source.Type()),
		ResourceVersion: a.Metadata.ResourceVersion,
	}
	if f.Automated {
		f.Prune, f.SelfHeal = a.Spec.SyncPolicy.Automated.Prune, a.Spec.SyncPolicy.Automated.SelfHeal
	}
	if a.Spec.SyncPolicy != nil {
		for _, o := range a.Spec.SyncPolicy.SyncOptions {
			f.SyncOptions[o] = true
		}
	}
	if as := a.Spec.Source.Applications; as != nil {
		f.ApplicationsFiles, f.ApplicationsProjects = strings.Join(as.Files, ", "), strings.Join(as.Projects, ", ")
	}
	if n := a.Spec.Source.Nomad; n != nil {
		f.NomadFiles, f.NomadVarFiles = strings.Join(n.Files, ", "), strings.Join(n.VarFiles, ", ")
		f.NomadVars, f.NomadConsulKV = formatKV(n.Vars), n.ConsulKV
	}
	if s := a.Spec.Source.Systemd; s != nil {
		f.SystemdValuesFile, f.SystemdSelector, f.SystemdConfigKey = s.ValuesFile, s.Selector, s.ConfigKey
		f.SystemdVersionKey, f.SystemdCPUKey, f.SystemdMemoryKey = s.VersionKey, s.CPUKey, s.MemoryKey
		f.SystemdUnit, f.SystemdPackage, f.SystemdServiceConfig, f.SystemdWorkdir = s.Unit, s.Package, s.ServiceConfig, s.Workdir
		if s.Replicas != nil {
			f.SystemdReplicas = strconv.Itoa(*s.Replicas)
		}
		f.SystemdNodePool, f.SystemdNodeSelector, f.SystemdDatacenters = s.NodePool, s.NodeSelector, strings.Join(s.Datacenters, ", ")
		f.SystemdSecretsDelivery, f.SystemdVaultRole, f.SystemdConfigDelivery, f.SystemdChangeMode = s.SecretsDelivery, s.VaultRole, s.ConfigDelivery, s.ChangeMode
		var secrets []string
		for _, sec := range s.Secrets {
			line := sec.VaultPath
			if sec.Destination != "" {
				line += ":" + sec.Destination
			}
			secrets = append(secrets, line)
		}
		f.SystemdSecrets = strings.Join(secrets, "\n")
		if s.Service != nil {
			f.SystemdServicePort, f.SystemdServiceName = strconv.Itoa(s.Service.Port), s.Service.Name
			if s.Service.Check != nil {
				f.SystemdCheckType, f.SystemdCheckPath = s.Service.Check.Type, s.Service.Check.Path
			}
		}
	}
	return f
}

// newAppForm is the empty NEW APP form.
func newAppForm(project, repo string) views.AppForm {
	if project == "" {
		project = v1.DefaultProject
	}
	return views.AppForm{Project: project, RepoURL: repo, DestName: v1.InClusterName, SourceType: string(v1.SourceTypeNomad), SyncOptions: map[string]bool{}}
}

// appManifest is the editable YAML of an Application: metadata the user
// owns and the spec; status is the controller's.
type appManifest struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   appManifestMeta    `yaml:"metadata"`
	Spec       v1.ApplicationSpec `yaml:"spec"`
}

type appManifestMeta struct {
	Name            string            `yaml:"name"`
	Labels          map[string]string `yaml:"labels,omitempty"`
	Annotations     map[string]string `yaml:"annotations,omitempty"`
	ResourceVersion string            `yaml:"resourceVersion,omitempty"`
}

func appYAML(a *v1.Application) string {
	m := appManifest{
		APIVersion: v1.APIVersion, Kind: v1.KindApplication,
		Metadata: appManifestMeta{Name: a.Metadata.Name, Labels: a.Metadata.Labels, Annotations: a.Metadata.Annotations, ResourceVersion: a.Metadata.ResourceVersion},
		Spec:     a.Spec,
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(m)
	return buf.String()
}

// appFromYAML parses an application manifest strictly (unknown fields are
// errors, so typos do not silently drop settings).
func appFromYAML(src string) (*v1.Application, error) {
	dec := yaml.NewDecoder(strings.NewReader(src))
	dec.KnownFields(true)
	var m appManifest
	if err := dec.Decode(&m); err != nil {
		return nil, badRequest("invalid application manifest: %v", err)
	}
	if m.Kind != "" && m.Kind != v1.KindApplication {
		return nil, badRequest("kind %q: expected %s", m.Kind, v1.KindApplication)
	}
	a := &v1.Application{APIVersion: v1.APIVersion, Kind: v1.KindApplication, Spec: m.Spec}
	a.Metadata.Name, a.Metadata.Labels, a.Metadata.Annotations = m.Metadata.Name, m.Metadata.Labels, m.Metadata.Annotations
	a.Metadata.ResourceVersion = m.Metadata.ResourceVersion
	return a, nil
}

// ─── projects and repositories ──────────────────────────────────────────

func projectForm(p *v1.AppProject) views.ProjectForm {
	var dests []string
	for _, d := range p.Spec.Destinations {
		dests = append(dests, destinationLine(d))
	}
	return views.ProjectForm{
		Name: p.Metadata.Name, Description: p.Spec.Description,
		SourceRepos: strings.Join(p.Spec.SourceRepos, "\n"), Destinations: strings.Join(dests, "\n"),
	}
}

func destinationLine(d v1.ApplicationDestination) string {
	name := d.Name
	if name == "" {
		name = "*"
	}
	ns := d.Namespace
	if ns == "" {
		ns = "*"
	}
	if d.Region != "" {
		return name + "/" + d.Region + "/" + ns
	}
	return name + "/" + ns
}

func projectFromForm(v url.Values) (*v1.AppProject, views.ProjectForm, error) {
	f := views.ProjectForm{
		Name: trimmed(v, "name"), Description: trimmed(v, "description"),
		SourceRepos: v.Get("sourceRepos"), Destinations: v.Get("destinations"),
	}
	p := &v1.AppProject{APIVersion: v1.APIVersion, Kind: v1.KindAppProject}
	p.Metadata.Name = f.Name
	p.Spec.Description = f.Description
	p.Spec.SourceRepos = views.TrimLines(f.SourceRepos)
	for _, line := range views.TrimLines(f.Destinations) {
		parts := strings.Split(line, "/")
		var d v1.ApplicationDestination
		switch len(parts) {
		case 2:
			d = v1.ApplicationDestination{Name: parts[0], Namespace: parts[1]}
		case 3:
			d = v1.ApplicationDestination{Name: parts[0], Region: parts[1], Namespace: parts[2]}
		default:
			return nil, f, badRequest("destination %q: expected cluster/namespace or cluster/region/namespace", line)
		}
		p.Spec.Destinations = append(p.Spec.Destinations, d)
	}
	return p, f, nil
}

func repoFromForm(v url.Values) (*v1.Repository, views.RepoForm) {
	f := views.RepoForm{
		Method: v.Get("method"), Repo: trimmed(v, "repo"), Name: trimmed(v, "name"), Project: trimmed(v, "project"),
		Username: trimmed(v, "username"), Password: v.Get("password"), SSHPrivateKey: v.Get("sshPrivateKey"),
		Insecure: v.Get("insecure") == "true", Force: v.Get("force") == "true",
	}
	r := &v1.Repository{Repo: f.Repo, Name: f.Name, Project: f.Project, Type: "git", Insecure: f.Insecure}
	if f.Method == "ssh" {
		r.SSHPrivateKey = strings.TrimSpace(f.SSHPrivateKey)
		if r.SSHPrivateKey != "" {
			r.SSHPrivateKey += "\n"
		}
	} else {
		r.Username, r.Password = f.Username, f.Password
	}
	return r, f
}

// syncRequestFromForm reads the SYNC panel. All resources selected means a
// full sync (no resource list).
func syncRequestFromForm(v url.Values, app *v1.Application) (SyncRequest, error) {
	req := SyncRequest{
		Revision:    trimmed(v, "revision"),
		Prune:       v.Get("prune") == "true",
		DryRun:      v.Get("dryRun") == "true",
		SyncOptions: v["syncOption"],
	}
	if req.Revision == app.Spec.Source.Revision() {
		req.Revision = ""
	}
	if v.Get("retry") == "true" {
		limit, err := strconv.ParseInt(v.Get("retryLimit"), 10, 64)
		if err != nil || limit < 1 {
			limit = 2
		}
		req.Retry = &v1.RetryStrategy{Limit: limit}
	}
	selected := v["resource"]
	syncable := len(syncResources(app))
	if len(selected) == 0 && syncable > 0 && v.Get("partial") != "" {
		return req, badRequest("select at least one resource to sync")
	}
	if len(selected) > 0 && len(selected) < syncable {
		for _, key := range selected {
			parts := strings.SplitN(key, "/", 3)
			if len(parts) != 3 {
				return req, badRequest("bad resource %q", key)
			}
			req.Resources = append(req.Resources, v1.SyncOperationResource{Kind: parts[0], Namespace: parts[1], Name: parts[2]})
		}
	}
	sort.Strings(req.SyncOptions)
	return req, nil
}

func describeErrors(errs []error) string {
	var b strings.Builder
	for i, e := range errs {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprint(&b, e)
	}
	return b.String()
}
