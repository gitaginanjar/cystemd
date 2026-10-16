// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package auth authenticates API callers: local accounts (bcrypt), Nomad
// ACL tokens exchanged for a session, single sign-on through Nomad's OIDC
// auth methods, and CYSTEMD v4 Ed25519 JWTs (so existing automation keeps
// working). Sessions are HS256 JWTs signed with a key generated once and
// kept in the store.
package auth

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"

	"github.com/gitaginanjar/cystemd/internal/store"
)

// CookieName carries the session token in the browser.
const CookieName = "cystemd.token"

// AdminUser is the bootstrap account.
const AdminUser = "admin"

// Capabilities of local accounts (Argo CD's): login with a password, and
// API tokens.
const (
	CapabilityLogin  = "login"
	CapabilityAPIKey = "apiKey"
)

// Providers of an identity.
const (
	ProviderLocal     = "local"
	ProviderNomad     = "nomad"
	ProviderSSO       = "sso"
	ProviderLegacy    = "legacy"
	ProviderAnonymous = "anonymous"
)

var (
	// ErrInvalidCredentials hides whether the user or the password was wrong.
	ErrInvalidCredentials = errors.New("invalid username or password")
	// ErrTooManyAttempts is returned while a user is throttled.
	ErrTooManyAttempts = errors.New("too many failed login attempts; try again later")
	// ErrUnauthenticated means no valid credentials were presented.
	ErrUnauthenticated = errors.New("no session information")
)

// Identity is an authenticated caller.
type Identity struct {
	Username string   `json:"username"`
	Groups   []string `json:"groups,omitempty"`
	Provider string   `json:"provider"`
	// IssuedAt is when the session started.
	IssuedAt time.Time `json:"iat"`
	// TokenID is the API token used, if any.
	TokenID string `json:"tokenId,omitempty"`
	// generation is the local account's session generation at login.
	generation int64
}

// Claims are the session JWT claims.
type Claims struct {
	jwt.RegisteredClaims
	Groups   []string `json:"groups,omitempty"`
	Provider string   `json:"provider,omitempty"`
	// Generation is the local account's session generation (see
	// store.Account.SessionGeneration).
	Generation int64 `json:"gen,omitempty"`
	// APIKey marks an API token, valid while the account lists its ID.
	APIKey bool `json:"apikey,omitempty"`
}

// NomadToken is what CYSTEMD needs of a Nomad ACL token.
type NomadToken struct {
	Name       string
	AccessorID string
	Management bool
	Policies   []string
	Roles      []string
}

// groups maps the token to RBAC groups: nomad:management for a management
// token, nomad:policy:<name> and nomad:role:<name> otherwise.
func (t *NomadToken) groups() []string {
	var out []string
	if t.Management {
		out = append(out, "nomad:management")
	}
	for _, p := range t.Policies {
		out = append(out, "nomad:policy:"+p)
	}
	for _, r := range t.Roles {
		out = append(out, "nomad:role:"+r)
	}
	return out
}

// name is the token's name, or the start of its accessor ID.
func (t *NomadToken) name() string {
	if t.Name != "" {
		return t.Name
	}
	return t.AccessorID[:min(8, len(t.AccessorID))]
}

// NomadTokenResolver looks a Nomad ACL token up by its secret.
type NomadTokenResolver func(ctx context.Context, secret string) (*NomadToken, error)

// Options configures a Manager.
type Options struct {
	Store         *store.Store
	SessionTTL    time.Duration
	AdminEnabled  bool
	AnonymousRole string
	// DataDir receives initial-admin-password on first start.
	DataDir string
	// AdminPasswordFile presets the admin password (bcrypt hash or plain).
	AdminPasswordFile string
	// LegacyPublicKey is an OpenSSH ssh-ed25519 public key (v4 JWTs).
	LegacyPublicKey string
	NomadToken      NomadTokenResolver
	Logger          *slog.Logger
}

// Manager issues and verifies sessions.
type Manager struct {
	store     *store.Store
	key       []byte
	ttl       time.Duration
	legacy    ed25519.PublicKey
	nomad     NomadTokenResolver
	adminOn   bool
	anonymous string
	logger    *slog.Logger
	throttle  *throttle
	accounts  sync.Map // username → cachedAccount
}

type cachedAccount struct {
	generation int64
	enabled    bool
	apiKey     bool
	groups     []string
	tokens     map[string]bool
	at         time.Time
}

// NewManager loads (or creates) the signing key and bootstraps the admin.
func NewManager(ctx context.Context, o Options) (*Manager, error) {
	if o.SessionTTL <= 0 {
		o.SessionTTL = 24 * time.Hour
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	m := &Manager{store: o.Store, ttl: o.SessionTTL, nomad: o.NomadToken, adminOn: o.AdminEnabled,
		anonymous: o.AnonymousRole, logger: o.Logger, throttle: newThrottle(5, 5*time.Minute)}

	fresh := make([]byte, 32)
	if _, err := rand.Read(fresh); err != nil {
		return nil, err
	}
	key, err := o.Store.SetSettingIfAbsent(ctx, "server.signingkey", []byte(base64.StdEncoding.EncodeToString(fresh)))
	if err != nil {
		return nil, fmt.Errorf("signing key: %w", err)
	}
	if m.key, err = base64.StdEncoding.DecodeString(string(key)); err != nil || len(m.key) < 32 {
		return nil, errors.New("signing key in the store is corrupt")
	}

	if o.LegacyPublicKey != "" {
		pk, err := parseEd25519(o.LegacyPublicKey)
		if err != nil {
			return nil, fmt.Errorf("legacy public key: %w", err)
		}
		m.legacy = pk
	}
	if o.AdminEnabled {
		if err := bootstrapAdmin(ctx, o.Store, o.DataDir, o.AdminPasswordFile, o.Logger); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// bootstrapAdmin creates the admin account on first start: with the preset
// password when given, else a random one written to
// <data_dir>/initial-admin-password (0600) — never to the log.
func bootstrapAdmin(ctx context.Context, st *store.Store, dataDir, presetFile string, logger *slog.Logger) error {
	if _, err := st.GetAccount(ctx, AdminUser); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	var hash string
	var generated string
	if presetFile != "" {
		b, err := os.ReadFile(presetFile)
		if err != nil {
			return fmt.Errorf("admin password file: %w", err)
		}
		p := strings.TrimSpace(string(b))
		if strings.HasPrefix(p, "$2") {
			hash = p
		} else if hash, err = HashPassword(p); err != nil {
			return err
		}
	} else {
		generated = randomPassword()
		var err error
		if hash, err = HashPassword(generated); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	created, err := st.CreateAccountIfAbsent(ctx, &store.Account{
		Name: AdminUser, PasswordHash: hash, PasswordMtime: &now, Enabled: true,
		Capabilities: []string{"login"}, Groups: []string{"role:admin"},
	})
	if err != nil || !created {
		return err
	}
	if generated != "" {
		path := filepath.Join(dataDir, "initial-admin-password")
		if err := os.MkdirAll(dataDir, 0o700); err == nil {
			if err := os.WriteFile(path, []byte(generated+"\n"), 0o600); err == nil {
				logger.Warn("created the admin account; its initial password is in a file readable only by this user — change it after the first login",
					"file", path)
				return nil
			}
		}
		logger.Error("created the admin account but could not write its initial password; set auth.admin_password_file and delete the account to reset", "dir", dataDir)
	}
	return nil
}

func randomPassword() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}

// HashPassword bcrypts a password.
func HashPassword(p string) (string, error) {
	if len(p) < 8 {
		return "", errors.New("password must be at least 8 characters")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(p), bcrypt.DefaultCost)
	return string(h), err
}

// Login checks a local account's password and issues a session.
func (m *Manager) Login(ctx context.Context, username, password string) (string, *Identity, error) {
	if username == AdminUser && !m.adminOn {
		return "", nil, ErrInvalidCredentials
	}
	if !m.throttle.allow(username) {
		return "", nil, ErrTooManyAttempts
	}
	acct, err := m.store.GetAccount(ctx, username)
	if err != nil || !acct.Enabled || !slices.Contains(acct.Capabilities, CapabilityLogin) {
		// Burn comparable time so account existence does not leak.
		_ = bcrypt.CompareHashAndPassword([]byte("$2a$10$CwTycUXWue0Thq9StjUM0uJ8.Ugt2r1wJm1RB5rjVEyk1Fh3jwqBi"), []byte(password))
		m.throttle.fail(username)
		return "", nil, ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(acct.PasswordHash), []byte(password)) != nil {
		m.throttle.fail(username)
		return "", nil, ErrInvalidCredentials
	}
	m.throttle.reset(username)
	id := &Identity{Username: username, Groups: acct.Groups, Provider: ProviderLocal, IssuedAt: time.Now().UTC().Truncate(time.Second),
		generation: acct.SessionGeneration}
	tok, err := m.issue(id)
	return tok, id, err
}

// LoginNomadToken exchanges a Nomad ACL token for a session.
func (m *Manager) LoginNomadToken(ctx context.Context, token string) (string, *Identity, error) {
	if m.nomad == nil {
		return "", nil, errors.New("Nomad token login is disabled")
	}
	key := "nomad-token"
	if !m.throttle.allow(key) {
		return "", nil, ErrTooManyAttempts
	}
	nt, err := m.nomad(ctx, token)
	if err != nil {
		m.throttle.fail(key)
		return "", nil, fmt.Errorf("Nomad rejected the token: %w", err)
	}
	id := &Identity{Username: "nomad:" + nt.name(), Groups: nt.groups(), Provider: ProviderNomad,
		IssuedAt: time.Now().UTC().Truncate(time.Second)}
	tok, err := m.issue(id)
	return tok, id, err
}

// LoginSSO starts a session for a user who logged in through one of
// Nomad's OIDC auth methods, which issued nt. The user name is the token's,
// which the auth method's token_name_format makes from the user's claims.
func (m *Manager) LoginSSO(nt *NomadToken) (string, *Identity, error) {
	id := &Identity{Username: nt.name(), Groups: nt.groups(), Provider: ProviderSSO,
		IssuedAt: time.Now().UTC().Truncate(time.Second)}
	tok, err := m.issue(id)
	return tok, id, err
}

func (m *Manager) issue(id *Identity) (string, error) {
	jti := make([]byte, 12)
	_, _ = rand.Read(jti)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    "cystemd",
			Subject:   id.Username,
			IssuedAt:  jwt.NewNumericDate(id.IssuedAt),
			NotBefore: jwt.NewNumericDate(id.IssuedAt),
			ExpiresAt: jwt.NewNumericDate(id.IssuedAt.Add(m.ttl)),
			ID:        base64.RawURLEncoding.EncodeToString(jti),
		},
		Groups:     id.Groups,
		Provider:   id.Provider,
		Generation: id.generation,
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.key)
}

// Verify validates a session token (or a v4 Ed25519 JWT).
func (m *Manager) Verify(ctx context.Context, token string) (*Identity, error) {
	parser := jwt.NewParser(jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	var claims Claims
	tok, err := parser.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		switch t.Method.(type) {
		case *jwt.SigningMethodHMAC:
			if t.Method.Alg() != jwt.SigningMethodHS256.Alg() {
				return nil, fmt.Errorf("unexpected algorithm %s", t.Method.Alg())
			}
			return m.key, nil
		case *jwt.SigningMethodEd25519:
			if m.legacy == nil {
				return nil, errors.New("legacy tokens are not accepted")
			}
			return m.legacy, nil
		}
		return nil, fmt.Errorf("unexpected signing method %s", t.Method.Alg())
	})
	if err != nil || !tok.Valid {
		return nil, ErrUnauthenticated
	}
	if _, ok := tok.Method.(*jwt.SigningMethodEd25519); ok {
		user := claims.Subject
		if user == "" {
			user = "legacy-jwt"
		}
		// v4 tokens authorized every mutating endpoint: keep that meaning.
		return &Identity{Username: user, Groups: []string{"role:admin"}, Provider: ProviderLegacy}, nil
	}
	id := &Identity{Username: claims.Subject, Groups: claims.Groups, Provider: claims.Provider}
	if claims.IssuedAt != nil {
		id.IssuedAt = claims.IssuedAt.Time
	}
	if id.Provider == ProviderLocal {
		st, err := m.accountState(ctx, id.Username)
		if err != nil || !st.enabled {
			return nil, ErrUnauthenticated
		}
		if claims.APIKey {
			// An API token lives while the account lists it; a password
			// change does not revoke it.
			if !st.apiKey || !st.tokens[claims.ID] {
				return nil, ErrUnauthenticated
			}
			id.TokenID = claims.ID
		} else if claims.Generation != st.generation {
			// A password change revokes older sessions: their generation
			// is behind the account's.
			return nil, ErrUnauthenticated
		}
		id.generation, id.Groups = st.generation, st.groups
	}
	return id, nil
}

// accountState is what Verify needs of a local account, cached for a few
// seconds (a change reaches the other control plane replicas within that
// time).
func (m *Manager) accountState(ctx context.Context, user string) (cachedAccount, error) {
	if c, ok := m.accounts.Load(user); ok {
		ca := c.(cachedAccount)
		if time.Since(ca.at) < 10*time.Second {
			return ca, nil
		}
	}
	acct, err := m.store.GetAccount(ctx, user)
	if err != nil {
		return cachedAccount{}, err
	}
	ca := cachedAccount{generation: acct.SessionGeneration, enabled: acct.Enabled, groups: acct.Groups,
		apiKey: slices.Contains(acct.Capabilities, CapabilityAPIKey), tokens: map[string]bool{}, at: time.Now()}
	for _, t := range acct.Tokens {
		ca.tokens[t.ID] = true
	}
	m.accounts.Store(user, ca)
	return ca, nil
}

// FromRequest authenticates a request (bearer token, then session cookie).
// Without credentials it returns the anonymous identity when configured.
func (m *Manager) FromRequest(r *http.Request) (*Identity, error) {
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	} else if c, err := r.Cookie(CookieName); err == nil {
		token = c.Value
	}
	if token == "" {
		if m.anonymous != "" {
			return &Identity{Username: "anonymous", Groups: []string{m.anonymous}, Provider: ProviderAnonymous}, nil
		}
		return nil, ErrUnauthenticated
	}
	return m.Verify(r.Context(), token)
}

// UpdatePassword changes a local account's password (sessions issued
// before the change stop working).
func (m *Manager) UpdatePassword(ctx context.Context, user, current, next string) error {
	acct, err := m.store.GetAccount(ctx, user)
	if err != nil {
		return ErrInvalidCredentials
	}
	if bcrypt.CompareHashAndPassword([]byte(acct.PasswordHash), []byte(current)) != nil {
		return ErrInvalidCredentials
	}
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	acct.PasswordHash, acct.PasswordMtime, acct.MustResetPassword = hash, &now, false
	acct.SessionGeneration++
	m.accounts.Delete(user)
	return m.store.PutAccount(ctx, acct)
}

// TTL is the session lifetime.
func (m *Manager) TTL() time.Duration { return m.ttl }

// DeclaredAccount is a local account of the configuration.
type DeclaredAccount struct {
	Name         string
	Capabilities []string
	Enabled      bool
}

// SyncAccounts makes the local accounts the declared ones, besides the
// admin: missing ones are created (without a password), the capabilities
// and enabled state of the others updated, undeclared ones deleted.
// Passwords and tokens are kept.
func (m *Manager) SyncAccounts(ctx context.Context, declared []DeclaredAccount) error {
	want := map[string]DeclaredAccount{}
	for _, d := range declared {
		want[d.Name] = d
	}
	existing, err := m.store.ListAccounts(ctx)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, a := range existing {
		have[a.Name] = true
		d, ok := want[a.Name]
		switch {
		case a.Name == AdminUser:
		case !ok:
			if err := m.store.DeleteAccount(ctx, a.Name); err != nil {
				return err
			}
			m.logger.Info("deleted a local account no longer declared", "account", a.Name)
		case a.Enabled != d.Enabled || !slices.Equal(a.Capabilities, d.Capabilities):
			if _, err := m.store.MutateAccount(ctx, a.Name, func(acct *store.Account) error {
				acct.Enabled, acct.Capabilities = d.Enabled, d.Capabilities
				return nil
			}); err != nil {
				return err
			}
		}
		m.accounts.Delete(a.Name)
	}
	for _, d := range declared {
		if have[d.Name] {
			continue
		}
		if _, err := m.store.CreateAccountIfAbsent(ctx, &store.Account{Name: d.Name, Enabled: d.Enabled, Capabilities: d.Capabilities}); err != nil {
			return err
		}
		m.logger.Info("created a local account", "account", d.Name, "capabilities", d.Capabilities)
	}
	return nil
}

// GenerateToken issues an API token for an account with the apiKey
// capability; ttl 0 never expires. An empty id is chosen at random.
func (m *Manager) GenerateToken(ctx context.Context, account, id string, ttl time.Duration) (string, store.APIToken, error) {
	if id == "" {
		b := make([]byte, 9)
		_, _ = rand.Read(b)
		id = base64.RawURLEncoding.EncodeToString(b)
	} else if strings.ContainsAny(id, " /") || len(id) > 64 {
		return "", store.APIToken{}, errors.New("a token ID has at most 64 characters, without spaces or slashes")
	}
	now := time.Now().UTC().Truncate(time.Second)
	rec := store.APIToken{ID: id, IssuedAt: now}
	exp := now.AddDate(100, 0, 0) // does not expire
	if ttl > 0 {
		e := now.Add(ttl)
		rec.ExpiresAt, exp = &e, e
	}
	acct, err := m.store.MutateAccount(ctx, account, func(a *store.Account) error {
		if !a.Enabled {
			return fmt.Errorf("account %s is disabled", account)
		}
		if !slices.Contains(a.Capabilities, CapabilityAPIKey) {
			return fmt.Errorf("account %s does not have the apiKey capability", account)
		}
		var kept []store.APIToken
		for _, t := range a.Tokens {
			if t.ID == id {
				return fmt.Errorf("account %s already has a token %q", account, id)
			}
			if t.ExpiresAt == nil || t.ExpiresAt.After(now) {
				kept = append(kept, t)
			}
		}
		a.Tokens = append(kept, rec)
		return nil
	})
	if err != nil {
		return "", store.APIToken{}, err
	}
	m.accounts.Delete(account)
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: "cystemd", Subject: account, ID: id,
			IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(exp),
		},
		Groups: acct.Groups, Provider: ProviderLocal, APIKey: true,
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.key)
	return tok, rec, err
}

// DeleteToken revokes an account's API token.
func (m *Manager) DeleteToken(ctx context.Context, account, id string) error {
	_, err := m.store.MutateAccount(ctx, account, func(a *store.Account) error {
		i := slices.IndexFunc(a.Tokens, func(t store.APIToken) bool { return t.ID == id })
		if i < 0 {
			return fmt.Errorf("account %s has no token %q", account, id)
		}
		a.Tokens = slices.Delete(a.Tokens, i, i+1)
		return nil
	})
	m.accounts.Delete(account)
	return err
}

// CheckPassword verifies a local account's password.
func (m *Manager) CheckPassword(ctx context.Context, user, password string) error {
	acct, err := m.store.GetAccount(ctx, user)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(acct.PasswordHash), []byte(password)) != nil {
		return ErrInvalidCredentials
	}
	return nil
}

// SetPassword sets a local account's password, an administrator's doing
// (the caller is checked by the API); the account's sessions end.
func (m *Manager) SetPassword(ctx context.Context, account, next string) error {
	hash, err := HashPassword(next)
	if err != nil {
		return err
	}
	_, err = m.store.MutateAccount(ctx, account, func(a *store.Account) error {
		now := time.Now().UTC()
		a.PasswordHash, a.PasswordMtime, a.MustResetPassword = hash, &now, false
		a.SessionGeneration++
		return nil
	})
	m.accounts.Delete(account)
	return err
}

func parseEd25519(key string) (ed25519.PublicKey, error) {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(key)))
	if err != nil {
		return nil, err
	}
	cpk, ok := pk.(ssh.CryptoPublicKey)
	if !ok {
		return nil, errors.New("not a crypto public key")
	}
	ed, ok := cpk.CryptoPublicKey().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key type %s is not ssh-ed25519", pk.Type())
	}
	return ed, nil
}

// throttle limits failed logins per key (brute-force protection).
type throttle struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	fails  map[string][]time.Time
}

func newThrottle(max int, window time.Duration) *throttle {
	return &throttle{max: max, window: window, fails: map[string][]time.Time{}}
}

func (t *throttle) prune(key string, now time.Time) []time.Time {
	var keep []time.Time
	for _, f := range t.fails[key] {
		if now.Sub(f) < t.window {
			keep = append(keep, f)
		}
	}
	t.fails[key] = keep
	return keep
}

func (t *throttle) allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.prune(key, time.Now())) < t.max
}

func (t *throttle) fail(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fails[key] = append(t.prune(key, time.Now()), time.Now())
	if len(t.fails) > 10000 { // bound memory under a spray of usernames
		t.fails = map[string][]time.Time{}
	}
}

func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
}
