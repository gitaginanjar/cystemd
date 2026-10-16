// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"
)

func TestRoleSubjectAndPolicies(t *testing.T) {
	if s := RoleSubject("payments", "ci"); s != "proj:payments:ci" {
		t.Fatal(s)
	}
	for in, want := range map[string][2]string{
		"proj:payments:ci": {"payments", "ci"},
		"proj:a:b:c":       {"a", "b:c"},
	} {
		if p, r, ok := ParseRoleSubject(in); !ok || p != want[0] || r != want[1] {
			t.Errorf("%s: %s %s %v", in, p, r, ok)
		}
	}
	for _, bad := range []string{"payments:ci", "proj:", "proj:payments", "proj::ci", "proj:payments:"} {
		if _, _, ok := ParseRoleSubject(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}

	rp, err := ParseRolePolicy(" p,proj:payments:ci , applications, sync, payments/*, allow ")
	if err != nil || rp.Subject != "proj:payments:ci" || rp.Object != "payments/*" || !rp.Allow {
		t.Fatalf("%+v %v", rp, err)
	}
	if rp.String() != "p, proj:payments:ci, applications, sync, payments/*, allow" {
		t.Fatal(rp.String())
	}
	for _, bad := range []string{"g, a, b", "p, s, r, a, o", "p, s, r, a, o, maybe", "p, , r, a, o, allow"} {
		if _, err := ParseRolePolicy(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if NewRolePolicy("payments", "ci", "logs", "get", "api-*", false).String() != "p, proj:payments:ci, logs, get, payments/api-*, deny" {
		t.Fatal("NewRolePolicy prefixes the object")
	}
	if NewRolePolicy("payments", "ci", "applications", "get", "payments/web", true).Object != "payments/web" {
		t.Fatal("NewRolePolicy keeps a prefixed object")
	}
	if !SamePolicy("p,proj:p:r,applications,get,p/*,allow", "p, proj:p:r, applications, get, p/*, allow") || SamePolicy("x", "x") {
		t.Fatal("SamePolicy")
	}
}

func TestProjectRolesValidation(t *testing.T) {
	ok := &AppProject{Metadata: ObjectMeta{Name: "payments"}, Spec: AppProjectSpec{Roles: []ProjectRole{{
		Name:     "ci",
		Policies: []string{"p, proj:payments:ci, applications, sync, payments/*, allow", "p, proj:payments:ci, logs, get, payments/api, deny"},
		Groups:   []string{"nomad:role:payments"},
	}}}}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	if ok.Role("ci") == nil || ok.Role("cd") != nil {
		t.Fatal("Role")
	}

	bad := &AppProject{Metadata: ObjectMeta{Name: "payments"}, Spec: AppProjectSpec{Roles: []ProjectRole{
		{Name: "-ci"},
		{Name: "ci", Policies: []string{
			"p, proj:billing:ci, applications, sync, payments/*, allow", // another subject
			"p, proj:payments:ci, projects, update, payments, allow",   // not an application
			"p, proj:payments:ci, applications, sync, */*, allow",      // another project's
			"p, proj:payments:ci, applications, sync",                  // short
		}, Groups: []string{"a,b", " "}},
		{Name: "ci"},
	}}}
	err := bad.Validate()
	for _, want := range []string{
		`roles[0]: role name "-ci"`,
		"the subject must be proj:payments:ci",
		"a role grants on applications and logs only",
		"a role grants on its project's applications only (payments/<application>)",
		"want p, <subject>",
		`group "a,b" is empty or holds a comma`,
		`group " " is empty`,
		"role ci is defined twice",
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}
