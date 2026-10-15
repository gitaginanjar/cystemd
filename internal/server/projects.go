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

func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	id := identity(r)
	out := []*v1.AppProject{}
	for _, p := range projects {
		if s.RBAC.Enforce(id.Username, id.Groups, "projects", "get", p.Metadata.Name) {
			out = append(out, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleGetProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.can(w, r, "projects", "get", name) {
		return
	}
	p, err := s.Store.GetProject(r.Context(), name)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var p v1.AppProject
	if err := decode(r, &p); err != nil {
		s.fail(w, err)
		return
	}
	p.SetDefaults()
	if err := p.Validate(); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if !s.can(w, r, "projects", "create", p.Metadata.Name) {
		return
	}
	if existing, err := s.Store.GetProject(r.Context(), p.Metadata.Name); err == nil && existing.Metadata.ResourceVersion != "" {
		s.fail(w, fmt.Errorf("%w: project %q already exists", store.ErrExists, p.Metadata.Name))
		return
	}
	if err := s.Store.PutProject(r.Context(), &p); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "projects/create", p.Metadata.Name, "ALLOWED", "")
	writeJSON(w, http.StatusOK, p)
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
	p.SetDefaults()
	if err := p.Validate(); err != nil {
		s.fail(w, badRequest("%v", err))
		return
	}
	if err := s.Store.PutProject(r.Context(), &p); err != nil {
		s.fail(w, err)
		return
	}
	s.audit(r, "projects/update", name, "ALLOWED", "")
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !s.can(w, r, "projects", "delete", name) {
		return
	}
	if name == v1.DefaultProject {
		writeError(w, http.StatusBadRequest, "the default project cannot be deleted")
		return
	}
	apps, err := s.Store.ListApplications(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, a := range apps {
		if a.Spec.Project == name {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("project %q is used by application %q", name, a.Metadata.Name))
			return
		}
	}
	if err := s.Store.DeleteProject(r.Context(), name); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	s.audit(r, "projects/delete", name, "ALLOWED", "")
	writeJSON(w, http.StatusOK, map[string]any{})
}
