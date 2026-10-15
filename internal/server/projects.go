// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// listProjects returns the projects the caller may see.
func (s *Server) listProjects(r *http.Request) ([]*v1.AppProject, error) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		return nil, err
	}
	id := identity(r)
	out := []*v1.AppProject{}
	for _, p := range projects {
		if s.RBAC.Enforce(id.Username, id.Groups, "projects", "get", p.Metadata.Name) {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	out, err := s.listProjects(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) getProject(r *http.Request, name string) (*v1.AppProject, error) {
	if err := s.authorize(r, "projects", "get", name); err != nil {
		return nil, err
	}
	return s.Store.GetProject(r.Context(), name)
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.getProject(r, r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) createProject(r *http.Request, p *v1.AppProject) error {
	p.SetDefaults()
	if err := p.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if err := s.authorize(r, "projects", "create", p.Metadata.Name); err != nil {
		return err
	}
	if existing, err := s.Store.GetProject(r.Context(), p.Metadata.Name); err == nil && existing.Metadata.ResourceVersion != "" {
		return fmt.Errorf("%w: project %q already exists", store.ErrExists, p.Metadata.Name)
	}
	if err := s.Store.PutProject(r.Context(), p); err != nil {
		return err
	}
	s.audit(r, "projects/create", p.Metadata.Name, "ALLOWED", "")
	return nil
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var p v1.AppProject
	if err := decode(r, &p); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.createProject(r, &p); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) updateProject(r *http.Request, p *v1.AppProject) error {
	if err := s.authorize(r, "projects", "update", p.Metadata.Name); err != nil {
		return err
	}
	p.SetDefaults()
	if err := p.Validate(); err != nil {
		return badRequest("%v", err)
	}
	if err := s.Store.PutProject(r.Context(), p); err != nil {
		return err
	}
	s.audit(r, "projects/update", p.Metadata.Name, "ALLOWED", "")
	return nil
}

func (s *Server) handleUpdateProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.can(w, r, "projects", "update", name) {
		return
	}
	var p v1.AppProject
	if err := decode(r, &p); err != nil {
		s.fail(w, err)
		return
	}
	p.Metadata.Name = name
	if err := s.updateProject(r, &p); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// deleteProject deletes an unused project (never the default one).
func (s *Server) deleteProject(r *http.Request, name string) error {
	if err := s.authorize(r, "projects", "delete", name); err != nil {
		return err
	}
	if name == v1.DefaultProject {
		return badRequest("the default project cannot be deleted")
	}
	apps, err := s.Store.ListApplications(r.Context())
	if err != nil {
		return err
	}
	for _, a := range apps {
		if a.Spec.Project == name {
			return badRequest("project %q is used by application %q", name, a.Metadata.Name)
		}
	}
	if err := s.Store.DeleteProject(r.Context(), name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	s.audit(r, "projects/delete", name, "ALLOWED", "")
	return nil
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteProject(r, r.PathValue("name")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}
