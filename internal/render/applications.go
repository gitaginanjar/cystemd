// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"fmt"
	"sort"
	"strings"

	"github.com/gitaginanjar/cystemd/internal/manifest"
	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// defaultApplicationPatterns select manifests when ApplicationsSource.Files
// is empty.
var defaultApplicationPatterns = []string{"*.yaml", "*.yml"}

// renderApplications reads the Application manifests of an app of apps.
// Each declared Application is labelled with its parent (AppMetaKey).
func (r *Renderer) renderApplications(app *v1alpha1.Application, dir string) (*Manifests, error) {
	patterns := app.Spec.Source.Applications.Files
	if len(patterns) == 0 {
		patterns = defaultApplicationPatterns
	}
	files, err := matchFiles(dir, patterns)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Application manifest matching %s in %q", strings.Join(patterns, ", "), app.Spec.Source.Path)
	}
	m := &Manifests{SourceType: v1alpha1.SourceTypeApplications}
	seen := map[string]string{}
	for _, f := range files {
		b, err := readInside(dir, f)
		if err != nil {
			return nil, err
		}
		objs, err := manifest.Decode(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		for _, o := range objs {
			child := o.Application
			if child == nil {
				return nil, fmt.Errorf("%s: %s %s: an application declares Applications only", f, o.Kind, o.Name())
			}
			child.SetDefaults()
			child.Metadata.ResourceVersion, child.Metadata.Generation = "", 0
			child.Metadata.CreationTimestamp, child.Metadata.DeletionTimestamp = nil, nil
			child.Status, child.Operation = v1alpha1.ApplicationStatus{}, nil
			if err := child.Validate(); err != nil {
				return nil, fmt.Errorf("%s: application %s: %w", f, child.Metadata.Name, err)
			}
			name := child.Metadata.Name
			if name == app.Metadata.Name {
				return nil, fmt.Errorf("%s: the application declares itself", f)
			}
			if prev, dup := seen[name]; dup {
				return nil, fmt.Errorf("application %s is declared twice (%s and %s)", name, prev, f)
			}
			seen[name] = f
			if child.Metadata.Labels == nil {
				child.Metadata.Labels = map[string]string{}
			}
			child.Metadata.Labels[v1alpha1.AppMetaKey] = app.Metadata.Name
			m.Applications = append(m.Applications, child)
		}
	}
	sort.Slice(m.Applications, func(i, j int) bool { return m.Applications[i].Metadata.Name < m.Applications[j].Metadata.Name })
	return m, nil
}
