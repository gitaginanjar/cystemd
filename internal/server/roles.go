// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/gitaginanjar/cystemd/internal/auth"
	"github.com/gitaginanjar/cystemd/internal/rbac"
	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Project roles (Argo CD's) add to the RBAC policy: a caller holding a
// role, by one of its tokens or by being in one of its groups, has the
// role's policies, which validation keeps to the project's applications.

// projectRoles indexes the projects' roles, reloaded from the store every
// few seconds and when a project changes through this server.
type projectRoles struct {
	mu    sync.Mutex
	at    time.Time
	roles []compiledRole
}

type compiledRole struct {
	subject string // proj:<project>:<role>
	groups  []string
	rules   []v1.RolePolicy
}

const projectRolesTTL = 5 * time.Second

func (pr *projectRoles) get(ctx context.Context, st *store.Store, logger interface{ Warn(string, ...any) }) []compiledRole {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if !pr.at.IsZero() && time.Since(pr.at) < projectRolesTTL {
		return pr.roles
	}
	projects, err := st.ListProjects(ctx)
	if err != nil {
		// Keep what is known; try again on the next check.
		logger.Warn("cannot load the project roles", "error", err)
		return pr.roles
	}
	var roles []compiledRole
	for _, p := range projects {
		for _, r := range p.Spec.Roles {
			cr := compiledRole{subject: v1.RoleSubject(p.Metadata.Name, r.Name), groups: r.Groups}
			for _, line := range r.Policies {
				if rp, err := v1.ParseRolePolicy(line); err == nil && rp.Subject == cr.subject {
					cr.rules = append(cr.rules, rp)
				}
			}
			roles = append(roles, cr)
		}
	}
	pr.roles, pr.at = roles, time.Now()
	return roles
}

func (pr *projectRoles) invalidate() {
	pr.mu.Lock()
	pr.at = time.Time{}
	pr.mu.Unlock()
}

// grant is what the caller's project roles add to the policy.
func (s *Server) grant(ctx context.Context, id *auth.Identity) *rbac.Grant {
	var g *rbac.Grant
	for _, cr := range s.roles.get(ctx, s.Store, s.logger) {
		holds := id.Provider == auth.ProviderProject && id.Username == cr.subject
		for _, group := range cr.groups {
			holds = holds || slices.Contains(id.Groups, group)
		}
		if !holds {
			continue
		}
		if g == nil {
			g = &rbac.Grant{}
		}
		g.Subjects = append(g.Subjects, cr.subject)
		for _, rp := range cr.rules {
			g.Allow(rp.Subject, rp.Resource, rp.Action, rp.Object, rp.Allow)
		}
	}
	return g
}

// enforce is the RBAC check of every permission: the policy, and the
// caller's project roles.
func (s *Server) enforce(r *http.Request, resource, action, object string) bool {
	id := identity(r)
	return s.RBAC.EnforceWith(id.Username, id.Groups, s.grant(r.Context(), id), resource, action, object)
}

// projectChanged forgets what is cached of a project.
func (s *Server) projectChanged(name string) {
	s.roles.invalidate()
	s.Auth.ForgetProject(name)
}

// editProject changes a project atomically, validating the result (needs
// projects/update).
func (s *Server) editProject(r *http.Request, name string, fn func(*v1.AppProject) error) (*v1.AppProject, error) {
	if err := s.authorize(r, "projects", "update", name); err != nil {
		return nil, err
	}
	p, err := s.Store.MutateProject(r.Context(), name, func(p *v1.AppProject) error {
		if err := fn(p); err != nil {
			return err
		}
		p.SetDefaults()
		if err := p.Validate(); err != nil {
			return badRequest("%v", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.projectChanged(name)
	s.audit(r, "projects/update", name, "ALLOWED", "")
	return p, nil
}

type roleTokenRequest struct {
	// ID names the token (default: random).
	ID string `json:"id"`
	// ExpiresIn is the token's lifetime in seconds; 0 never expires.
	ExpiresIn int64 `json:"expiresIn"`
}

// RoleTokenResponse is a new role token, shown this once.
type RoleTokenResponse struct {
	Token string `json:"token"`
	v1.JWTToken
}

// createRoleToken issues a token of a project role (needs projects/update
// on the project).
func (s *Server) createRoleToken(r *http.Request, project, role string, req roleTokenRequest) (*RoleTokenResponse, error) {
	if err := s.authorize(r, "projects", "update", project); err != nil {
		return nil, err
	}
	if req.ExpiresIn < 0 || req.ExpiresIn > maxTokenLifetime {
		return nil, badRequest("expiresIn is a number of seconds (at most a century), 0 for a token that does not expire")
	}
	tok, rec, err := s.Auth.GenerateProjectToken(r.Context(), project, role, req.ID, time.Duration(req.ExpiresIn)*time.Second)
	s.projectChanged(project)
	if errors.Is(err, store.ErrNotFound) {
		err = notFound("project %s not found", project)
	}
	object := project + "/" + role
	if err != nil {
		s.audit(r, "projects/create-token", object, "FAILED", err.Error())
		return nil, err
	}
	s.audit(r, "projects/create-token", object+"/"+rec.ID, "ALLOWED", "")
	return &RoleTokenResponse{Token: tok, JWTToken: rec}, nil
}

func (s *Server) handleCreateRoleToken(w http.ResponseWriter, r *http.Request) {
	var req roleTokenRequest
	if err := decode(r, &req); err != nil {
		s.fail(w, err)
		return
	}
	out, err := s.createRoleToken(r, r.PathValue("name"), r.PathValue("role"), req)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteRoleToken revokes a role token, named by its ID or issue time.
func (s *Server) deleteRoleToken(r *http.Request, project, role, token string) error {
	if err := s.authorize(r, "projects", "update", project); err != nil {
		return err
	}
	err := s.Auth.DeleteProjectToken(r.Context(), project, role, token)
	s.projectChanged(project)
	if errors.Is(err, store.ErrNotFound) {
		err = notFound("project %s not found", project)
	}
	object := project + "/" + role + "/" + token
	if err != nil {
		s.audit(r, "projects/delete-token", object, "FAILED", err.Error())
		return err
	}
	s.audit(r, "projects/delete-token", object, "ALLOWED", "")
	return nil
}

func (s *Server) handleDeleteRoleToken(w http.ResponseWriter, r *http.Request) {
	if err := s.deleteRoleToken(r, r.PathValue("name"), r.PathValue("role"), r.PathValue("token")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{})
}
