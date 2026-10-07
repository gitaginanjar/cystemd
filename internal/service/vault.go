package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	vault "github.com/hashicorp/vault-client-go"
	"github.com/hashicorp/vault-client-go/schema"
)

// ─── VaultAuthMethod ───────────────────────────────────────────────────────

// VaultAuthMethod identifies which authentication method was used for the
// current Vault session.
type VaultAuthMethod string

const (
	VaultAuthMethodNone  VaultAuthMethod = "none"
	VaultAuthMethodToken VaultAuthMethod = "token"

	// VaultAuthMethodAppRole is the generic AppRole label. Production code no
	// longer records it: a session records one of the two shapes below.
	VaultAuthMethodAppRole VaultAuthMethod = "approle"

	// The AppRole shapes are reported distinctly so / shows whether the wrapping
	// token worked or the host fell back to the raw SecretID. They equal the
	// authAttempts labels, so a value read from / greps verbatim in the journal.
	VaultAuthMethodAppRoleWrapped  VaultAuthMethod = "approle (wrapping token)"
	VaultAuthMethodAppRoleSecretID VaultAuthMethod = "approle (secret_id)"
)

// appRoleMethod maps a credential shape to its reported auth method.
func appRoleMethod(wrapped bool) VaultAuthMethod {
	if wrapped {
		return VaultAuthMethodAppRoleWrapped
	}
	return VaultAuthMethodAppRoleSecretID
}

// ─── VaultConfig ───────────────────────────────────────────────────────────

// VaultConfig holds the Vault settings LoadVaultConfig reads from the
// environment; an empty Address skips Vault (and secret fetching) entirely.
// Auth is a fallback chain in priority order: VAULT_WRAPPING_TOKEN, then
// VAULT_SECRET_ID (both AppRole, both need VAULT_ROLE_ID), then VAULT_TOKEN.
// Path/EnvFile drive FetchAndWriteSecrets. See ADR-0009.
type VaultConfig struct {
	Address  string // VAULT_ADDRESS
	Token    string // VAULT_TOKEN   — direct token authentication
	RoleID   string // VAULT_ROLE_ID  — AppRole authentication
	SecretID string // VAULT_SECRET_ID — the RAW AppRole SecretID
	// WrappingToken is the single-use response-wrapping token (-wrap-ttl),
	// tried before SecretID; each variable holds exactly one kind of value.
	WrappingToken string // VAULT_WRAPPING_TOKEN
	// SecretIDWrapped is derived, true exactly when WrappingToken is set; there
	// is no VAULT_SECRET_ID_WRAPPED flag.
	SecretIDWrapped bool
	SkipVerify      bool   // VAULT_SKIP_VERIFY
	Path            string // VAULT_PATH      — secret path (default kubernetes/devtools/cystemd/cystemd)
	EnvFile         string // VAULT_ENV_FILE  — destination .env file (default .env)
	// Templates are parsed from VAULT_TEMPLATES and rendered by RenderTemplates.
	Templates []TemplateSpec
}

// appRoleSecret returns the AppRole credential tried FIRST and its env var
// name: VAULT_WRAPPING_TOKEN when set, else VAULT_SECRET_ID. Intent only:
// InitVault falls back to the raw SecretID. Only tests call it today.
func (c *VaultConfig) appRoleSecret() (value, source string) {
	if c.WrappingToken != "" {
		return c.WrappingToken, "VAULT_WRAPPING_TOKEN"
	}
	return c.SecretID, "VAULT_SECRET_ID"
}

// DefaultVaultPath is the secret path used when VAULT_PATH is unset.
const DefaultVaultPath = "kubernetes/devtools/cystemd/cystemd"

// DefaultVaultEnvFile is the destination used when VAULT_ENV_FILE is unset,
// relative to the process working directory.
const DefaultVaultEnvFile = ".env"

// DefaultVaultRefreshInterval is the refresh cadence when neither
// VAULT_REFRESH_INTERVAL nor vault.refresh_interval is set. A resolved 0
// disables periodic refresh (the .env is then written only at startup).
const DefaultVaultRefreshInterval = time.Minute

// DefaultVaultRenewalInterval is the CEILING of the lease-aware renewal
// schedule: renewal normally fires at 2/3 of the lease (nextRenewalDelay), and
// at this cadence for a token without a usable TTL or when 2/3 exceeds it.
// A resolved 0 disables renewal (the token then expires on Vault's schedule).
const DefaultVaultRenewalInterval = 30 * time.Minute

// DefaultVaultRenewalFloor is the default minimum delay between lease-driven
// renewals (see ResolveRenewalFloor); it is also the collapsed-lease threshold
// of leaseTooShortToRenew.
const DefaultVaultRenewalFloor = 10 * time.Second

// LoadVaultConfig reads VaultConfig from the environment and logs the resolved
// values at Debug (credentials only as presence). VAULT_SKIP_VERIFY defaults to
// true; an invalid value warns and keeps true.
func LoadVaultConfig() *VaultConfig {
	address := strings.TrimSpace(os.Getenv("VAULT_ADDRESS"))
	token := strings.TrimSpace(os.Getenv("VAULT_TOKEN"))
	roleID := strings.TrimSpace(os.Getenv("VAULT_ROLE_ID"))
	secretID := strings.TrimSpace(os.Getenv("VAULT_SECRET_ID"))
	wrappingToken := strings.TrimSpace(os.Getenv("VAULT_WRAPPING_TOKEN"))

	skipVerify := true
	if raw := strings.TrimSpace(os.Getenv("VAULT_SKIP_VERIFY")); raw != "" {
		parsed, err := parseBoolEnv(raw)
		if err != nil {
			Warningf("vault: VAULT_SKIP_VERIFY=%q is not a valid boolean — defaulting to true", raw)
		} else {
			skipVerify = parsed
		}
	}

	// Wrapped mode is the PRESENCE of VAULT_WRAPPING_TOKEN; no flag exists that
	// could contradict it.
	secretIDWrapped := wrappingToken != ""

	// Log label for the highest-priority configured method: intent, not outcome
	// (InitVault logs what actually authenticated).
	authMethod := "none"
	switch {
	case roleID != "" && wrappingToken != "":
		authMethod = "approle (wrapped)"
	case roleID != "" && secretID != "":
		authMethod = "approle"
	case token != "":
		authMethod = "token"
	case roleID != "" || secretID != "" || wrappingToken != "":
		authMethod = "approle (incomplete)"
	}

	// Unusable: AppRole login needs the role_id, and the unwrap yields only the
	// secret_id.
	if wrappingToken != "" && roleID == "" {
		Warningf("vault: VAULT_WRAPPING_TOKEN is set but VAULT_ROLE_ID is empty — the wrapping token cannot be used (AppRole login needs both)")
	}

	path := os.Getenv("VAULT_PATH")
	if path == "" {
		path = DefaultVaultPath
	}
	envFile := os.Getenv("VAULT_ENV_FILE")
	if envFile == "" {
		envFile = DefaultVaultEnvFile
	}

	templates := parseTemplateSpecs(os.Getenv("VAULT_TEMPLATES"))

	Debugf("vault: config loaded: address=%q auth_method=%s skip_verify=%t secret_id_wrapped=%t wrapping_token_set=%t path=%q env_file=%q templates=%d",
		address, authMethod, skipVerify, secretIDWrapped, wrappingToken != "", path, envFile, len(templates))

	return &VaultConfig{
		Address:         address,
		Token:           token,
		RoleID:          roleID,
		SecretID:        secretID,
		WrappingToken:   wrappingToken,
		SecretIDWrapped: secretIDWrapped,
		SkipVerify:      skipVerify,
		Path:            path,
		EnvFile:         envFile,
		Templates:       templates,
	}
}

// ─── VaultState ────────────────────────────────────────────────────────────

// VaultState holds the live Vault client and the token metadata from
// authentication; the package singleton is read via GetVaultClient.
type VaultState struct {
	Client        *vault.Client
	Token         string
	Accessor      string
	Policies      []string
	LeaseDuration int
	Renewable     bool
	EntityID      string
	AuthMethod    VaultAuthMethod // which method was used to authenticate
	Address       string          // Vault server address
}

var (
	globalVaultState   *VaultState
	globalVaultStateMu sync.RWMutex
)

// GetVaultClient returns the live VaultState, or nil when Vault is not
// configured, not yet initialised or degraded; callers must handle nil. Guarded
// by globalVaultStateMu: the renewal goroutine writes while handlers read.
func GetVaultClient() *VaultState {
	globalVaultStateMu.RLock()
	defer globalVaultStateMu.RUnlock()
	return globalVaultState
}

// setGlobalVaultState replaces the VaultState under the write lock; every
// production write goes through here. Tests may assign globalVaultState
// directly once no renewal goroutine is running.
func setGlobalVaultState(s *VaultState) {
	globalVaultStateMu.Lock()
	defer globalVaultStateMu.Unlock()
	globalVaultState = s
}

// ─── VaultStatusJSON ───────────────────────────────────────────────────────

// VaultStatusJSON.Status values, derived from in-process state only (no probe).
const (
	// VaultStatusAuthenticated: a live, authenticated session is held.
	VaultStatusAuthenticated = "authenticated"
	// VaultStatusUnauthenticated: VAULT_ADDRESS is set but no live token is held
	// (startup auth failed, credentials missing/incomplete, not yet established).
	VaultStatusUnauthenticated = "unauthenticated"
	// VaultStatusNotConfigured: VAULT_ADDRESS is empty.
	VaultStatusNotConfigured = "not_configured"
)

// VaultStatusJSON is the vault block of the open / page: only "wired up, how,
// authenticated?" plus address and lease. Never the token, accessor, entity_id
// or policies (reconnaissance). Configured means VAULT_ADDRESS is set, not that
// auth succeeded; a degraded host shows {configured:true,
// status:"unauthenticated"}. Keep fields in JSON-tag alphabetical order.
type VaultStatusJSON struct {
	Address       string `json:"address,omitempty"`
	AuthMethod    string `json:"auth_method"`
	Configured    bool   `json:"configured"`
	LeaseDuration int    `json:"lease_duration,omitempty"`
	Renewable     bool   `json:"renewable,omitempty"`
	// SecretIDWrapped: VAULT_WRAPPING_TOKEN is set (the credential SHAPE, i.e.
	// intent; never a credential). Read with AuthMethod to spot a fallback.
	SecretIDWrapped bool   `json:"secret_id_wrapped"`
	Status          string `json:"status"`
}

// envSecretIDWrapped reports whether VAULT_WRAPPING_TOKEN is set. It reads the
// env, not VaultState, so / reflects the CURRENT configuration and VaultState
// needs no field only the usage page reads.
func envSecretIDWrapped() bool {
	return strings.TrimSpace(os.Getenv("VAULT_WRAPPING_TOKEN")) != ""
}

// BuildVaultStatus reports the in-process Vault state: a live session →
// "authenticated" with the method that actually authenticated; VAULT_ADDRESS
// set but no session (degraded, revealed on purpose) → "unauthenticated" with
// the intended method (envAuthMethod); VAULT_ADDRESS empty → "not_configured".
func BuildVaultStatus() VaultStatusJSON {
	state := GetVaultClient()
	if state != nil {
		return VaultStatusJSON{
			Address:         state.Address,
			AuthMethod:      string(state.AuthMethod),
			Configured:      true,
			LeaseDuration:   state.LeaseDuration,
			Renewable:       state.Renewable,
			SecretIDWrapped: envSecretIDWrapped(),
			Status:          VaultStatusAuthenticated,
		}
	}

	// No session and no retained VaultConfig: only the env says whether Vault
	// is wired up.
	address := strings.TrimSpace(os.Getenv("VAULT_ADDRESS"))
	if address == "" {
		return VaultStatusJSON{
			AuthMethod: string(VaultAuthMethodNone),
			Configured: false,
			Status:     VaultStatusNotConfigured,
		}
	}
	return VaultStatusJSON{
		Address:         address,
		AuthMethod:      envAuthMethod(),
		Configured:      true,
		SecretIDWrapped: envSecretIDWrapped(),
		Status:          VaultStatusUnauthenticated,
	}
}

// envAuthMethod names, for a degraded host, the method it WOULD try first, in
// authAttempts order: approle (wrapping token), approle (secret_id), token;
// "none" when no complete method is configured.
func envAuthMethod() string {
	roleID := strings.TrimSpace(os.Getenv("VAULT_ROLE_ID"))
	secretID := strings.TrimSpace(os.Getenv("VAULT_SECRET_ID"))
	wrapping := strings.TrimSpace(os.Getenv("VAULT_WRAPPING_TOKEN"))

	if roleID != "" && wrapping != "" {
		return string(VaultAuthMethodAppRoleWrapped)
	}
	if roleID != "" && secretID != "" {
		return string(VaultAuthMethodAppRoleSecretID)
	}
	if strings.TrimSpace(os.Getenv("VAULT_TOKEN")) != "" {
		return string(VaultAuthMethodToken)
	}
	return string(VaultAuthMethodNone)
}

// ─── InitVault ─────────────────────────────────────────────────────────────

// InitVault walks the authAttempts chain (each method falls through on failure)
// and stores the first success via setGlobalVaultState; a failed call never
// touches a live session. Returns nil on success AND on its skip paths (no
// VAULT_ADDRESS, no usable credentials), so check GetVaultClient; an error when
// the client cannot be built or every method fails. Severity: a failed method
// is Warning, only those two cases are Error; credentials are logged only via
// vaultSafePrefix.
func InitVault(cfg *VaultConfig) error {
	if cfg.Address == "" {
		Infof("vault: VAULT_ADDRESS not set — skipping Vault initialisation")
		return nil
	}

	attempts := authAttempts(cfg)
	if len(attempts) == 0 {
		Warningf("vault: VAULT_ADDRESS=%q is configured but no usable credentials found "+
			"(set VAULT_ROLE_ID plus VAULT_WRAPPING_TOKEN or VAULT_SECRET_ID for AppRole, "+
			"or VAULT_TOKEN for token auth) — skipping Vault initialisation", cfg.Address)
		return nil
	}

	labels := make([]string, 0, len(attempts))
	for _, a := range attempts {
		labels = append(labels, a.label)
	}
	Infof("vault: initialising — address=%s auth_order=[%s] skip_verify=%t",
		cfg.Address, strings.Join(labels, " → "), cfg.SkipVerify)

	opts := []vault.ClientOption{
		vault.WithAddress(cfg.Address),
		vault.WithRequestTimeout(30 * time.Second),
	}
	Debugf("vault: request timeout set to 30s")

	if cfg.SkipVerify {
		Warningf("vault: TLS certificate verification is DISABLED (VAULT_SKIP_VERIFY=true) — do not use this in production")
		opts = append(opts, vault.WithTLS(vault.TLSConfiguration{
			InsecureSkipVerify: true,
		}))
		Debugf("vault: InsecureSkipVerify=true appended to TLS configuration")
	} else {
		Debugf("vault: TLS certificate verification is ENABLED")
	}

	Debugf("vault: creating vault-client-go client for address=%s", cfg.Address)
	client, err := vault.New(opts...)
	if err != nil {
		Errorf("vault: failed to create client: %v", err)
		return fmt.Errorf("vault: failed to create client: %w", err)
	}
	Debugf("vault: client object created successfully")

	// Each fallback is a Warning and a success below first place names what
	// failed: a downgrade to a weaker credential must never be silent.
	var failures []string
	for i, a := range attempts {
		Debugf("vault: attempt %d/%d: %s", i+1, len(attempts), a.label)
		err := a.run(client, cfg)
		if err == nil {
			if i > 0 {
				Warningf("vault: authenticated via %s — %d higher-priority method(s) failed first: %s",
					a.label, i, strings.Join(failures, "; "))
			}
			return nil
		}
		Warningf("vault: %s authentication failed: %v", a.label, err)
		failures = append(failures, fmt.Sprintf("%s: %v", a.label, err))
	}

	Errorf("vault: all %d configured authentication method(s) failed", len(attempts))
	return fmt.Errorf("vault: all authentication methods failed: %s", strings.Join(failures, "; "))
}

// authAttempt is one candidate authentication method. method is what a
// successful run records on VaultState; it is carried explicitly, not derived
// from label, because maybeUpgradeVaultAuth compares it with the live session's
// method. Keep the two equal (TestAuthAttempts_MethodMatchesLabel).
type authAttempt struct {
	label  string
	method VaultAuthMethod
	run    func(*vault.Client, *VaultConfig) error
}

// authAttempts returns every method this host has credentials for, in priority
// order: AppRole via VAULT_WRAPPING_TOKEN (single-use), AppRole via
// VAULT_SECRET_ID (raw, multi-use), VAULT_TOKEN (long-lived, never re-minted).
// Both AppRole shapes need VAULT_ROLE_ID. See ADR-0009.
func authAttempts(cfg *VaultConfig) []authAttempt {
	var out []authAttempt
	if cfg.RoleID != "" && cfg.WrappingToken != "" {
		out = append(out, authAttempt{
			label:  "approle (wrapping token)",
			method: VaultAuthMethodAppRoleWrapped,
			run:    func(c *vault.Client, cf *VaultConfig) error { return initVaultWithAppRole(c, cf, true) },
		})
	}
	if cfg.RoleID != "" && cfg.SecretID != "" {
		out = append(out, authAttempt{
			label:  "approle (secret_id)",
			method: VaultAuthMethodAppRoleSecretID,
			run:    func(c *vault.Client, cf *VaultConfig) error { return initVaultWithAppRole(c, cf, false) },
		})
	}
	if cfg.Token != "" {
		out = append(out, authAttempt{
			label:  "token",
			method: VaultAuthMethodToken,
			run:    initVaultWithToken,
		})
	}
	return out
}

// ─── initVaultWithToken ────────────────────────────────────────────────────

// initVaultWithToken authenticates with VAULT_TOKEN. SetToken is local (no
// network call). The token is never re-minted; only renew-self extends it.
func initVaultWithToken(client *vault.Client, cfg *VaultConfig) error {
	Infof("vault: starting token authentication: address=%s token_prefix=%s...",
		cfg.Address, vaultSafePrefix(cfg.Token))
	Debugf("vault: setting client token directly (token auth — no AppRole exchange required)")

	if err := client.SetToken(cfg.Token); err != nil {
		Debugf("vault: failed to set client token: %v", err)
		return fmt.Errorf("vault: failed to set client token: %w", err)
	}
	Debugf("vault: client token applied successfully")
	Tracef("vault: client token prefix: %s...", vaultSafePrefix(cfg.Token))

	setGlobalVaultState(&VaultState{
		Client:     client,
		Token:      cfg.Token,
		AuthMethod: VaultAuthMethodToken,
		Address:    cfg.Address,
	})

	Infof("vault: initialisation complete — client is ready (auth_method=token)")
	return nil
}

// ─── response-wrapped SecretID ─────────────────────────────────────────────

// The unwrapped SecretID, cached per wrapping-token value: the token is
// single-use and InitVault re-runs on every re-auth, so an unchanged token must
// reuse the cache; a rotated one (via StartEnvReload) re-unwraps exactly once.
var (
	wrappedSecretIDMu     sync.Mutex
	lastWrappingToken     string
	cachedUnwrappedSecret string
)

// unwrapSecretID exchanges a single-use wrapping token for the SecretID it wraps
// (PUT /v1/sys/wrapping/unwrap; sent as a per-request token, so the client's own
// token is untouched). An error fails only this chain attempt. Never log the
// wrapping token verbatim.
func unwrapSecretID(client *vault.Client, wrappingToken string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := vault.Unwrap[map[string]interface{}](ctx, client, wrappingToken)
	if err != nil {
		return "", fmt.Errorf("unwrap request failed: %w", err)
	}
	if resp == nil || resp.Data == nil {
		return "", fmt.Errorf("unwrap returned an empty response")
	}
	raw, ok := resp.Data["secret_id"]
	if !ok {
		return "", fmt.Errorf("unwrap response has no secret_id field (was the SecretID generated with -wrap-ttl?)")
	}
	secretID, ok := raw.(string)
	if !ok || secretID == "" {
		return "", fmt.Errorf("unwrap response secret_id is empty or not a string")
	}
	return secretID, nil
}

// resolveSecretID returns the SecretID for an AppRole login: cfg.SecretID
// verbatim when !wrapped, otherwise the SecretID unwrapped from
// cfg.WrappingToken, cached per token value (the same token reuses the cache,
// which keeps re-auth working; a changed token re-unwraps once).
func resolveSecretID(client *vault.Client, cfg *VaultConfig, wrapped bool) (string, error) {
	if !wrapped {
		return cfg.SecretID, nil
	}

	// Never cfg.SecretID here: that is the next chain attempt's credential.
	wrappingToken, source := cfg.WrappingToken, "VAULT_WRAPPING_TOKEN"

	wrappedSecretIDMu.Lock()
	defer wrappedSecretIDMu.Unlock()

	if wrappingToken == lastWrappingToken && cachedUnwrappedSecret != "" {
		Debugf("vault: reusing previously-unwrapped secret_id (wrapping token unchanged — avoids re-unwrapping a single-use token)")
		return cachedUnwrappedSecret, nil
	}

	Infof("vault: %s is response-wrapped — unwrapping (wrapping_token_prefix=%s...)", source, vaultSafePrefix(wrappingToken))
	secretID, err := unwrapSecretID(client, wrappingToken)
	if err != nil {
		return "", err
	}
	lastWrappingToken = wrappingToken
	cachedUnwrappedSecret = secretID
	Debugf("vault: secret_id successfully unwrapped and cached")
	return secretID, nil
}

// ─── initVaultWithAppRole ──────────────────────────────────────────────────

// initVaultWithAppRole performs one AppRole login (wrapped: the credential is
// VAULT_WRAPPING_TOKEN, else VAULT_SECRET_ID) and on success stores the token
// and metadata via setGlobalVaultState.
func initVaultWithAppRole(client *vault.Client, cfg *VaultConfig, wrapped bool) error {
	credSource := "VAULT_SECRET_ID"
	if wrapped {
		credSource = "VAULT_WRAPPING_TOKEN"
	}
	Infof("vault: starting AppRole authentication: address=%s role_id_prefix=%s... credential=%s",
		cfg.Address, vaultSafePrefix(cfg.RoleID), credSource)

	// Failures below log at Debug, never Error: this is one attempt of
	// InitVault's chain, which reports it at Warning. Pinned by
	// TestInitVault_PerAttemptFailuresAreNotLoggedAsErrors.
	secretID, err := resolveSecretID(client, cfg, wrapped)
	if err != nil {
		Debugf("vault: failed to resolve secret_id: %v", err)
		return fmt.Errorf("vault: resolve secret_id failed: %w", err)
	}

	Debugf("vault: AppRole login → POST %s/v1/auth/approle/login role_id_prefix=%s... secret_id_prefix=%s...",
		cfg.Address, vaultSafePrefix(cfg.RoleID), vaultSafePrefix(secretID))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	Debugf("vault: dispatching AppRoleLogin request (context timeout=30s)")
	resp, err := client.Auth.AppRoleLogin(ctx, schema.AppRoleLoginRequest{
		RoleId:   cfg.RoleID,
		SecretId: secretID,
	})
	if err != nil {
		Debugf("vault: AppRole authentication failed: %v", err)
		return fmt.Errorf("vault: AppRole login failed: %w", err)
	}
	Debugf("vault: AppRoleLogin HTTP call returned without error")

	if resp == nil {
		Debugf("vault: AppRole authentication returned a nil response object")
		return fmt.Errorf("vault: AppRole login returned nil response")
	}
	Debugf("vault: response object is non-nil, inspecting Auth field")

	if resp.Auth == nil {
		Debugf("vault: AppRole authentication response has nil Auth field — check role_id and secret_id")
		return fmt.Errorf("vault: AppRole login response has nil Auth — credentials may be invalid")
	}
	Debugf("vault: Auth field is present on login response")

	token := resp.Auth.ClientToken
	accessor := resp.Auth.Accessor
	policies := resp.Auth.Policies
	leaseDuration := resp.Auth.LeaseDuration
	renewable := resp.Auth.Renewable
	entityID := resp.Auth.EntityID

	if token == "" {
		Debugf("vault: AppRole authentication returned an empty client token — authentication may have partially failed")
		return fmt.Errorf("vault: AppRole login returned empty client token")
	}

	Infof("vault: AppRole authentication successful — lease_duration=%ds renewable=%t policies=[%s]",
		leaseDuration, renewable, strings.Join(policies, ", "))
	Debugf("vault: token metadata: accessor=%s entity_id=%s lease_duration=%d renewable=%t",
		accessor, entityID, leaseDuration, renewable)
	Debugf("vault: token policies: %v", policies)
	Tracef("vault: client token acquired (first 8 chars: %s...)", vaultSafePrefix(token))

	Debugf("vault: setting client token on vault client")
	if err := client.SetToken(token); err != nil {
		Debugf("vault: failed to set client token: %v", err)
		return fmt.Errorf("vault: failed to set client token: %w", err)
	}
	Debugf("vault: client token applied successfully")

	setGlobalVaultState(&VaultState{
		Client:        client,
		Token:         token,
		Accessor:      accessor,
		Policies:      policies,
		LeaseDuration: leaseDuration,
		Renewable:     renewable,
		EntityID:      entityID,
		AuthMethod:    appRoleMethod(wrapped),
		Address:       cfg.Address,
	})

	Infof("vault: initialisation complete — client is ready (accessor=%s)", accessor)
	return nil
}

// ─── helpers ───────────────────────────────────────────────────────────────

// parseBoolEnv parses a boolean env value, trimmed and case-insensitive:
// true/yes/1 or false/no/0; anything else is an error.
func parseBoolEnv(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "true", "yes", "1":
		return true, nil
	case "false", "no", "0":
		return false, nil
	default:
		return false, fmt.Errorf("unrecognised boolean value %q (accepted: true/false, yes/no, 1/0)", raw)
	}
}

// vaultSafePrefix returns the first 8 characters of s: the only form in which a
// credential may be logged. Shorter strings (not secrets in practice) are
// returned whole, "" as "<empty>".
func vaultSafePrefix(s string) string {
	if len(s) == 0 {
		return "<empty>"
	}
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

// EnsureEnvFileSecure chmods the bootstrap dir(configPath)/.env (Vault
// credentials, SSH deploy key, JWT key) to 0600 via ensureEnvFileMode. main.go
// runs it once, unconditionally and before InitVault, so it holds even when
// degraded or when the content never changes. Missing or already-0600 → no-op;
// chmod failure → Warning.
func EnsureEnvFileSecure(configPath string) {
	ensureEnvFileMode(filepath.Join(filepath.Dir(configPath), ".env"))
}
