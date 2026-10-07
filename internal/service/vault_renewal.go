package service

// vault_renewal.go: the in-process Vault Agent — lease-aware token renewal,
// re-auth fallback, and the preferred-credential "auth upgrade".
// Why: DOCS/CLAUDE.md § In-process Vault Agent

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hashicorp/vault-client-go/schema"
)

// vaultRenewalTimeout caps each renew-self RPC; the re-auth fallback uses
// InitVault's own 30s timeout.
const vaultRenewalTimeout = 30 * time.Second

// Renew at 2/3 of the lease, leaving 1/3 of the TTL for the renewal RPC and a
// re-auth fallback to complete before the token expires.
const (
	vaultRenewalLeaseFractionNum = 2
	vaultRenewalLeaseFractionDen = 3
)

// vaultRenewalFloor is the minimum delay between lease-driven renewals (a
// busy-loop guard against a tiny lease), the base of backoffDelay, and the
// collapsed-lease threshold of leaseTooShortToRenew. Atomic because tests
// lower it while the renewal goroutine reads it (-race).
var vaultRenewalFloor atomic.Int64 // nanoseconds; initialised in init()

func init() { vaultRenewalFloor.Store(int64(DefaultVaultRenewalFloor)) }

// SetRenewalFloor installs the renewal floor (non-positive → default). Called
// at startup and on every config hot-reload; the floor is read afresh per
// scheduling decision, so unlike renewal_interval it needs no restart.
func SetRenewalFloor(d time.Duration) {
	if d <= 0 {
		d = DefaultVaultRenewalFloor
	}
	vaultRenewalFloor.Store(int64(d))
}

// ResolveRenewalFloor returns the renewal floor from envValue
// (VAULT_RENEWAL_FLOOR), else configValue (vault.renewal_floor), else
// DefaultVaultRenewalFloor. Invalid, zero or negative → default: unlike the
// interval, 0 never disables (a zero floor would let the loop busy-loop).
func ResolveRenewalFloor(envValue, configValue string) time.Duration {
	s := strings.TrimSpace(envValue)
	source := "VAULT_RENEWAL_FLOOR env"
	if s == "" {
		s = strings.TrimSpace(configValue)
		source = "config.yml vault.renewal_floor"
	}
	if s == "" {
		return DefaultVaultRenewalFloor
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		Warningf("vault: invalid renewal floor %q (source: %s): %v — using default %s",
			s, source, err, DefaultVaultRenewalFloor)
		return DefaultVaultRenewalFloor
	}
	if d <= 0 {
		Warningf("vault: renewal floor %s (source: %s) is not positive — using default %s",
			d, source, DefaultVaultRenewalFloor)
		return DefaultVaultRenewalFloor
	}
	Infof("vault: renewal floor set to %s (source: %s)", d, source)
	return d
}

// leaseTooShortToRenew reports whether a renewable lease has collapsed (the
// token is at max_ttl): 2/3 of it, as in nextRenewalDelay, is below the floor.
// Tokens with no lease signal (non-renewable, static VAULT_TOKEN) never collapse.
// Why: DOCS/CLAUDE.md § In-process Vault Agent
func leaseTooShortToRenew(state *VaultState) bool {
	if state == nil || !state.Renewable || state.LeaseDuration <= 0 {
		return false
	}
	lease := time.Duration(state.LeaseDuration) * time.Second
	leaseDelay := lease * vaultRenewalLeaseFractionNum / vaultRenewalLeaseFractionDen
	return leaseDelay < time.Duration(vaultRenewalFloor.Load())
}

// ResolveRenewalInterval returns the renewal ceiling from envValue
// (VAULT_RENEWAL_INTERVAL), else configValue (vault.renewal_interval), else
// DefaultVaultRenewalInterval. Values are trimmed Go durations; invalid or
// negative → Warning + default; 0 disables periodic renewal.
func ResolveRenewalInterval(envValue, configValue string) time.Duration {
	s := strings.TrimSpace(envValue)
	source := "VAULT_RENEWAL_INTERVAL env"
	if s == "" {
		s = strings.TrimSpace(configValue)
		source = "config.yml vault.renewal_interval"
	}
	if s == "" {
		Infof("vault: renewal interval defaults to %s (no override set)",
			DefaultVaultRenewalInterval)
		return DefaultVaultRenewalInterval
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		Warningf("vault: invalid renewal interval %q (source: %s): %v — using default %s",
			s, source, err, DefaultVaultRenewalInterval)
		return DefaultVaultRenewalInterval
	}
	if d < 0 {
		Warningf("vault: negative renewal interval %s (source: %s) — using default %s",
			d, source, DefaultVaultRenewalInterval)
		return DefaultVaultRenewalInterval
	}
	if d == 0 {
		Infof("vault: renewal interval set to 0 (source: %s) — periodic renewal disabled", source)
		return 0
	}
	Infof("vault: renewal interval set to %s (source: %s)", d, source)
	return d
}

// ─── preferred-credential re-attempt ("auth upgrade") ──────────────────────

// The upgrade cadence is a fixed backstop, deliberately not an env/config knob.
const (
	// vaultAuthUpgradeInterval is the base delay between re-attempts.
	vaultAuthUpgradeInterval = time.Hour

	// vaultAuthUpgradeMaxInterval caps the backoff below.
	vaultAuthUpgradeMaxInterval = 24 * time.Hour
)

var (
	// vaultAuthUpgradeNow is a test seam for the clock, matching unitHealthNow.
	vaultAuthUpgradeNow = time.Now

	// Atomics: the renewal goroutine writes them while tests read/reset them (-race).
	lastAuthUpgradeUnixNano  atomic.Int64
	authUpgradeNoChangeCount atomic.Int64
)

// resetAuthUpgradeStateForTest clears the upgrade scheduler's memory so one
// test's attempt does not suppress another's.
func resetAuthUpgradeStateForTest() {
	lastAuthUpgradeUnixNano.Store(0)
	authUpgradeNoChangeCount.Store(0)
}

// authUpgradeDelay returns the delay between upgrade attempts: the base
// interval doubled per consecutive attempt that did not change the auth
// method, capped at vaultAuthUpgradeMaxInterval. Why the backoff: see ADR-0009.
func authUpgradeDelay() time.Duration {
	d := vaultAuthUpgradeInterval
	for i := int64(0); i < authUpgradeNoChangeCount.Load(); i++ {
		if d >= vaultAuthUpgradeMaxInterval {
			return vaultAuthUpgradeMaxInterval
		}
		d *= 2
	}
	if d > vaultAuthUpgradeMaxInterval {
		return vaultAuthUpgradeMaxInterval
	}
	return d
}

// preferredAuthMethod returns the method of the highest-priority configured
// credential (false if none). It reads authAttempts, the list InitVault walks,
// so the two cannot disagree on "preferred".
func preferredAuthMethod(cfg *VaultConfig) (VaultAuthMethod, bool) {
	attempts := authAttempts(cfg)
	if len(attempts) == 0 {
		return VaultAuthMethodNone, false
	}
	return attempts[0].method, true
}

// maybeUpgradeVaultAuth re-attempts the preferred credential, at most every
// authUpgradeDelay, when the live session runs on a weaker one. Call it only
// after a HEALTHY cycle. A no-op (no Vault traffic) when already preferred;
// non-destructive, since InitVault replaces the state only on success. See ADR-0009.
func maybeUpgradeVaultAuth(cfg *VaultConfig) {
	state := GetVaultClient()
	if state == nil || state.Client == nil {
		return // nothing live to upgrade; the failure path owns this case
	}
	preferred, ok := preferredAuthMethod(cfg)
	if !ok || state.AuthMethod == preferred {
		return // already on the best credential configured (the normal case)
	}

	now := vaultAuthUpgradeNow()
	if last := lastAuthUpgradeUnixNano.Load(); last != 0 {
		if now.Sub(time.Unix(0, last)) < authUpgradeDelay() {
			return
		}
	}
	lastAuthUpgradeUnixNano.Store(now.UnixNano())

	Infof("vault: session is authenticated via %s but %s is configured and preferred — re-attempting the preferred credential",
		state.AuthMethod, preferred)

	// Re-read the env, not the startup snapshot, so a credential rotated into .env is used.
	if err := InitVault(LoadVaultConfig()); err != nil {
		authUpgradeNoChangeCount.Add(1)
		Warningf("vault: auth upgrade attempt failed: %v — continuing on %s (next attempt in %s)",
			err, state.AuthMethod, authUpgradeDelay())
		return
	}

	newState := GetVaultClient()
	if newState == nil || newState.AuthMethod == state.AuthMethod {
		authUpgradeNoChangeCount.Add(1)
		Debugf("vault: auth upgrade attempt did not change the method (still %s) — next attempt in %s; a consumed single-use wrapping token stays unusable until rotated",
			state.AuthMethod, authUpgradeDelay())
		return
	}

	authUpgradeNoChangeCount.Store(0)
	Infof("vault: auth upgraded %s → %s (lease_duration=%ds renewable=%t)",
		state.AuthMethod, newState.AuthMethod, newState.LeaseDuration, newState.Renewable)
}

// RenewVaultToken runs one fail-soft renewal cycle: renew-self on the live
// token, else InitVault from the re-read env. It returns false when the cycle
// ends without a live token (both failed, or InitVault only hit a no-credentials
// skip), which makes the loop back off; true otherwise, incl. the no-config skips.
// Why: DOCS/CLAUDE.md § In-process Vault Agent
func RenewVaultToken(cfg *VaultConfig) bool {
	if cfg == nil {
		Debugf("vault: renewal cycle skipped — no Vault config")
		return true
	}
	if cfg.Address == "" {
		Debugf("vault: renewal cycle skipped — VAULT_ADDRESS not set")
		return true
	}

	state := GetVaultClient()
	if state == nil || state.Client == nil {
		// No live state (e.g. Vault was down at startup): authenticate from scratch.
		Infof("vault: no current Vault state — attempting initial authentication")
		// Re-read the env, not the startup snapshot: StartEnvReload may have
		// rotated VAULT_SECRET_ID/VAULT_ROLE_ID into it.
		if err := InitVault(LoadVaultConfig()); err != nil {
			Warningf("vault: initial authentication failed: %v — will retry with backoff", err)
			recordVaultRenewFailure()
			return false
		}
		if GetVaultClient() == nil {
			// A nil error also covers InitVault's no-credentials skip, which sets
			// no state: count a failure, never a reauth (a false health signal).
			Warningf("vault: initial authentication did not produce a live token — will retry with backoff")
			recordVaultRenewFailure()
			return false
		}
		recordVaultRenewReauth()
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), vaultRenewalTimeout)
	defer cancel()

	Debugf("vault: attempting token-renew-self (auth_method=%s)", state.AuthMethod)
	resp, err := state.Client.Auth.TokenRenewSelf(ctx, schema.TokenRenewSelfRequest{})
	if err == nil && resp != nil && resp.Auth != nil {
		newState := *state
		newState.LeaseDuration = resp.Auth.LeaseDuration
		newState.Renewable = resp.Auth.Renewable
		if resp.Auth.ClientToken != "" {
			// Defensive: some Vault paths rotate the token on renewal.
			newState.Token = resp.Auth.ClientToken
			_ = state.Client.SetToken(resp.Auth.ClientToken)
		}
		setGlobalVaultState(&newState)
		recordVaultRenewSuccess()
		Infof("vault: token renewed (lease_duration=%ds renewable=%t)",
			resp.Auth.LeaseDuration, resp.Auth.Renewable)

		// Lease collapsed (token at max_ttl): re-authenticate NOW, while the token
		// is still valid, instead of scheduling one more renewal that ends in a 403.
		if leaseTooShortToRenew(&newState) {
			Infof("vault: renewed lease (%ds) is too short to renew again — token is at its max_ttl; re-authenticating now",
				newState.LeaseDuration)
			if err := InitVault(LoadVaultConfig()); err != nil { // re-read env, as above
				// The short-lived token is still live: not a failed cycle, no backoff.
				Warningf("vault: pre-emptive re-authentication failed: %v — continuing on the short-lived token", err)
				return true
			}
			if GetVaultClient() == nil {
				Warningf("vault: pre-emptive re-authentication produced no live token — continuing on the short-lived token")
				return true
			}
			recordVaultRenewReauth()
		}
		return true
	}

	if err != nil {
		Warningf("vault: token renewal failed (%v) — re-authenticating", err)
	} else {
		Warningf("vault: token renewal returned no auth payload — re-authenticating")
	}

	// Re-read the env: the startup snapshot would keep old, possibly revoked credentials.
	if err := InitVault(LoadVaultConfig()); err != nil {
		Warningf("vault: re-authentication failed: %v — keeping previous (now stale) state; will retry with backoff", err)
		recordVaultRenewFailure()
		return false
	}
	if newState := GetVaultClient(); newState == nil || newState == state {
		// A no-credentials skip also returns nil and leaves the stale state in
		// place, so identity with the entry state (not the nil error) tells a
		// genuine re-auth from a no-op.
		Warningf("vault: re-authentication did not produce a live token — keeping previous (now stale) state; will retry with backoff")
		recordVaultRenewFailure()
		return false
	}
	recordVaultRenewReauth()
	return true
}

// nextRenewalDelay returns the wait before the next renewal: min(ceiling, 2/3
// of the live lease), the lease part floored at vaultRenewalFloor. A token with
// no lease signal (non-renewable, zero lease) gets the ceiling, which is always
// positive here (StartVaultRenewal disables the loop otherwise).
func nextRenewalDelay(ceiling time.Duration, state *VaultState) time.Duration {
	if state == nil || !state.Renewable || state.LeaseDuration <= 0 {
		return ceiling
	}
	lease := time.Duration(state.LeaseDuration) * time.Second
	leaseDelay := lease * vaultRenewalLeaseFractionNum / vaultRenewalLeaseFractionDen
	if floor := time.Duration(vaultRenewalFloor.Load()); leaseDelay < floor {
		leaseDelay = floor
	}
	if leaseDelay < ceiling {
		return leaseDelay
	}
	return ceiling
}

// backoffDelay returns the wait after `failures` (>= 1) consecutive failed
// cycles: vaultRenewalFloor doubled per extra failure (10s → 20s → 40s …),
// capped at the ceiling. Never classify errors (no 4xx fail-fast): a 403 can
// succeed once StartEnvReload rotates a fresh SecretID into the env.
func backoffDelay(failures int, ceiling time.Duration) time.Duration {
	base := time.Duration(vaultRenewalFloor.Load())
	if base <= 0 {
		base = 10 * time.Second
	}
	if ceiling <= 0 {
		return base
	}
	delay := base
	for i := 1; i < failures; i++ {
		if delay >= ceiling {
			return ceiling
		}
		delay *= 2
		if delay <= 0 { // overflow guard for a pathological failure count
			return ceiling
		}
	}
	if delay > ceiling {
		return ceiling
	}
	return delay
}

// StartVaultRenewal starts a goroutine running RenewVaultToken every
// nextRenewalDelay (interval is the ceiling), or every backoffDelay after failed
// cycles and when startup auth failed. It exits on ctx.Done; a nil cfg, empty
// VAULT_ADDRESS or non-positive interval makes it a no-op.
func StartVaultRenewal(ctx context.Context, cfg *VaultConfig, interval time.Duration) {
	if cfg == nil {
		Infof("vault: renewal disabled — no Vault config")
		return
	}
	if cfg.Address == "" {
		Infof("vault: renewal disabled — VAULT_ADDRESS not set")
		return
	}
	if interval <= 0 {
		Infof("vault: periodic renewal disabled (interval=%s)", interval)
		return
	}

	Infof("vault: token renewal loop started (interval ceiling=%s, lease-aware)", interval)
	go func() {
		initialState := GetVaultClient()
		initialDelay := nextRenewalDelay(interval, initialState)
		if initialState == nil {
			// Degraded since startup: no lease to schedule from, so retry at the
			// backoff floor rather than the full ceiling.
			initialDelay = backoffDelay(1, interval)
			Infof("vault: no Vault token from startup — first renewal attempt in %s (ceiling=%s)",
				initialDelay, interval)
		}
		timer := time.NewTimer(initialDelay)
		defer timer.Stop()
		failures := 0
		for {
			select {
			case <-ctx.Done():
				Infof("vault: token renewal loop stopping (context cancelled)")
				return
			case <-timer.C:
				var delay time.Duration
				// safeCycleBool: a panic counts as a failed cycle (backoff below)
				// and never kills the daemon.
				if safeCycleBool("vault-renewal", func() bool {
					ok := RenewVaultToken(cfg)
					if ok {
						// Healthy cycles only (the failure path already re-runs the
						// chain); inside safeCycleBool so its panics are caught too.
						maybeUpgradeVaultAuth(cfg)
					}
					return ok
				}) {
					failures = 0
					delay = nextRenewalDelay(interval, GetVaultClient())
				} else {
					// Keyed on the cycle OUTCOME, not nil state: a failed re-auth
					// keeps stale non-nil state.
					failures++
					delay = backoffDelay(failures, interval)
					Warningf("vault: renewal cycle failed (%d consecutive) — retrying in %s", failures, delay)
				}
				Debugf("vault: next token renewal scheduled in %s", delay)
				timer.Reset(delay)
			}
		}
	}()
}
