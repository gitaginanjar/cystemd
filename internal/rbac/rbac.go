// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package rbac enforces Argo CD-style RBAC policies:
//
//	p, <subject>, <resource>, <action>, <object>, <allow|deny>
//	g, <user or group>, <role>
//
// Resources: applications, applicationsets, projects, repositories,
// clusters, accounts, logs.
// Actions: get, create, update, delete, sync, override, action, * …
// Objects: "<project>/<application>" for applications, a name otherwise.
// Globs (*) match any characters, "/" included. A deny always wins.
package rbac

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// Built-in roles.
const (
	RoleAdmin    = "role:admin"
	RoleReadonly = "role:readonly"
)

const builtinPolicy = `
p, role:readonly, applications, get, */*, allow
p, role:readonly, applicationsets, get, */*, allow
p, role:readonly, projects, get, *, allow
p, role:readonly, repositories, get, *, allow
p, role:readonly, clusters, get, *, allow
p, role:readonly, logs, get, */*, allow
p, role:admin, *, *, *, allow
p, role:admin, *, *, */*, allow
`

type rule struct {
	subject, resource, action, object string
	allow                             bool
}

// Grant is policy that applies to one caller besides the configured one:
// subjects it holds (a project role, proj:<project>:<role>) and their
// rules. A deny still wins over any allow.
type Grant struct {
	Subjects []string
	rules    []rule
}

// Allow adds a rule of one of the grant's subjects.
func (g *Grant) Allow(subject, resource, action, object string, allow bool) {
	g.rules = append(g.rules, rule{subject, resource, action, object, allow})
}

// Empty reports whether the grant adds nothing.
func (g *Grant) Empty() bool { return g == nil || len(g.Subjects) == 0 }

// Enforcer evaluates a policy.
type Enforcer struct {
	mu          sync.RWMutex
	rules       []rule
	groups      map[string][]string // member → roles
	defaultRole string
}

// New parses policy (CSV lines) on top of the built-in roles.
func New(policy, defaultRole string) (*Enforcer, error) {
	e := &Enforcer{groups: map[string][]string{}, defaultRole: defaultRole}
	if err := e.load(builtinPolicy + "\n" + policy); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Enforcer) load(policy string) error {
	sc := bufio.NewScanner(strings.NewReader(policy))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}
		switch fields[0] {
		case "p":
			if len(fields) != 6 {
				return fmt.Errorf("policy line %d: want p, subject, resource, action, object, effect", n)
			}
			if fields[5] != "allow" && fields[5] != "deny" {
				return fmt.Errorf("policy line %d: effect must be allow or deny", n)
			}
			e.rules = append(e.rules, rule{fields[1], fields[2], fields[3], fields[4], fields[5] == "allow"})
		case "g":
			if len(fields) != 3 {
				return fmt.Errorf("policy line %d: want g, member, role", n)
			}
			e.groups[fields[1]] = append(e.groups[fields[1]], fields[2])
		default:
			return fmt.Errorf("policy line %d: unknown type %q", n, fields[0])
		}
	}
	return sc.Err()
}

// subjects expands a user and its groups into every subject that applies:
// the user, its groups, their roles (transitively) and the default role.
func (e *Enforcer) subjects(user string, groups []string) []string {
	seen := map[string]bool{}
	var out []string
	var visit func(s string, depth int)
	visit = func(s string, depth int) {
		if s == "" || seen[s] || depth > 10 {
			return
		}
		seen[s] = true
		out = append(out, s)
		for _, r := range e.groups[s] {
			visit(r, depth+1)
		}
	}
	visit(user, 0)
	for _, g := range groups {
		visit(g, 0)
	}
	visit(e.defaultRole, 0)
	return out
}

// Enforce reports whether user (member of groups) may perform action on
// object of resource.
func (e *Enforcer) Enforce(user string, groups []string, resource, action, object string) bool {
	return e.EnforceWith(user, groups, nil, resource, action, object)
}

// EnforceWith is Enforce with a grant's subjects and rules added.
func (e *Enforcer) EnforceWith(user string, groups []string, g *Grant, resource, action, object string) bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	subjects := e.subjects(user, groups)
	if !g.Empty() {
		subjects = append(subjects, g.Subjects...)
	}
	allowed := false
	for _, s := range subjects {
		for _, rules := range [][]rule{e.rules, grantRules(g)} {
			for _, r := range rules {
				if r.subject != s || !match(r.resource, resource) || !match(r.action, action) || !match(r.object, object) {
					continue
				}
				if !r.allow {
					return false
				}
				allowed = true
			}
		}
	}
	return allowed
}

func grantRules(g *Grant) []rule {
	if g == nil {
		return nil
	}
	return g.rules
}

// Roles lists the roles user holds (for the user-info page).
func (e *Enforcer) Roles(user string, groups []string) []string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	var out []string
	for _, s := range e.subjects(user, groups) {
		if strings.HasPrefix(s, "role:") {
			out = append(out, s)
		}
	}
	return out
}

var globCache sync.Map

func match(pattern, value string) bool {
	if pattern == "*" || pattern == value {
		return true
	}
	if !strings.ContainsAny(pattern, "*?") {
		return false
	}
	re, ok := globCache.Load(pattern)
	if !ok {
		compiled := regexp.MustCompile("^" + strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern)) + "$")
		globCache.Store(pattern, compiled)
		re = compiled
	}
	return re.(*regexp.Regexp).MatchString(value)
}
