// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// validName is a DNS-1123 label-like name (Argo CD applies the same rule).
var validName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9._]{0,251}[a-z0-9])?$`)

// ValidateName checks an object name.
func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("invalid name %q: lowercase alphanumerics, '-', '.' and '_', starting and ending alphanumeric", name)
	}
	return nil
}

// Validate checks an Application's metadata and spec.
func (a *Application) Validate() error {
	var errs []error
	if err := ValidateName(a.Metadata.Name); err != nil {
		errs = append(errs, err)
	}
	s := &a.Spec
	if s.Project == "" {
		errs = append(errs, errors.New("spec.project is required"))
	}
	if strings.TrimSpace(s.Source.RepoURL) == "" {
		errs = append(errs, errors.New("spec.source.repoURL is required"))
	}
	if strings.HasPrefix(s.Source.Path, "/") || strings.Contains(path.Clean("/"+s.Source.Path), "..") {
		errs = append(errs, fmt.Errorf("spec.source.path %q must be relative and stay inside the repository", s.Source.Path))
	}
	n := 0
	for _, set := range []bool{s.Source.Nomad != nil, s.Source.Systemd != nil, s.Source.Applications != nil} {
		if set {
			n++
		}
	}
	if n > 1 {
		errs = append(errs, errors.New("spec.source: set only one of nomad, systemd and applications"))
	}
	if as := s.Source.Applications; as != nil {
		for i, p := range as.Projects {
			if err := ValidateName(p); err != nil {
				errs = append(errs, fmt.Errorf("spec.source.applications.projects[%d]: %v", i, err))
			}
		}
	}
	if sd := s.Source.Systemd; sd != nil {
		if sd.Replicas != nil && *sd.Replicas < 0 {
			errs = append(errs, errors.New("spec.source.systemd.replicas must not be negative"))
		}
		switch sd.SecretsDelivery {
		case "", "nomad", "vault-agent":
		default:
			errs = append(errs, fmt.Errorf("spec.source.systemd.secretsDelivery %q: nomad or vault-agent", sd.SecretsDelivery))
		}
		switch sd.ConfigDelivery {
		case "", "consul", "inline":
		default:
			errs = append(errs, fmt.Errorf("spec.source.systemd.configDelivery %q: consul or inline", sd.ConfigDelivery))
		}
		switch sd.ChangeMode {
		case "", "restart", "signal", "noop":
		default:
			errs = append(errs, fmt.Errorf("spec.source.systemd.changeMode %q: restart, signal or noop", sd.ChangeMode))
		}
		if sd.ChangeMode == "signal" && sd.ChangeSignal == "" {
			errs = append(errs, errors.New("spec.source.systemd.changeSignal is required with changeMode signal"))
		}
		for i, sec := range sd.Secrets {
			if sec.VaultPath == "" || strings.Contains(sec.VaultPath, "/data/") {
				errs = append(errs, fmt.Errorf("spec.source.systemd.secrets[%d].vaultPath %q: logical KV path without /data/", i, sec.VaultPath))
			}
			if sec.KVVersion != 0 && sec.KVVersion != 1 && sec.KVVersion != 2 {
				errs = append(errs, fmt.Errorf("spec.source.systemd.secrets[%d].kvVersion must be 1 or 2", i))
			}
			if strings.Contains(sec.Destination, "/") {
				errs = append(errs, fmt.Errorf("spec.source.systemd.secrets[%d].destination must be a file name", i))
			}
		}
	}
	if s.RevisionHistoryLimit != nil && *s.RevisionHistoryLimit < 0 {
		errs = append(errs, errors.New("spec.revisionHistoryLimit must not be negative"))
	}
	return errors.Join(errs...)
}

// SetDefaults fills apiVersion, kind and the project.
func (a *Application) SetDefaults() {
	a.APIVersion = APIVersion
	a.Kind = KindApplication
	if a.Spec.Project == "" {
		a.Spec.Project = DefaultProject
	}
}

// DeepCopy returns an independent copy.
func (a *Application) DeepCopy() *Application {
	if a == nil {
		return nil
	}
	var out Application
	b, _ := json.Marshal(a)
	_ = json.Unmarshal(b, &out)
	return &out
}

// Validate checks a project.
func (p *AppProject) Validate() error {
	if err := ValidateName(p.Metadata.Name); err != nil {
		return err
	}
	for _, r := range p.Spec.SourceRepos {
		if _, err := path.Match(r, ""); err != nil {
			return fmt.Errorf("sourceRepos %q: %v", r, err)
		}
	}
	if o := p.Spec.OrphanedResources; o != nil {
		for i, k := range o.Ignore {
			if k.Kind != "" && k.Kind != KindJob {
				return fmt.Errorf("orphanedResources.ignore[%d]: kind %q: only jobs can be orphaned", i, k.Kind)
			}
		}
	}
	for i, w := range p.Spec.SyncWindows {
		if w == nil {
			return fmt.Errorf("syncWindows[%d] is empty", i)
		}
		if err := w.Validate(); err != nil {
			return fmt.Errorf("syncWindows[%d]: %w", i, err)
		}
	}
	return p.validateRoles()
}

// SetDefaults fills apiVersion and kind.
func (p *AppProject) SetDefaults() {
	p.APIVersion = APIVersion
	p.Kind = KindAppProject
}

// DefaultAppProject is the implicit project allowing everything.
func DefaultAppProject() *AppProject {
	return &AppProject{
		APIVersion: APIVersion, Kind: KindAppProject,
		Metadata: ObjectMeta{Name: DefaultProject},
		Spec: AppProjectSpec{
			Description:  "Default project: any repository, any destination.",
			SourceRepos:  []string{"*"},
			Destinations: []ApplicationDestination{{Name: "*", Region: "*", Namespace: "*"}},
		},
	}
}

// globMatch matches a * / ? glob over whole strings, "/" included.
func globMatch(pattern, s string) bool {
	if pattern == "*" {
		return true
	}
	re := "^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + "$"
	ok, _ := regexp.MatchString(re, s)
	return ok
}

// Permits checks that the project allows the Application's repository and
// destination.
func (p *AppProject) Permits(app *Application) error {
	if !p.IsSourcePermitted(app.Spec.Source.RepoURL) {
		return fmt.Errorf("application repository %s is not permitted in project %q", app.Spec.Source.RepoURL, p.Metadata.Name)
	}
	if d := app.Spec.Destination; !p.IsDestinationPermitted(d) {
		return fmt.Errorf("application destination %s/%s/%s is not permitted in project %q",
			d.ClusterName(), d.Region, d.NamespaceOrDefault(), p.Metadata.Name)
	}
	return nil
}

// IsSourcePermitted reports whether the project allows repoURL.
func (p *AppProject) IsSourcePermitted(repoURL string) bool {
	for _, r := range p.Spec.SourceRepos {
		if globMatch(r, repoURL) || globMatch(r, strings.TrimSuffix(repoURL, ".git")) {
			return true
		}
	}
	return false
}

// IsDestinationPermitted reports whether the project allows dest.
func (p *AppProject) IsDestinationPermitted(dest ApplicationDestination) bool {
	for _, d := range p.Spec.Destinations {
		name, region, ns := d.Name, d.Region, d.Namespace
		if name == "" {
			name = "*"
		}
		if region == "" {
			region = "*"
		}
		if ns == "" {
			ns = "*"
		}
		if globMatch(name, dest.ClusterName()) && globMatch(region, dest.Region) && globMatch(ns, dest.NamespaceOrDefault()) {
			return true
		}
	}
	return false
}
