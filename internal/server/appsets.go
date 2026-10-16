// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gitaginanjar/cystemd/internal/controller"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// ApplicationSets: RBAC's resource applicationsets, objects
// <project>/<name> (the project of the Applications it generates).

func appSetObject(set *v1.ApplicationSet) string { return set.Project() + "/" + set.Metadata.Name }

// listAppSets returns the ApplicationSets the caller may see.
func (s *Server) listAppSets(r *http.Request) ([]*v1.ApplicationSet, error) {
	sets, err := s.Store.ListApplicationSets(r.Context())
	if err != nil {
		return nil, err
	}
	out := []*v1.ApplicationSet{}
	for _, set := range sets {
		if s.enforce(r, "applicationsets", "get", appSetObject(set)) {
			out = append(out, set)
		}
	}
	return out, nil
}

func (s *Server) handleListAppSets(w http.ResponseWriter, r *http.Request) {
	out, err := s.listAppSets(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// getAppSet returns an ApplicationSet the caller may act on; a set it may
// not see is not found.
func (s *Server) getAppSet(r *http.Request, name, action string) (*v1.ApplicationSet, error) {
	set, err := s.Store.GetApplicationSet(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound("applicationset %s not found", name)
	}
	if err != nil {
		return nil, err
	}
	if !s.enforce(r, "applicationsets", "get", appSetObject(set)) {
		return nil, notFound("applicationset %s not found", name)
	}
	if action != "get" {
		if err := s.authorize(r, "applicationsets", action, appSetObject(set)); err != nil {
			return nil, err
		}
	}
	return set, nil
}

func (s *Server) handleGetAppSet(w http.ResponseWriter, r *http.Request) {
	set, err := s.getAppSet(r, r.PathValue("name"), "get")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// checkAppSet validates an ApplicationSet and its project.
func (s *Server) checkAppSet(ctx context.Context, set *v1.ApplicationSet) error {
	set.SetDefaults()
	if err := set.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if _, err := s.Store.GetProject(ctx, set.Project()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return badRequest("project %q does not exist", set.Project())
		}
		return err
	}
	return nil
}

// createAppSet creates an ApplicationSet, or with upsert replaces it.
func (s *Server) createAppSet(r *http.Request, set *v1.ApplicationSet, upsert bool) (*v1.ApplicationSet, error) {
	if err := s.checkAppSet(r.Context(), set); err != nil {
		return nil, err
	}
	if err := s.authorize(r, "applicationsets", "create", appSetObject(set)); err != nil {
		return nil, err
	}
	created, err := s.Store.CreateApplicationSet(r.Context(), set)
	if errors.Is(err, store.ErrExists) {
		if !upsert {
			return nil, statusErr(http.StatusConflict, "applicationset %s already exists", set.Metadata.Name)
		}
		cur, err := s.getAppSet(r, set.Metadata.Name, "update")
		if err != nil {
			return nil, err
		}
		return s.updateAppSet(r, cur, set)
	}
	if err != nil {
		return nil, err
	}
	s.audit(r, "applicationsets/create", appSetObject(created), "ALLOWED", "")
	return created, nil
}

func (s *Server) handleCreateAppSet(w http.ResponseWriter, r *http.Request) {
	var set v1.ApplicationSet
	if err := decode(r, &set); err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.createAppSet(r, &set, r.URL.Query().Get("upsert") == "true")
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// updateAppSet replaces an ApplicationSet's spec, labels and annotations.
func (s *Server) updateAppSet(r *http.Request, cur, in *v1.ApplicationSet) (*v1.ApplicationSet, error) {
	in.Metadata.Name = cur.Metadata.Name
	if err := s.checkAppSet(r.Context(), in); err != nil {
		return nil, err
	}
	if in.Project() != cur.Project() {
		if err := s.authorize(r, "applicationsets", "update", appSetObject(in)); err != nil {
			return nil, err
		}
	}
	if cur.Metadata.DeletionTimestamp != nil {
		return nil, statusErr(http.StatusConflict, "applicationset %s is being deleted", cur.Metadata.Name)
	}
	out, err := s.Store.UpdateApplicationSet(r.Context(), in)
	if err != nil {
		return nil, err
	}
	s.audit(r, "applicationsets/update", appSetObject(out), "ALLOWED", "")
	return out, nil
}

func (s *Server) handleUpdateAppSet(w http.ResponseWriter, r *http.Request) {
	cur, err := s.getAppSet(r, r.PathValue("name"), "update")
	if err != nil {
		s.fail(w, err)
		return
	}
	var in v1.ApplicationSet
	if err := decode(r, &in); err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.updateAppSet(r, cur, &in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteAppSet deletes an ApplicationSet and, with cascade, its
// Applications: with their jobs and keys, unless the set preserves them.
// Without cascade, its Applications stay, no longer its own.
func (s *Server) deleteAppSet(r *http.Request, set *v1.ApplicationSet, cascade bool) error {
	ctx := r.Context()
	name := set.Metadata.Name
	// Marked first, so that the controller stops generating.
	if _, err := s.Store.MutateApplicationSet(ctx, name, func(a *v1.ApplicationSet) error {
		if a.Metadata.DeletionTimestamp == nil {
			now := time.Now().UTC().Truncate(time.Second)
			a.Metadata.DeletionTimestamp = &now
		}
		return nil
	}); err != nil {
		return err
	}
	apps, err := s.Store.ListApplications(ctx)
	if err != nil {
		return err
	}
	preserve := set.Spec.SyncPolicy != nil && set.Spec.SyncPolicy.PreserveResourcesOnDeletion
	for _, app := range apps {
		if app.Metadata.Labels[v1.AppSetLabelKey] != name {
			continue
		}
		switch {
		case !cascade:
			_, err = s.Store.MutateApplication(ctx, app.Metadata.Name, func(a *v1.Application) error {
				delete(a.Metadata.Labels, v1.AppSetLabelKey)
				return nil
			})
		case preserve:
			err = s.deleteApp(r, app, false)
		default:
			err = s.deleteApp(r, app, true)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
	}
	if err := s.Store.DeleteApplicationSet(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	detail := "cascade"
	if !cascade {
		detail = "non-cascading: its applications are left"
	} else if preserve {
		detail = "cascade, preserving the applications' jobs and keys"
	}
	s.audit(r, "applicationsets/delete", appSetObject(set), "ALLOWED", detail)
	return nil
}

func (s *Server) handleDeleteAppSet(w http.ResponseWriter, r *http.Request) {
	set, err := s.getAppSet(r, r.PathValue("name"), "delete")
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.deleteAppSet(r, set, r.URL.Query().Get("cascade") != "false"); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// refreshAppSet asks the controller to generate an ApplicationSet's
// Applications again now, Git's latest revisions included.
func (s *Server) refreshAppSet(r *http.Request, name string) error {
	if _, err := s.getAppSet(r, name, "get"); err != nil {
		return err
	}
	_, err := s.Store.MutateApplicationSet(r.Context(), name, func(a *v1.ApplicationSet) error {
		if a.Metadata.Annotations == nil {
			a.Metadata.Annotations = map[string]string{}
		}
		a.Metadata.Annotations[controller.RefreshAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		return nil
	})
	return err
}

func (s *Server) handleRefreshAppSet(w http.ResponseWriter, r *http.Request) {
	if err := s.refreshAppSet(r, r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}

// GenerateResponse lists the Applications an ApplicationSet would
// generate (Argo CD's appset generate).
type GenerateResponse struct {
	Applications []*v1.Application `json:"applications"`
}

// generateAppSet previews an ApplicationSet's Applications without
// applying them (needs applicationsets/create: it reads the repositories).
func (s *Server) generateAppSet(r *http.Request, set *v1.ApplicationSet) (*GenerateResponse, error) {
	if err := s.checkAppSet(r.Context(), set); err != nil {
		return nil, err
	}
	if err := s.authorize(r, "applicationsets", "create", appSetObject(set)); err != nil {
		return nil, err
	}
	if s.AppSets == nil {
		return nil, statusErr(http.StatusNotImplemented, "this server does not generate applications")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	apps, err := s.AppSets.Applications(ctx, set)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	if apps == nil {
		apps = []*v1.Application{}
	}
	return &GenerateResponse{Applications: apps}, nil
}

func (s *Server) handleGenerateAppSet(w http.ResponseWriter, r *http.Request) {
	var set v1.ApplicationSet
	if err := decode(r, &set); err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.generateAppSet(r, &set)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}
