// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/store"
)

func TestSyncAccounts(t *testing.T) {
	ctx := context.Background()
	m, st, dir := newManager(t, nil)
	pw := initialPassword(t, dir)

	declared := []DeclaredAccount{
		{Name: "alice", Capabilities: []string{CapabilityLogin}, Enabled: true},
		{Name: "ci", Capabilities: []string{CapabilityAPIKey}, Enabled: true},
		{Name: AdminUser, Capabilities: []string{CapabilityLogin, CapabilityAPIKey}, Enabled: true},
	}
	if err := m.SyncAccounts(ctx, declared); err != nil {
		t.Fatal(err)
	}
	accts, _ := st.ListAccounts(ctx)
	var names []string
	for _, a := range accts {
		names = append(names, a.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"admin", "alice", "ci"}) {
		t.Fatalf("accounts = %v", names)
	}
	admin, _ := st.GetAccount(ctx, AdminUser)
	if !slices.Equal(admin.Capabilities, []string{CapabilityLogin, CapabilityAPIKey}) || admin.PasswordHash == "" {
		t.Fatalf("admin = %+v", admin)
	}
	// A declared account has no password until one is set.
	if _, _, err := m.Login(ctx, "alice", ""); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login without a password: %v", err)
	}
	if err := m.SetPassword(ctx, "alice", "alice-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Login(ctx, "alice", "alice-password"); err != nil {
		t.Fatal(err)
	}
	// Without the login capability, a correct password does not log in.
	if err := m.SetPassword(ctx, "ci", "ci-password"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Login(ctx, "ci", "ci-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login of an apiKey-only account: %v", err)
	}

	// Disabling keeps the password; dropping the declaration deletes the
	// account; the admin goes back to login only, and stays.
	if err := m.SyncAccounts(ctx, []DeclaredAccount{{Name: "alice", Capabilities: []string{CapabilityLogin}}}); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetAccount(ctx, "alice"); a.Enabled || a.PasswordHash == "" {
		t.Fatalf("alice = %+v", a)
	}
	if _, _, err := m.Login(ctx, "alice", "alice-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login of a disabled account: %v", err)
	}
	if _, err := st.GetAccount(ctx, "ci"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("ci: %v", err)
	}
	if a, _ := st.GetAccount(ctx, AdminUser); !slices.Equal(a.Capabilities, []string{CapabilityLogin}) {
		t.Fatalf("admin = %+v", a)
	}
	if _, _, err := m.Login(ctx, AdminUser, pw); err != nil {
		t.Fatal(err)
	}
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	m, st, _ := newManager(t, nil)
	if err := m.SyncAccounts(ctx, []DeclaredAccount{
		{Name: "ci", Capabilities: []string{CapabilityAPIKey}, Enabled: true},
		{Name: "alice", Capabilities: []string{CapabilityLogin}, Enabled: true},
	}); err != nil {
		t.Fatal(err)
	}

	// Only an account with the apiKey capability has tokens.
	var re *RequestError
	if _, _, err := m.GenerateToken(ctx, "alice", "", 0); !errors.As(err, &re) {
		t.Fatalf("token for alice: %v", err)
	}
	if _, _, err := m.GenerateToken(ctx, "nobody", "", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("token for nobody: %v", err)
	}

	tok, rec, err := m.GenerateToken(ctx, "ci", "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID != "deploy" || rec.ExpiresAt != nil {
		t.Fatalf("record = %+v", rec)
	}
	id, err := m.Verify(ctx, tok)
	if err != nil || id.Username != "ci" || id.Provider != ProviderLocal || id.TokenID != "deploy" {
		t.Fatalf("verify: %+v %v", id, err)
	}
	if _, _, err := m.GenerateToken(ctx, "ci", "deploy", 0); !errors.As(err, &re) {
		t.Fatalf("duplicate ID: %v", err)
	}
	random, rec2, err := m.GenerateToken(ctx, "ci", "", time.Hour)
	if err != nil || rec2.ID == "" || rec2.ExpiresAt == nil || rec2.ExpiresAt.Sub(rec2.IssuedAt) != time.Hour {
		t.Fatalf("random ID: %+v %v", rec2, err)
	}
	if _, err := m.Verify(ctx, random); err != nil {
		t.Fatal(err)
	}

	// A password change leaves tokens alone.
	if err := m.SetPassword(ctx, "ci", "ci-password-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, tok); err != nil {
		t.Fatalf("after a password change: %v", err)
	}

	// Deleting revokes at once on this replica.
	if err := m.DeleteToken(ctx, "ci", rec2.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, random); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("deleted token: %v", err)
	}
	if err := m.DeleteToken(ctx, "ci", rec2.ID); !errors.As(err, &re) || !re.NotFound {
		t.Fatalf("delete twice: %v", err)
	}

	// A token generated again under a deleted ID is another token: the
	// old one stays revoked.
	if err := m.DeleteToken(ctx, "ci", "deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MutateAccount(ctx, "ci", func(a *store.Account) error {
		a.Tokens = append(a.Tokens, store.APIToken{ID: "deploy", IssuedAt: rec.IssuedAt.Add(time.Second)})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.accounts.Delete("ci")
	if _, err := m.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("old token under a reused ID: %v", err)
	}

	// Losing the capability, or the account being disabled, revokes them.
	fresh, _, err := m.GenerateToken(ctx, "ci", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SyncAccounts(ctx, []DeclaredAccount{{Name: "ci", Capabilities: []string{CapabilityLogin}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, fresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("without the capability: %v", err)
	}
	if err := m.SyncAccounts(ctx, []DeclaredAccount{{Name: "ci", Capabilities: []string{CapabilityAPIKey}, Enabled: false}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, fresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("disabled account: %v", err)
	}
	if _, _, err := m.GenerateToken(ctx, "ci", "", 0); !errors.As(err, &re) {
		t.Fatalf("token for a disabled account: %v", err)
	}
	if err := m.SyncAccounts(ctx, []DeclaredAccount{{Name: "ci", Capabilities: []string{CapabilityAPIKey}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, fresh); err != nil {
		t.Fatalf("enabled again: %v", err)
	}

	// Bad IDs.
	for _, bad := range []string{"a b", "a/b", "x?y", string(make([]byte, 65))} {
		if _, _, err := m.GenerateToken(ctx, "ci", bad, 0); !errors.As(err, &re) {
			t.Errorf("ID %q: %v", bad, err)
		}
	}
}

func TestAPITokenExpiry(t *testing.T) {
	ctx := context.Background()
	m, st, _ := newManager(t, nil)
	if err := m.SyncAccounts(ctx, []DeclaredAccount{{Name: "ci", Capabilities: []string{CapabilityAPIKey}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := m.GenerateToken(ctx, "ci", "short", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, tok); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := m.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expired token: %v", err)
	}
	// Expired tokens are dropped from the account when the next one is
	// generated.
	if _, _, err := m.GenerateToken(ctx, "ci", "next", 0); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetAccount(ctx, "ci")
	if len(a.Tokens) != 1 || a.Tokens[0].ID != "next" {
		t.Fatalf("tokens = %+v", a.Tokens)
	}
}

func TestAdminTokensFollowAdminEnabled(t *testing.T) {
	ctx := context.Background()
	m, st, _ := newManager(t, nil)
	if err := m.SyncAccounts(ctx, []DeclaredAccount{{Name: AdminUser, Capabilities: []string{CapabilityLogin, CapabilityAPIKey}, Enabled: true}}); err != nil {
		t.Fatal(err)
	}
	tok, _, err := m.GenerateToken(ctx, AdminUser, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := m.Verify(ctx, tok); err != nil || !slices.Contains(id.Groups, "role:admin") {
		t.Fatalf("admin token: %+v %v", id, err)
	}
	// The same store, the admin disabled.
	off, err := NewManager(ctx, Options{Store: st, AdminEnabled: false, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := off.Verify(ctx, tok); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("admin token with the admin disabled: %v", err)
	}
	var re *RequestError
	if _, _, err := off.GenerateToken(ctx, AdminUser, "", 0); !errors.As(err, &re) {
		t.Fatalf("token for a disabled admin: %v", err)
	}
}

func TestCheckPasswordThrottles(t *testing.T) {
	ctx := context.Background()
	m, _, dir := newManager(t, nil)
	pw := initialPassword(t, dir)
	for range 5 {
		if err := m.CheckPassword(ctx, AdminUser, "wrong"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatal(err)
		}
	}
	if err := m.CheckPassword(ctx, AdminUser, pw); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("after 5 failures: %v", err)
	}
}
