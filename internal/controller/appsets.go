// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// ApplicationSets (Argo CD's): the leader generates a set's Applications
// when the set changes, when one of its Applications changes or goes, and
// every reconcile interval (Git moves); it creates, updates and deletes
// them as the set's sync policy allows. Its Applications carry its label
// (v1.AppSetLabelKey); it leaves others alone.

// runAppSets reconciles ApplicationSets until ctx ends.
func (c *Controller) runAppSets(ctx context.Context, q *workQueue) {
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			for {
				name, _, ok := q.get()
				if !ok {
					return
				}
				c.processAppSet(ctx, name)
				q.done(name)
			}
		})
	}
	go func() {
		for name := range c.store.WatchApplicationSets(ctx) {
			q.add(name, false)
		}
	}()
	ticker := time.NewTicker(c.cfg.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			q.shutdown()
			wg.Wait()
			return
		case <-ticker.C:
			sets, err := c.store.ListApplicationSets(ctx)
			if err != nil {
				c.logger.Warn("list applicationsets", "error", err)
				continue
			}
			for _, s := range sets {
				q.add(s.Metadata.Name, false)
			}
		}
	}
}

// RefreshAppSet asks for an ApplicationSet's reconciliation (the API, in
// the leader's process).
func (c *Controller) RefreshAppSet(name string) {
	if q := c.setQueue; q != nil {
		q.add(name, false)
	}
}

// appFingerprint is what an ApplicationSet controls of an Application: a
// change of it calls for the set's reconciliation.
func appFingerprint(a *v1.Application) string {
	b, _ := json.Marshal(struct {
		Gen         int64
		Labels      map[string]string
		Annotations map[string]string
		Finalizers  []string
		Deleting    bool
	}{a.Metadata.Generation, a.Metadata.Labels, a.Metadata.Annotations, a.Metadata.Finalizers, a.Metadata.DeletionTimestamp != nil})
	return string(b)
}

// ownedApps are the Applications of an ApplicationSet.
func (c *Controller) ownedApps(ctx context.Context, set string) (map[string]*v1.Application, error) {
	all, err := c.store.ListApplications(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]*v1.Application{}
	for _, a := range all {
		if a.Metadata.Labels[v1.AppSetLabelKey] == set {
			out[a.Metadata.Name] = a
		}
	}
	return out, nil
}

// generatedInSync reports whether an Application is as its set generates
// it: spec, labels, annotations (a pending refresh request aside) and
// finalizers.
func generatedInSync(want, got *v1.Application) bool {
	a, _ := json.Marshal(want.Spec)
	b, _ := json.Marshal(got.Spec)
	if string(a) != string(b) || !maps.Equal(want.Metadata.Labels, got.Metadata.Labels) {
		return false
	}
	gotAnn := maps.Clone(got.Metadata.Annotations)
	delete(gotAnn, RefreshAnnotation)
	if len(gotAnn) != len(want.Metadata.Annotations) || (len(gotAnn) > 0 && !maps.Equal(gotAnn, want.Metadata.Annotations)) {
		return false
	}
	wf, gf := slices.Clone(want.Metadata.Finalizers), slices.Clone(got.Metadata.Finalizers)
	slices.Sort(wf)
	slices.Sort(gf)
	return slices.Equal(wf, gf)
}

// processAppSet is one reconciliation of an ApplicationSet.
func (c *Controller) processAppSet(ctx context.Context, name string) {
	set, err := c.store.GetApplicationSet(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return
	}
	if err != nil {
		c.logger.Warn("get applicationset", "name", name, "error", err)
		return
	}
	if set.Metadata.DeletionTimestamp != nil {
		return // the API deletes it and its Applications
	}
	now := c.now().UTC().Truncate(time.Second)
	apps, genErr := c.appsets.Applications(ctx, set)
	if genErr != nil {
		c.logger.Info("applicationset generation failed", "name", name, "error", genErr)
		c.setAppSetStatus(ctx, name, now, func(st *v1.ApplicationSetStatus) {
			st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionErrorOccurred, Status: "True",
				Reason: "ApplicationGenerationFromParamsError", Message: genErr.Error()}, now)
			st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionParametersGenerated, Status: "False",
				Reason: "ErrorOccurred", Message: genErr.Error()}, now)
			st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionResourcesUpToDate, Status: "False",
				Reason: "ErrorOccurred", Message: "Applications were not generated"}, now)
		})
		return
	}

	owned, err := c.ownedApps(ctx, name)
	if err != nil {
		c.logger.Warn("list applications", "error", err)
		return
	}
	policy := set.Spec.SyncPolicy
	var errs []string
	wanted := map[string]bool{}
	for _, want := range apps {
		wanted[want.Metadata.Name] = true
		if err := c.applyGenerated(ctx, set, want, owned[want.Metadata.Name], policy); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if policy.AllowsDelete() {
		for _, n := range slices.Sorted(maps.Keys(owned)) {
			if wanted[n] || owned[n].Metadata.DeletionTimestamp != nil {
				continue
			}
			if err := c.deleteApp(ctx, n); err != nil {
				errs = append(errs, fmt.Sprintf("delete application %s: %v", n, err))
				continue
			}
			c.recordEvent(ctx, owned[n], "Normal", "ResourceDeleted", "deleted by ApplicationSet "+name+": no longer generated", "")
		}
	}

	var resources []v1.ApplicationSetResource
	for _, want := range apps {
		r := v1.ApplicationSetResource{Name: want.Metadata.Name}
		if live, err := c.store.GetApplication(ctx, want.Metadata.Name); err == nil && live.Metadata.Labels[v1.AppSetLabelKey] == name {
			r.Status, r.Health = live.Status.Sync.Status, live.Status.Health.Status
		}
		resources = append(resources, r)
	}
	sort.Slice(resources, func(i, j int) bool { return resources[i].Name < resources[j].Name })
	c.setAppSetStatus(ctx, name, now, func(st *v1.ApplicationSetStatus) {
		st.Resources = resources
		st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionParametersGenerated, Status: "True",
			Reason: "ParametersGenerated", Message: fmt.Sprintf("Successfully generated parameters for %d applications", len(apps))}, now)
		if len(errs) > 0 {
			msg := strings.Join(errs, "; ")
			st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionErrorOccurred, Status: "True", Reason: "ApplicationSetError", Message: msg}, now)
			st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionResourcesUpToDate, Status: "False", Reason: "ApplicationSetError", Message: msg}, now)
			return
		}
		st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionErrorOccurred, Status: "False", Reason: "ApplicationSetUpToDate"}, now)
		st.SetCondition(v1.ApplicationSetCondition{Type: v1.AppSetConditionResourcesUpToDate, Status: "True",
			Reason: "ApplicationSetUpToDate", Message: "All applications have been generated successfully"}, now)
	})
}

// applyGenerated creates or updates one generated Application.
func (c *Controller) applyGenerated(ctx context.Context, set *v1.ApplicationSet, want, owned *v1.Application, policy *v1.ApplicationSetSyncPolicy) error {
	name := want.Metadata.Name
	proj, err := c.store.GetProject(ctx, want.Spec.Project)
	if err != nil {
		return fmt.Errorf("application %s: project %q: %v", name, want.Spec.Project, err)
	}
	if err := proj.Permits(want); err != nil {
		return fmt.Errorf("application %s: %v", name, err)
	}
	if _, err := c.clusters.Get(want.Spec.Destination.Name); err != nil {
		return fmt.Errorf("application %s: %v", name, err)
	}
	if owned == nil {
		created, err := c.store.CreateApplication(ctx, want)
		if errors.Is(err, store.ErrExists) {
			live, gerr := c.store.GetApplication(ctx, name)
			owner := ""
			if gerr == nil {
				owner = live.Metadata.Labels[v1.AppSetLabelKey]
			}
			if owner == "" {
				return fmt.Errorf("application %s exists and is not this ApplicationSet's", name)
			}
			return fmt.Errorf("application %s belongs to ApplicationSet %q", name, owner)
		}
		if err != nil {
			return fmt.Errorf("create application %s: %v", name, err)
		}
		c.recordEvent(ctx, created, "Normal", "ResourceCreated", "created by ApplicationSet "+set.Metadata.Name, "")
		return nil
	}
	if !policy.AllowsUpdate() || owned.Metadata.DeletionTimestamp != nil || generatedInSync(want, owned) {
		return nil
	}
	updated, err := c.store.MutateApplication(ctx, name, func(a *v1.Application) error {
		if a.Metadata.Labels[v1.AppSetLabelKey] != set.Metadata.Name {
			return fmt.Errorf("application %s is no longer this ApplicationSet's", name)
		}
		refresh, pending := a.Metadata.Annotations[RefreshAnnotation]
		a.Spec = want.Spec
		a.Metadata.Labels = maps.Clone(want.Metadata.Labels)
		a.Metadata.Annotations = maps.Clone(want.Metadata.Annotations)
		if pending {
			if a.Metadata.Annotations == nil {
				a.Metadata.Annotations = map[string]string{}
			}
			a.Metadata.Annotations[RefreshAnnotation] = refresh
		}
		a.Metadata.Finalizers = slices.Clone(want.Metadata.Finalizers)
		return nil
	})
	if err != nil {
		return fmt.Errorf("update application %s: %v", name, err)
	}
	c.recordEvent(ctx, updated, "Normal", "ResourceUpdated", "updated by ApplicationSet "+set.Metadata.Name, "")
	return nil
}

// setAppSetStatus records an ApplicationSet's status, writing only when it
// changed.
func (c *Controller) setAppSetStatus(ctx context.Context, name string, now time.Time, fn func(*v1.ApplicationSetStatus)) {
	_, err := c.store.MutateApplicationSet(ctx, name, func(s *v1.ApplicationSet) error {
		before, _ := json.Marshal(s.Status)
		fn(&s.Status)
		after, _ := json.Marshal(s.Status)
		if string(before) == string(after) && s.Status.ReconciledAt != nil && now.Sub(*s.Status.ReconciledAt) < c.cfg.ReconcileInterval {
			return errUnchanged
		}
		s.Status.ReconciledAt = &now
		return nil
	})
	if err != nil && !errors.Is(err, errUnchanged) && !errors.Is(err, store.ErrNotFound) {
		c.logger.Warn("record applicationset status", "name", name, "error", err)
	}
}

var errUnchanged = errors.New("unchanged")
