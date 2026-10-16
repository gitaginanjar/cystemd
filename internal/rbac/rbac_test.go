// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package rbac

import "testing"

func TestEnforce(t *testing.T) {
	e, err := New(`
# team A deploys its own project
p, role:team-a, applications, *, team-a/*, allow
p, role:team-a, applications, delete, team-a/prod-*, deny
g, alice, role:team-a
g, ldap:ops, role:admin
g, nested, role:alice-alias
g, role:alice-alias, role:team-a
`, RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		user     string
		groups   []string
		res, act string
		obj      string
		want     bool
	}{
		{"alice", nil, "applications", "sync", "team-a/web", true},
		{"alice", nil, "applications", "sync", "team-b/web", false},
		{"alice", nil, "applications", "get", "team-b/web", true}, // default readonly
		{"alice", nil, "applications", "delete", "team-a/prod-db", false},
		{"alice", nil, "applications", "delete", "team-a/dev-db", true},
		{"bob", []string{"ldap:ops"}, "clusters", "update", "in-cluster", true},
		{"bob", nil, "repositories", "create", "x", false},
		{"nested", nil, "applications", "sync", "team-a/x", true},
		{"anyone", nil, "logs", "get", "p/a", true},
	}
	for _, c := range cases {
		if got := e.Enforce(c.user, c.groups, c.res, c.act, c.obj); got != c.want {
			t.Errorf("%s %v %s %s %s = %v, want %v", c.user, c.groups, c.res, c.act, c.obj, got, c.want)
		}
	}
	if roles := e.Roles("alice", nil); len(roles) != 2 {
		t.Errorf("roles = %v", roles)
	}
}

func TestNoDefaultRoleDeniesEverything(t *testing.T) {
	e, _ := New("", "")
	if e.Enforce("anon", nil, "applications", "get", "default/x") {
		t.Fatal("no default role must deny")
	}
	if !e.Enforce("admin", []string{RoleAdmin}, "applications", "delete", "default/x") {
		t.Fatal("admin must be allowed")
	}
}

func TestPolicyErrors(t *testing.T) {
	for _, p := range []string{"p, a, b", "x, y", "p, a, b, c, d, maybe", "g, onlyone"} {
		if _, err := New(p, ""); err == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestEnforceWithGrant(t *testing.T) {
	e, err := New(`
p, role:team-a, applications, *, team-a/*, allow
p, ldap:auditors, applications, sync, payments/frozen, deny
g, alice, role:team-a
`, RoleReadonly)
	if err != nil {
		t.Fatal(err)
	}
	ci := &Grant{Subjects: []string{"proj:payments:ci"}}
	ci.Allow("proj:payments:ci", "applications", "sync", "payments/*", true)
	ci.Allow("proj:payments:ci", "applications", "sync", "payments/db", false)
	cases := []struct {
		user   string
		groups []string
		g      *Grant
		act    string
		obj    string
		want   bool
	}{
		{"proj:payments:ci", nil, ci, "sync", "payments/web", true},
		{"proj:payments:ci", nil, nil, "sync", "payments/web", false},            // without the grant
		{"proj:payments:ci", nil, ci, "sync", "payments/db", false},              // the role's deny
		{"proj:payments:ci", nil, ci, "sync", "billing/web", false},              // another project
		{"proj:payments:ci", nil, ci, "get", "billing/web", true},                // the default role still applies
		{"bob", []string{"ldap:auditors"}, ci, "sync", "payments/frozen", false}, // a policy deny wins over the grant
		{"alice", nil, ci, "sync", "team-a/x", true},
		{"alice", nil, &Grant{}, "sync", "payments/web", false},
	}
	for _, c := range cases {
		if got := e.EnforceWith(c.user, c.groups, c.g, "applications", c.act, c.obj); got != c.want {
			t.Errorf("%s %v grant=%v %s %s = %v, want %v", c.user, c.groups, c.g != nil, c.act, c.obj, got, c.want)
		}
	}
	if !(*Grant)(nil).Empty() || !(&Grant{}).Empty() || ci.Empty() {
		t.Fatal("Empty")
	}
}
