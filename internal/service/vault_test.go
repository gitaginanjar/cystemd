package service

// vault_test.go: pure helpers are unit-tested; InitVault skip and token paths
// need no network; AppRole flows use httptest, live Vault only under
// hasVaultConfig(). Every test that changes the global VaultState restores it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	vault "github.com/hashicorp/vault-client-go"
)

// ─── environment probe ─────────────────────────────────────────────────────

// hasVaultConfig reports whether all three required AppRole env vars are set,
// indicating a real Vault server is reachable in this environment.
func hasVaultConfig() bool {
	return os.Getenv("VAULT_ADDRESS") != "" &&
		os.Getenv("VAULT_ROLE_ID") != "" &&
		os.Getenv("VAULT_SECRET_ID") != ""
}

// ─── vaultSafePrefix ──────────────────────────────────────────────────────

func TestVaultSafePrefix_EmptyString(t *testing.T) {
	if got := vaultSafePrefix(""); got != "<empty>" {
		t.Errorf("vaultSafePrefix(%q) = %q, want %q", "", got, "<empty>")
	}
}

func TestVaultSafePrefix_ShortString(t *testing.T) {
	cases := []string{"abc", "12345678"}
	for _, s := range cases {
		got := vaultSafePrefix(s)
		if got != s {
			t.Errorf("vaultSafePrefix(%q) = %q, want %q (unchanged)", s, got, s)
		}
	}
}

func TestVaultSafePrefix_LongStringTruncated(t *testing.T) {
	long := "1820bc9b-7989-177a-1e77-8d68861e7374"
	got := vaultSafePrefix(long)
	if len(got) != 8 {
		t.Errorf("vaultSafePrefix(long) = %q (len=%d), want 8 chars", got, len(got))
	}
	if got != long[:8] {
		t.Errorf("vaultSafePrefix(long) = %q, want %q", got, long[:8])
	}
}

func TestVaultSafePrefix_ExactlyNineChars(t *testing.T) {
	s := "123456789"
	got := vaultSafePrefix(s)
	if got != "12345678" {
		t.Errorf("vaultSafePrefix(%q) = %q, want %q", s, got, "12345678")
	}
}

func TestVaultSafePrefix_NeverLeaksFullSecret(t *testing.T) {
	secret := "7072f88e-80bd-9e7d-5c74-1bb6bccb5eee"
	got := vaultSafePrefix(secret)
	if got == secret {
		t.Error("vaultSafePrefix must not return the full secret string")
	}
}

// ─── LoadVaultConfig ──────────────────────────────────────────────────────

func TestLoadVaultConfig_AllEnvSet(t *testing.T) {
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	t.Setenv("VAULT_ROLE_ID", "test-role-id")
	t.Setenv("VAULT_SECRET_ID", "test-secret-id")
	t.Setenv("VAULT_SKIP_VERIFY", "true")

	cfg := LoadVaultConfig()

	if cfg.Address != "https://vault.example.com" {
		t.Errorf("Address: got %q, want %q", cfg.Address, "https://vault.example.com")
	}
	if cfg.RoleID != "test-role-id" {
		t.Errorf("RoleID: got %q, want %q", cfg.RoleID, "test-role-id")
	}
	if cfg.SecretID != "test-secret-id" {
		t.Errorf("SecretID: got %q, want %q", cfg.SecretID, "test-secret-id")
	}
	if !cfg.SkipVerify {
		t.Error("SkipVerify: got false, want true")
	}
}

func TestLoadVaultConfig_TokenEnvSet(t *testing.T) {
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	t.Setenv("VAULT_TOKEN", "hvs.testtoken12345")
	os.Unsetenv("VAULT_ROLE_ID")
	os.Unsetenv("VAULT_SECRET_ID")

	cfg := LoadVaultConfig()

	if cfg.Token != "hvs.testtoken12345" {
		t.Errorf("Token: got %q, want %q", cfg.Token, "hvs.testtoken12345")
	}
	if cfg.RoleID != "" {
		t.Errorf("RoleID: expected empty, got %q", cfg.RoleID)
	}
	if cfg.SecretID != "" {
		t.Errorf("SecretID: expected empty, got %q", cfg.SecretID)
	}
}

func TestLoadVaultConfig_TokenAndAppRoleSet(t *testing.T) {
	// Both are loaded; the order (AppRole before token) is InitVault's chain.
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	t.Setenv("VAULT_TOKEN", "hvs.directtoken")
	t.Setenv("VAULT_ROLE_ID", "some-role-id")
	t.Setenv("VAULT_SECRET_ID", "some-secret-id")

	cfg := LoadVaultConfig()

	if cfg.Token != "hvs.directtoken" {
		t.Errorf("Token: got %q, want %q", cfg.Token, "hvs.directtoken")
	}
	if cfg.RoleID != "some-role-id" {
		t.Errorf("RoleID: got %q, want %q", cfg.RoleID, "some-role-id")
	}
}

func TestLoadVaultConfig_NoEnvSet(t *testing.T) {
	os.Unsetenv("VAULT_ADDRESS")
	os.Unsetenv("VAULT_TOKEN")
	os.Unsetenv("VAULT_ROLE_ID")
	os.Unsetenv("VAULT_SECRET_ID")
	os.Unsetenv("VAULT_SKIP_VERIFY")

	cfg := LoadVaultConfig()

	if cfg.Address != "" {
		t.Errorf("Address: got %q, want empty", cfg.Address)
	}
	if cfg.Token != "" {
		t.Errorf("Token: got %q, want empty", cfg.Token)
	}
	if cfg.RoleID != "" {
		t.Errorf("RoleID: got %q, want empty", cfg.RoleID)
	}
	if cfg.SecretID != "" {
		t.Errorf("SecretID: got %q, want empty", cfg.SecretID)
	}
	if !cfg.SkipVerify {
		t.Error("SkipVerify: got false, want true (default)")
	}
}

func TestLoadVaultConfig_SkipVerifyFalse(t *testing.T) {
	for _, val := range []string{"false", "False", "FALSE", "no", "No", "NO", "0", "fAlse"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("VAULT_SKIP_VERIFY", val)
			cfg := LoadVaultConfig()
			if cfg.SkipVerify {
				t.Errorf("VAULT_SKIP_VERIFY=%q: got true, want false", val)
			}
		})
	}
}

func TestLoadVaultConfig_SkipVerifyTrue(t *testing.T) {
	for _, val := range []string{"true", "True", "TRUE", "yes", "Yes", "YES", "1", "tRue"} {
		t.Run(val, func(t *testing.T) {
			t.Setenv("VAULT_SKIP_VERIFY", val)
			cfg := LoadVaultConfig()
			if !cfg.SkipVerify {
				t.Errorf("VAULT_SKIP_VERIFY=%q: got false, want true", val)
			}
		})
	}
}

func TestLoadVaultConfig_SkipVerifyInvalidFallsToDefault(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	t.Setenv("VAULT_SKIP_VERIFY", "maybe")

	cfg := LoadVaultConfig()

	if !cfg.SkipVerify {
		t.Error("SkipVerify: got false for invalid value, want true (default)")
	}
	if !strings.Contains(buf.String(), "not a valid boolean") {
		t.Errorf("expected warning about invalid VAULT_SKIP_VERIFY; got:\n%s", buf.String())
	}
}

// ─── parseBoolEnv ──────────────────────────────────────────────────────────

func TestParseBoolEnv_TruthyValues(t *testing.T) {
	for _, val := range []string{"true", "True", "TRUE", "tRuE", "yes", "Yes", "YES", "yEs", "1"} {
		got, err := parseBoolEnv(val)
		if err != nil {
			t.Errorf("parseBoolEnv(%q) unexpected error: %v", val, err)
		}
		if !got {
			t.Errorf("parseBoolEnv(%q) = false, want true", val)
		}
	}
}

func TestParseBoolEnv_FalsyValues(t *testing.T) {
	for _, val := range []string{"false", "False", "FALSE", "fAlSe", "no", "No", "NO", "nO", "0"} {
		got, err := parseBoolEnv(val)
		if err != nil {
			t.Errorf("parseBoolEnv(%q) unexpected error: %v", val, err)
		}
		if got {
			t.Errorf("parseBoolEnv(%q) = true, want false", val)
		}
	}
}

func TestParseBoolEnv_InvalidValues(t *testing.T) {
	for _, val := range []string{"maybe", "ok", "enabled", "on", "off", "2", "-1", "", "  "} {
		_, err := parseBoolEnv(val)
		if err == nil {
			t.Errorf("parseBoolEnv(%q) expected error, got nil", val)
		}
	}
}

func TestParseBoolEnv_TrimsWhitespace(t *testing.T) {
	for _, val := range []string{"  true  ", "\tyes\t", " 1 ", "  false  ", " no ", " 0 "} {
		_, err := parseBoolEnv(val)
		if err != nil {
			t.Errorf("parseBoolEnv(%q) unexpected error for whitespace-padded value: %v", val, err)
		}
	}
}

func TestLoadVaultConfig_LogsAtDebug(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	t.Setenv("VAULT_ROLE_ID", "some-role")
	t.Setenv("VAULT_SECRET_ID", "some-secret")

	LoadVaultConfig()

	if !strings.Contains(buf.String(), "vault: config loaded") {
		t.Errorf("expected debug log line from LoadVaultConfig; got:\n%s", buf.String())
	}
}

func TestLoadVaultConfig_LogsSkipVerifyState(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	t.Setenv("VAULT_SKIP_VERIFY", "true")

	LoadVaultConfig()

	if !strings.Contains(buf.String(), "skip_verify=true") {
		t.Errorf("expected skip_verify=true in debug log; got:\n%s", buf.String())
	}
}

func TestLoadVaultConfig_LogsTokenAuthMethod(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	t.Setenv("VAULT_TOKEN", "hvs.mytoken")
	os.Unsetenv("VAULT_ROLE_ID")
	os.Unsetenv("VAULT_SECRET_ID")

	LoadVaultConfig()

	if !strings.Contains(buf.String(), "auth_method=token") {
		t.Errorf("expected auth_method=token in debug log; got:\n%s", buf.String())
	}
}

func TestLoadVaultConfig_LogsAppRoleAuthMethod(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	os.Unsetenv("VAULT_TOKEN")
	t.Setenv("VAULT_ROLE_ID", "rid")
	t.Setenv("VAULT_SECRET_ID", "sid")

	LoadVaultConfig()

	if !strings.Contains(buf.String(), "auth_method=approle") {
		t.Errorf("expected auth_method=approle in debug log; got:\n%s", buf.String())
	}
}

func TestLoadVaultConfig_LogsAppRoleIncomplete_OnlyRoleID(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	os.Unsetenv("VAULT_TOKEN")
	t.Setenv("VAULT_ROLE_ID", "rid-only")
	os.Unsetenv("VAULT_SECRET_ID")

	LoadVaultConfig()

	if !strings.Contains(buf.String(), "approle (incomplete)") {
		t.Errorf("expected 'approle (incomplete)' in debug log when only role_id set; got:\n%s", buf.String())
	}
}

func TestLoadVaultConfig_LogsAppRoleIncomplete_OnlySecretID(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	os.Unsetenv("VAULT_TOKEN")
	os.Unsetenv("VAULT_ROLE_ID")
	t.Setenv("VAULT_SECRET_ID", "sid-only")

	LoadVaultConfig()

	if !strings.Contains(buf.String(), "approle (incomplete)") {
		t.Errorf("expected 'approle (incomplete)' in debug log when only secret_id set; got:\n%s", buf.String())
	}
}

// ─── InitVault — no-network paths ─────────────────────────────────────────

func TestInitVault_EmptyAddress_SkipsInit(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{Address: "", RoleID: "rid", SecretID: "sid"}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault with empty address must not error; got: %v", err)
	}
	if !strings.Contains(buf.String(), "VAULT_ADDRESS not set") {
		t.Errorf("expected skip message; got:\n%s", buf.String())
	}
	if globalVaultState != prev {
		t.Error("globalVaultState must not be modified when address is empty")
	}
}

func TestInitVault_EmptyRoleID_SkipsInit(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{Address: "https://vault.example.com", RoleID: "", SecretID: "sid"}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault with empty VAULT_ROLE_ID must not error; got: %v", err)
	}
	// No role_id and no token: the chain is empty and init skips.
	if !strings.Contains(buf.String(), "no usable credentials found") {
		t.Errorf("expected 'no usable credentials found' warning; got:\n%s", buf.String())
	}
}

func TestInitVault_EmptySecretID_SkipsInit(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{Address: "https://vault.example.com", RoleID: "rid", SecretID: ""}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault with empty VAULT_SECRET_ID must not error; got: %v", err)
	}
	if !strings.Contains(buf.String(), "no usable credentials found") {
		t.Errorf("expected 'no usable credentials found' warning; got:\n%s", buf.String())
	}
}

func TestInitVault_NoCredentials_SkipsInit(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	// Address set, but neither token nor AppRole credentials.
	cfg := &VaultConfig{Address: "https://vault.example.com"}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault with no credentials must not error; got: %v", err)
	}
	if !strings.Contains(buf.String(), "no usable credentials found") {
		t.Errorf("expected 'no usable credentials found' warning; got:\n%s", buf.String())
	}
	if globalVaultState != prev {
		t.Error("globalVaultState must remain unchanged when init is skipped")
	}
}

func TestInitVault_SkipsInit_DoesNotSetGlobalState(t *testing.T) {
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	_ = InitVault(&VaultConfig{})
	if globalVaultState != prev {
		t.Error("globalVaultState must remain unchanged when init is skipped")
	}
}

// ─── InitVault — token auth: SetToken is local, so no network is needed ───

func TestInitVault_TokenAuth_SetsGlobalState(t *testing.T) {
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address:    "https://127.0.0.1:19999", // unreachable — no network call in token auth
		Token:      "hvs.testtoken123456",
		SkipVerify: true,
	}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault with token auth must not error: %v", err)
	}
	state := GetVaultClient()
	if state == nil {
		t.Fatal("globalVaultState should be set after token auth")
	}
	if state.AuthMethod != VaultAuthMethodToken {
		t.Errorf("AuthMethod: got %q, want %q", state.AuthMethod, VaultAuthMethodToken)
	}
	if state.Token != "hvs.testtoken123456" {
		t.Errorf("Token: got %q, want %q", state.Token, "hvs.testtoken123456")
	}
	if state.Address != "https://127.0.0.1:19999" {
		t.Errorf("Address: got %q, want %q", state.Address, "https://127.0.0.1:19999")
	}
}

func TestInitVault_TokenAuth_LogsTokenMethod(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address: "https://127.0.0.1:19999",
		Token:   "hvs.testtoken123456",
	}
	_ = InitVault(cfg)

	out := buf.String()
	if !strings.Contains(out, "token authentication") {
		t.Errorf("expected 'token authentication' in info log; got:\n%s", out)
	}
	if !strings.Contains(out, "auth_method=token") {
		t.Errorf("expected 'auth_method=token' in info log; got:\n%s", out)
	}
}

func TestInitVault_TokenAuth_LogsInitComplete(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address: "https://127.0.0.1:19999",
		Token:   "hvs.testtoken123456",
	}
	_ = InitVault(cfg)

	if !strings.Contains(buf.String(), "initialisation complete") {
		t.Errorf("expected 'initialisation complete' info log; got:\n%s", buf.String())
	}
}

func TestInitVault_TokenAuth_NeverLogsFullToken(t *testing.T) {
	buf := captureLogs(t, LevelTrace)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	secret := "hvs.verylongsecrettoken99999999"
	cfg := &VaultConfig{
		Address: "https://127.0.0.1:19999",
		Token:   secret,
	}
	_ = InitVault(cfg)

	if strings.Contains(buf.String(), secret) {
		t.Errorf("full token must never appear in logs; got:\n%s", buf.String())
	}
}

func TestInitVault_TokenAuth_TakesPriorityOverAppRole(t *testing.T) {
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	// AppRole is tried first but fails against the unreachable address, so the
	// chain falls through to the token (the name predates the fallback chain).
	cfg := &VaultConfig{
		Address:  "https://127.0.0.1:19999",
		Token:    "hvs.directtoken12345",
		RoleID:   "some-role",
		SecretID: "some-secret",
	}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault must not error: %v", err)
	}
	state := GetVaultClient()
	if state == nil {
		t.Fatal("globalVaultState should be set")
	}
	if state.AuthMethod != VaultAuthMethodToken {
		t.Errorf("AuthMethod: got %q, want %q (token must take priority)",
			state.AuthMethod, VaultAuthMethodToken)
	}
}

func TestInitVault_TokenAuth_LogsSkipVerifyWarning(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address:    "https://127.0.0.1:19999",
		Token:      "hvs.testtoken123456",
		SkipVerify: true,
	}
	_ = InitVault(cfg)

	if !strings.Contains(buf.String(), "TLS certificate verification is DISABLED") {
		t.Errorf("expected TLS-skip warning; got:\n%s", buf.String())
	}
}

// ─── InitVault — AppRole no-network paths ─────────────────────────────────

func TestInitVault_UnreachableAddress_ReturnsError(t *testing.T) {
	buf := captureLogs(t, LevelError)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address:    "https://127.0.0.1:19999",
		RoleID:     "test-role-id",
		SecretID:   "test-secret-id",
		SkipVerify: true,
	}
	err := InitVault(cfg)
	if err == nil {
		t.Fatal("expected error when Vault address is unreachable")
	}
	if !strings.Contains(err.Error(), "vault:") {
		t.Errorf("error should be prefixed with 'vault:'; got: %v", err)
	}
	if !strings.Contains(buf.String(), `"level":"error"`) {
		t.Errorf("expected an error log line for unreachable server; got:\n%s", buf.String())
	}
}

func TestInitVault_LogsSkipVerifyWarning(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address:    "https://127.0.0.1:19999",
		RoleID:     "rid",
		SecretID:   "sid",
		SkipVerify: true,
	}
	_ = InitVault(cfg)

	if !strings.Contains(buf.String(), "TLS certificate verification is DISABLED") {
		t.Errorf("expected TLS-skip warning; got:\n%s", buf.String())
	}
}

func TestInitVault_LogsInitialisationStart(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address:  "https://127.0.0.1:19999",
		RoleID:   "rid",
		SecretID: "sid",
	}
	_ = InitVault(cfg)

	if !strings.Contains(buf.String(), "vault: initialising") {
		t.Errorf("expected 'vault: initialising' info line; got:\n%s", buf.String())
	}
}

func TestInitVault_LogsAppRoleAuthAttempt(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address:    "https://127.0.0.1:19999",
		RoleID:     "1820bc9b-test-role",
		SecretID:   "7072f88e-test-secret",
		SkipVerify: true,
	}
	_ = InitVault(cfg)

	out := buf.String()
	if !strings.Contains(out, "AppRole") {
		t.Errorf("expected 'AppRole' in debug log; got:\n%s", out)
	}
	// Never the full role_id or secret_id: only the vaultSafePrefix form.
	if strings.Contains(out, "7072f88e-test-secret") {
		t.Errorf("full secret_id must never appear in logs; got:\n%s", out)
	}
	if strings.Contains(out, "1820bc9b-test-role") {
		t.Errorf("full role_id must never appear in logs; got:\n%s", out)
	}
	// Both 8-char prefixes must appear.
	if !strings.Contains(out, "1820bc9b") {
		t.Errorf("expected role_id 8-char prefix in log; got:\n%s", out)
	}
	if !strings.Contains(out, "7072f88e") {
		t.Errorf("expected secret_id 8-char prefix in log; got:\n%s", out)
	}
}

// ─── Degraded mode: after a failed InitVault the API stays safe to call ─────

// TestInitVault_DegradedMode_GlobalStateRemainsNilAfterFailure pins that a
// failed AppRole login leaves globalVaultState nil (never partially set).
func TestInitVault_DegradedMode_GlobalStateRemainsNilAfterFailure(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil // start from known nil state
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &VaultConfig{
		Address:    "https://127.0.0.1:19999", // unreachable
		RoleID:     "rid",
		SecretID:   "sid",
		SkipVerify: true,
	}
	err := InitVault(cfg)
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
	if globalVaultState != nil {
		t.Error("globalVaultState must remain nil after a failed InitVault")
	}
}

// TestInitVault_DegradedMode_GetVaultClientReturnsNil pins that GetVaultClient
// returns nil after a failed init.
func TestInitVault_DegradedMode_GetVaultClientReturnsNil(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })

	_ = InitVault(&VaultConfig{
		Address:    "https://127.0.0.1:19999",
		RoleID:     "rid",
		SecretID:   "sid",
		SkipVerify: true,
	})

	if got := GetVaultClient(); got != nil {
		t.Errorf("GetVaultClient after failed init: got %v, want nil", got)
	}
}

// TestInitVault_DegradedMode_BuildVaultStatusSafeAfterFailure pins that after a
// failed init BuildVaultStatus reports {configured:true, status:"unauthenticated"}.
func TestInitVault_DegradedMode_BuildVaultStatusSafeAfterFailure(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })
	// A degraded host still has its Vault env populated; mirror that.
	t.Setenv("VAULT_ADDRESS", "https://127.0.0.1:19999")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "rid")
	t.Setenv("VAULT_SECRET_ID", "sid")

	_ = InitVault(&VaultConfig{
		Address:    "https://127.0.0.1:19999",
		RoleID:     "rid",
		SecretID:   "sid",
		SkipVerify: true,
	})

	s := BuildVaultStatus()
	if !s.Configured {
		t.Error("BuildVaultStatus.Configured must be true when VAULT_ADDRESS is set, even degraded")
	}
	if s.Status != VaultStatusUnauthenticated {
		t.Errorf("BuildVaultStatus.Status: got %q, want %q", s.Status, VaultStatusUnauthenticated)
	}
}

// TestInitVault_DegradedMode_ErrorContainsVaultPrefix pins the "vault:" error
// prefix that keeps main.go's Warning self-describing.
func TestInitVault_DegradedMode_ErrorContainsVaultPrefix(t *testing.T) {
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	err := InitVault(&VaultConfig{
		Address:    "https://127.0.0.1:19999",
		RoleID:     "rid",
		SecretID:   "sid",
		SkipVerify: true,
	})
	if err == nil {
		t.Fatal("expected error from unreachable server")
	}
	if !strings.HasPrefix(err.Error(), "vault:") {
		t.Errorf("error must start with 'vault:' for a self-describing warning; got: %v", err)
	}
}

// TestInitVault_DegradedMode_SecondCallCanSucceed pins that after a failed
// AppRole init a later successful init (token auth: no network) sets the state.
func TestInitVault_DegradedMode_SecondCallCanSucceed(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })

	// First call fails (AppRole, unreachable).
	_ = InitVault(&VaultConfig{
		Address:    "https://127.0.0.1:19999",
		RoleID:     "rid",
		SecretID:   "sid",
		SkipVerify: true,
	})
	if globalVaultState != nil {
		t.Fatal("precondition failed: state should still be nil after first failure")
	}

	// Second call succeeds (token auth, local operation).
	if err := InitVault(&VaultConfig{
		Address: "https://127.0.0.1:19999",
		Token:   "hvs.recovery.token.12345",
	}); err != nil {
		t.Fatalf("second InitVault with token auth must succeed: %v", err)
	}
	state := GetVaultClient()
	if state == nil {
		t.Fatal("globalVaultState must be set after successful second init")
	}
	if state.AuthMethod != VaultAuthMethodToken {
		t.Errorf("AuthMethod after recovery: got %q, want %q", state.AuthMethod, VaultAuthMethodToken)
	}
}

// ─── GetVaultClient ────────────────────────────────────────────────────────

func TestGetVaultClient_ReturnsNilBeforeInit(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })

	if got := GetVaultClient(); got != nil {
		t.Errorf("GetVaultClient: got non-nil before init, want nil")
	}
}

// ─── BuildVaultStatus ─────────────────────────────────────────────────────

func TestBuildVaultStatus_NotConfigured(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })
	// Without live state, configured-ness comes from the env: clear it so a
	// host with a live Vault env still sees not_configured.
	t.Setenv("VAULT_ADDRESS", "")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "")
	t.Setenv("VAULT_SECRET_ID", "")

	s := BuildVaultStatus()

	if s.Configured {
		t.Error("Configured: got true, want false when vault is not configured")
	}
	if s.Status != VaultStatusNotConfigured {
		t.Errorf("Status: got %q, want %q", s.Status, VaultStatusNotConfigured)
	}
	if s.AuthMethod != string(VaultAuthMethodNone) {
		t.Errorf("AuthMethod: got %q, want %q", s.AuthMethod, VaultAuthMethodNone)
	}
	if s.Address != "" {
		t.Errorf("Address: got %q, want empty", s.Address)
	}
}

// TestBuildVaultStatus_Unauthenticated pins the degraded shape (VAULT_ADDRESS
// set, no live token): configured, "unauthenticated", the address, and the
// method the chain would try first.
func TestBuildVaultStatus_Unauthenticated(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })
	t.Setenv("VAULT_ADDRESS", "https://vault.example.com")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "rid")
	t.Setenv("VAULT_SECRET_ID", "sid")
	// An ambient wrapping token would switch envAuthMethod to the wrapped variant.
	t.Setenv("VAULT_WRAPPING_TOKEN", "")

	s := BuildVaultStatus()

	if !s.Configured {
		t.Error("Configured: got false, want true when VAULT_ADDRESS is set")
	}
	if s.Status != VaultStatusUnauthenticated {
		t.Errorf("Status: got %q, want %q", s.Status, VaultStatusUnauthenticated)
	}
	if s.Address != "https://vault.example.com" {
		t.Errorf("Address: got %q, want the configured address", s.Address)
	}
	// role_id + secret_id, no wrapping token: the secret_id variant.
	if s.AuthMethod != string(VaultAuthMethodAppRoleSecretID) {
		t.Errorf("AuthMethod: got %q, want %q", s.AuthMethod, VaultAuthMethodAppRoleSecretID)
	}
}

func TestBuildVaultStatus_TokenAuth(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		Token:      "hvs.testtoken",
		AuthMethod: VaultAuthMethodToken,
		Address:    "https://vault.example.com",
		Accessor:   "acc-token-001",
		Policies:   []string{"default", "myapp"},
		Renewable:  false,
	}
	t.Cleanup(func() { globalVaultState = prev })

	s := BuildVaultStatus()

	if !s.Configured {
		t.Error("Configured: got false, want true")
	}
	if s.Status != VaultStatusAuthenticated {
		t.Errorf("Status: got %q, want %q", s.Status, VaultStatusAuthenticated)
	}
	if s.AuthMethod != string(VaultAuthMethodToken) {
		t.Errorf("AuthMethod: got %q, want %q", s.AuthMethod, VaultAuthMethodToken)
	}
	if s.Address != "https://vault.example.com" {
		t.Errorf("Address: got %q, want %q", s.Address, "https://vault.example.com")
	}
	// Accessor and policies must not surface: TestBuildVaultStatus_DoesNotExposeReconFields.
}

func TestBuildVaultStatus_AppRoleAuth(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		Token:         "hvs.approletoken",
		AuthMethod:    VaultAuthMethodAppRole,
		Address:       "https://vault.example.com",
		Accessor:      "acc-approle-001",
		Policies:      []string{"default"},
		LeaseDuration: 3600,
		Renewable:     true,
		EntityID:      "ent-abc-123",
	}
	t.Cleanup(func() { globalVaultState = prev })

	s := BuildVaultStatus()

	if !s.Configured {
		t.Error("Configured: got false, want true")
	}
	if s.AuthMethod != string(VaultAuthMethodAppRole) {
		t.Errorf("AuthMethod: got %q, want %q", s.AuthMethod, VaultAuthMethodAppRole)
	}
	if s.LeaseDuration != 3600 {
		t.Errorf("LeaseDuration: got %d, want 3600", s.LeaseDuration)
	}
	if !s.Renewable {
		t.Error("Renewable: got false, want true")
	}
}

func TestBuildVaultStatus_NeverExposesToken(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		Token:      "hvs.supersecrettoken999",
		AuthMethod: VaultAuthMethodToken,
		Address:    "https://vault.example.com",
	}
	t.Cleanup(func() { globalVaultState = prev })

	// The raw token must never appear in the open-endpoint JSON.
	out, _ := json.Marshal(BuildVaultStatus())
	if strings.Contains(string(out), "hvs.supersecrettoken999") {
		t.Errorf("vault status must never contain the raw token; got: %s", out)
	}
}

// TestBuildVaultStatus_DoesNotExposeReconFields pins that the open / status
// exposes none of the accessor, entity_id or policy names VaultState carries.
func TestBuildVaultStatus_DoesNotExposeReconFields(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		AuthMethod: VaultAuthMethodAppRole,
		Address:    "https://vault.example.com",
		Accessor:   "acc-secret-accessor",
		Policies:   []string{"root-ish-policy", "another-policy"},
		EntityID:   "ent-secret-id",
	}
	t.Cleanup(func() { globalVaultState = prev })

	out, _ := json.Marshal(BuildVaultStatus())
	for _, leak := range []string{"accessor", "acc-secret-accessor", "entity_id", "ent-secret-id", "policies", "root-ish-policy"} {
		if strings.Contains(string(out), leak) {
			t.Errorf("open vault status must not expose %q; got: %s", leak, out)
		}
	}
}

// ─── Integration: live Vault ───────────────────────────────────────────────

func TestInitVault_LiveVault_AuthSucceeds(t *testing.T) {
	if !hasVaultConfig() {
		t.Skip("VAULT_ADDRESS / VAULT_ROLE_ID / VAULT_SECRET_ID not set; skipping live Vault test")
	}

	buf := captureLogs(t, LevelDebug)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := LoadVaultConfig()
	if err := InitVault(cfg); err != nil {
		t.Fatalf("live Vault InitVault failed: %v\nlog output:\n%s", err, buf.String())
	}

	state := GetVaultClient()
	if state == nil {
		t.Fatal("GetVaultClient returned nil after successful InitVault")
	}
	if state.Token == "" {
		t.Error("VaultState.Token is empty after successful auth")
	}
	if state.Client == nil {
		t.Error("VaultState.Client is nil after successful auth")
	}
	if state.AuthMethod != VaultAuthMethodAppRole {
		t.Errorf("AuthMethod: got %q, want %q", state.AuthMethod, VaultAuthMethodAppRole)
	}

	out := buf.String()
	if !strings.Contains(out, "initialisation complete") {
		t.Errorf("expected 'initialisation complete' log; got:\n%s", out)
	}
	// Full token must never appear in logs.
	if strings.Contains(out, state.Token) {
		t.Errorf("full vault token must not appear in logs; token leaked:\n%s", out)
	}
}

func TestInitVault_LiveVault_LogsAllLifecycleSteps(t *testing.T) {
	if !hasVaultConfig() {
		t.Skip("VAULT_ADDRESS / VAULT_ROLE_ID / VAULT_SECRET_ID not set; skipping live Vault test")
	}

	buf := captureLogs(t, LevelDebug)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	_ = InitVault(LoadVaultConfig())

	out := buf.String()
	for _, want := range []string{
		"vault: initialising",
		"vault: creating vault-client-go client",
		"vault: client object created",
		"vault: starting AppRole authentication",
		"vault: dispatching AppRoleLogin request",
		"vault: AppRoleLogin HTTP call returned",
		"vault: AppRole authentication successful",
		"vault: token metadata:",
		"vault: setting client token",
		"vault: client token applied",
		"vault: initialisation complete",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected log line containing %q; full output:\n%s", want, out)
		}
	}
}

func TestInitVault_LiveVault_BuildVaultStatusPopulated(t *testing.T) {
	if !hasVaultConfig() {
		t.Skip("VAULT_ADDRESS / VAULT_ROLE_ID / VAULT_SECRET_ID not set; skipping live Vault test")
	}

	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	if err := InitVault(LoadVaultConfig()); err != nil {
		t.Fatalf("InitVault failed: %v", err)
	}

	s := BuildVaultStatus()
	if !s.Configured {
		t.Error("BuildVaultStatus.Configured: got false after successful auth")
	}
	if s.AuthMethod != string(VaultAuthMethodAppRole) {
		t.Errorf("BuildVaultStatus.AuthMethod: got %q, want %q", s.AuthMethod, VaultAuthMethodAppRole)
	}
	if s.Address == "" {
		t.Error("BuildVaultStatus.Address: empty after successful auth")
	}
}

// ─── EnsureEnvFileSecure ──────────────────────────────────────────────────

func TestEnsureEnvFileSecure_MissingEnvFile_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	dir := mkTempDir(t)
	// No .env file exists next to config.yml.
	EnsureEnvFileSecure(dir + "/config.yml")

	if strings.Contains(buf.String(), "tightened") || strings.Contains(buf.String(), "cannot tighten") {
		t.Errorf("missing .env should be a silent no-op; got:\n%s", buf.String())
	}
}

func TestEnsureEnvFileSecure_AlreadyRestrictive_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	dir := mkTempDir(t)
	envPath := dir + "/.env"
	if err := os.WriteFile(envPath, []byte("VAULT_SECRET_ID=secret\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	EnsureEnvFileSecure(dir + "/config.yml")

	if strings.Contains(buf.String(), "tightened") {
		t.Errorf("a 0600 .env should not be touched; got:\n%s", buf.String())
	}
	info, _ := os.Stat(envPath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode changed unexpectedly: got %04o, want 0600", info.Mode().Perm())
	}
}

func TestEnsureEnvFileSecure_WorldReadable_TightenedTo0600(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	dir := mkTempDir(t)
	envPath := dir + "/.env"
	if err := os.WriteFile(envPath, []byte("VAULT_SECRET_ID=secret\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	EnsureEnvFileSecure(dir + "/config.yml")

	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("world-readable .env must be tightened to 0600; got %04o", info.Mode().Perm())
	}
	if !strings.Contains(buf.String(), "tightened") {
		t.Errorf("expected Info 'tightened' log; got:\n%s", buf.String())
	}
}

func TestEnsureEnvFileSecure_GroupReadable_TightenedTo0600(t *testing.T) {
	dir := mkTempDir(t)
	envPath := dir + "/.env"
	if err := os.WriteFile(envPath, []byte("VAULT_SECRET_ID=secret\n"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	EnsureEnvFileSecure(dir + "/config.yml")

	info, _ := os.Stat(envPath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("group-readable .env must be tightened to 0600; got %04o", info.Mode().Perm())
	}
}

// TestEnsureEnvFileSecure_NoLongerGatedOnSecretID pins that the bootstrap .env is
// tightened even without VAULT_SECRET_ID: it also holds the SSH and JWT keys.
func TestEnsureEnvFileSecure_NoLongerGatedOnSecretID(t *testing.T) {
	t.Setenv("VAULT_SECRET_ID", "")
	dir := mkTempDir(t)
	envPath := dir + "/.env"
	if err := os.WriteFile(envPath, []byte("GIT_SSH_KEY_PRIVATE=deadbeef\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	EnsureEnvFileSecure(dir + "/config.yml")

	info, _ := os.Stat(envPath)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("must tighten even without VAULT_SECRET_ID; got %04o", info.Mode().Perm())
	}
}

// ─── response-wrapped SecretID ─────────────────────────────────────────────

// resetWrappedSecretIDCache clears the wrapped-SecretID cache now and on cleanup.
func resetWrappedSecretIDCache(t *testing.T) {
	t.Helper()
	clearCache := func() {
		wrappedSecretIDMu.Lock()
		lastWrappingToken = ""
		cachedUnwrappedSecret = ""
		wrappedSecretIDMu.Unlock()
	}
	clearCache()
	t.Cleanup(clearCache)
}

// newWrapTestClient builds a vault.Client pointed at addr with retries off so
// failures surface immediately.
func newWrapTestClient(t *testing.T, addr string) *vault.Client {
	t.Helper()
	c, err := vault.New(vault.WithAddress(addr), vault.WithRetryConfiguration(vault.RetryConfiguration{RetryMax: 0}))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	return c
}

// unwrapHandler returns a handler that answers /v1/sys/wrapping/unwrap with the
// given secret_id and bumps calls (when non-nil) on each unwrap.
func unwrapHandler(t *testing.T, secretID string, calls *atomic.Int32) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/sys/wrapping/unwrap") {
			http.NotFound(w, r)
			return
		}
		if calls != nil {
			calls.Add(1)
		}
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake-unwrap",
			"data": map[string]interface{}{
				"secret_id":          secretID,
				"secret_id_accessor": "acc-" + secretID,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

func TestLoadVaultConfig_WrappedInferredFromWrappingTokenPresence(t *testing.T) {
	// No flag: the PRESENCE of VAULT_WRAPPING_TOKEN is the discriminator.
	captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ROLE_ID", "rid")

	t.Setenv("VAULT_WRAPPING_TOKEN", "hvs.wrapped")
	if cfg := LoadVaultConfig(); !cfg.SecretIDWrapped {
		t.Error("a set VAULT_WRAPPING_TOKEN must mark the config wrapped")
	}

	t.Setenv("VAULT_WRAPPING_TOKEN", "")
	if cfg := LoadVaultConfig(); cfg.SecretIDWrapped {
		t.Error("an empty VAULT_WRAPPING_TOKEN must not mark the config wrapped")
	}
}

func TestLoadVaultConfig_SecretIDWrappedFlagIsNoLongerRead(t *testing.T) {
	// A stale VAULT_SECRET_ID_WRAPPED in .env must be inert, in either direction.
	captureLogs(t, LevelDebug)
	t.Setenv("VAULT_ROLE_ID", "rid")
	t.Setenv("VAULT_SECRET_ID", "raw-sid")

	t.Setenv("VAULT_SECRET_ID_WRAPPED", "true")
	t.Setenv("VAULT_WRAPPING_TOKEN", "")
	if cfg := LoadVaultConfig(); cfg.SecretIDWrapped {
		t.Error("stale VAULT_SECRET_ID_WRAPPED=true must not make a raw SecretID wrapped")
	}

	t.Setenv("VAULT_SECRET_ID_WRAPPED", "false")
	t.Setenv("VAULT_WRAPPING_TOKEN", "hvs.wrapped")
	if cfg := LoadVaultConfig(); !cfg.SecretIDWrapped {
		t.Error("stale VAULT_SECRET_ID_WRAPPED=false must not un-wrap a real wrapping token")
	}
}

func TestUnwrapSecretID_HappyPath(t *testing.T) {
	server := httptest.NewServer(unwrapHandler(t, "real-secret-id", nil))
	t.Cleanup(server.Close)
	client := newWrapTestClient(t, server.URL)
	got, err := unwrapSecretID(client, "wrap-token")
	if err != nil {
		t.Fatalf("unwrapSecretID: %v", err)
	}
	if got != "real-secret-id" {
		t.Errorf("got %q, want real-secret-id", got)
	}
}

func TestUnwrapSecretID_MissingSecretIDField_Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake",
			"data":       map[string]interface{}{"not_secret_id": "x"},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(server.Close)
	client := newWrapTestClient(t, server.URL)
	if _, err := unwrapSecretID(client, "wrap-token"); err == nil {
		t.Error("expected error when unwrap response has no secret_id field")
	}
}

func TestUnwrapSecretID_ServerError_Errors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":["wrapping token is not valid or does not exist"]}`, http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)
	client := newWrapTestClient(t, server.URL)
	if _, err := unwrapSecretID(client, "consumed-token"); err == nil {
		t.Error("expected error when unwrap endpoint returns an error")
	}
}

func TestResolveSecretID_NotWrapped_ReturnsVerbatim(t *testing.T) {
	// Not wrapped → returns the raw SecretID without touching the client.
	got, err := resolveSecretID(nil, &VaultConfig{SecretID: "plain-secret"}, false)
	if err != nil {
		t.Fatalf("resolveSecretID: %v", err)
	}
	if got != "plain-secret" {
		t.Errorf("got %q, want plain-secret (verbatim, no unwrap)", got)
	}
}

// TestResolveSecretID_CachesUnwrap_SingleUseSafe pins the single-use cache: an
// unchanged wrapping token unwraps exactly once, a rotated one once more.
func TestResolveSecretID_CachesUnwrap_SingleUseSafe(t *testing.T) {
	resetWrappedSecretIDCache(t)
	var calls atomic.Int32
	server := httptest.NewServer(unwrapHandler(t, "unwrapped-secret", &calls))
	t.Cleanup(server.Close)
	client := newWrapTestClient(t, server.URL)

	cfg := &VaultConfig{WrappingToken: "wrap-token-A"}

	got1, err := resolveSecretID(client, cfg, true)
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}
	if got1 != "unwrapped-secret" {
		t.Errorf("resolve 1: got %q", got1)
	}

	// Same wrapping token again → MUST reuse cache, not unwrap a consumed token.
	if _, err := resolveSecretID(client, cfg, true); err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("unchanged wrapping token must unwrap exactly once; got %d unwrap calls", n)
	}

	// Rotated wrapping token → exactly one more unwrap.
	cfg.WrappingToken = "wrap-token-B"
	if _, err := resolveSecretID(client, cfg, true); err != nil {
		t.Fatalf("resolve 3: %v", err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("rotated wrapping token must re-unwrap once; got %d total unwrap calls", n)
	}
}

// TestInitVault_WrappedSecretID_UnwrapsThenLogsIn pins end to end (httptest:
// InitVault builds its own client) that login sends the UNWRAPPED secret_id.
func TestInitVault_WrappedSecretID_UnwrapsThenLogsIn(t *testing.T) {
	resetWrappedSecretIDCache(t)
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })

	var sawUnwrap, sawLogin bool
	var loginSecretID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/sys/wrapping/unwrap"):
			sawUnwrap = true
			body, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-unwrap",
				"data":       map[string]interface{}{"secret_id": "unwrapped-secret"},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		case strings.Contains(r.URL.Path, "/v1/auth/approle/login"):
			sawLogin = true
			var reqBody map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&reqBody)
			loginSecretID, _ = reqBody["secret_id"].(string)
			body, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-login",
				"data":       nil,
				"auth": map[string]interface{}{
					"client_token":   "hvs.wrapped-login",
					"accessor":       "acc",
					"policies":       []string{"default"},
					"lease_duration": 3600,
					"renewable":      true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	cfg := &VaultConfig{
		Address:       server.URL,
		RoleID:        "rid",
		WrappingToken: "wrap-token",
		SkipVerify:    true,
	}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault: %v", err)
	}
	if !sawUnwrap {
		t.Error("expected an unwrap call")
	}
	if !sawLogin {
		t.Error("expected an AppRole login call")
	}
	if loginSecretID != "unwrapped-secret" {
		t.Errorf("login must use the UNWRAPPED secret_id; got %q (wrapping token was 'wrap-token')", loginSecretID)
	}
	if GetVaultClient() == nil {
		t.Error("authenticated state should be set after wrapped login")
	}
}

// ─── VAULT_WRAPPING_TOKEN, split out of VAULT_SECRET_ID ─────────────────────

// setWrapEnv sets VAULT_ROLE_ID, VAULT_SECRET_ID and VAULT_WRAPPING_TOKEN for
// one case ("" = unset) and restores them via t.Cleanup.
func setWrapEnv(t *testing.T, roleID, secretID, wrappingToken string) {
	t.Helper()
	for _, kv := range []struct{ k, v string }{
		{"VAULT_ROLE_ID", roleID},
		{"VAULT_SECRET_ID", secretID},
		{"VAULT_WRAPPING_TOKEN", wrappingToken},
	} {
		prev, had := os.LookupEnv(kv.k)
		k, v := kv.k, kv.v
		t.Cleanup(func() {
			if had {
				_ = os.Setenv(k, prev)
			} else {
				_ = os.Unsetenv(k)
			}
		})
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
}

func TestAppRoleSecret_WrappingTokenOutranksRawSecretID(t *testing.T) {
	// A wrapping token, when present, is tried first; no flag is consulted.
	cases := []struct {
		name       string
		cfg        VaultConfig
		wantValue  string
		wantSource string
	}{
		{
			name:       "both set: the wrapping token is tried first",
			cfg:        VaultConfig{SecretID: "raw-secret", WrappingToken: "hvs.wrap"},
			wantValue:  "hvs.wrap",
			wantSource: "VAULT_WRAPPING_TOKEN",
		},
		{
			name:       "only a raw SecretID",
			cfg:        VaultConfig{SecretID: "raw-secret"},
			wantValue:  "raw-secret",
			wantSource: "VAULT_SECRET_ID",
		},
		{
			name:       "only a wrapping token",
			cfg:        VaultConfig{WrappingToken: "hvs.wrap"},
			wantValue:  "hvs.wrap",
			wantSource: "VAULT_WRAPPING_TOKEN",
		},
		{
			name:       "neither set resolves empty",
			cfg:        VaultConfig{},
			wantValue:  "",
			wantSource: "VAULT_SECRET_ID",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, source := tc.cfg.appRoleSecret()
			if got != tc.wantValue {
				t.Fatalf("appRoleSecret() value = %q, want %q", got, tc.wantValue)
			}
			if source != tc.wantSource {
				t.Fatalf("appRoleSecret() source = %q, want %q", source, tc.wantSource)
			}
		})
	}
}

func TestAuthAttempts_PriorityOrderIsWrappedThenAppRoleThenToken(t *testing.T) {
	// The operator-chosen order: single-use first, never-re-minted token last.
	cfg := &VaultConfig{
		Address: "https://vault.example.com",
		RoleID:  "rid", SecretID: "sid", WrappingToken: "hvs.wrap", Token: "hvs.token",
	}
	got := labelsOf(authAttempts(cfg))
	want := []string{"approle (wrapping token)", "approle (secret_id)", "token"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("auth order = %v, want %v", got, want)
	}
}

func TestAuthAttempts_OnlyOffersConfiguredMethods(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  VaultConfig
		want []string
	}{
		{"token only", VaultConfig{Token: "t"}, []string{"token"}},
		{"raw approle only", VaultConfig{RoleID: "r", SecretID: "s"}, []string{"approle (secret_id)"}},
		{"wrapped approle only", VaultConfig{RoleID: "r", WrappingToken: "w"}, []string{"approle (wrapping token)"}},
		{"nothing configured", VaultConfig{}, nil},
		// Both AppRole shapes need a role_id; a lone wrapping token cannot log in.
		{"wrapping token without role_id", VaultConfig{WrappingToken: "w"}, nil},
		{"secret_id without role_id", VaultConfig{SecretID: "s"}, nil},
		{"no role_id but a token", VaultConfig{SecretID: "s", Token: "t"}, []string{"token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelsOf(authAttempts(&tc.cfg)); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("attempts = %v, want %v", got, tc.want)
			}
		})
	}
}

func labelsOf(attempts []authAttempt) []string {
	var out []string
	for _, a := range attempts {
		out = append(out, a.label)
	}
	return out
}

func TestLoadVaultConfig_ReadsWrappingTokenSeparately(t *testing.T) {
	setWrapEnv(t, "role-abc", "raw-secret", "hvs.wrapping")
	cfg := LoadVaultConfig()
	if cfg.WrappingToken != "hvs.wrapping" {
		t.Fatalf("WrappingToken = %q, want %q", cfg.WrappingToken, "hvs.wrapping")
	}
	if cfg.SecretID != "raw-secret" {
		t.Fatalf("SecretID = %q, want it preserved verbatim", cfg.SecretID)
	}
	if !cfg.SecretIDWrapped {
		t.Fatal("SecretIDWrapped = false, want true")
	}
	if v, src := cfg.appRoleSecret(); v != "hvs.wrapping" || src != "VAULT_WRAPPING_TOKEN" {
		t.Fatalf("appRoleSecret() = (%q,%q), want the wrapping token", v, src)
	}
}

// TestEnvAuthMethod_WrappedHostReportsAppRole pins that a degraded host with only
// role_id + wrapping token reports "approle (wrapping token)", never "none".
func TestEnvAuthMethod_WrappedHostReportsAppRole(t *testing.T) {
	setWrapEnv(t, "role-abc", "", "hvs.wrapping")
	prevTok, hadTok := os.LookupEnv("VAULT_TOKEN")
	_ = os.Unsetenv("VAULT_TOKEN")
	t.Cleanup(func() {
		if hadTok {
			_ = os.Setenv("VAULT_TOKEN", prevTok)
		}
	})
	if got := envAuthMethod(); got != string(VaultAuthMethodAppRoleWrapped) {
		t.Fatalf("envAuthMethod() = %q, want %q", got, VaultAuthMethodAppRoleWrapped)
	}
}

// TestResolveSecretID_NeverLogsFullWrappingToken pins the redaction contract:
// the wrapping token's prefix appears in the log, the full value never.
func TestResolveSecretID_NeverLogsFullWrappingToken(t *testing.T) {
	buf := captureLogs(t, LevelTrace)
	const full = "hvs.SUPERSECRETWRAPPINGTOKENVALUE123456"
	cfg := &VaultConfig{RoleID: "role-abc", WrappingToken: full, SecretIDWrapped: true}
	// A client on a dead port fails fast; the prefix line is logged before the
	// request. A nil client would panic inside vault.Unwrap.
	client, err := vault.New(vault.WithAddress("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	_, _ = resolveSecretID(client, cfg, true)
	out := buf.String()
	if strings.Contains(out, full) {
		t.Fatalf("full wrapping token leaked into logs:\n%s", out)
	}
	if !strings.Contains(out, vaultSafePrefix(full)) {
		t.Fatalf("expected the redacted prefix %q in logs; got:\n%s", vaultSafePrefix(full), out)
	}
}

// TestBuildVaultStatus_ReportsSecretIDWrapped pins secret_id_wrapped (the
// credential shape) on both branches, degraded and authenticated.
func TestBuildVaultStatus_ReportsSecretIDWrapped(t *testing.T) {
	prevState := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prevState) })

	prevAddr, hadAddr := os.LookupEnv("VAULT_ADDRESS")
	t.Cleanup(func() {
		if hadAddr {
			_ = os.Setenv("VAULT_ADDRESS", prevAddr)
		} else {
			_ = os.Unsetenv("VAULT_ADDRESS")
		}
	})
	_ = os.Setenv("VAULT_ADDRESS", "https://vault.example")

	for _, tc := range []struct {
		name          string
		wrappingToken string
		live          bool
		want          bool
	}{
		{"degraded and wrapped", "hvs.wrap", false, true},
		{"degraded and not wrapped", "", false, false},
		{"authenticated and wrapped", "hvs.wrap", true, true},
		{"authenticated and not wrapped", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setWrapEnv(t, "role-abc", "sid", tc.wrappingToken)
			if tc.live {
				setGlobalVaultState(&VaultState{
					Client: nil, AuthMethod: VaultAuthMethodAppRole,
					Address: "https://vault.example",
				})
			} else {
				setGlobalVaultState(nil)
			}
			got := BuildVaultStatus()
			if got.SecretIDWrapped != tc.want {
				t.Fatalf("SecretIDWrapped = %t, want %t (status=%q)", got.SecretIDWrapped, tc.want, got.Status)
			}
			// Never leak a credential through this unauthenticated page.
			for _, secret := range []string{"sid", "hvs.wrap"} {
				if got.Address == secret || got.AuthMethod == secret || got.Status == secret {
					t.Fatalf("credential %q leaked into VaultStatusJSON: %+v", secret, got)
				}
			}
		})
	}
}

// ─── auth fallback chain ───────────────────────────────────────────────────

// fallbackVaultServer serves unwrap + approle login + token lookup-self, failing
// whichever stages the test asks it to, so a fallback can be driven end to end.
func fallbackVaultServer(t *testing.T, failUnwrap, failLogin bool, sawLogin *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/sys/wrapping/unwrap"):
			if failUnwrap {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"errors":["wrapping token is not valid or does not exist"]}`))
				return
			}
			b, _ := json.Marshal(map[string]interface{}{
				"request_id": "u", "data": map[string]interface{}{"secret_id": "unwrapped-secret"},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		case strings.Contains(r.URL.Path, "/v1/auth/approle/login"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if sawLogin != nil {
				*sawLogin, _ = body["secret_id"].(string)
			}
			if failLogin {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"errors":["permission denied"]}`))
				return
			}
			b, _ := json.Marshal(map[string]interface{}{
				"request_id": "l", "data": nil,
				"auth": map[string]interface{}{
					"client_token": "hvs.approle", "accessor": "acc",
					"policies": []string{"default"}, "lease_duration": 3600, "renewable": true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(b)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestInitVault_FallsBackToRawSecretIDWhenUnwrapFails(t *testing.T) {
	// The 2026-08-19 outage shape: a dead wrapping token must fall back to the
	// raw SecretID instead of degrading.
	resetWrappedSecretIDCache(t)
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	buf := captureLogs(t, LevelWarning)

	var loginSecretID string
	srv := fallbackVaultServer(t, true /*failUnwrap*/, false, &loginSecretID)

	cfg := &VaultConfig{
		Address: srv.URL, RoleID: "rid",
		WrappingToken: "dead-wrap-token", SecretID: "raw-secret",
		SkipVerify: true,
	}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault should have fallen back to the raw SecretID; got: %v", err)
	}
	if loginSecretID != "raw-secret" {
		t.Errorf("login used %q, want the raw SecretID after the unwrap failed", loginSecretID)
	}
	if GetVaultClient() == nil {
		t.Fatal("expected a live Vault state after a successful fallback")
	}
	// The downgrade to a weaker credential must be visible.
	out := buf.String()
	if !strings.Contains(out, "higher-priority method(s) failed") {
		t.Errorf("a fallback must warn that it downgraded; got:\n%s", out)
	}
	if strings.Contains(out, "dead-wrap-token") || strings.Contains(out, "raw-secret") {
		t.Error("credentials must never appear verbatim in a log line")
	}
}

func TestInitVault_FallsBackToTokenWhenAllAppRoleFails(t *testing.T) {
	resetWrappedSecretIDCache(t)
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })

	srv := fallbackVaultServer(t, true /*failUnwrap*/, true /*failLogin*/, nil)

	cfg := &VaultConfig{
		Address: srv.URL, RoleID: "rid",
		WrappingToken: "dead-wrap", SecretID: "bad-secret", Token: "hvs.static",
		SkipVerify: true,
	}
	if err := InitVault(cfg); err != nil {
		t.Fatalf("InitVault should have fallen back to VAULT_TOKEN; got: %v", err)
	}
	st := GetVaultClient()
	if st == nil {
		t.Fatal("expected a live Vault state from the token fallback")
	}
	if st.AuthMethod != VaultAuthMethodToken {
		t.Errorf("AuthMethod = %q, want %q", st.AuthMethod, VaultAuthMethodToken)
	}
}

func TestInitVault_AllMethodsFail_ReturnsErrorAndStaysDegraded(t *testing.T) {
	resetWrappedSecretIDCache(t)
	prev := GetVaultClient()
	setGlobalVaultState(nil)
	t.Cleanup(func() { setGlobalVaultState(prev) })

	srv := fallbackVaultServer(t, true, true, nil)

	cfg := &VaultConfig{
		Address: srv.URL, RoleID: "rid",
		WrappingToken: "dead-wrap", SecretID: "bad-secret",
		SkipVerify: true,
	}
	err := InitVault(cfg)
	if err == nil {
		t.Fatal("InitVault must return an error when every configured method fails")
	}
	// Degraded mode: the caller warns and continues.
	if GetVaultClient() != nil {
		t.Error("no method succeeded, so global state must stay nil (degraded)")
	}
}

// TestInitVault_PerAttemptFailuresAreNotLoggedAsErrors pins the severity
// contract by COUNT: two failing methods yield exactly one error line (the
// exhaustion summary); each attempt stays visible at Warning.
// See DOCS/MEMORY.md § Vault fallback logged a recovered condition as an error — FIXED.
func TestInitVault_PerAttemptFailuresAreNotLoggedAsErrors(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	// RoleID + WrappingToken + SecretID → two AppRole attempts, both of which
	// fail against an unreachable address.
	cfg := &VaultConfig{
		Address:       "https://127.0.0.1:19999",
		RoleID:        "test-role-id",
		WrappingToken: "hvs.testwrappingtoken",
		SecretID:      "test-secret-id",
		SkipVerify:    true,
	}
	if err := InitVault(cfg); err == nil {
		t.Fatal("expected InitVault to fail against an unreachable address")
	}

	errorLines := strings.Count(buf.String(), `"level":"error"`)
	if errorLines != 1 {
		t.Errorf("got %d error-level lines, want exactly 1 (the chain's \"all methods failed\" summary).\n"+
			"A per-attempt failure must be logged at Warning by the chain, not at Error by the attempt "+
			"— otherwise a RECOVERED fallback looks like a fault in the journal.\nLogs:\n%s",
			errorLines, buf.String())
	}

	// The one legitimate error must be the exhaustion summary.
	if !strings.Contains(buf.String(), "all 2 configured authentication method(s) failed") {
		t.Errorf("expected the single error to be the chain's exhaustion summary; got:\n%s", buf.String())
	}

	// The per-attempt failures must still be VISIBLE — downgraded, not deleted.
	if !strings.Contains(buf.String(), "approle (wrapping token) authentication failed") {
		t.Errorf("the wrapping-token attempt's failure should still be reported at Warning; got:\n%s", buf.String())
	}
}
