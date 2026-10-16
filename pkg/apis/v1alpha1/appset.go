// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// KindApplicationSet is the kind of an ApplicationSet.
const KindApplicationSet = "ApplicationSet"

// AppSetLabelKey labels the Applications an ApplicationSet generates with
// its name: they are its own, to update and delete.
const AppSetLabelKey = "cystemd.applicationset"

// ApplicationSet is Argo CD's: a template of an Application, stamped out
// once for each set of parameters its generators produce (a list, the
// clusters, directories or files of a repository, and combinations).
type ApplicationSet struct {
	APIVersion string               `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind       string               `json:"kind,omitempty" yaml:"kind,omitempty"`
	Metadata   ObjectMeta           `json:"metadata" yaml:"metadata"`
	Spec       ApplicationSetSpec   `json:"spec" yaml:"spec"`
	Status     ApplicationSetStatus `json:"status,omitzero" yaml:"status,omitempty"`
}

// ApplicationSetSpec is what an ApplicationSet generates.
type ApplicationSetSpec struct {
	// Generators produce the parameters; each set of parameters is one
	// Application.
	Generators []ApplicationSetGenerator `json:"generators" yaml:"generators"`
	// Template is the Application, whose strings are Go templates of the
	// parameters ("{{ .path.basename }}"), with Sprig's functions.
	Template   ApplicationSetTemplate    `json:"template" yaml:"template"`
	SyncPolicy *ApplicationSetSyncPolicy `json:"syncPolicy,omitempty" yaml:"syncPolicy,omitempty"`
	// GoTemplateOptions are the templates' options (default:
	// missingkey=error, an unknown parameter is an error).
	GoTemplateOptions []string `json:"goTemplateOptions,omitempty" yaml:"goTemplateOptions,omitempty"`
}

// ApplicationSetTemplate is the Application an ApplicationSet stamps out.
type ApplicationSetTemplate struct {
	Metadata ApplicationSetTemplateMeta `json:"metadata" yaml:"metadata"`
	Spec     ApplicationSpec            `json:"spec" yaml:"spec"`
}

// ApplicationSetTemplateMeta is the generated Applications' metadata.
type ApplicationSetTemplateMeta struct {
	Name        string            `json:"name" yaml:"name"`
	Labels      map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
	Finalizers  []string          `json:"finalizers,omitempty" yaml:"finalizers,omitempty"`
}

// ApplicationSetGenerator is one generator: exactly one field is set.
type ApplicationSetGenerator struct {
	List     *ListGenerator    `json:"list,omitempty" yaml:"list,omitempty"`
	Clusters *ClusterGenerator `json:"clusters,omitempty" yaml:"clusters,omitempty"`
	Git      *GitGenerator     `json:"git,omitempty" yaml:"git,omitempty"`
	Matrix   *MatrixGenerator  `json:"matrix,omitempty" yaml:"matrix,omitempty"`
	Merge    *MergeGenerator   `json:"merge,omitempty" yaml:"merge,omitempty"`
}

// ListGenerator produces one set of parameters per element.
type ListGenerator struct {
	Elements []map[string]any `json:"elements" yaml:"elements"`
}

// ClusterGenerator produces one set of parameters per destination cluster:
// name, nameNormalized, server and metadata.labels.
type ClusterGenerator struct {
	// Selector matches the clusters' labels (none: every cluster).
	Selector *LabelSelector `json:"selector,omitempty" yaml:"selector,omitempty"`
	// Values are added as values.<key>; they may use the cluster's
	// parameters ("{{ .name }}-eu").
	Values map[string]string `json:"values,omitempty" yaml:"values,omitempty"`
}

// GitGenerator produces parameters from a repository at a revision: one
// set per directory matching directories, or per file matching files (the
// file's YAML or JSON content, flattened, besides its path).
type GitGenerator struct {
	RepoURL     string                      `json:"repoURL" yaml:"repoURL"`
	Revision    string                      `json:"revision,omitempty" yaml:"revision,omitempty"`
	Directories []GitDirectoryGeneratorItem `json:"directories,omitempty" yaml:"directories,omitempty"`
	Files       []GitFileGeneratorItem      `json:"files,omitempty" yaml:"files,omitempty"`
	// Values are added as values.<key>; they may use the parameters.
	Values map[string]string `json:"values,omitempty" yaml:"values,omitempty"`
	// PathParamPrefix nests the path parameters under it
	// (<prefix>.path.basename).
	PathParamPrefix string `json:"pathParamPrefix,omitempty" yaml:"pathParamPrefix,omitempty"`
}

// GitDirectoryGeneratorItem matches directories by a glob (* within a
// segment, ** across segments); an exclude item removes its matches.
type GitDirectoryGeneratorItem struct {
	Path    string `json:"path" yaml:"path"`
	Exclude bool   `json:"exclude,omitempty" yaml:"exclude,omitempty"`
}

// GitFileGeneratorItem matches files by a glob.
type GitFileGeneratorItem struct {
	Path string `json:"path" yaml:"path"`
}

// MatrixGenerator combines two generators: every set of the first with
// every set of the second, whose own fields may use the first's
// parameters.
type MatrixGenerator struct {
	Generators []ApplicationSetGenerator `json:"generators" yaml:"generators"`
}

// MergeGenerator merges the parameter sets of its generators into the
// first's, where the values of all the merge keys are equal.
type MergeGenerator struct {
	MergeKeys  []string                  `json:"mergeKeys" yaml:"mergeKeys"`
	Generators []ApplicationSetGenerator `json:"generators" yaml:"generators"`
}

// LabelSelector matches labels (Kubernetes' and Argo CD's).
type LabelSelector struct {
	MatchLabels      map[string]string          `json:"matchLabels,omitempty" yaml:"matchLabels,omitempty"`
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitempty" yaml:"matchExpressions,omitempty"`
}

// LabelSelectorRequirement is a condition on one label: In, NotIn, Exists
// or DoesNotExist.
type LabelSelectorRequirement struct {
	Key      string   `json:"key" yaml:"key"`
	Operator string   `json:"operator" yaml:"operator"`
	Values   []string `json:"values,omitempty" yaml:"values,omitempty"`
}

// Matches reports whether labels satisfy the selector (nil matches all).
func (s *LabelSelector) Matches(labels map[string]string) bool {
	if s == nil {
		return true
	}
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, r := range s.MatchExpressions {
		v, ok := labels[r.Key]
		switch r.Operator {
		case "In":
			if !ok || !slices.Contains(r.Values, v) {
				return false
			}
		case "NotIn":
			if ok && slices.Contains(r.Values, v) {
				return false
			}
		case "Exists":
			if !ok {
				return false
			}
		case "DoesNotExist":
			if ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// What an ApplicationSet may do to its Applications (Argo CD's
// applicationsSync policy); the default is everything.
const (
	AppSetSyncCreateOnly   = "create-only"
	AppSetSyncCreateUpdate = "create-update"
	AppSetSyncCreateDelete = "create-delete"
	AppSetSyncAll          = "sync"
)

// ApplicationSetSyncPolicy says how an ApplicationSet manages its
// Applications.
type ApplicationSetSyncPolicy struct {
	// ApplicationsSync limits the changes: create-only, create-update,
	// create-delete, or sync (the default: create, update and delete).
	ApplicationsSync string `json:"applicationsSync,omitempty" yaml:"applicationsSync,omitempty"`
	// PreserveResourcesOnDeletion keeps the jobs and keys of the
	// Applications deleted with the ApplicationSet.
	PreserveResourcesOnDeletion bool `json:"preserveResourcesOnDeletion,omitempty" yaml:"preserveResourcesOnDeletion,omitempty"`
}

// AllowsUpdate reports whether generated Applications are kept as
// generated.
func (p *ApplicationSetSyncPolicy) AllowsUpdate() bool {
	return p == nil || p.ApplicationsSync == "" || p.ApplicationsSync == AppSetSyncAll || p.ApplicationsSync == AppSetSyncCreateUpdate
}

// AllowsDelete reports whether Applications no longer generated are
// deleted.
func (p *ApplicationSetSyncPolicy) AllowsDelete() bool {
	return p == nil || p.ApplicationsSync == "" || p.ApplicationsSync == AppSetSyncAll || p.ApplicationsSync == AppSetSyncCreateDelete
}

// ApplicationSetStatus is the control plane's record of an ApplicationSet.
type ApplicationSetStatus struct {
	Conditions []ApplicationSetCondition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	// Resources are the Applications it generated.
	Resources []ApplicationSetResource `json:"resources,omitempty" yaml:"resources,omitempty"`
	// ReconciledAt is when it last generated its Applications.
	ReconciledAt *time.Time `json:"reconciledAt,omitempty" yaml:"reconciledAt,omitempty"`
}

// Conditions of an ApplicationSet (Argo CD's).
const (
	AppSetConditionErrorOccurred       = "ErrorOccurred"
	AppSetConditionParametersGenerated = "ParametersGenerated"
	AppSetConditionResourcesUpToDate   = "ResourcesUpToDate"
)

// ApplicationSetCondition is a condition: Status "True" or "False".
type ApplicationSetCondition struct {
	Type               string     `json:"type" yaml:"type"`
	Status             string     `json:"status" yaml:"status"`
	Reason             string     `json:"reason,omitempty" yaml:"reason,omitempty"`
	Message            string     `json:"message,omitempty" yaml:"message,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty" yaml:"lastTransitionTime,omitempty"`
}

// ApplicationSetResource is a generated Application and its state.
type ApplicationSetResource struct {
	Name   string           `json:"name" yaml:"name"`
	Status SyncStatusCode   `json:"status,omitempty" yaml:"status,omitempty"`
	Health HealthStatusCode `json:"health,omitempty" yaml:"health,omitempty"`
}

// Condition returns the condition of a type, or nil.
func (s *ApplicationSetStatus) Condition(typ string) *ApplicationSetCondition {
	for i := range s.Conditions {
		if s.Conditions[i].Type == typ {
			return &s.Conditions[i]
		}
	}
	return nil
}

// SetCondition sets a condition, keeping its transition time when its
// status does not change.
func (s *ApplicationSetStatus) SetCondition(c ApplicationSetCondition, now time.Time) {
	if cur := s.Condition(c.Type); cur != nil {
		if cur.Status == c.Status && cur.LastTransitionTime != nil {
			c.LastTransitionTime = cur.LastTransitionTime
		} else {
			c.LastTransitionTime = &now
		}
		*cur = c
		return
	}
	c.LastTransitionTime = &now
	s.Conditions = append(s.Conditions, c)
}

// SetDefaults fills apiVersion and kind.
func (a *ApplicationSet) SetDefaults() {
	a.APIVersion = APIVersion
	a.Kind = KindApplicationSet
}

// DeepCopy copies the ApplicationSet.
func (a *ApplicationSet) DeepCopy() *ApplicationSet {
	b, _ := json.Marshal(a)
	var out ApplicationSet
	_ = json.Unmarshal(b, &out)
	return &out
}

// Project is the project of the generated Applications.
func (a *ApplicationSet) Project() string {
	if a.Spec.Template.Spec.Project == "" {
		return DefaultProject
	}
	return a.Spec.Template.Spec.Project
}

// GitRepos are the repositories its git generators read.
func (a *ApplicationSet) GitRepos() []string {
	var out []string
	var visit func(gens []ApplicationSetGenerator)
	visit = func(gens []ApplicationSetGenerator) {
		for _, g := range gens {
			switch {
			case g.Git != nil:
				if !slices.Contains(out, g.Git.RepoURL) {
					out = append(out, g.Git.RepoURL)
				}
			case g.Matrix != nil:
				visit(g.Matrix.Generators)
			case g.Merge != nil:
				visit(g.Merge.Generators)
			}
		}
	}
	visit(a.Spec.Generators)
	return out
}

var goTemplateOptions = []string{"missingkey=error", "missingkey=zero", "missingkey=default", "missingkey=invalid"}

// Validate checks what can be checked without generating: the generators'
// shape and the template's fixed fields. The templates themselves are
// checked when they are executed.
func (a *ApplicationSet) Validate() error {
	if err := ValidateName(a.Metadata.Name); err != nil {
		return err
	}
	var errs []error
	if len(a.Spec.Generators) == 0 {
		errs = append(errs, errors.New("spec.generators: an ApplicationSet needs a generator"))
	}
	for i := range a.Spec.Generators {
		if err := a.Spec.Generators[i].validate(0); err != nil {
			errs = append(errs, fmt.Errorf("spec.generators[%d]: %w", i, err))
		}
	}
	t := a.Spec.Template
	if t.Metadata.Name == "" {
		errs = append(errs, errors.New("spec.template.metadata.name is required (a template such as {{ .name }}-web)"))
	}
	if strings.Contains(t.Spec.Project, "{{") {
		errs = append(errs, errors.New("spec.template.spec.project cannot be a template: an ApplicationSet generates Applications of one project"))
	}
	if t.Metadata.Labels[AppSetLabelKey] != "" {
		errs = append(errs, fmt.Errorf("spec.template.metadata.labels: %s is the ApplicationSet's", AppSetLabelKey))
	}
	if p := a.Spec.SyncPolicy; p != nil {
		switch p.ApplicationsSync {
		case "", AppSetSyncCreateOnly, AppSetSyncCreateUpdate, AppSetSyncCreateDelete, AppSetSyncAll:
		default:
			errs = append(errs, fmt.Errorf("spec.syncPolicy.applicationsSync %q is none of create-only, create-update, create-delete, sync", p.ApplicationsSync))
		}
	}
	for _, o := range a.Spec.GoTemplateOptions {
		if !slices.Contains(goTemplateOptions, o) {
			errs = append(errs, fmt.Errorf("spec.goTemplateOptions: %q is none of %s", o, strings.Join(goTemplateOptions, ", ")))
		}
	}
	return errors.Join(errs...)
}

func (g *ApplicationSetGenerator) validate(depth int) error {
	n := 0
	for _, set := range []bool{g.List != nil, g.Clusters != nil, g.Git != nil, g.Matrix != nil, g.Merge != nil} {
		if set {
			n++
		}
	}
	if n != 1 {
		return errors.New("a generator is exactly one of list, clusters, git, matrix, merge")
	}
	nested := func(kind string, gens []ApplicationSetGenerator) error {
		if depth >= 1 {
			return fmt.Errorf("%s: a %s cannot nest another matrix or merge more than once", kind, kind)
		}
		for i := range gens {
			if err := gens[i].validate(depth + 1); err != nil {
				return fmt.Errorf("%s.generators[%d]: %w", kind, i, err)
			}
		}
		return nil
	}
	switch {
	case g.Git != nil:
		if g.Git.RepoURL == "" {
			return errors.New("git.repoURL is required")
		}
		if (len(g.Git.Directories) == 0) == (len(g.Git.Files) == 0) {
			return errors.New("git: give directories or files, not both")
		}
		for _, d := range g.Git.Directories {
			if d.Path == "" {
				return errors.New("git.directories: a path is required")
			}
		}
		for _, f := range g.Git.Files {
			if f.Path == "" {
				return errors.New("git.files: a path is required")
			}
		}
	case g.Matrix != nil:
		if len(g.Matrix.Generators) != 2 {
			return errors.New("matrix: combine exactly two generators")
		}
		return nested("matrix", g.Matrix.Generators)
	case g.Merge != nil:
		if len(g.Merge.Generators) < 2 {
			return errors.New("merge: merge two generators or more")
		}
		if len(g.Merge.MergeKeys) == 0 {
			return errors.New("merge: mergeKeys are required")
		}
		return nested("merge", g.Merge.Generators)
	case g.Clusters != nil && g.Clusters.Selector != nil:
		for _, r := range g.Clusters.Selector.MatchExpressions {
			switch r.Operator {
			case "In", "NotIn", "Exists", "DoesNotExist":
			default:
				return fmt.Errorf("clusters.selector: operator %q is none of In, NotIn, Exists, DoesNotExist", r.Operator)
			}
		}
	}
	return nil
}
