// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func withIdentity(r *http.Request, id *auth.Identity) context.Context {
	return context.WithValue(r.Context(), identityKey{}, id)
}

// ApplicationList is the list response.
type ApplicationList struct {
	Items []*v1.Application `json:"items"`
}

// listFilter selects Applications from query parameters: search (name
// substring), projects (comma list), selector (key=value labels).
func listFilter(r *http.Request) func(*v1.Application) bool {
	q := r.URL.Query()
	search := strings.ToLower(q.Get("search"))
	projects := map[string]bool{}
	for _, p := range strings.Split(q.Get("projects"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			projects[p] = true
		}
	}
	labels := map[string]string{}
	for _, sel := range strings.Split(q.Get("selector"), ",") {
		if k, v, ok := strings.Cut(strings.TrimSpace(sel), "="); ok {
			labels[k] = v
		}
	}
	name := q.Get("name")
	return func(a *v1.Application) bool {
		if name != "" && a.Metadata.Name != name {
			return false
		}
		if search != "" && !strings.Contains(strings.ToLower(a.Metadata.Name), search) {
			return false
		}
		if len(projects) > 0 && !projects[a.Spec.Project] {
			return false
		}
		for k, v := range labels {
			if a.Metadata.Labels[k] != v {
				return false
			}
		}
		return true
	}
}

func (s *Server) visible(r *http.Request, a *v1.Application) bool {
	id := identity(r)
	return s.RBAC.Enforce(id.Username, id.Groups, "applications", "get", appObject(a))
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.Store.ListApplications(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	match := listFilter(r)
	out := []*v1.Application{}
	for _, a := range apps {
		if match(a) && s.visible(r, a) {
			out = append(out, a)
		}
	}
	writeJSON(w, http.StatusOK, ApplicationList{Items: out})
}

// getApp loads an Application and checks the caller may perform action.
func (s *Server) getApp(w http.ResponseWriter, r *http.Request, action string) *v1.Application {
	name := r.PathValue("name")
	app, err := s.Store.GetApplication(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Same answer whether it does not exist or is not visible.
			writeError(w, http.StatusNotFound, fmt.Sprintf("application %q not found", name))
			return nil
		}
		s.fail(w, err)
		return nil
	}
	if !s.visible(r, app) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("application %q not found", name))
		return nil
	}
	if action != "get" && !s.can(w, r, "applications", action, appObject(app)) {
		return nil
	}
	return app
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	if refresh := r.URL.Query().Get("refresh"); refresh == "normal" || refresh == "hard" {
		updated, err := s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
			if a.Metadata.Annotations == nil {
				a.Metadata.Annotations = map[string]string{}
			}
			a.Metadata.Annotations[controller.RefreshAnnotation] = refresh
			return nil
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		if refresh == "hard" && s.Repos != nil {
			s.Repos.InvalidateRevision(app.Spec.Source.RepoURL)
		}
		app = updated
	}
	writeJSON(w, http.StatusOK, app)
}

// checkProject validates the spec against its project.
func (s *Server) checkProject(ctx context.Context, app *v1.Application) error {
	proj, err := s.Store.GetProject(ctx, app.Spec.Project)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return badRequest("project %q does not exist", app.Spec.Project)
		}
		return err
	}
	if !proj.IsSourcePermitted(app.Spec.Source.RepoURL) {
		return badRequest("application repository %s is not permitted in project %q", app.Spec.Source.RepoURL, proj.Metadata.Name)
	}
	if !proj.IsDestinationPermitted(app.Spec.Destination) {
		return badRequest("application destination %s/%s/%s is not permitted in project %q",
			app.Spec.Destination.ClusterName(), app.Spec.Destination.Region, app.Spec.Destination.NamespaceOrDefault(), proj.Metadata.Name)
	}
	if _, err := s.Clusters.Get(app.Spec.Destination.Name); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var app v1.Application
	if err := decode(r, &app); err != nil {
		s.fail(w, err)
		return
	}
	app.SetDefaults()
	app.Status = v1.ApplicationStatus{}
	app.Operation = nil
	app.Metadata.DeletionTimestamp = nil
	if err := app.Validate(); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if !s.can(w, r, "applications", "create", appObject(&app)) {
		return
	}
	if err := s.checkProject(r.Context(), &app); err != nil {
		s.fail(w, err)
		return
	}
	created, err := s.Store.CreateApplication(r.Context(), &app)
	if errors.Is(err, store.ErrExists) && r.URL.Query().Get("upsert") == "true" {
		if !s.can(w, r, "applications", "update", appObject(&app)) {
			return
		}
		created, err = s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
			a.Spec = app.Spec
			a.Metadata.Labels, a.Metadata.Annotations = app.Metadata.Labels, app.Metadata.Annotations
			return nil
		})
	}
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			s.fail(w, fmt.Errorf("%w: application %q already exists", store.ErrExists, app.Metadata.Name))
			return
		}
		s.fail(w, err)
		return
	}
	s.audit(r, "applications/create", appObject(created), "ALLOWED", "")
	s.event(r, created, "ResourceCreated", "created application")
	writeJSON(w, http.StatusOK, created)
}

func (s *Server) handleUpdateApp(w http.ResponseWriter, r *http.Request) {
	cur := s.getApp(w, r, "update")
	if cur == nil {
		return
	}
	var in v1.Application
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	in.Metadata.Name = cur.Metadata.Name
	in.SetDefaults()
	if err := in.Validate(); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if in.Spec.Project != cur.Spec.Project && !s.can(w, r, "applications", "update", appObject(&in)) {
		return
	}
	if err := s.checkProject(r.Context(), &in); err != nil {
		s.fail(w, err)
		return
	}
	update := func(a *v1.Application) {
		a.Spec = in.Spec
		a.Metadata.Labels = in.Metadata.Labels
		a.Metadata.Annotations = in.Metadata.Annotations
	}
	var out *v1.Application
	var err error
	if in.Metadata.ResourceVersion != "" {
		// Optimistic concurrency: the caller edited a specific version.
		next := cur.DeepCopy()
		next.Metadata.ResourceVersion = in.Metadata.ResourceVersion
		update(next)
		out, err = s.Store.UpdateApplication(r.Context(), next)
	} else {
		out, err = s.Store.MutateApplication(r.Context(), cur.Metadata.Name, func(a *v1.Application) error { update(a); return nil })
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "applications/update", appObject(out), "ALLOWED", "")
	s.event(r, out, "ResourceUpdated", "updated application spec")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "delete")
	if app == nil {
		return
	}
	cascade := r.URL.Query().Get("cascade") != "false"
	if !cascade {
		if err := s.Store.DeleteApplication(r.Context(), app.Metadata.Name); err != nil {
			s.fail(w, err)
			return
		}
		s.audit(r, "applications/delete", appObject(app), "ALLOWED", "non-cascading: jobs and keys are left running")
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	_, err := s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
		now := time.Now().UTC().Truncate(time.Second)
		a.Metadata.DeletionTimestamp = &now
		if !a.Metadata.HasFinalizer(v1.ResourcesFinalizer) {
			a.Metadata.Finalizers = append(a.Metadata.Finalizers, v1.ResourcesFinalizer)
		}
		return nil
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "applications/delete", appObject(app), "ALLOWED", "cascade")
	writeJSON(w, http.StatusOK, map[string]any{})
}

// SyncRequest is the sync body (Argo CD's ApplicationSyncRequest).
type SyncRequest struct {
	Revision    string                     `json:"revision"`
	Prune       bool                       `json:"prune"`
	DryRun      bool                       `json:"dryRun"`
	Resources   []v1.SyncOperationResource `json:"resources"`
	SyncOptions []string                   `json:"syncOptions"`
	Retry       *v1.RetryStrategy          `json:"retryStrategy"`
	Infos       []v1.Info                  `json:"infos,omitempty"`
	// Source syncs to a different source once (needs "override").
	Source *v1.ApplicationSource `json:"source,omitempty"`
}

func (s *Server) requestOperation(w http.ResponseWriter, r *http.Request, app *v1.Application, op *v1.Operation, describe string) {
	updated, err := s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
		if a.Operation != nil {
			return fmt.Errorf("%w: another operation is already in progress", store.ErrConflict)
		}
		if a.Metadata.DeletionTimestamp != nil {
			return badRequest("application is being deleted")
		}
		a.Operation = op
		return nil
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	// The controller records OperationStarted when it picks the request up.
	s.audit(r, "applications/sync", appObject(app), "ALLOWED", describe)
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "sync")
	if app == nil {
		return
	}
	var req SyncRequest
	if r.ContentLength != 0 {
		if err := decode(r, &req); err != nil {
			s.fail(w, err)
			return
		}
	}
	if req.Source != nil && !s.can(w, r, "applications", "override", appObject(app)) {
		return
	}
	id := identity(r)
	op := &v1.Operation{
		Sync: &v1.SyncOperation{
			Revision: req.Revision, Prune: req.Prune, DryRun: req.DryRun,
			Resources: req.Resources, SyncOptions: req.SyncOptions, Source: req.Source,
		},
		InitiatedBy: v1.OperationInitiator{Username: id.Username},
		Retry:       req.Retry,
		Info:        req.Infos,
	}
	rev := req.Revision
	if rev == "" {
		rev = app.Spec.Source.Revision()
	}
	s.requestOperation(w, r, app, op, fmt.Sprintf("%s initiated sync to %s", id.Username, rev))
}

// RollbackRequest is the rollback body.
type RollbackRequest struct {
	ID     int64 `json:"id"`
	Prune  bool  `json:"prune"`
	DryRun bool  `json:"dryRun"`
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "sync")
	if app == nil {
		return
	}
	var req RollbackRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if app.Spec.SyncPolicy.IsAutomated() {
		writeError(w, http.StatusBadRequest, "rollback cannot be initiated when auto-sync is enabled")
		return
	}
	found := false
	for _, h := range app.Status.History {
		found = found || h.ID == req.ID
	}
	if !found {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("history entry %d not found", req.ID))
		return
	}
	id := identity(r)
	op := &v1.Operation{
		Sync:        &v1.SyncOperation{RollbackID: &req.ID, Prune: req.Prune, DryRun: req.DryRun},
		InitiatedBy: v1.OperationInitiator{Username: id.Username},
	}
	s.requestOperation(w, r, app, op, fmt.Sprintf("%s initiated rollback to %d", id.Username, req.ID))
}

func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "sync")
	if app == nil {
		return
	}
	updated, err := s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
		os := a.Status.OperationState
		switch {
		case os != nil && os.Phase == v1.OperationRunning:
			os.Phase = v1.OperationTerminating
		case a.Operation != nil:
			a.Operation = nil
		default:
			return badRequest("no operation is in progress")
		}
		return nil
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "applications/terminate-op", appObject(app), "ALLOWED", "")
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	events, err := s.Store.ListEvents(r.Context(), app.Metadata.Name)
	if err != nil {
		s.fail(w, err)
		return
	}
	kind, name := r.URL.Query().Get("resourceKind"), r.URL.Query().Get("resourceName")
	out := []v1.Event{}
	for _, e := range events {
		if (kind == "" || e.Kind == kind) && (name == "" || e.Name == name) {
			out = append(out, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) event(r *http.Request, app *v1.Application, reason, message string) {
	id := identity(r)
	_ = s.Store.RecordEvent(r.Context(), app.Metadata.Name, v1.Event{
		Type: "Normal", Reason: reason, Message: message, Username: id.Username,
		Kind: v1.KindApplication, Name: app.Metadata.Name,
	})
}

func (s *Server) handleRevisionMetadata(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	md, err := s.Repos.Commit(r.Context(), app.Spec.Source.RepoURL, r.PathValue("revision"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, md)
}

// ManifestResponse lists rendered manifests (YAML) at a revision.
type ManifestResponse struct {
	Manifests  []string      `json:"manifests"`
	Revision   string        `json:"revision"`
	SourceType v1.SourceType `json:"sourceType"`
}

func (s *Server) handleManifests(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	rev := r.URL.Query().Get("revision")
	if rev == "" {
		rev = app.Spec.Source.Revision()
	}
	sha, err := s.Repos.Resolve(r.Context(), app.Spec.Source.RepoURL, rev)
	if err != nil {
		s.fail(w, err)
		return
	}
	m, err := s.Repos.Manifests(r.Context(), app, app.Spec.Source, sha, false)
	if err != nil {
		s.fail(w, err)
		return
	}
	out := ManifestResponse{Revision: sha, SourceType: m.SourceType, Manifests: []string{}}
	for _, j := range m.Jobs {
		y, err := jobYAML(j)
		if err != nil {
			s.fail(w, err)
			return
		}
		out.Manifests = append(out.Manifests, y)
	}
	for _, kv := range m.KV {
		out.Manifests = append(out.Manifests, fmt.Sprintf("# Consul key %s\n%s", kv.Key, kv.Value))
	}
	writeJSON(w, http.StatusOK, out)
}
