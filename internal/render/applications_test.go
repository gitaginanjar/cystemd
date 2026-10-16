// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"strings"
	"testing"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func appOfApps(name string) *v1alpha1.Application {
	return &v1alpha1.Application{
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec: v1alpha1.ApplicationSpec{Project: "default", Source: v1alpha1.ApplicationSource{
			RepoURL: "https://git.example.com/platform/deploy.git", Path: "applications",
			Applications: &v1alpha1.ApplicationsSource{},
		}},
	}
}

const childManifests = `
apiVersion: cystemd.io/v1alpha1
kind: Application
metadata:
  name: web
  labels: {team: web}
  finalizers: [resources-finalizer.cystemd.io]
spec:
  source: {repoURL: "https://git.example.com/platform/deploy.git", path: apps/web}
---
apiVersion: cystemd.io/v1alpha1
kind: Application
metadata: {name: api}
spec:
  project: default
  source: {repoURL: "https://git.example.com/platform/deploy.git", path: apps/api}
  syncPolicy: {automated: {prune: true}}
`

func TestApplicationsRenderer(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"apps.yaml":  childManifests,
		"README.md":  "not a manifest",
		"extra.yml":  "apiVersion: cystemd.io/v1alpha1\nkind: Application\nmetadata: {name: batch}\nspec: {source: {repoURL: x}}\n",
		"sub/x.yaml": "not read: only the path itself",
	})
	m, err := (&Renderer{}).Render(context.Background(), appOfApps("platform"), dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range m.Applications {
		names = append(names, a.Metadata.Name)
		if a.Metadata.Labels[v1alpha1.AppMetaKey] != "platform" || a.Spec.Project != "default" || a.Kind != v1alpha1.KindApplication {
			t.Fatalf("%s: %+v", a.Metadata.Name, a)
		}
	}
	if strings.Join(names, ",") != "api,batch,web" || len(m.Jobs) != 0 || m.SourceType != v1alpha1.SourceTypeApplications {
		t.Fatalf("applications = %v, jobs = %d, type %s", names, len(m.Jobs), m.SourceType)
	}
	web := m.Applications[2]
	if web.Metadata.Labels["team"] != "web" || !web.Metadata.HasFinalizer(v1alpha1.ResourcesFinalizer) {
		t.Fatalf("web = %+v", web.Metadata)
	}

	for _, tc := range []struct {
		files map[string]string
		want  string
	}{
		{map[string]string{"p.yaml": "apiVersion: cystemd.io/v1alpha1\nkind: AppProject\nmetadata: {name: p}\nspec: {}\n"}, "declares Applications only"},
		{map[string]string{"self.yaml": "apiVersion: cystemd.io/v1alpha1\nkind: Application\nmetadata: {name: platform}\nspec: {source: {repoURL: x}}\n"}, "declares itself"},
		{map[string]string{"a.yaml": childManifests, "b.yaml": childManifests}, "declared twice"},
		{map[string]string{"bad.yaml": "apiVersion: cystemd.io/v1alpha1\nkind: Application\nmetadata: {name: Bad_Name}\nspec: {source: {repoURL: x}}\n"}, "invalid name"},
		{map[string]string{"typo.yaml": "apiVersion: cystemd.io/v1alpha1\nkind: Application\nmetadata: {name: t}\nspec: {sorce: {}}\n"}, "sorce"},
		{map[string]string{"notes.txt": "x"}, "no Application manifest"},
	} {
		dir := writeFiles(t, tc.files)
		if _, err := (&Renderer{}).Render(context.Background(), appOfApps("platform"), dir); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.files, err, tc.want)
		}
	}
}
