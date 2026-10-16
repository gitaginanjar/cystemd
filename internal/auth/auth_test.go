// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/ssh"

	"github.com/gitaginanjar/cystemd/internal/store"
)

func newManager(t *testing.T, mutate func(*Options)) (*Manager, *store.Store, string) {
	t.Helper()
	st := store.New(store.NewMemoryKV(), "cystemd")
	dir := t.TempDir()
	o := Options{Store: st, AdminEnabled: true, DataDir: dir}
	if mutate != nil {
		mutate(&o)
	}
	m, err := NewManager(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return m, st, dir
}

func initialPassword(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "initial-admin-password"))
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "initial-admin-password"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("initial password file mode %o", fi.Mode().Perm())
	}
	return strings.TrimSpace(string(b))
}

func TestBootstrapLoginAndPasswordChange(t *testing.T) {
	ctx := context.Background()
	m, st, dir := newManager(t, nil)
	pw := initialPassword(t, dir)

	// A second replica does not overwrite the admin or the signing key.
	m2, err := NewManager(ctx, Options{Store: st, AdminEnabled: true, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}

	tok, id, err := m.Login(ctx, "admin", pw)
	if err != nil || id.Username != "admin" || id.Groups[0] != "role:admin" {
		t.Fatalf("login: %v %+v", err, id)
	}
	// Sessions verify on every replica (shared key).
	if got, err := m2.Verify(ctx, tok); err != nil || got.Username != "admin" || got.Provider != ProviderLocal {
		t.Fatalf("verify on replica: %v %+v", err, got)
	}
	if _, _, err := m.Login(ctx, "admin", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := m.Login(ctx, "nobody", "x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user: %v", err)
	}

	// Changing the password revokes earlier sessions, and only those: a
	// login right after it works (revocation is by session generation,
	// not by time).
	if err := m.UpdatePassword(ctx, "admin", pw, "a-new-long-password"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, tok); err == nil {
		t.Fatal("session survived a password change")
	}
	tok2, _, err := m.Login(ctx, "admin", "a-new-long-password")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(ctx, tok2); err != nil {
		t.Fatalf("session after the change: %v", err)
	}
	if err := m.UpdatePassword(ctx, "admin", "a-new-long-password", "short"); err == nil {
		t.Fatal("short password accepted")
	}
}

func TestThrottling(t *testing.T) {
	ctx := context.Background()
	m, _, dir := newManager(t, nil)
	pw := initialPassword(t, dir)
	for i := 0; i < 5; i++ {
		_, _, _ = m.Login(ctx, "admin", "bad")
	}
	if _, _, err := m.Login(ctx, "admin", pw); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("throttle not applied: %v", err)
	}
}

func TestTokenTampering(t *testing.T) {
	ctx := context.Background()
	m, _, dir := newManager(t, nil)
	tok, _, _ := m.Login(ctx, "admin", initialPassword(t, dir))
	parts := strings.Split(tok, ".")
	forged := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, err := m.Verify(ctx, forged); err == nil {
		t.Fatal("tampered token accepted")
	}
	// "none" and foreign keys are rejected.
	none := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "admin", "exp": time.Now().Add(time.Hour).Unix()})
	s, _ := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if _, err := m.Verify(ctx, s); err == nil {
		t.Fatal("alg=none accepted")
	}
	other := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: "admin", ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}, Provider: ProviderLocal})
	s, _ = other.SignedString([]byte("not-the-server-key-not-the-server-key"))
	if _, err := m.Verify(ctx, s); err == nil {
		t.Fatal("foreign HMAC key accepted")
	}
}

func TestLegacyEd25519Token(t *testing.T) {
	ctx := context.Background()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	m, _, _ := newManager(t, func(o *Options) { o.LegacyPublicKey = string(ssh.MarshalAuthorizedKey(sshPub)) })

	legacy := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"exp": time.Now().Add(time.Hour).Unix()})
	s, err := legacy.SignedString(priv)
	if err != nil {
		t.Fatal(err)
	}
	id, err := m.Verify(ctx, s)
	if err != nil || id.Provider != ProviderLegacy || id.Groups[0] != "role:admin" {
		t.Fatalf("legacy token: %v %+v", err, id)
	}
	// Without exp a v4 token is refused (v4 required it too).
	noExp, _ := jwt.NewWithClaims(jwt.SigningMethodEdDSA, jwt.MapClaims{"sub": "x"}).SignedString(priv)
	if _, err := m.Verify(ctx, noExp); err == nil {
		t.Fatal("token without exp accepted")
	}
	// A manager without the legacy key refuses EdDSA tokens.
	plain, _, _ := newManager(t, nil)
	if _, err := plain.Verify(ctx, s); err == nil {
		t.Fatal("legacy token accepted without a configured key")
	}
}

func TestFromRequestAndAnonymous(t *testing.T) {
	m, _, _ := newManager(t, nil)
	r := httptest.NewRequest("GET", "/api/v1/applications", nil)
	if _, err := m.FromRequest(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no credentials: %v", err)
	}
	anon, _, _ := newManager(t, func(o *Options) { o.AnonymousRole = "role:readonly" })
	id, err := anon.FromRequest(r)
	if err != nil || id.Provider != ProviderAnonymous || id.Groups[0] != "role:readonly" {
		t.Fatalf("anonymous: %v %+v", err, id)
	}
}

func TestAdminDisabled(t *testing.T) {
	ctx := context.Background()
	m, st, _ := newManager(t, func(o *Options) { o.AdminEnabled = false })
	if _, err := st.GetAccount(ctx, "admin"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("admin created while disabled")
	}
	if _, _, err := m.Login(ctx, "admin", "anything"); err == nil {
		t.Fatal("admin login while disabled")
	}
}

func TestNomadTokenLogin(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newManager(t, func(o *Options) {
		o.NomadToken = func(_ context.Context, token string) (*NomadToken, error) {
			switch token {
			case "good":
				return &NomadToken{Name: "ops", AccessorID: "0123456789abcdef", Management: true}, nil
			case "anonymous":
				return &NomadToken{AccessorID: "fedcba9876543210", Policies: []string{"read"}, Roles: []string{"deployers"}}, nil
			}
			return nil, errors.New("ACL token not found")
		}
	})
	tok, id, err := m.LoginNomadToken(ctx, "good")
	if err != nil || id.Provider != ProviderNomad || id.Username != "nomad:ops" {
		t.Fatalf("%v %+v", err, id)
	}
	if got, err := m.Verify(ctx, tok); err != nil || strings.Join(got.Groups, ",") != "nomad:management" {
		t.Fatalf("%v %+v", err, got)
	}
	// A token without a name goes by its accessor; its policies and roles
	// are groups.
	if _, id, err := m.LoginNomadToken(ctx, "anonymous"); err != nil || id.Username != "nomad:fedcba98" ||
		strings.Join(id.Groups, ",") != "nomad:policy:read,nomad:role:deployers" {
		t.Fatalf("%v %+v", err, id)
	}
	if _, _, err := m.LoginNomadToken(ctx, "bad"); err == nil {
		t.Fatal("bad token accepted")
	}
}

func TestSSOLogin(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newManager(t, nil)
	tok, id, err := m.LoginSSO(&NomadToken{Name: "alice@example.com", AccessorID: "0123456789", Roles: []string{"platform"}})
	if err != nil || id.Provider != ProviderSSO || id.Username != "alice@example.com" {
		t.Fatalf("%v %+v", err, id)
	}
	got, err := m.Verify(ctx, tok)
	if err != nil || got.Username != "alice@example.com" || got.Provider != ProviderSSO || strings.Join(got.Groups, ",") != "nomad:role:platform" {
		t.Fatalf("%v %+v", err, got)
	}
}
