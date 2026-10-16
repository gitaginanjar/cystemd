// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"testing"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func TestAppInSync(t *testing.T) {
	want := &v1.Application{
		Metadata: v1.ObjectMeta{Name: "web", Labels: map[string]string{v1.AppMetaKey: "platform", "team": "web"},
			Annotations: map[string]string{"owner": "ops", RefreshAnnotation: "hard"}, Finalizers: []string{v1.ResourcesFinalizer}},
		Spec: v1.ApplicationSpec{Project: "default", Source: v1.ApplicationSource{RepoURL: "x", Path: "apps/web"}},
	}
	live := want.DeepCopy()
	// Labels, annotations and finalizers the declaration does not set are
	// not compared, nor is a refresh request.
	live.Metadata.Labels["extra"] = "1"
	live.Metadata.Annotations = map[string]string{"owner": "ops", "note": "by hand"}
	live.Metadata.Finalizers = append(live.Metadata.Finalizers, "other")
	if !appInSync(want, live) {
		t.Fatal("in sync")
	}
	for name, edit := range map[string]func(a *v1.Application){
		"spec":       func(a *v1.Application) { a.Spec.Source.Path = "apps/other" },
		"label":      func(a *v1.Application) { a.Metadata.Labels["team"] = "api" },
		"annotation": func(a *v1.Application) { delete(a.Metadata.Annotations, "owner") },
		"finalizer":  func(a *v1.Application) { a.Metadata.Finalizers = nil },
	} {
		drifted := live.DeepCopy()
		edit(drifted)
		if appInSync(want, drifted) {
			t.Errorf("%s drift not seen", name)
		}
	}
}
