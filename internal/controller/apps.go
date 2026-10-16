// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// An app of apps declares Applications in Git (Argo CD's pattern): it
// creates and updates them on sync, prunes those it no longer declares,
// and deletes them when it is deleted with cascade. It tracks them by the
// label cystemd.app. Their health is theirs: it is shown, not added to the
// parent's.

// tracksApps reports whether an Application declares, or declared,
// Applications.
func tracksApps(app *v1.Application) bool {
	if app.Spec.Source.Applications != nil {
		return true
	}
	return slices.ContainsFunc(app.Status.Resources, func(r v1.ResourceStatus) bool { return r.Kind == v1.KindApplication })
}

// childApps are the stored Applications that app tracks.
func (c *Controller) childApps(ctx context.Context, app *v1.Application) ([]*v1.Application, error) {
	all, err := c.store.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	var out []*v1.Application
	for _, a := range all {
		if a.Metadata.Labels[v1.AppMetaKey] == app.Metadata.Name && a.Metadata.Name != app.Metadata.Name {
			out = append(out, a)
		}
	}
	return out, nil
}

// checkAncestry refuses to declare an Application that declares the
// declaring one, directly or not.
func (c *Controller) checkAncestry(ctx context.Context, app *v1.Application, child string) error {
	cur := app
	for range 32 {
		owner := cur.Metadata.Labels[v1.AppMetaKey]
		if owner == "" {
			return nil
		}
		if owner == child {
			return fmt.Errorf("application %s declares %s, directly or not: it cannot be declared by it", child, app.Metadata.Name)
		}
		next, err := c.store.GetApplication(ctx, owner)
		if err != nil {
			return nil
		}
		cur = next
	}
	return errors.New("the chain of applications declaring applications is too deep")
}

// appInSync reports whether a stored Application is as declared: the same
// spec, and the labels, annotations and finalizers the declaration sets.
func appInSync(want, got *v1.Application) bool {
	a, _ := json.Marshal(want.Spec)
	b, _ := json.Marshal(got.Spec)
	if string(a) != string(b) {
		return false
	}
	for k, v := range want.Metadata.Labels {
		if got.Metadata.Labels[k] != v {
			return false
		}
	}
	for k, v := range want.Metadata.Annotations {
		if k != RefreshAnnotation && got.Metadata.Annotations[k] != v {
			return false
		}
	}
	for _, f := range want.Metadata.Finalizers {
		if !got.Metadata.HasFinalizer(f) {
			return false
		}
	}
	return true
}

// compareApps compares the declared Applications with the stored ones.
func (c *Controller) compareApps(ctx context.Context, app *v1.Application, manifests *render.Manifests, cmp *comparison) ([]v1.ResourceStatus, error) {
	if !tracksApps(app) {
		return nil, nil
	}
	cmp.apps = map[string]*v1.Application{}
	var out []v1.ResourceStatus
	for _, want := range manifests.Applications {
		name := want.Metadata.Name
		if !app.Spec.Source.Applications.AllowsProject(app.Spec.Project, want.Spec.Project) {
			return nil, fmt.Errorf("application %s: its project %q is not one this application may declare applications in (spec.source.applications.projects)", name, want.Spec.Project)
		}
		if err := c.checkAncestry(ctx, app, name); err != nil {
			return nil, err
		}
		cmp.apps[name] = want
		rs := v1.ResourceStatus{Kind: v1.KindApplication, Name: name}
		live, err := c.store.GetApplication(ctx, name)
		switch {
		case errors.Is(err, store.ErrNotFound):
			rs.Status = v1.SyncStatusOutOfSync
			rs.Health = &v1.HealthStatus{Status: v1.HealthStatusMissing, Message: "Application does not exist"}
		case err != nil:
			rs.Status = v1.SyncStatusUnknown
			cmp.conditions = append(cmp.conditions, condition(v1.ConditionComparisonError, "application %s: %v", name, err))
		default:
			if owner := live.Metadata.Labels[v1.AppMetaKey]; owner != "" && owner != app.Metadata.Name {
				cmp.conditions = append(cmp.conditions, condition("SharedResourceWarning",
					"application %s is also managed by application %q", name, owner))
			}
			rs.Status = v1.SyncStatusOutOfSync
			if appInSync(want, live) {
				rs.Status = v1.SyncStatusSynced
			}
			h := live.Status.Health
			rs.Health = &h
		}
		out = append(out, rs)
	}
	// Those it declared before and no longer does.
	children, err := c.childApps(ctx, app)
	if err != nil {
		cmp.conditions = append(cmp.conditions, condition(v1.ConditionComparisonError, "list applications: %v", err))
	}
	for _, a := range children {
		if cmp.apps[a.Metadata.Name] != nil || a.Metadata.DeletionTimestamp != nil {
			continue
		}
		h := a.Status.Health
		rs := v1.ResourceStatus{Kind: v1.KindApplication, Name: a.Metadata.Name, Status: v1.SyncStatusOutOfSync, RequiresPruning: true, Health: &h}
		out = append(out, rs)
		cmp.prune = append(cmp.prune, rs)
	}
	return out, nil
}

// syncApp creates or updates one declared Application.
func (c *Controller) syncApp(ctx context.Context, app, want *v1.Application, dryRun bool) (v1.ResultCode, string) {
	name := want.Metadata.Name
	if !app.Spec.Source.Applications.AllowsProject(app.Spec.Project, want.Spec.Project) {
		return v1.ResultSyncFailed, fmt.Sprintf("project %q is not one this application may declare applications in", want.Spec.Project)
	}
	proj, err := c.store.GetProject(ctx, want.Spec.Project)
	if err != nil {
		return v1.ResultSyncFailed, fmt.Sprintf("project %q: %v", want.Spec.Project, err)
	}
	if err := proj.Permits(want); err != nil {
		return v1.ResultSyncFailed, err.Error()
	}
	if _, err := c.clusters.Get(want.Spec.Destination.Name); err != nil {
		return v1.ResultSyncFailed, err.Error()
	}
	if err := c.checkAncestry(ctx, app, name); err != nil {
		return v1.ResultSyncFailed, err.Error()
	}
	live, err := c.store.GetApplication(ctx, name)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if dryRun {
			return v1.ResultSynced, "would be created (dry run)"
		}
		created, err := c.store.CreateApplication(ctx, want)
		if err != nil {
			return v1.ResultSyncFailed, err.Error()
		}
		c.recordEvent(ctx, created, "Normal", "ResourceCreated", "created by application "+app.Metadata.Name, "")
		return v1.ResultSynced, "application created"
	case err != nil:
		return v1.ResultSyncFailed, err.Error()
	}
	if owner := live.Metadata.Labels[v1.AppMetaKey]; owner != "" && owner != app.Metadata.Name {
		return v1.ResultSyncFailed, fmt.Sprintf("application %s is managed by application %q", name, owner)
	}
	if live.Metadata.DeletionTimestamp != nil {
		return v1.ResultSyncFailed, fmt.Sprintf("application %s is being deleted", name)
	}
	if appInSync(want, live) {
		return v1.ResultSynced, "unchanged"
	}
	if dryRun {
		return v1.ResultSynced, "would be updated (dry run)"
	}
	updated, err := c.store.MutateApplication(ctx, name, func(a *v1.Application) error {
		a.Spec = want.Spec
		if a.Metadata.Labels == nil {
			a.Metadata.Labels = map[string]string{}
		}
		for k, v := range want.Metadata.Labels {
			a.Metadata.Labels[k] = v
		}
		if len(want.Metadata.Annotations) > 0 && a.Metadata.Annotations == nil {
			a.Metadata.Annotations = map[string]string{}
		}
		for k, v := range want.Metadata.Annotations {
			// A refresh request is the controller's to clear: declaring one
			// would refresh the Application forever.
			if k != RefreshAnnotation {
				a.Metadata.Annotations[k] = v
			}
		}
		for _, f := range want.Metadata.Finalizers {
			if !a.Metadata.HasFinalizer(f) {
				a.Metadata.Finalizers = append(a.Metadata.Finalizers, f)
			}
		}
		return nil
	})
	if err != nil {
		return v1.ResultSyncFailed, err.Error()
	}
	c.recordEvent(ctx, updated, "Normal", "ResourceUpdated", "updated by application "+app.Metadata.Name, "")
	return v1.ResultSynced, "application updated"
}

// deleteApp deletes a declared Application: its finalizers decide whether
// its jobs and keys go too.
func (c *Controller) deleteApp(ctx context.Context, name string) error {
	_, err := c.store.MutateApplication(ctx, name, func(a *v1.Application) error {
		if a.Metadata.DeletionTimestamp == nil {
			now := c.now().UTC().Truncate(time.Second)
			a.Metadata.DeletionTimestamp = &now
		}
		return nil
	})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	return err
}
