// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/store"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func TestProjectTokens(t *testing.T) {
	ctx := context.Background()
	m, st, _ := newManager(t, nil)
	if err := st.PutProject(ctx, &v1.AppProject{Metadata: v1.ObjectMeta{Name: "payments"}, Spec: v1.AppProjectSpec{
		Roles: []v1.ProjectRole{{Name: "ci"}, {Name: "ops"}},
	}}); err != nil {
		t.Fatal(err)
	}
	var re *RequestError
	if _, _, err := m.GenerateProjectToken(ctx, "payments", "nope", "", 0); !errors.As(err, &re) || !re.NotFound {
		t.Fatalf("missing role: %v", err)
	}
	if _, _, err := m.GenerateProjectToken(ctx, "billing", "ci", "", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing project: %v", err)
	}

	tok, rec, err := m.GenerateProjectToken(ctx, "payments", "ci", "gitlab", time.Hour)
	if err != nil || rec.ID != "gitlab" || rec.ExpiresAt-rec.IssuedAt != 3600 {
		t.Fatalf("%+v %v", rec, err)
	}
	id, err := m.Verify(ctx, tok)
	if err != nil || id.Username != "proj:payments:ci" || id.Provider != ProviderProject || id.TokenID != "gitlab" || len(id.Groups) != 0 {
		t.Fatalf("verify: %+v %v", id, err)
	}
	if _, _, err := m.GenerateProjectToken(ctx, "payments", "ci", "gitlab", 0); !errors.As(err, &re) {
		t.Fatalf("duplicate ID: %v", err)
	}
	other, rec2, err := m.GenerateProjectToken(ctx, "payments", "ops", "", 0)
	if err != nil || rec2.ID == "" || rec2.ExpiresAt != 0 {
		t.Fatalf("%+v %v", rec2, err)
	}
	if id, err := m.Verify(ctx, other); err != nil || id.Username != "proj:payments:ops" {
		t.Fatalf("ops: %+v %v", id, err)
	}

	// Deleting by issue time (Argo CD's way) or ID revokes at once.
	if err := m.DeleteProjectToken(ctx, "payments", "ops", strconv.FormatInt(rec2.IssuedAt, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, other); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("deleted token: %v", err)
	}
	if err := m.DeleteProjectToken(ctx, "payments", "ops", rec2.ID); !errors.As(err, &re) || !re.NotFound {
		t.Fatalf("delete twice: %v", err)
	}

	// A token generated again under a deleted ID is another one.
	if err := m.DeleteProjectToken(ctx, "payments", "ci", "gitlab"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MutateProject(ctx, "payments", func(p *v1.AppProject) error {
		p.Status.JWTTokensByRole = map[string]v1.JWTTokens{"ci": {Items: []v1.JWTToken{{ID: "gitlab", IssuedAt: rec.IssuedAt + 1}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.ForgetProject("payments")
	if _, err := m.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old token under a reused ID: %v", err)
	}

	// Removing the role revokes its tokens; ForgetProject makes it
	// immediate on this replica (the cache expires on the others).
	fresh, _, err := m.GenerateProjectToken(ctx, "payments", "ci", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetProject(ctx, "payments")
	p.Spec.Roles = p.Spec.Roles[1:]
	if err := st.PutProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, fresh); err != nil {
		t.Fatalf("cached for a few seconds: %v", err)
	}
	m.ForgetProject("payments")
	if _, err := m.Verify(ctx, fresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("token of a removed role: %v", err)
	}
	// And the project going away does too.
	if err := st.DeleteProject(ctx, "payments"); err != nil {
		t.Fatal(err)
	}
	m.ForgetProject("payments")
	if _, err := m.Verify(ctx, other); !errors.Is(err, ErrUnauthenticated) {
		t.Fatal("token of a deleted project")
	}
}

func TestProjectTokenExpiry(t *testing.T) {
	ctx := context.Background()
	m, st, _ := newManager(t, nil)
	if err := st.PutProject(ctx, &v1.AppProject{Metadata: v1.ObjectMeta{Name: "payments"}, Spec: v1.AppProjectSpec{
		Roles: []v1.ProjectRole{{Name: "ci"}},
	}}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := m.GenerateProjectToken(ctx, "payments", "ci", "short", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := m.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired: %v", err)
	}
	if _, _, err := m.GenerateProjectToken(ctx, "payments", "ci", "next", 0); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetProject(ctx, "payments")
	if toks := p.RoleTokens("ci"); len(toks) != 1 || toks[0].ID != "next" {
		t.Fatalf("expired tokens are dropped: %+v", toks)
	}
}
