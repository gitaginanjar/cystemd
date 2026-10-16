// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/a-h/templ"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
	"github.com/gitaginanjar/cystemd/ui/views"
)

// The roles of a project, in the UI: the project page lists them, and a
// role's panel edits its policies, groups and tokens in place.

// rolePanel describes a project role for its panel.
func (s *Server) rolePanel(r *http.Request, proj *v1.AppProject, role string) (views.RolePanel, error) {
	rr := proj.Role(role)
	if rr == nil {
		return views.RolePanel{}, notFound("project %s has no role %q", proj.Metadata.Name, role)
	}
	return views.RolePanel{
		Project: proj.Metadata.Name, Role: *rr, Tokens: proj.RoleTokens(role),
		CanUpdate: s.allowed(r, "projects", "update", proj.Metadata.Name),
		CloseHref: views.ProjectHref(proj.Metadata.Name),
	}, nil
}

// projectPanel is the panel a project page opens (?panel=...).
func (s *Server) projectPanel(r *http.Request, proj *v1.AppProject, canUpdate bool) (templ.Component, error) {
	name := proj.Metadata.Name
	switch r.URL.Query().Get("panel") {
	case "edit":
		if canUpdate {
			return views.ProjectPanelView(views.ProjectPanel{Form: projectForm(proj), CloseHref: views.ProjectHref(name)}), nil
		}
	case "new-role":
		if canUpdate {
			return views.NewRolePanelView(views.NewRolePanel{Project: name, CloseHref: views.ProjectHref(name)}), nil
		}
	case "role":
		rp, err := s.rolePanel(r, proj, r.URL.Query().Get("role"))
		if err != nil {
			return nil, err
		}
		return views.RolePanelView(rp), nil
	}
	return nil, nil
}

// webCreateRole adds a role to a project and opens it.
func (s *Server) webCreateRole(w http.ResponseWriter, r *http.Request) {
	project := r.PathValue("name")
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	np := views.NewRolePanel{Project: project, Name: strings.TrimSpace(form.Get("name")),
		Description: strings.TrimSpace(form.Get("description")), CloseHref: views.ProjectHref(project)}
	_, err = s.editProject(r, project, func(p *v1.AppProject) error {
		if err := v1.ValidateRoleName(np.Name); err != nil {
			return badRequest("%v", err)
		}
		if p.Role(np.Name) != nil {
			return badRequest("project %s already has a role %q", project, np.Name)
		}
		p.Spec.Roles = append(p.Spec.Roles, v1.ProjectRole{Name: np.Name, Description: np.Description})
		return nil
	})
	if err != nil {
		np.Error = err.Error()
		s.render(w, r, errorCode(err), views.NewRolePanelView(np))
		return
	}
	s.navigate(w, views.RoleHref(project, np.Name), success("Role "+np.Name+" created."))
}

// editRole changes one role of a project.
func (s *Server) editRole(r *http.Request, project, role string, fn func(*v1.ProjectRole) error) error {
	_, err := s.editProject(r, project, func(p *v1.AppProject) error {
		rr := p.Role(role)
		if rr == nil {
			return notFound("project %s has no role %q", project, role)
		}
		return fn(rr)
	})
	return err
}

// webRoleOp changes a project role from its panel (the form's op): its
// policies, groups and tokens; or deletes it.
func (s *Server) webRoleOp(w http.ResponseWriter, r *http.Request) {
	project, role := r.PathValue("name"), r.PathValue("role")
	form, err := formValues(r)
	if err != nil {
		s.webFail(w, r, err)
		return
	}
	var done string
	var created *RoleTokenResponse
	switch op := form.Get("op"); op {
	case "add-policy", "remove-policy":
		line := form.Get("policy")
		if op == "add-policy" {
			action, object := strings.TrimSpace(form.Get("action")), strings.TrimSpace(form.Get("object"))
			if action == "" || object == "" {
				err = badRequest("a policy needs an action and an object")
				break
			}
			line = v1.NewRolePolicy(project, role, form.Get("resource"), action, object, form.Get("permission") != "deny").String()
		}
		err = s.editRole(r, project, role, func(rr *v1.ProjectRole) error {
			i := slices.IndexFunc(rr.Policies, func(l string) bool { return l == line || v1.SamePolicy(l, line) })
			switch {
			case op == "remove-policy" && i < 0:
				return badRequest("role %s has no policy %q", role, line)
			case op == "remove-policy":
				rr.Policies = slices.Delete(rr.Policies, i, i+1)
			case i < 0:
				rr.Policies = append(rr.Policies, line)
			}
			return nil
		})
		done = "Policy added: " + line
		if op == "remove-policy" {
			done = "Policy removed."
		}
	case "add-group", "remove-group":
		group := strings.TrimSpace(form.Get("group"))
		err = s.editRole(r, project, role, func(rr *v1.ProjectRole) error {
			i := slices.Index(rr.Groups, group)
			switch {
			case group == "":
				return badRequest("which group?")
			case op == "remove-group" && i < 0:
				return badRequest("role %s does not list group %q", role, group)
			case op == "remove-group":
				rr.Groups = slices.Delete(rr.Groups, i, i+1)
			case i < 0:
				rr.Groups = append(rr.Groups, group)
			}
			return nil
		})
		done = "Group " + group + " has the role."
		if op == "remove-group" {
			done = "Group " + group + " no longer has the role."
		}
	case "create-token":
		ttl, perr := client.ParseLifetime(form.Get("expiresIn"))
		if perr != nil {
			err = badRequest("expires in: %v", perr)
			break
		}
		created, err = s.createRoleToken(r, project, role, roleTokenRequest{ID: strings.TrimSpace(form.Get("id")), ExpiresIn: int64(ttl / time.Second)})
		if err == nil {
			done = "Token " + created.ID + " created."
		}
	case "delete-token":
		token := form.Get("token")
		err = s.deleteRoleToken(r, project, role, token)
		done = "Token " + token + " deleted."
	case "delete-role":
		_, err = s.editProject(r, project, func(p *v1.AppProject) error {
			if p.Role(role) == nil {
				return notFound("project %s has no role %q", project, role)
			}
			p.Spec.Roles = slices.DeleteFunc(p.Spec.Roles, func(x v1.ProjectRole) bool { return x.Name == role })
			return nil
		})
		if err == nil {
			s.navigate(w, views.ProjectHref(project), success("Role "+role+" deleted."))
			return
		}
	default:
		err = badRequest("unknown operation %q", op)
	}
	proj, gerr := s.getProject(r, project)
	if gerr != nil {
		s.webFail(w, r, gerr)
		return
	}
	panel, perr := s.rolePanel(r, proj, role)
	if perr != nil {
		s.webFail(w, r, perr)
		return
	}
	if err != nil {
		panel.Error = err.Error()
		s.render(w, r, errorCode(err), views.RolePanelView(panel))
		return
	}
	if created != nil {
		panel.NewToken, panel.NewID = created.Token, created.ID
	}
	s.render(w, r, http.StatusOK, templ.Join(views.RolePanelView(panel), views.Toasts(success(done))))
}
