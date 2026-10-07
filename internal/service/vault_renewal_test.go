package service

// Renew-self paths use setupFakeVault's in-memory transport; any path reaching
// InitVault (which builds its own client) uses installRealVaultState. Loop tests
// run with ~30ms intervals and assert on side effects.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	vault "github.com/hashicorp/vault-client-go"
)

// installRealVaultState serves handler from an httptest server, installs a
// client for it (RetryMax 0) as globalVaultState until cleanup, and returns the
// URL. Needed when InitVault's fallback must reach the same fake.
func installRealVaultState(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client, err := vault.New(
		vault.WithAddress(server.URL),
		vault.WithRetryConfiguration(vault.RetryConfiguration{RetryMax: 0}),
	)
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}

	prev := GetVaultClient()
	setGlobalVaultState(&VaultState{
		Client:     client,
		AuthMethod: VaultAuthMethodAppRole,
		Token:      "hvs.preexisting",
		Address:    server.URL,
	})
	t.Cleanup(func() { setGlobalVaultState(prev) })

	return server.URL
}

// ─── ResolveRenewalInterval ────────────────────────────────────────────────

func TestResolveRenewalInterval_Default_BothEmpty(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRenewalInterval("", ""); got != DefaultVaultRenewalInterval {
		t.Errorf("default: got %v, want %v", got, DefaultVaultRenewalInterval)
	}
	if DefaultVaultRenewalInterval != 30*time.Minute {
		t.Errorf("DefaultVaultRenewalInterval: got %v, want 30m", DefaultVaultRenewalInterval)
	}
}

func TestResolveRenewalInterval_EnvWinsOverConfig(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRenewalInterval("10m", "1h"); got != 10*time.Minute {
		t.Errorf("got %v, want 10m (env should win)", got)
	}
}

func TestResolveRenewalInterval_ConfigUsedWhenEnvEmpty(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRenewalInterval("", "15m"); got != 15*time.Minute {
		t.Errorf("got %v, want 15m (config when env empty)", got)
	}
}

func TestResolveRenewalInterval_TrimsWhitespace(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRenewalInterval("  20m  ", ""); got != 20*time.Minute {
		t.Errorf("got %v, want 20m (whitespace should be trimmed)", got)
	}
}

func TestResolveRenewalInterval_Zero_DisablesLoop(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	if got := ResolveRenewalInterval("0", ""); got != 0 {
		t.Errorf("got %v, want 0 (renewal disabled)", got)
	}
	if !strings.Contains(buf.String(), "periodic renewal disabled") {
		t.Errorf("expected disable log; got:\n%s", buf.String())
	}
}

func TestResolveRenewalInterval_InvalidFallsBackToDefault(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	if got := ResolveRenewalInterval("not-a-duration", ""); got != DefaultVaultRenewalInterval {
		t.Errorf("invalid env: got %v, want default", got)
	}
	if !strings.Contains(buf.String(), "invalid renewal interval") {
		t.Errorf("expected warning; got:\n%s", buf.String())
	}
}

func TestResolveRenewalInterval_NegativeFallsBackToDefault(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	if got := ResolveRenewalInterval("-5m", ""); got != DefaultVaultRenewalInterval {
		t.Errorf("negative env: got %v, want default", got)
	}
	if !strings.Contains(buf.String(), "negative renewal interval") {
		t.Errorf("expected warning; got:\n%s", buf.String())
	}
}

func TestResolveRenewalInterval_LogsSourceEnv(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ResolveRenewalInterval("45m", "ignored")
	if !strings.Contains(buf.String(), "VAULT_RENEWAL_INTERVAL env") {
		t.Errorf("expected source=env in log; got:\n%s", buf.String())
	}
}

func TestResolveRenewalInterval_LogsSourceConfig(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ResolveRenewalInterval("", "45m")
	if !strings.Contains(buf.String(), "vault.renewal_interval") {
		t.Errorf("expected source=config in log; got:\n%s", buf.String())
	}
}

// ─── RenewVaultToken — fail-soft no-op guards ─────────────────────────────

func TestRenewVaultToken_NilCfg_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	if !RenewVaultToken(nil) {
		t.Error("RenewVaultToken(nil) should return true (skip, not a failure — no backoff)")
	}
	if !strings.Contains(buf.String(), "no Vault config") {
		t.Errorf("expected debug log; got:\n%s", buf.String())
	}
}

func TestRenewVaultToken_EmptyAddress_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	if !RenewVaultToken(&VaultConfig{}) {
		t.Error("RenewVaultToken(empty address) should return true (skip, not a failure — no backoff)")
	}
	if !strings.Contains(buf.String(), "VAULT_ADDRESS not set") {
		t.Errorf("expected 'VAULT_ADDRESS not set' debug log; got:\n%s", buf.String())
	}
}

// ─── RenewVaultToken — happy path: renew-self succeeds ─────────────────────

// renewSelfHappyHandler answers renew-self with lease_duration 7200. Keep the
// explicit "data": null: vault-client-go fills Response[T].Auth only when a
// `data` field is present, as real Vault always sends.
func renewSelfHappyHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/auth/token/renew-self") {
			http.NotFound(w, r)
			return
		}
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake-renew",
			"data":       nil,
			"auth": map[string]interface{}{
				"client_token":   "hvs.renewed-token",
				"accessor":       "acc-renewed",
				"policies":       []string{"default"},
				"lease_duration": 7200,
				"renewable":      true,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

func TestRenewVaultToken_RenewSelfSucceeds_UpdatesLease(t *testing.T) {
	setupFakeVault(t, renewSelfHappyHandler(t))

	// Pre-populate state with an old lease so we can verify it changes.
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	state := *prev // copy the fake state set by setupFakeVault
	state.LeaseDuration = 60
	state.AuthMethod = VaultAuthMethodAppRole
	setGlobalVaultState(&state)

	cfg := &VaultConfig{Address: "http://fake-vault.invalid", Token: "irrelevant"}

	buf := captureLogs(t, LevelInfo)
	if !RenewVaultToken(cfg) {
		t.Error("RenewVaultToken should return true when renew-self succeeds")
	}

	got := GetVaultClient()
	if got == nil {
		t.Fatal("state should still be set after successful renewal")
	}
	if got.LeaseDuration != 7200 {
		t.Errorf("LeaseDuration: got %d, want 7200", got.LeaseDuration)
	}
	if !got.Renewable {
		t.Error("Renewable: got false, want true")
	}
	if !strings.Contains(buf.String(), "token renewed") {
		t.Errorf("expected 'token renewed' info log; got:\n%s", buf.String())
	}
}

// ─── RenewVaultToken — renewal fails, falls back to AppRole re-auth ───────

// renewSelfFailsApproleSucceeds returns 403 on renew-self and a fresh
// AppRole login response on auth/approle/login.
func renewSelfFailsApproleSucceeds(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/auth/token/renew-self"):
			http.Error(w, `{"errors":["max_ttl exceeded"]}`, http.StatusForbidden)
		case strings.Contains(r.URL.Path, "/v1/auth/approle/login"):
			body, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-login",
				"data":       nil,
				"auth": map[string]interface{}{
					"client_token":   "hvs.fresh-token",
					"accessor":       "acc-fresh",
					"policies":       []string{"default", "myapp"},
					"lease_duration": 3600,
					"renewable":      true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestRenewVaultToken_RenewFails_FallsBackToAppRoleLogin(t *testing.T) {
	serverURL := installRealVaultState(t, renewSelfFailsApproleSucceeds(t))

	// Re-auth reads the env, not cfg, so the env must point at the fake.
	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "") // force the AppRole path
	t.Setenv("VAULT_ROLE_ID", "test-role-id")
	t.Setenv("VAULT_SECRET_ID", "test-secret-id")

	cfg := &VaultConfig{
		Address:  serverURL,
		RoleID:   "test-role-id",
		SecretID: "test-secret-id",
	}

	buf := captureLogs(t, LevelWarning)
	if !RenewVaultToken(cfg) {
		t.Error("RenewVaultToken should return true when re-auth recovers a token")
	}

	got := GetVaultClient()
	if got == nil {
		t.Fatal("state should be set after re-auth")
	}
	if got.Token != "hvs.fresh-token" {
		t.Errorf("Token: got %q, want hvs.fresh-token (re-auth should replace the token)", got.Token)
	}
	if got.LeaseDuration != 3600 {
		t.Errorf("LeaseDuration: got %d, want 3600 (from re-auth response)", got.LeaseDuration)
	}
	if !strings.Contains(buf.String(), "token renewal failed") {
		t.Errorf("expected 'token renewal failed' warning; got:\n%s", buf.String())
	}
}

// TestRenewVaultToken_ReAuth_UsesRotatedEnvSecretID pins the hot-reload
// contract: re-auth uses VAULT_SECRET_ID from the process env, not the stale cfg.
func TestRenewVaultToken_ReAuth_UsesRotatedEnvSecretID(t *testing.T) {
	var gotSecretID string
	serverURL := installRealVaultState(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/auth/token/renew-self"):
			http.Error(w, `{"errors":["max_ttl exceeded"]}`, http.StatusForbidden)
		case strings.Contains(r.URL.Path, "/v1/auth/approle/login"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotSecretID, _ = body["secret_id"].(string)
			out, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-login", "data": nil,
				"auth": map[string]interface{}{
					"client_token": "hvs.rotated", "lease_duration": 3600, "renewable": true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(out)
		default:
			http.NotFound(w, r)
		}
	})

	// ENV carries the ROTATED secret; the passed cfg carries the STALE one.
	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "test-role-id")
	t.Setenv("VAULT_SECRET_ID", "rotated-secret-id")

	RenewVaultToken(&VaultConfig{Address: serverURL, RoleID: "test-role-id", SecretID: "stale-startup-secret-id"})

	if gotSecretID != "rotated-secret-id" {
		t.Errorf("re-auth must use the ROTATED env secret_id; got %q (the stale cfg value is 'stale-startup-secret-id')", gotSecretID)
	}
}

// ─── RenewVaultToken — both paths fail, state preserved (stale) ───────────

func TestRenewVaultToken_BothPathsFail_KeepsPreviousState(t *testing.T) {
	serverURL := installRealVaultState(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":["service unavailable"]}`, http.StatusServiceUnavailable)
	})

	// Plant the stale token the test asserts survives.
	prev := GetVaultClient()
	state := *prev
	state.Token = "hvs.stale-but-still-here"
	state.LeaseDuration = 60
	setGlobalVaultState(&state)

	// Point the env at the failing fake so the re-auth really attempts AppRole.
	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "rid")
	t.Setenv("VAULT_SECRET_ID", "sid")

	cfg := &VaultConfig{
		Address:  serverURL,
		RoleID:   "rid",
		SecretID: "sid",
	}

	buf := captureLogs(t, LevelWarning)
	if RenewVaultToken(cfg) {
		t.Error("RenewVaultToken should return false when both renew-self and re-auth fail (triggers backoff)")
	}

	got := GetVaultClient()
	if got == nil {
		t.Fatal("state must NOT be wiped when both renewal and re-auth fail")
	}
	if got.Token != "hvs.stale-but-still-here" {
		t.Errorf("stale token must be preserved; got %q", got.Token)
	}
	out := buf.String()
	if !strings.Contains(out, "token renewal failed") {
		t.Errorf("expected renewal-failed warning; got:\n%s", out)
	}
	if !strings.Contains(out, "re-authentication failed") &&
		!strings.Contains(out, "AppRole login failed") {
		t.Errorf("expected re-auth-failed warning; got:\n%s", out)
	}
}

// ─── RenewVaultToken — recovery from nil state ────────────────────────────

func TestRenewVaultToken_NilState_AttemptsInitialAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/auth/approle/login") {
			http.NotFound(w, r)
			return
		}
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake-login",
			"data":       nil,
			"auth": map[string]interface{}{
				"client_token":   "hvs.recovered",
				"accessor":       "acc-recovered",
				"policies":       []string{"default"},
				"lease_duration": 3600,
				"renewable":      true,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(server.Close)

	// Vault down at startup: no state was ever set.
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	setGlobalVaultState(nil)

	// The recovery path also re-reads the env (not the passed cfg).
	t.Setenv("VAULT_ADDRESS", server.URL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "rid")
	t.Setenv("VAULT_SECRET_ID", "sid")

	cfg := &VaultConfig{
		Address:  server.URL,
		RoleID:   "rid",
		SecretID: "sid",
	}

	buf := captureLogs(t, LevelInfo)
	if !RenewVaultToken(cfg) {
		t.Error("RenewVaultToken should return true when nil-state recovery succeeds")
	}

	got := GetVaultClient()
	if got == nil {
		t.Fatal("nil state should be recovered by RenewVaultToken")
	}
	if got.Token != "hvs.recovered" {
		t.Errorf("Token after recovery: got %q, want hvs.recovered", got.Token)
	}
	if !strings.Contains(buf.String(), "no current Vault state") {
		t.Errorf("expected 'no current Vault state' info log; got:\n%s", buf.String())
	}
}

// TestRenewVaultToken_NilState_NoCredentials_DoesNotRecordFalseReauth pins that
// InitVault's no-credentials skip (nil error, no state) is a failed cycle, not a reauth.
func TestRenewVaultToken_NilState_NoCredentials_DoesNotRecordFalseReauth(t *testing.T) {
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	setGlobalVaultState(nil)

	// Address set, no credentials: InitVault's skip guard.
	t.Setenv("VAULT_ADDRESS", "https://vault.example.invalid")
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "")
	t.Setenv("VAULT_SECRET_ID", "")

	reauth0 := metVaultRenewReauth.Load()
	failure0 := metVaultRenewFailure.Load()

	buf := captureLogs(t, LevelWarning)
	cfg := &VaultConfig{Address: "https://vault.example.invalid"}
	if RenewVaultToken(cfg) {
		t.Error("RenewVaultToken should return false when InitVault only hit its no-credentials skip path")
	}

	if got := GetVaultClient(); got != nil {
		t.Errorf("state should remain nil after a credential-less skip; got %+v", got)
	}
	if metVaultRenewReauth.Load() != reauth0 {
		t.Errorf("reauth counter must NOT increment on a skip (false health signal); got delta %d",
			metVaultRenewReauth.Load()-reauth0)
	}
	if metVaultRenewFailure.Load() != failure0+1 {
		t.Errorf("failure counter should increment by 1 on a skip; got delta %d",
			metVaultRenewFailure.Load()-failure0)
	}
	if !strings.Contains(buf.String(), "did not produce a live token") {
		t.Errorf("expected 'did not produce a live token' warning; got:\n%s", buf.String())
	}
}

// TestRenewVaultToken_ReAuthFallback_NoCredentials_DoesNotRecordFalseReauth is
// the same for the re-auth fallback, where the stale state stays non-nil after
// the skip, so detection must be by identity, not a nil check.
func TestRenewVaultToken_ReAuthFallback_NoCredentials_DoesNotRecordFalseReauth(t *testing.T) {
	serverURL := installRealVaultState(t, func(w http.ResponseWriter, r *http.Request) {
		// renew-self fails; with no credentials the login endpoint is never reached.
		http.Error(w, `{"errors":["max_ttl exceeded"]}`, http.StatusForbidden)
	})

	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "")
	t.Setenv("VAULT_SECRET_ID", "")

	reauth0 := metVaultRenewReauth.Load()
	failure0 := metVaultRenewFailure.Load()

	prevState := GetVaultClient()
	buf := captureLogs(t, LevelWarning)
	cfg := &VaultConfig{Address: serverURL}
	if RenewVaultToken(cfg) {
		t.Error("RenewVaultToken should return false when the re-auth fallback only hit InitVault's no-credentials skip")
	}

	got := GetVaultClient()
	if got != prevState {
		t.Errorf("stale state must be preserved unchanged on a skip; got %+v, want %+v", got, prevState)
	}
	if metVaultRenewReauth.Load() != reauth0 {
		t.Errorf("reauth counter must NOT increment on a skip (false health signal); got delta %d",
			metVaultRenewReauth.Load()-reauth0)
	}
	if metVaultRenewFailure.Load() != failure0+1 {
		t.Errorf("failure counter should increment by 1 on a skip; got delta %d",
			metVaultRenewFailure.Load()-failure0)
	}
	if !strings.Contains(buf.String(), "did not produce a live token") {
		t.Errorf("expected 'did not produce a live token' warning; got:\n%s", buf.String())
	}
}

// ─── StartVaultRenewal ─────────────────────────────────────────────────────

func TestStartVaultRenewal_NilCfg_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartVaultRenewal(ctx, nil, time.Hour)

	if !strings.Contains(buf.String(), "renewal disabled") {
		t.Errorf("expected disabled log; got:\n%s", buf.String())
	}
}

func TestStartVaultRenewal_EmptyAddress_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartVaultRenewal(ctx, &VaultConfig{}, time.Hour)

	if !strings.Contains(buf.String(), "VAULT_ADDRESS not set") {
		t.Errorf("expected 'VAULT_ADDRESS not set' log; got:\n%s", buf.String())
	}
}

func TestStartVaultRenewal_ZeroInterval_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartVaultRenewal(ctx, &VaultConfig{Address: "http://x"}, 0)

	if !strings.Contains(buf.String(), "periodic renewal disabled") {
		t.Errorf("expected zero-interval disabled log; got:\n%s", buf.String())
	}
}

func TestStartVaultRenewal_NegativeInterval_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartVaultRenewal(ctx, &VaultConfig{Address: "http://x"}, -10*time.Second)

	if !strings.Contains(buf.String(), "periodic renewal disabled") {
		t.Errorf("expected negative-interval disabled log; got:\n%s", buf.String())
	}
}

func TestStartVaultRenewal_PeriodicLoopFires(t *testing.T) {
	var renewCount atomic.Int32
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/auth/token/renew-self") {
			http.NotFound(w, r)
			return
		}
		renewCount.Add(1)
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake-renew",
			"data":       nil,
			"auth": map[string]interface{}{
				"client_token":   "hvs.renewed",
				"accessor":       "acc-renewed",
				"policies":       []string{"default"},
				"lease_duration": 3600,
				"renewable":      true,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// Ensure state has a sane AuthMethod so renew-self is the path taken.
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	state := *prev
	state.AuthMethod = VaultAuthMethodAppRole
	setGlobalVaultState(&state)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartVaultRenewal(ctx, &VaultConfig{Address: "http://fake-vault.invalid"}, 30*time.Millisecond)

	time.Sleep(200 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond) // let goroutine observe ctx.Done

	got := renewCount.Load()
	if got < 2 {
		t.Errorf("expected at least 2 periodic renewals in 200ms (interval=30ms); got %d", got)
	}
}

func TestStartVaultRenewal_ContextCancel_StopsLoop(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake",
			"data":       nil,
			"auth": map[string]interface{}{
				"client_token":   "hvs.x",
				"lease_duration": 3600,
				"renewable":      true,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	state := *prev
	state.AuthMethod = VaultAuthMethodAppRole
	setGlobalVaultState(&state)

	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	StartVaultRenewal(ctx, &VaultConfig{Address: "http://fake-vault.invalid"}, 25*time.Millisecond)

	time.Sleep(80 * time.Millisecond)
	cancel()
	time.Sleep(120 * time.Millisecond)

	if !strings.Contains(buf.String(), "loop stopping") {
		t.Errorf("expected 'loop stopping' log after cancel; got:\n%s", buf.String())
	}
}

// ─── nextRenewalDelay — lease-aware scheduling ─────────────────────────────

func TestNextRenewalDelay_NilState_ReturnsCeiling(t *testing.T) {
	if got := nextRenewalDelay(30*time.Minute, nil); got != 30*time.Minute {
		t.Errorf("nil state: got %v, want ceiling 30m", got)
	}
}

func TestNextRenewalDelay_NonRenewable_ReturnsCeiling(t *testing.T) {
	st := &VaultState{LeaseDuration: 3600, Renewable: false}
	if got := nextRenewalDelay(30*time.Minute, st); got != 30*time.Minute {
		t.Errorf("non-renewable: got %v, want ceiling 30m", got)
	}
}

func TestNextRenewalDelay_ZeroLease_ReturnsCeiling(t *testing.T) {
	st := &VaultState{LeaseDuration: 0, Renewable: true}
	if got := nextRenewalDelay(30*time.Minute, st); got != 30*time.Minute {
		t.Errorf("zero lease: got %v, want ceiling 30m", got)
	}
}

func TestNextRenewalDelay_LeaseBelowCeiling_UsesTwoThirdsOfLease(t *testing.T) {
	// lease 1200s, 2/3 = 800s, below the 30m ceiling → the lease drives the delay.
	st := &VaultState{LeaseDuration: 1200, Renewable: true}
	if got := nextRenewalDelay(30*time.Minute, st); got != 800*time.Second {
		t.Errorf("got %v, want 800s (2/3 of a 1200s lease)", got)
	}
}

func TestNextRenewalDelay_LeaseAboveCeiling_ReturnsCeiling(t *testing.T) {
	// lease 3600s, 2/3 = 2400s (40m), exceeds the 30m ceiling → ceiling caps it.
	st := &VaultState{LeaseDuration: 3600, Renewable: true}
	if got := nextRenewalDelay(30*time.Minute, st); got != 30*time.Minute {
		t.Errorf("got %v, want ceiling 30m", got)
	}
}

func TestNextRenewalDelay_ShortLease_ClampedToFloor(t *testing.T) {
	// lease 6s, 2/3 = 4s, below the floor → clamped up to vaultRenewalFloor.
	st := &VaultState{LeaseDuration: 6, Renewable: true}
	floor := time.Duration(vaultRenewalFloor.Load())
	if got := nextRenewalDelay(30*time.Minute, st); got != floor {
		t.Errorf("got %v, want floor %v", got, floor)
	}
}

// TestStartVaultRenewal_LeaseAware_RenewsFasterThanCeiling pins that both the
// initial timer and each Reset use nextRenewalDelay, not the raw ceiling.
func TestStartVaultRenewal_LeaseAware_RenewsFasterThanCeiling(t *testing.T) {
	// Lower the floor so the 1s lease's 2/3 (~666ms) is not clamped up.
	origFloor := vaultRenewalFloor.Load()
	vaultRenewalFloor.Store(int64(time.Millisecond))
	t.Cleanup(func() { vaultRenewalFloor.Store(origFloor) })

	var renewCount atomic.Int32
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/auth/token/renew-self") {
			http.NotFound(w, r)
			return
		}
		renewCount.Add(1)
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake-renew",
			"data":       nil,
			"auth": map[string]interface{}{
				"client_token":   "hvs.renewed",
				"lease_duration": 1, // 1s lease → ~666ms renewal cadence
				"renewable":      true,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// Seed a renewable short-lease state so the FIRST timer is lease-aware too.
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	state := *prev
	state.AuthMethod = VaultAuthMethodAppRole
	state.LeaseDuration = 1
	state.Renewable = true
	setGlobalVaultState(&state)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Ceiling 5s ≫ the test window: any renewal proves the lease drove the schedule.
	StartVaultRenewal(ctx, &VaultConfig{Address: "http://fake-vault.invalid"}, 5*time.Second)

	time.Sleep(1600 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if got := renewCount.Load(); got < 2 {
		t.Errorf("lease-aware loop should renew ~every 666ms (1s lease) despite a 5s ceiling; got %d renewals in 1.6s", got)
	}
}

// TestStartVaultRenewal_NilStateAtStartup_RetriesAtFloorNotCeiling pins that,
// with no state at startup, the first attempt waits the backoff floor, not the
// full ceiling nextRenewalDelay would return.
func TestStartVaultRenewal_NilStateAtStartup_RetriesAtFloorNotCeiling(t *testing.T) {
	origFloor := vaultRenewalFloor.Load()
	vaultRenewalFloor.Store(int64(20 * time.Millisecond))
	t.Cleanup(func() { vaultRenewalFloor.Store(origFloor) })

	var loginCount atomic.Int32
	serverURL := installRealVaultState(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/auth/approle/login") {
			http.NotFound(w, r)
			return
		}
		loginCount.Add(1)
		body, _ := json.Marshal(map[string]interface{}{
			"request_id": "fake-login",
			"data":       nil,
			"auth": map[string]interface{}{
				"client_token":   "hvs.recovered-at-startup",
				"lease_duration": 3600,
				"renewable":      true,
			},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// Simulate "Vault was down at startup" — no state was ever established.
	prev := GetVaultClient()
	t.Cleanup(func() { setGlobalVaultState(prev) })
	setGlobalVaultState(nil)

	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "rid")
	t.Setenv("VAULT_SECRET_ID", "sid")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Ceiling ≫ the test window: waiting it would land zero logins.
	StartVaultRenewal(ctx, &VaultConfig{Address: serverURL, RoleID: "rid", SecretID: "sid"}, 5*time.Second)

	time.Sleep(300 * time.Millisecond)
	cancel()
	time.Sleep(50 * time.Millisecond)

	if loginCount.Load() < 1 {
		t.Fatalf("expected the first re-auth attempt within ~300ms (floor=20ms) despite a 5s ceiling; got %d logins", loginCount.Load())
	}
	if got := GetVaultClient(); got == nil || got.Token != "hvs.recovered-at-startup" {
		t.Errorf("expected recovered token after startup retry; got %+v", got)
	}
}

// ─── backoffDelay — failure retry cadence ──────────────────────────────────

// TestBackoffDelay pins the exponential-from-floor, capped-at-ceiling schedule
// the renewal loop uses after a failed cycle.
func TestBackoffDelay(t *testing.T) {
	origFloor := vaultRenewalFloor.Load()
	vaultRenewalFloor.Store(int64(10 * time.Second))
	t.Cleanup(func() { vaultRenewalFloor.Store(origFloor) })

	ceiling := 30 * time.Minute
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{4, 80 * time.Second},
		{5, 160 * time.Second},
		{100, ceiling}, // exponential growth is capped at the ceiling
	}
	for _, c := range cases {
		if got := backoffDelay(c.failures, ceiling); got != c.want {
			t.Errorf("backoffDelay(%d, %s): got %s, want %s", c.failures, ceiling, got, c.want)
		}
	}
}

// TestBackoffDelay_CeilingBelowFloor clamps every attempt to a ceiling shorter
// than the floor (a degenerate but valid config).
func TestBackoffDelay_CeilingBelowFloor(t *testing.T) {
	origFloor := vaultRenewalFloor.Load()
	vaultRenewalFloor.Store(int64(10 * time.Second))
	t.Cleanup(func() { vaultRenewalFloor.Store(origFloor) })

	if got := backoffDelay(1, 3*time.Second); got != 3*time.Second {
		t.Errorf("backoffDelay(1, 3s) with 10s floor: got %s, want 3s", got)
	}
	if got := backoffDelay(9, 3*time.Second); got != 3*time.Second {
		t.Errorf("backoffDelay(9, 3s) with 10s floor: got %s, want 3s", got)
	}
}

// ─── Renewal floor + collapsed-lease re-auth ───────────────────────────────

// withRenewalFloor sets the floor for one test and restores it afterwards.
func withRenewalFloor(t *testing.T, d time.Duration) {
	t.Helper()
	orig := vaultRenewalFloor.Load()
	vaultRenewalFloor.Store(int64(d))
	t.Cleanup(func() { vaultRenewalFloor.Store(orig) })
}

func TestResolveRenewalFloor_Precedence(t *testing.T) {
	cases := []struct {
		name, env, cfg string
		want           time.Duration
	}{
		{"env wins", "5s", "30s", 5 * time.Second},
		{"config when env empty", "", "30s", 30 * time.Second},
		{"default when both empty", "", "", DefaultVaultRenewalFloor},
		{"invalid falls back", "not-a-duration", "", DefaultVaultRenewalFloor},
		{"whitespace-only env falls through to config", "   ", "7s", 7 * time.Second},
		// Unlike the interval, 0 never disables the floor (a zero floor busy-loops).
		{"zero falls back", "0s", "", DefaultVaultRenewalFloor},
		{"negative falls back", "-5s", "", DefaultVaultRenewalFloor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveRenewalFloor(c.env, c.cfg); got != c.want {
				t.Errorf("ResolveRenewalFloor(%q, %q) = %s, want %s", c.env, c.cfg, got, c.want)
			}
		})
	}
}

// SetRenewalFloor is the holder guard: a caller bypassing the resolver must not
// be able to install a zero floor.
func TestSetRenewalFloor_NonPositiveTakesDefault(t *testing.T) {
	withRenewalFloor(t, time.Hour)
	for _, d := range []time.Duration{0, -time.Second} {
		SetRenewalFloor(d)
		if got := time.Duration(vaultRenewalFloor.Load()); got != DefaultVaultRenewalFloor {
			t.Errorf("SetRenewalFloor(%s) stored %s, want %s", d, got, DefaultVaultRenewalFloor)
		}
	}
	SetRenewalFloor(3 * time.Second)
	if got := time.Duration(vaultRenewalFloor.Load()); got != 3*time.Second {
		t.Errorf("SetRenewalFloor(3s) stored %s", got)
	}
}

// TestLeaseTooShortToRenew_MatchesObservedCollapse replays the observed
// 900→600→200→67→22→7s lease sequence: only 7s (followed by the 403) collapses.
// See DOCS/CLAUDE.md § In-process Vault Agent
func TestLeaseTooShortToRenew_MatchesObservedCollapse(t *testing.T) {
	withRenewalFloor(t, DefaultVaultRenewalFloor) // 10s, as deployed

	healthy := []int{900, 900, 900, 900, 600, 200, 67}
	for _, lease := range healthy {
		st := &VaultState{Renewable: true, LeaseDuration: lease}
		if leaseTooShortToRenew(st) {
			// 67s → 2/3 = 44.6s, still comfortably above a 10s floor.
			t.Errorf("lease %ds must still be renewable (2/3 = %s >= floor)",
				lease, time.Duration(lease)*time.Second*2/3)
		}
	}

	// 22s → 14.6s, above the floor; 7s → 4.6s, below it — the boundary the 403 marked.
	if leaseTooShortToRenew(&VaultState{Renewable: true, LeaseDuration: 22}) {
		t.Error("lease 22s is still renewable above a 10s floor")
	}
	if !leaseTooShortToRenew(&VaultState{Renewable: true, LeaseDuration: 7}) {
		t.Error("lease 7s must be judged collapsed — this is the renewal that took a 403 on .18")
	}
}

// A token with no TTL signal never collapses; otherwise a static VAULT_TOKEN
// would re-run InitVault every cycle.
func TestLeaseTooShortToRenew_IgnoresTokensWithNoLeaseSignal(t *testing.T) {
	withRenewalFloor(t, time.Hour) // absurdly high: would collapse anything real
	cases := []struct {
		name  string
		state *VaultState
	}{
		{"nil state", nil},
		{"non-renewable", &VaultState{Renewable: false, LeaseDuration: 1}},
		{"zero lease", &VaultState{Renewable: true, LeaseDuration: 0}},
		{"negative lease", &VaultState{Renewable: true, LeaseDuration: -5}},
	}
	for _, c := range cases {
		if leaseTooShortToRenew(c.state) {
			t.Errorf("%s must never be judged collapsed", c.name)
		}
	}
}

// The floor is what separates healthy from collapsed, so raising it must move
// the boundary — proving the knob is wired to the decision and not decorative.
func TestLeaseTooShortToRenew_FloorIsTheThreshold(t *testing.T) {
	st := &VaultState{Renewable: true, LeaseDuration: 60} // 2/3 = 40s

	withRenewalFloor(t, 10*time.Second)
	if leaseTooShortToRenew(st) {
		t.Error("40s of headroom is not collapsed under a 10s floor")
	}

	withRenewalFloor(t, 45*time.Second)
	if !leaseTooShortToRenew(st) {
		t.Error("40s of headroom must be collapsed under a 45s floor")
	}
}

// TestRenewVaultToken_CollapsedLease_ReAuthsWhileTokenStillValid pins that a
// SUCCESSFUL renew-self returning a collapsed 7s lease re-auths at once.
// See DOCS/CLAUDE.md § In-process Vault Agent
func TestRenewVaultToken_CollapsedLease_ReAuthsWhileTokenStillValid(t *testing.T) {
	withRenewalFloor(t, DefaultVaultRenewalFloor)

	var loginCalls int
	serverURL := installRealVaultState(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/auth/token/renew-self"):
			// Success with a lease too short to renew: what Vault returns at max_ttl.
			out, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-renew", "data": nil,
				"auth": map[string]interface{}{
					"client_token": "hvs.nearly-dead", "lease_duration": 7, "renewable": true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(out)
		case strings.Contains(r.URL.Path, "/v1/auth/approle/login"):
			loginCalls++
			out, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-login", "data": nil,
				"auth": map[string]interface{}{
					"client_token": "hvs.fresh", "lease_duration": 900, "renewable": true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(out)
		default:
			http.NotFound(w, r)
		}
	})

	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "test-role-id")
	t.Setenv("VAULT_SECRET_ID", "test-secret-id")

	buf := captureLogs(t, LevelInfo)
	if !RenewVaultToken(&VaultConfig{Address: serverURL, RoleID: "test-role-id", SecretID: "test-secret-id"}) {
		t.Fatal("a collapsed lease is still a LIVE token — the cycle must not report failure or the loop enters backoff")
	}

	if loginCalls != 1 {
		t.Fatalf("expected exactly one pre-emptive AppRole login; got %d", loginCalls)
	}
	got := GetVaultClient()
	if got == nil {
		t.Fatal("state must be live after the pre-emptive re-auth")
	}
	if got.LeaseDuration != 900 {
		t.Errorf("state must carry the FRESH lease, not the collapsed one: got %d, want 900", got.LeaseDuration)
	}
	if !strings.Contains(buf.String(), "max_ttl") {
		t.Errorf("the re-auth must say why it fired; got:\n%s", buf.String())
	}
}

// A healthy lease must NOT trigger a re-auth (a login per cycle).
func TestRenewVaultToken_HealthyLease_DoesNotReAuth(t *testing.T) {
	withRenewalFloor(t, DefaultVaultRenewalFloor)

	var loginCalls int
	serverURL := installRealVaultState(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/auth/token/renew-self"):
			out, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-renew", "data": nil,
				"auth": map[string]interface{}{
					"client_token": "hvs.healthy", "lease_duration": 900, "renewable": true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(out)
		case strings.Contains(r.URL.Path, "/v1/auth/approle/login"):
			loginCalls++
			http.Error(w, `{"errors":["should not be called"]}`, http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	})

	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "test-role-id")
	t.Setenv("VAULT_SECRET_ID", "test-secret-id")

	if !RenewVaultToken(&VaultConfig{Address: serverURL, RoleID: "test-role-id", SecretID: "test-secret-id"}) {
		t.Fatal("a healthy renewal must report success")
	}
	if loginCalls != 0 {
		t.Errorf("a healthy 900s lease must not trigger re-auth; got %d login(s)", loginCalls)
	}
}

// A failed pre-emptive re-auth still reports SUCCESS: the short-lived token is
// live, and failure would push the loop into backoff.
func TestRenewVaultToken_CollapsedLease_ReAuthFailureStillReportsLiveToken(t *testing.T) {
	withRenewalFloor(t, DefaultVaultRenewalFloor)

	serverURL := installRealVaultState(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/auth/token/renew-self"):
			out, _ := json.Marshal(map[string]interface{}{
				"request_id": "fake-renew", "data": nil,
				"auth": map[string]interface{}{
					"client_token": "hvs.nearly-dead", "lease_duration": 7, "renewable": true,
				},
			})
			w.Header().Set("Content-Type", "application/json")
			w.Write(out)
		case strings.Contains(r.URL.Path, "/v1/auth/approle/login"):
			http.Error(w, `{"errors":["vault sealed"]}`, http.StatusServiceUnavailable)
		default:
			http.NotFound(w, r)
		}
	})

	t.Setenv("VAULT_ADDRESS", serverURL)
	t.Setenv("VAULT_TOKEN", "")
	t.Setenv("VAULT_ROLE_ID", "test-role-id")
	t.Setenv("VAULT_SECRET_ID", "test-secret-id")

	buf := captureLogs(t, LevelWarning)
	if !RenewVaultToken(&VaultConfig{Address: serverURL, RoleID: "test-role-id", SecretID: "test-secret-id"}) {
		t.Error("a failed pre-emptive re-auth must not turn a live (if short) token into a failed cycle")
	}
	if !strings.Contains(buf.String(), "pre-emptive re-authentication failed") {
		t.Errorf("expected a warning naming the failed pre-emptive re-auth; got:\n%s", buf.String())
	}
}

// ─── auth upgrade (preferred-credential re-attempt) ────────────────────────

// newTestVaultClient returns a client for an unroutable address: the upgrade
// tests need only a non-nil state.Client, never a successful call.
func newTestVaultClient(t *testing.T) *vault.Client {
	t.Helper()
	c, err := vault.New(
		vault.WithAddress("http://127.0.0.1:1"),
		vault.WithRetryConfiguration(vault.RetryConfiguration{RetryMax: 0}),
	)
	if err != nil {
		t.Fatalf("vault.New: %v", err)
	}
	return c
}

// TestAuthAttempts_MethodMatchesLabel pins each attempt's method to its label and
// preferredAuthMethod to the chain head, which the upgrade comparison relies on.
func TestAuthAttempts_MethodMatchesLabel(t *testing.T) {
	cfg := &VaultConfig{RoleID: "r", WrappingToken: "w", SecretID: "s", Token: "t"}
	attempts := authAttempts(cfg)
	if len(attempts) != 3 {
		t.Fatalf("got %d attempts, want 3 (wrapping, secret_id, token)", len(attempts))
	}
	for _, a := range attempts {
		if string(a.method) != a.label {
			t.Errorf("attempt label %q has method %q — they must match so a value read from / can be grepped in the journal", a.label, a.method)
		}
	}
	if got, ok := preferredAuthMethod(cfg); !ok || got != VaultAuthMethodAppRoleWrapped {
		t.Errorf("preferredAuthMethod = (%q,%v), want (%q,true)", got, ok, VaultAuthMethodAppRoleWrapped)
	}
}

func TestPreferredAuthMethod_FollowsConfiguredPriority(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *VaultConfig
		want VaultAuthMethod
		ok   bool
	}{
		{"wrapping wins", &VaultConfig{RoleID: "r", WrappingToken: "w", SecretID: "s", Token: "t"}, VaultAuthMethodAppRoleWrapped, true},
		{"secret_id over token", &VaultConfig{RoleID: "r", SecretID: "s", Token: "t"}, VaultAuthMethodAppRoleSecretID, true},
		{"token only", &VaultConfig{Token: "t"}, VaultAuthMethodToken, true},
		{"approle needs role_id", &VaultConfig{SecretID: "s", Token: "t"}, VaultAuthMethodToken, true},
		{"nothing configured", &VaultConfig{}, VaultAuthMethodNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := preferredAuthMethod(tc.cfg)
			if got != tc.want || ok != tc.ok {
				t.Errorf("got (%q,%v), want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// The normal case must cost nothing: a session already on the preferred
// credential performs no Vault traffic and schedules no attempt.
func TestMaybeUpgradeVaultAuth_NoopWhenAlreadyPreferred(t *testing.T) {
	resetAuthUpgradeStateForTest()
	t.Cleanup(resetAuthUpgradeStateForTest)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	globalVaultState = &VaultState{Client: newTestVaultClient(t), AuthMethod: VaultAuthMethodToken}
	maybeUpgradeVaultAuth(&VaultConfig{Token: "t"}) // token IS the preferred here

	if lastAuthUpgradeUnixNano.Load() != 0 {
		t.Error("an attempt was scheduled for a session already on the preferred credential")
	}
}

// A downgraded session must not re-attempt more often than the backoff allows.
func TestMaybeUpgradeVaultAuth_RespectsInterval(t *testing.T) {
	resetAuthUpgradeStateForTest()
	t.Cleanup(resetAuthUpgradeStateForTest)
	prev := globalVaultState
	prevNow := vaultAuthUpgradeNow
	t.Cleanup(func() { globalVaultState = prev; vaultAuthUpgradeNow = prevNow })

	base := time.Now()
	vaultAuthUpgradeNow = func() time.Time { return base }

	// Live session on `token` while `approle (secret_id)` is configured and preferred.
	globalVaultState = &VaultState{Client: newTestVaultClient(t), AuthMethod: VaultAuthMethodToken}
	cfg := &VaultConfig{Address: "http://127.0.0.1:1", RoleID: "r", SecretID: "s", Token: "t"}

	maybeUpgradeVaultAuth(cfg)
	first := lastAuthUpgradeUnixNano.Load()
	if first == 0 {
		t.Fatal("first downgraded call should have attempted an upgrade")
	}

	// Immediately after, still inside the interval: must not re-attempt.
	vaultAuthUpgradeNow = func() time.Time { return base.Add(time.Minute) }
	maybeUpgradeVaultAuth(cfg)
	if lastAuthUpgradeUnixNano.Load() != first {
		t.Error("re-attempted inside the backoff interval")
	}
}

// The upgrade backoff widens on attempts that change nothing and caps at 24h.
// See ADR-0009.
func TestAuthUpgradeDelay_BacksOffAndCaps(t *testing.T) {
	resetAuthUpgradeStateForTest()
	t.Cleanup(resetAuthUpgradeStateForTest)

	if got := authUpgradeDelay(); got != vaultAuthUpgradeInterval {
		t.Errorf("with no failures got %s, want base %s", got, vaultAuthUpgradeInterval)
	}
	authUpgradeNoChangeCount.Store(1)
	if got := authUpgradeDelay(); got != 2*vaultAuthUpgradeInterval {
		t.Errorf("after 1 no-change got %s, want %s", got, 2*vaultAuthUpgradeInterval)
	}
	authUpgradeNoChangeCount.Store(3)
	if got := authUpgradeDelay(); got != 8*vaultAuthUpgradeInterval {
		t.Errorf("after 3 no-change got %s, want %s", got, 8*vaultAuthUpgradeInterval)
	}
	authUpgradeNoChangeCount.Store(99)
	if got := authUpgradeDelay(); got != vaultAuthUpgradeMaxInterval {
		t.Errorf("after many no-change got %s, want cap %s", got, vaultAuthUpgradeMaxInterval)
	}
}

// A failed upgrade attempt must never destroy a working session — that is what
// makes this safe to run against a healthy host.
func TestMaybeUpgradeVaultAuth_FailedAttemptKeepsExistingSession(t *testing.T) {
	resetAuthUpgradeStateForTest()
	t.Cleanup(resetAuthUpgradeStateForTest)
	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	before := &VaultState{Client: newTestVaultClient(t), AuthMethod: VaultAuthMethodToken, Token: "keep-me"}
	globalVaultState = before

	// Unroutable: every method fails, and InitVault writes state only on success.
	t.Setenv("VAULT_ADDRESS", "http://127.0.0.1:1")
	t.Setenv("VAULT_ROLE_ID", "r")
	t.Setenv("VAULT_SECRET_ID", "s")
	t.Setenv("VAULT_TOKEN", "")
	maybeUpgradeVaultAuth(&VaultConfig{Address: "http://127.0.0.1:1", RoleID: "r", SecretID: "s", Token: "t"})

	after := GetVaultClient()
	if after == nil || after.Token != "keep-me" {
		t.Errorf("a failed upgrade attempt disturbed the live session: %+v", after)
	}
	if authUpgradeNoChangeCount.Load() == 0 {
		t.Error("a failed attempt should widen the backoff")
	}
}
