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
