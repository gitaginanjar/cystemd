package service

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ─── parseSSHED25519PublicKey ───────────────────────────────────────────────

func TestParseSSHED25519PublicKey_ValidKey(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)

	got, err := parseSSHED25519PublicKey(sshKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !pub.Equal(got) {
		t.Error("parsed public key does not match the original")
	}
}

func TestParseSSHED25519PublicKey_WithComment(t *testing.T) {
	_, _, sshKey := mustGenerateSSHKeyPair(t)
	// Trailing comment fields (the helper's and this one) must be ignored.
	got, err := parseSSHED25519PublicKey(sshKey + " extra comment")
	if err != nil {
		t.Fatalf("unexpected error with extra comment: %v", err)
	}
	if got == nil {
		t.Error("expected non-nil public key")
	}
}

func TestParseSSHED25519PublicKey_WrongKeyType(t *testing.T) {
	_, err := parseSSHED25519PublicKey("ssh-rsa AAAAB3NzaC1yc2EAAAA user@host")
	if err == nil {
		t.Error("expected error for non-ed25519 key type")
	}
}

func TestParseSSHED25519PublicKey_TooFewFields(t *testing.T) {
	_, err := parseSSHED25519PublicKey("ssh-ed25519")
	if err == nil {
		t.Error("expected error for missing base64 blob")
	}
}

func TestParseSSHED25519PublicKey_EmptyString(t *testing.T) {
	_, err := parseSSHED25519PublicKey("")
	if err == nil {
		t.Error("expected error for empty key string")
	}
}

func TestParseSSHED25519PublicKey_InvalidBase64(t *testing.T) {
	_, err := parseSSHED25519PublicKey("ssh-ed25519 !@#$%^&*() user@host")
	if err == nil {
		t.Error("expected error for invalid base64 data")
	}
}

func TestParseSSHED25519PublicKey_TruncatedBlob(t *testing.T) {
	truncated := base64.StdEncoding.EncodeToString([]byte{0, 0, 0})
	_, err := parseSSHED25519PublicKey("ssh-ed25519 " + truncated + " user@host")
	if err == nil {
		t.Error("expected error for truncated key blob")
	}
}

// TestParseSSHED25519PublicKey_HugeLength_NoPanic pins the uint32-overflow fix: a
// near-2^32 key-type length returns a clean error instead of wrapping
// 4+keyTypeLen+4 past the bounds check and panicking the running server.
func TestParseSSHED25519PublicKey_HugeLength_NoPanic(t *testing.T) {
	// First 4 bytes = 0xFFFFFFF8 (huge key-type length); blob is far too short.
	blob := []byte{0xFF, 0xFF, 0xFF, 0xF8, 0x01, 0x02}
	encoded := base64.StdEncoding.EncodeToString(blob)
	_, err := parseSSHED25519PublicKey("ssh-ed25519 " + encoded + " user@host")
	if err == nil {
		t.Fatal("expected a clean error for an overflowing key-type length, got nil")
	}
}

func TestParseSSHED25519PublicKey_WrongKeyTypeInBlob(t *testing.T) {
	// Build a blob that claims ssh-rsa inside but is labeled ssh-ed25519 outside
	keyType := "ssh-rsa"
	buf := buildSSHBlobWithType(keyType, make([]byte, 32))
	encoded := base64.StdEncoding.EncodeToString(buf)
	_, err := parseSSHED25519PublicKey("ssh-ed25519 " + encoded + " user@host")
	if err == nil {
		t.Error("expected error when blob key type doesn't match ssh-ed25519")
	}
}

func TestParseSSHED25519PublicKey_WrongKeySize(t *testing.T) {
	// Build a valid-looking blob with 16-byte key instead of 32
	buf := buildSSHBlobWithType("ssh-ed25519", make([]byte, 16))
	encoded := base64.StdEncoding.EncodeToString(buf)
	_, err := parseSSHED25519PublicKey("ssh-ed25519 " + encoded + " user@host")
	if err == nil {
		t.Error("expected error for wrong key size (16 instead of 32)")
	}
}

func TestParseSSHED25519PublicKey_KeyTypeTruncated(t *testing.T) {
	// 4-byte blob declaring keyTypeLen = 0x7fffffff but no type bytes follow.
	buf := []byte{0x7f, 0xff, 0xff, 0xff}
	encoded := base64.StdEncoding.EncodeToString(buf)
	_, err := parseSSHED25519PublicKey("ssh-ed25519 " + encoded + " user@host")
	if err == nil {
		t.Error("expected error when key type length exceeds blob size")
	}
}

func TestParseSSHED25519PublicKey_KeyDataTruncated(t *testing.T) {
	// Valid ssh-ed25519 header + keyDataLen that overruns the actual data.
	typeBytes := []byte("ssh-ed25519")
	buf := make([]byte, 4+len(typeBytes)+4)
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(typeBytes)))
	copy(buf[4:], typeBytes)
	binary.BigEndian.PutUint32(buf[4+len(typeBytes):], 1000) // declares 1000 bytes but provides 0
	encoded := base64.StdEncoding.EncodeToString(buf)
	_, err := parseSSHED25519PublicKey("ssh-ed25519 " + encoded + " user@host")
	if err == nil {
		t.Error("expected error when key data length exceeds blob size")
	}
}

// ─── InitAuth ──────────────────────────────────────────────────────────────

func TestInitAuth_Disabled(t *testing.T) {
	cfg := &AuthConfig{Enabled: false}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("InitAuth with disabled auth should not error: %v", err)
	}
}

func TestInitAuth_LogsSourceWhenDisabled(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	if err := InitAuth(&AuthConfig{Enabled: false}); err != nil {
		t.Fatalf("InitAuth: %v", err)
	}
	if !strings.Contains(buf.String(), "auth: disabled") {
		t.Errorf("expected 'auth: disabled' in log; got:\n%s", buf.String())
	}
}

func TestInitAuth_LogsEnvKeySource(t *testing.T) {
	_, _, sshKey := mustGenerateSSHKeyPair(t)
	t.Setenv("PUBLIC_KEY", sshKey)

	buf := captureLogs(t, LevelInfo)
	if err := InitAuth(&AuthConfig{Enabled: true}); err != nil {
		t.Fatalf("InitAuth: %v", err)
	}
	if !strings.Contains(buf.String(), "public key loaded from PUBLIC_KEY env") {
		t.Errorf("expected env-source log line; got:\n%s", buf.String())
	}
}

func TestInitAuth_LogsFileKeySource(t *testing.T) {
	os.Unsetenv("PUBLIC_KEY")

	_, _, sshKey := mustGenerateSSHKeyPair(t)
	f, err := os.CreateTemp("", "jwt_pub_*.pub")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(sshKey)
	f.Close()
	defer os.Remove(f.Name())

	buf := captureLogs(t, LevelInfo)
	if err := InitAuth(&AuthConfig{Enabled: true, PublicKeyFile: f.Name()}); err != nil {
		t.Fatalf("InitAuth: %v", err)
	}
	want := "public key loaded from " + f.Name()
	if !strings.Contains(buf.String(), want) {
		t.Errorf("expected %q in log; got:\n%s", want, buf.String())
	}
}

func TestInitAuth_FromEnvVar(t *testing.T) {
	_, _, sshKey := mustGenerateSSHKeyPair(t)
	t.Setenv("PUBLIC_KEY", sshKey)

	cfg := &AuthConfig{Enabled: true}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("InitAuth from env: unexpected error: %v", err)
	}
}

func TestInitAuth_FromFile(t *testing.T) {
	os.Unsetenv("PUBLIC_KEY")

	_, _, sshKey := mustGenerateSSHKeyPair(t)
	f, err := os.CreateTemp("", "jwt_pub_*.pub")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(sshKey)
	f.Close()
	defer os.Remove(f.Name())

	cfg := &AuthConfig{Enabled: true, PublicKeyFile: f.Name()}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("InitAuth from file: unexpected error: %v", err)
	}
}

func TestInitAuth_FileNotFound(t *testing.T) {
	os.Unsetenv("PUBLIC_KEY")

	cfg := &AuthConfig{Enabled: true, PublicKeyFile: "/nonexistent/key.pub"}
	if err := InitAuth(cfg); err == nil {
		t.Error("expected error for missing public key file")
	}
}

func TestInitAuth_EnvVarTakesPrecedenceOverFile(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	t.Setenv("PUBLIC_KEY", sshKey)

	cfg := &AuthConfig{Enabled: true, PublicKeyFile: "/should/not/be/read.pub"}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("InitAuth env precedence: unexpected error: %v", err)
	}
	if !publicKey.Equal(pub) {
		t.Error("publicKey should have been loaded from env var")
	}
}

func TestInitAuth_InvalidEnvKey(t *testing.T) {
	t.Setenv("PUBLIC_KEY", "ssh-ed25519 not_valid_base64 user@host")

	cfg := &AuthConfig{Enabled: true}
	if err := InitAuth(cfg); err == nil {
		t.Error("expected error for invalid PUBLIC_KEY env var")
	}
}

// ─── RequireJWT ────────────────────────────────────────────────────────────

func TestRequireJWT_Disabled_PassesThrough(t *testing.T) {
	cfg := &AuthConfig{Enabled: false}
	called := false
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	handler(w, r)

	if !called {
		t.Error("inner handler should be called when auth is disabled")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestRequireJWT_MissingAuthHeader(t *testing.T) {
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	_ = priv
	mustSetPublicKey(t, pub, sshKey)

	cfg := &AuthConfig{Enabled: true}
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	w := httptest.NewRecorder()
	handler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing header, got %d", w.Code)
	}
}

func TestRequireJWT_ValidToken(t *testing.T) {
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	tokenStr := mustSignToken(t, priv)
	cfg := &AuthConfig{Enabled: true}
	called := false
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	handler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 for valid token, got %d", w.Code)
	}
	if !called {
		t.Error("inner handler should be called for valid token")
	}
}

func TestRequireJWT_InvalidToken(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	cfg := &AuthConfig{Enabled: true}
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("Authorization", "Bearer totally.invalid.token")
	w := httptest.NewRecorder()
	handler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid token, got %d", w.Code)
	}
}

func TestRequireJWT_WrongSigningKey(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	// Sign with a DIFFERENT private key
	_, wrongPriv, _ := mustGenerateSSHKeyPair(t)
	tokenStr := mustSignToken(t, wrongPriv)

	cfg := &AuthConfig{Enabled: true}
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	handler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for wrong signing key, got %d", w.Code)
	}
}

func TestRequireJWT_BearerPrefixStripped(t *testing.T) {
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	tokenStr := mustSignToken(t, priv)
	cfg := &AuthConfig{Enabled: true}
	called := false
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	handler(w, r)

	if !called {
		t.Error("handler should be called when Bearer prefix is present and token is valid")
	}
}

// TestRequireJWT_UnexpectedSigningMethod pins that a non-Ed25519 (HMAC) token is
// rejected by the signing-method check, a branch distinct from the wrong-key one.
func TestRequireJWT_UnexpectedSigningMethod(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	claims := jwt.MapClaims{
		"sub": "test",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	hmacToken := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := hmacToken.SignedString([]byte("any-shared-secret"))
	if err != nil {
		t.Fatalf("HMAC sign failed: %v", err)
	}

	cfg := &AuthConfig{Enabled: true}
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("Authorization", "Bearer "+signed)
	w := httptest.NewRecorder()
	handler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for non-Ed25519 (HMAC) token, got %d", w.Code)
	}
}

// TestRequireJWT_TokenWithoutExp_Rejected pins jwt.WithExpirationRequired(): a
// token with a valid signature and algorithm but no exp is rejected. See ADR-0006.
func TestRequireJWT_TokenWithoutExp_Rejected(t *testing.T) {
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	claims := jwt.MapClaims{
		"sub": "test-subject",
		"iat": time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tokenStr, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}

	cfg := &AuthConfig{Enabled: true}
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("Authorization", "Bearer "+tokenStr)
	w := httptest.NewRecorder()
	handler(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for token without exp claim, got %d", w.Code)
	}
}

// TestRequireJWT_BareToken_NoBearerPrefix pins that a valid token sent without
// the "Bearer " prefix is accepted.
func TestRequireJWT_BareToken_NoBearerPrefix(t *testing.T) {
	pub, priv, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	tokenStr := mustSignToken(t, priv)
	cfg := &AuthConfig{Enabled: true}
	called := false
	handler := RequireJWT(cfg, func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("Authorization", tokenStr) // no "Bearer " prefix
	w := httptest.NewRecorder()
	handler(w, r)

	if !called {
		t.Error("handler should be called when token is valid even without Bearer prefix")
	}
}

// ─── AUTH_ENABLED env var ─────────────────────────────────────────────────

func TestInitAuth_AuthEnabledEnv_TrueOverridesConfigFalse(t *testing.T) {
	_, _, sshKey := mustGenerateSSHKeyPair(t)
	t.Setenv("PUBLIC_KEY", sshKey)
	t.Setenv("AUTH_ENABLED", "true")

	cfg := &AuthConfig{Enabled: false}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Error("expected AUTH_ENABLED=true to override config Enabled=false")
	}
}

func TestInitAuth_AuthEnabledEnv_FalseOverridesConfigTrue(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "false")
	os.Unsetenv("PUBLIC_KEY")

	cfg := &AuthConfig{Enabled: true, PublicKeyFile: "/nonexistent/should-not-be-read.pub"}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Error("expected AUTH_ENABLED=false to override config Enabled=true")
	}
}

func TestInitAuth_AuthEnabledEnv_CaseInsensitive(t *testing.T) {
	_, _, sshKey := mustGenerateSSHKeyPair(t)
	t.Setenv("PUBLIC_KEY", sshKey)

	for _, val := range []string{"YES", "Yes", "yes", "TRUE", "True", "1"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("AUTH_ENABLED", val)
			cfg := &AuthConfig{Enabled: false}
			if err := InitAuth(cfg); err != nil {
				t.Fatalf("AUTH_ENABLED=%q: unexpected error: %v", val, err)
			}
			if !cfg.Enabled {
				t.Errorf("AUTH_ENABLED=%q should enable auth", val)
			}
		})
	}
}

func TestInitAuth_AuthEnabledEnv_InvalidFallsToConfig(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "maybe")
	os.Unsetenv("PUBLIC_KEY")

	buf := captureLogs(t, LevelDebug)
	cfg := &AuthConfig{Enabled: false}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Enabled {
		t.Error("invalid AUTH_ENABLED should leave config value unchanged (false)")
	}
	if !strings.Contains(buf.String(), "invalid AUTH_ENABLED") {
		t.Errorf("expected warning about invalid AUTH_ENABLED; got:\n%s", buf.String())
	}
}

func TestInitAuth_AuthPublicKeyFileEnv_OverridesConfig(t *testing.T) {
	os.Unsetenv("PUBLIC_KEY")
	os.Unsetenv("AUTH_ENABLED")

	_, _, sshKey := mustGenerateSSHKeyPair(t)
	f, err := os.CreateTemp("", "jwt_pub_via_env_*.pub")
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(sshKey)
	f.Close()
	defer os.Remove(f.Name())

	t.Setenv("AUTH_PUBLIC_KEY_FILE", f.Name())
	cfg := &AuthConfig{Enabled: true, PublicKeyFile: "/should/not/be/read.pub"}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.PublicKeyFile != f.Name() {
		t.Errorf("expected PublicKeyFile=%q, got %q", f.Name(), cfg.PublicKeyFile)
	}
}

func TestInitAuth_AuthPublicKeyFileEnv_PublicKeyEnvStillWins(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	t.Setenv("PUBLIC_KEY", sshKey)
	t.Setenv("AUTH_PUBLIC_KEY_FILE", "/should/not/be/read.pub")
	os.Unsetenv("AUTH_ENABLED")

	publicKeyMu.RLock()
	prev := publicKey
	publicKeyMu.RUnlock()
	t.Cleanup(func() {
		publicKeyMu.Lock()
		publicKey = prev
		publicKeyMu.Unlock()
	})

	cfg := &AuthConfig{Enabled: true}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	publicKeyMu.RLock()
	currentKey := publicKey
	publicKeyMu.RUnlock()
	if !currentKey.Equal(pub) {
		t.Error("PUBLIC_KEY env should take precedence over AUTH_PUBLIC_KEY_FILE")
	}
}

// ─── test helpers ──────────────────────────────────────────────────────────

// mustGenerateSSHKeyPair generates an Ed25519 key pair and returns the public
// key, private key, and the OpenSSH public key string.
func mustGenerateSSHKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	sshKey := "ssh-ed25519 " + base64.StdEncoding.EncodeToString(buildSSHBlobWithType("ssh-ed25519", pub)) + " test@cystemd"
	return pub, priv, sshKey
}

// buildSSHBlobWithType builds the OpenSSH wire-format blob for a given key type and key bytes.
func buildSSHBlobWithType(keyType string, keyBytes []byte) []byte {
	typeBytes := []byte(keyType)
	buf := make([]byte, 4+len(typeBytes)+4+len(keyBytes))
	binary.BigEndian.PutUint32(buf[0:4], uint32(len(typeBytes)))
	copy(buf[4:], typeBytes)
	binary.BigEndian.PutUint32(buf[4+len(typeBytes):], uint32(len(keyBytes)))
	copy(buf[4+len(typeBytes)+4:], keyBytes)
	return buf
}

// mustSetPublicKey initialises auth using the given public key, resetting it
// after the test via t.Cleanup.
func mustSetPublicKey(t *testing.T, pub ed25519.PublicKey, sshKey string) {
	t.Helper()
	publicKeyMu.RLock()
	prev := publicKey
	publicKeyMu.RUnlock()
	t.Cleanup(func() {
		publicKeyMu.Lock()
		publicKey = prev
		publicKeyMu.Unlock()
	})

	os.Unsetenv("PUBLIC_KEY")
	cfg := &AuthConfig{Enabled: true}
	t.Setenv("PUBLIC_KEY", sshKey)
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("InitAuth: %v", err)
	}
}

// mustSignToken creates a signed Ed25519 JWT with a 1-hour expiry.
func mustSignToken(t *testing.T, priv ed25519.PrivateKey) string {
	t.Helper()
	claims := jwt.MapClaims{
		"sub": "test-subject",
		"exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := token.SignedString(priv)
	if err != nil {
		t.Fatalf("SignedString: %v", err)
	}
	return signed
}

// ─── AUTH_PUBLIC_KEY rename (2026-09-07) ───────────────────────────────────

// TestResolveInlinePublicKey_PrefersNewName pins that AUTH_PUBLIC_KEY is read.
func TestResolveInlinePublicKey_PrefersNewName(t *testing.T) {
	t.Setenv("AUTH_PUBLIC_KEY", "new-value")
	t.Setenv("PUBLIC_KEY", "")
	if got, src := resolveInlinePublicKey(); got != "new-value" || src != "AUTH_PUBLIC_KEY" {
		t.Errorf("AUTH_PUBLIC_KEY must be read and reported as the source; got %q from %q", got, src)
	}
}

// TestResolveInlinePublicKey_LegacyStillWorks pins the rename's compatibility
// guarantee: PUBLIC_KEY still works, is reported as the source, and warns
// naming AUTH_PUBLIC_KEY. See DOCS/MEMORY.md § `PUBLIC_KEY` renamed to `AUTH_PUBLIC_KEY`.
func TestResolveInlinePublicKey_LegacyStillWorks(t *testing.T) {
	t.Setenv("AUTH_PUBLIC_KEY", "")
	t.Setenv("PUBLIC_KEY", "legacy-value")

	buf := captureLogs(t, LevelDebug)
	got, src := resolveInlinePublicKey()

	if src != "PUBLIC_KEY" {
		t.Errorf("the source must name the variable actually set, so a half-migrated host is not told it loaded from a variable it never set; got %q", src)
	}
	if got != "legacy-value" {
		t.Errorf("the deprecated PUBLIC_KEY must still be honoured; got %q", got)
	}
	if !strings.Contains(buf.String(), `"level":"warning"`) {
		t.Errorf("using the deprecated name must warn so the migration is visible; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "AUTH_PUBLIC_KEY") {
		t.Errorf("the warning must name the replacement variable; got:\n%s", buf.String())
	}
}

// TestResolveInlinePublicKey_BothSetPrefersNewAndWarns pins the half-migrated
// state: AUTH_PUBLIC_KEY wins and the collision is logged at Warning.
func TestResolveInlinePublicKey_BothSetPrefersNewAndWarns(t *testing.T) {
	t.Setenv("AUTH_PUBLIC_KEY", "new-value")
	t.Setenv("PUBLIC_KEY", "legacy-value")

	buf := captureLogs(t, LevelDebug)
	got, _ := resolveInlinePublicKey()

	if got != "new-value" {
		t.Errorf("the new name must win when both are set; got %q", got)
	}
	if !strings.Contains(buf.String(), `"level":"warning"`) {
		t.Errorf("a both-set collision must warn; got:\n%s", buf.String())
	}
}

// TestResolveInlinePublicKey_NeitherSetIsSilent pins that file-based-key hosts
// (the common case) get no deprecation Warning on every InitAuth.
func TestResolveInlinePublicKey_NeitherSetIsSilent(t *testing.T) {
	t.Setenv("AUTH_PUBLIC_KEY", "")
	t.Setenv("PUBLIC_KEY", "")

	buf := captureLogs(t, LevelDebug)
	if got, src := resolveInlinePublicKey(); got != "" || src != "" {
		t.Errorf("neither set must yield empty key and empty source; got %q from %q", got, src)
	}
	if strings.Contains(buf.String(), `"level":"warning"`) {
		t.Errorf("hosts using AUTH_PUBLIC_KEY_FILE must not be warned at every InitAuth; got:\n%s", buf.String())
	}
}

// TestAuthEnvKeys_TriggerReloadForBothSpellings pins that a change to either
// key spelling (or the other auth keys) makes StartEnvReload re-run InitAuth.
func TestAuthEnvKeys_TriggerReloadForBothSpellings(t *testing.T) {
	for _, k := range []string{"AUTH_PUBLIC_KEY", "PUBLIC_KEY", "AUTH_PUBLIC_KEY_FILE", "AUTH_ENABLED"} {
		if !authEnvKeys[k] {
			t.Errorf("a change to %s must trigger an InitAuth re-run", k)
		}
	}
}

// TestInitAuth_LoadsKeyFromNewEnvName pins end to end that a key in
// AUTH_PUBLIC_KEY is parsed and installed, not merely read.
func TestInitAuth_LoadsKeyFromNewEnvName(t *testing.T) {
	_, _, sshKey := mustGenerateSSHKeyPair(t)
	t.Setenv("AUTH_PUBLIC_KEY", sshKey)
	t.Setenv("PUBLIC_KEY", "")

	cfg := &AuthConfig{Enabled: true}
	if err := InitAuth(cfg); err != nil {
		t.Fatalf("InitAuth with AUTH_PUBLIC_KEY: %v", err)
	}
	publicKeyMu.RLock()
	defer publicKeyMu.RUnlock()
	if publicKey == nil {
		t.Error("AUTH_PUBLIC_KEY must install a usable verification key")
	}
}
