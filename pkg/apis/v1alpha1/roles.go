// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Project roles are Argo CD's: RBAC policies on a project's applications,
// whose subject is proj:<project>:<role>. They apply to the members of the
// role's groups and to the holders of the role's tokens, which the control
// plane issues for automation scoped to one project.

// ProjectRole is a role of a project.
type ProjectRole struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	// Policies are RBAC policy lines of the role's subject on the
	// project's applications:
	// "p, proj:<project>:<role>, applications, sync, <project>/*, allow".
	Policies []string `json:"policies,omitempty" yaml:"policies,omitempty"`
	// Groups have the role: SSO and Nomad token groups (nomad:role:<name>,
	// nomad:policy:<name>) or local accounts' groups.
	Groups []string `json:"groups,omitempty" yaml:"groups,omitempty"`
}

// AppProjectStatus is what the control plane records of a project; it is
// not part of what is applied.
type AppProjectStatus struct {
	// JWTTokensByRole lists the tokens issued for each role.
	JWTTokensByRole map[string]JWTTokens `json:"jwtTokensByRole,omitempty" yaml:"jwtTokensByRole,omitempty"`
}

// JWTTokens are the tokens of a role.
type JWTTokens struct {
	Items []JWTToken `json:"items,omitempty" yaml:"items,omitempty"`
}

// JWTToken records a role token; the token itself is shown once, when
// created. Times are Unix seconds; an exp of 0 never expires.
type JWTToken struct {
	IssuedAt  int64  `json:"iat" yaml:"iat"`
	ExpiresAt int64  `json:"exp,omitempty" yaml:"exp,omitempty"`
	ID        string `json:"id,omitempty" yaml:"id,omitempty"`
}

// RoleSubject is the RBAC subject of a project role.
func RoleSubject(project, role string) string { return "proj:" + project + ":" + role }

// ParseRoleSubject splits a project role's subject.
func ParseRoleSubject(s string) (project, role string, ok bool) {
	rest, ok := strings.CutPrefix(s, "proj:")
	if !ok {
		return "", "", false
	}
	project, role, ok = strings.Cut(rest, ":")
	return project, role, ok && project != "" && role != ""
}

// Role returns the project's role of that name, or nil.
func (p *AppProject) Role(name string) *ProjectRole {
	for i := range p.Spec.Roles {
		if p.Spec.Roles[i].Name == name {
			return &p.Spec.Roles[i]
		}
	}
	return nil
}

// RoleTokens are the tokens issued for a role.
func (p *AppProject) RoleTokens(role string) []JWTToken {
	return p.Status.JWTTokensByRole[role].Items
}

// RolePolicy is a role's policy line, parsed.
type RolePolicy struct {
	Subject, Resource, Action, Object string
	Allow                             bool
}

// ParseRolePolicy parses "p, <subject>, <resource>, <action>, <object>,
// <allow|deny>".
func ParseRolePolicy(line string) (RolePolicy, error) {
	f := strings.Split(line, ",")
	for i := range f {
		f[i] = strings.TrimSpace(f[i])
	}
	if len(f) != 6 || f[0] != "p" {
		return RolePolicy{}, fmt.Errorf("policy %q: want p, <subject>, <resource>, <action>, <object>, <allow|deny>", line)
	}
	if f[5] != "allow" && f[5] != "deny" {
		return RolePolicy{}, fmt.Errorf("policy %q: the effect is allow or deny", line)
	}
	for _, v := range f[1:5] {
		if v == "" {
			return RolePolicy{}, fmt.Errorf("policy %q has an empty field", line)
		}
	}
	return RolePolicy{Subject: f[1], Resource: f[2], Action: f[3], Object: f[4], Allow: f[5] == "allow"}, nil
}

// String is the policy line.
func (rp RolePolicy) String() string {
	effect := "deny"
	if rp.Allow {
		effect = "allow"
	}
	return strings.Join([]string{"p", rp.Subject, rp.Resource, rp.Action, rp.Object, effect}, ", ")
}

// RoleResources are what a role's policies may grant on: the project's
// applications and their logs.
var RoleResources = []string{"applications", "logs"}

var roleName = regexp.MustCompile(`^[a-zA-Z0-9]([-_a-zA-Z0-9]{0,61}[a-zA-Z0-9])?$`)

// ValidateRoleName checks a project role's name.
func ValidateRoleName(name string) error {
	if !roleName.MatchString(name) {
		return fmt.Errorf("role name %q: at most 63 letters, digits, '-' and '_', starting and ending with a letter or digit", name)
	}
	return nil
}

// validateRoles checks the project's roles: their policies may only grant
// on the project's own applications, as the role's subject.
func (p *AppProject) validateRoles() error {
	var errs []error
	seen := map[string]bool{}
	for i, r := range p.Spec.Roles {
		where := fmt.Sprintf("roles[%d]", i)
		if err := ValidateRoleName(r.Name); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
			continue
		}
		where = fmt.Sprintf("role %s", r.Name)
		if seen[r.Name] {
			errs = append(errs, fmt.Errorf("%s is defined twice", where))
		}
		seen[r.Name] = true
		subject := RoleSubject(p.Metadata.Name, r.Name)
		for _, line := range r.Policies {
			rp, err := ParseRolePolicy(line)
			switch {
			case err != nil:
				errs = append(errs, fmt.Errorf("%s: %w", where, err))
			case rp.Subject != subject:
				errs = append(errs, fmt.Errorf("%s: policy %q: the subject must be %s", where, line, subject))
			case !slices.Contains(RoleResources, rp.Resource):
				errs = append(errs, fmt.Errorf("%s: policy %q: a role grants on %s only", where, line, strings.Join(RoleResources, " and ")))
			case !strings.HasPrefix(rp.Object, p.Metadata.Name+"/"):
				errs = append(errs, fmt.Errorf("%s: policy %q: a role grants on its project's applications only (%s/<application>)", where, line, p.Metadata.Name))
			}
		}
		for _, g := range r.Groups {
			if strings.TrimSpace(g) == "" || strings.ContainsAny(g, ",\n") {
				errs = append(errs, fmt.Errorf("%s: group %q is empty or holds a comma", where, g))
			}
		}
	}
	return errors.Join(errs...)
}
