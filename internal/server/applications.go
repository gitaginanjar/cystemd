// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Application operations are shared by the JSON API (handle*) and the web
// UI: each takes the request (context, caller, client address for the
// audit) and returns an error errorCode maps to an HTTP status.

func withIdentity(r *http.Request, id *auth.Identity) context.Context {
	return context.WithValue(r.Context(), identityKey{}, id)
}

// ApplicationList is the list response.
type ApplicationList struct {
	Items []*v1.Application `json:"items"`
}

// listFilter selects Applications from query parameters: search (name
// substring), projects (comma list), selector (key=value labels).
func listFilter(q url.Values) func(*v1.Application) bool {
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

// listApps returns the Applications the caller may see that match.
func (s *Server) listApps(r *http.Request, match func(*v1.Application) bool) ([]*v1.Application, error) {
	apps, err := s.Store.ListApplications(r.Context())
	if err != nil {
		return nil, err
	}
	out := []*v1.Application{}
	for _, a := range apps {
		if (match == nil || match(a)) && s.visible(r, a) {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.listApps(r, listFilter(r.URL.Query()))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ApplicationList{Items: apps})
}

// loadApp returns the named Application when the caller may see it and
// perform action on it. A hidden Application reads as not found.
func (s *Server) loadApp(r *http.Request, name, action string) (*v1.Application, error) {
	app, err := s.Store.GetApplication(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, notFound("application %q not found", name)
		}
		return nil, err
	}
	if !s.visible(r, app) {
		return nil, notFound("application %q not found", name)
	}
	if action != "get" {
		if err := s.authorize(r, "applications", action, appObject(app)); err != nil {
			return nil, err
		}
	}
	return app, nil
}

// getApp is loadApp for JSON handlers: it writes the error itself.
func (s *Server) getApp(w http.ResponseWriter, r *http.Request, action string) *v1.Application {
	app, err := s.loadApp(r, r.PathValue("name"), action)
	if err != nil {
		s.fail(w, err)
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
		updated, err := s.refreshApp(r, app, refresh)
		if err != nil {
			s.fail(w, err)
			return
		}
		app = updated
	}
	writeJSON(w, http.StatusOK, app)
}

// refreshApp asks the controller to re-compare the Application now; a hard
// refresh also drops the cached revision and manifests of its repository.
func (s *Server) refreshApp(r *http.Request, app *v1.Application, mode string) (*v1.Application, error) {
	if mode != "hard" {
		mode = "normal"
	}
	updated, err := s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
		if a.Metadata.Annotations == nil {
			a.Metadata.Annotations = map[string]string{}
		}
		a.Metadata.Annotations[controller.RefreshAnnotation] = mode
		return nil
	})
	if err != nil {
		return nil, err
	}
	if mode == "hard" && s.Repos != nil {
		s.Repos.InvalidateRevision(app.Spec.Source.RepoURL)
	}
	return updated, nil
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
	if err := proj.Permits(app); err != nil {
		return badRequest("%v", err)
	}
	if _, err := s.Clusters.Get(app.Spec.Destination.Name); err != nil {
		return badRequest("%v", err)
	}
	return nil
}

// createApp creates app; with upsert an existing Application's spec,
// labels and annotations are replaced instead.
func (s *Server) createApp(r *http.Request, app *v1.Application, upsert bool) (*v1.Application, error) {
	app.SetDefaults()
	app.Status = v1.ApplicationStatus{}
	app.Operation = nil
	app.Metadata.DeletionTimestamp = nil
	if err := app.Validate(); err != nil {
		return nil, badRequest("%v", err)
	}
	if err := s.authorize(r, "applications", "create", appObject(app)); err != nil {
		return nil, err
	}
	if err := s.checkProject(r.Context(), app); err != nil {
		return nil, err
	}
	created, err := s.Store.CreateApplication(r.Context(), app)
	if errors.Is(err, store.ErrExists) && upsert {
		if err := s.authorize(r, "applications", "update", appObject(app)); err != nil {
			return nil, err
		}
		created, err = s.Store.MutateApplication(r.Context(), app.Metadata.Name, func(a *v1.Application) error {
			a.Spec = app.Spec
			a.Metadata.Labels, a.Metadata.Annotations = app.Metadata.Labels, app.Metadata.Annotations
			return nil
		})
	}
	if err != nil {
		if errors.Is(err, store.ErrExists) {
			return nil, statusErr(http.StatusConflict, "application %q already exists (use upsert to replace its spec)", app.Metadata.Name)
		}
		return nil, err
	}
	s.audit(r, "applications/create", appObject(created), "ALLOWED", "")
	s.event(r, created, "ResourceCreated", "created application")
	return created, nil
}

func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	var app v1.Application
	if err := decode(r, &app); err != nil {
		s.fail(w, err)
		return
	}
	created, err := s.createApp(r, &app, r.URL.Query().Get("upsert") == "true")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, created)
}

// updateApp replaces cur's spec, labels and annotations with in's. A
// resourceVersion on in makes the write conditional on it.
func (s *Server) updateApp(r *http.Request, cur, in *v1.Application) (*v1.Application, error) {
	in.Metadata.Name = cur.Metadata.Name
	in.SetDefaults()
	if err := in.Validate(); err != nil {
		return nil, badRequest("%v", err)
	}
	if in.Spec.Project != cur.Spec.Project {
		if err := s.authorize(r, "applications", "update", appObject(in)); err != nil {
			return nil, err
		}
	}
	if err := s.checkProject(r.Context(), in); err != nil {
		return nil, err
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
		return nil, err
	}
	s.audit(r, "applications/update", appObject(out), "ALLOWED", "")
	s.event(r, out, "ResourceUpdated", "updated application spec")
	return out, nil
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
	out, err := s.updateApp(r, cur, &in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteApp deletes app. Cascading (the default) marks it for deletion:
// the controller deletes its jobs and keys, then the Application.
func (s *Server) deleteApp(r *http.Request, app *v1.Application, cascade bool) error {
	if !cascade {
		if err := s.Store.DeleteApplication(r.Context(), app.Metadata.Name); err != nil {
			return err
		}
		s.audit(r, "applications/delete", appObject(app), "ALLOWED", "non-cascading: jobs and keys are left running")
		return nil
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
		return err
	}
	s.audit(r, "applications/delete", appObject(app), "ALLOWED", "cascade")
	return nil
}

func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "delete")
	if app == nil {
		return
	}
	if err := s.deleteApp(r, app, r.URL.Query().Get("cascade") != "false"); err != nil {
		s.fail(w, err)
		return
	}
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

func (s *Server) requestOperation(r *http.Request, app *v1.Application, op *v1.Operation, describe string) (*v1.Application, error) {
	// The project's sync windows decide (a dry run changes nothing).
	if op.Sync != nil && !op.Sync.DryRun {
		proj, err := s.Store.GetProject(r.Context(), app.Spec.Project)
		if err != nil {
			return nil, err
		}
		if !proj.WindowState(app, time.Now()).CanSync {
			s.audit(r, "applications/sync", appObject(app), "DENIED", "blocked by sync window")
			return nil, forbidden("cannot sync: blocked by sync window")
		}
	}
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
		return nil, err
	}
	// The controller records OperationStarted when it picks the request up.
	s.audit(r, "applications/sync", appObject(app), "ALLOWED", describe)
	return updated, nil
}

// syncApp requests a sync operation (needs "sync" on the Application).
func (s *Server) syncApp(r *http.Request, app *v1.Application, req SyncRequest) (*v1.Application, error) {
	if req.Source != nil {
		if err := s.authorize(r, "applications", "override", appObject(app)); err != nil {
			return nil, err
		}
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
	return s.requestOperation(r, app, op, fmt.Sprintf("%s initiated sync to %s", id.Username, rev))
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
	out, err := s.syncApp(r, app, req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// RollbackRequest is the rollback body.
type RollbackRequest struct {
	ID     int64 `json:"id"`
	Prune  bool  `json:"prune"`
	DryRun bool  `json:"dryRun"`
}

// rollbackApp requests a rollback to a history entry's job versions.
func (s *Server) rollbackApp(r *http.Request, app *v1.Application, req RollbackRequest) (*v1.Application, error) {
	if app.Spec.SyncPolicy.IsAutomated() {
		return nil, badRequest("rollback cannot be initiated when auto-sync is enabled")
	}
	found := false
	for _, h := range app.Status.History {
		found = found || h.ID == req.ID
	}
	if !found {
		return nil, badRequest("history entry %d not found", req.ID)
	}
	id := identity(r)
	op := &v1.Operation{
		Sync:        &v1.SyncOperation{RollbackID: &req.ID, Prune: req.Prune, DryRun: req.DryRun},
		InitiatedBy: v1.OperationInitiator{Username: id.Username},
	}
	return s.requestOperation(r, app, op, fmt.Sprintf("%s initiated rollback to %d", id.Username, req.ID))
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
	out, err := s.rollbackApp(r, app, req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// terminateOperation stops the running operation (or drops a requested one
// the controller has not picked up yet).
func (s *Server) terminateOperation(r *http.Request, app *v1.Application) (*v1.Application, error) {
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
		return nil, err
	}
	s.audit(r, "applications/terminate-op", appObject(app), "ALLOWED", "")
	return updated, nil
}

func (s *Server) handleTerminate(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "sync")
	if app == nil {
		return
	}
	out, err := s.terminateOperation(r, app)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// appEvents lists the Application's events, optionally for one resource.
func (s *Server) appEvents(r *http.Request, app *v1.Application, kind, name string) ([]v1.Event, error) {
	events, err := s.Store.ListEvents(r.Context(), app.Metadata.Name)
	if err != nil {
		return nil, err
	}
	out := []v1.Event{}
	for _, e := range events {
		if (kind == "" || e.Kind == kind) && (name == "" || e.Name == name) {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	out, err := s.appEvents(r, app, r.URL.Query().Get("resourceKind"), r.URL.Query().Get("resourceName"))
	if err != nil {
		s.fail(w, err)
		return
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

// handleSyncWindows is how the project's sync windows stand for the
// Application.
func (s *Server) handleSyncWindows(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	proj, err := s.Store.GetProject(r.Context(), app.Spec.Project)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, proj.WindowState(app, time.Now()))
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

// appManifests renders the Application's source at rev (default: its
// target revision).
func (s *Server) appManifests(r *http.Request, app *v1.Application, rev string) (*ManifestResponse, error) {
	if rev == "" {
		rev = app.Spec.Source.Revision()
	}
	sha, err := s.Repos.Resolve(r.Context(), app.Spec.Source.RepoURL, rev)
	if err != nil {
		return nil, err
	}
	m, err := s.Repos.Manifests(r.Context(), app, app.Spec.Source, sha, false)
	if err != nil {
		return nil, err
	}
	out := &ManifestResponse{Revision: sha, SourceType: m.SourceType, Manifests: []string{}}
	for _, j := range m.Jobs {
		y, err := jobYAML(j)
		if err != nil {
			return nil, err
		}
		out.Manifests = append(out.Manifests, y)
	}
	for _, kv := range m.KV {
		out.Manifests = append(out.Manifests, fmt.Sprintf("# Consul key %s\n%s", kv.Key, kv.Value))
	}
	return out, nil
}

func (s *Server) handleManifests(w http.ResponseWriter, r *http.Request) {
	app := s.getApp(w, r, "get")
	if app == nil {
		return
	}
	out, err := s.appManifests(r, app, r.URL.Query().Get("revision"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
