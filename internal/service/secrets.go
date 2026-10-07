package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	vault "github.com/hashicorp/vault-client-go"
)

// secretReadTimeout caps each Vault read attempt. Two attempts (KV v2 then
// KV v1) means the worst-case wait is twice this.
const secretReadTimeout = 30 * time.Second

// FetchAndWriteSecrets writes the secret at cfg.Path (KV v2, then v1) to
// cfg.EnvFile (default DefaultVaultEnvFile) as sorted KEY=VALUE lines,
// atomically at 0600, skipping unchanged content. Fail-soft: every failure
// logs and returns. An empty VAULT_ADDRESS (no client) disables it.
// Why: DOCS/CLAUDE.md § Vault secret fetch
func FetchAndWriteSecrets(cfg *VaultConfig) {
	if cfg == nil {
		Infof("vault: skipping secret fetch — no Vault config")
		return
	}

	state := GetVaultClient()
	if state == nil || state.Client == nil {
		Infof("vault: skipping secret fetch — Vault client not initialised (degraded mode or not configured)")
		return
	}
	if cfg.Path == "" {
		Infof("vault: skipping secret fetch — VAULT_PATH is empty")
		return
	}

	envFile := cfg.EnvFile
	if envFile == "" {
		envFile = DefaultVaultEnvFile
	}

	// Before the unchanged-content skip, so an existing file is tightened every cycle.
	ensureEnvFileMode(envFile)

	// Debug: this runs every refresh tick.
	Debugf("vault: fetching secrets from %q (target file %q)", cfg.Path, envFile)

	secrets, newContent, ok := readAndBuildSecrets(state.Client, cfg.Path,
		func(err error) {
			Warningf("vault: failed to read secrets at %q (tried KV v2 then v1) — continuing without secrets: %v",
				cfg.Path, err)
		},
		func() {
			Warningf("vault: no usable secrets at %q — nothing to write to %q", cfg.Path, envFile)
		},
	)
	if !ok {
		return
	}

	// Unchanged: no rewrite, so the mtime stays stable and the journal quiet.
	if existing, readErr := os.ReadFile(envFile); readErr == nil && bytes.Equal(existing, newContent) {
		Debugf("vault: secrets unchanged; %q not rewritten", envFile)
		return
	}

	// Atomic, never a plain os.WriteFile: StartEnvReload would Unsetenv the
	// credentials of a torn read, and a crash could empty the bootstrap .env.
	if !writeSecretFileAtomic(envFile, newContent) {
		return
	}

	Infof("vault: wrote %d secret(s) to %q", len(secrets), envFile)
}

// buildKvV2Path inserts "data/" after the first segment ("kubernetes/devtools/foo"
// → "kubernetes/data/devtools/foo"), stripping a leading slash; a bare mount
// name has no KV v2 layout and is returned unchanged.
func buildKvV2Path(p string) string {
	p = strings.TrimPrefix(p, "/")
	idx := strings.Index(p, "/")
	if idx < 0 {
		return p
	}
	return p[:idx] + "/data/" + p[idx+1:]
}

// readVault reads path once and returns the response data map; a nil
// response is an error.
func readVault(ctx context.Context, client *vault.Client, path string) (map[string]interface{}, error) {
	resp, err := client.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("nil response")
	}
	return resp.Data, nil
}

// readSecretsKvV2 reads a KV v2 path and flattens its inner "data" map; a
// response without that envelope is an error, so the caller falls back to v1.
func readSecretsKvV2(ctx context.Context, client *vault.Client, v2Path string) (map[string]string, error) {
	data, err := readVault(ctx, client, v2Path)
	if err != nil {
		return nil, err
	}
	inner, ok := data["data"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("response missing KV v2 envelope (no nested data map)")
	}
	return flattenStringMap(inner), nil
}

// readSecretsKvV1 reads a KV v1 path and flattens its data map.
func readSecretsKvV1(ctx context.Context, client *vault.Client, v1Path string) (map[string]string, error) {
	data, err := readVault(ctx, client, v1Path)
	if err != nil {
		return nil, err
	}
	return flattenStringMap(data), nil
}

// readSecretsV2ThenV1 reads logicalPath as KV v2 (buildKvV2Path), then as KV
// v1, each bounded by secretReadTimeout, and returns the first success
// flattened. It errors only when both fail; per-attempt lines are Debug here,
// and every caller logs its own final Warning.
func readSecretsV2ThenV1(client *vault.Client, logicalPath string) (map[string]string, error) {
	v2Path := buildKvV2Path(logicalPath)
	Debugf("vault: trying KV v2 path %q first", v2Path)

	ctx, cancel := context.WithTimeout(context.Background(), secretReadTimeout)
	secrets, err := readSecretsKvV2(ctx, client, v2Path)
	cancel()
	if err == nil {
		Debugf("vault: KV v2 read at %q succeeded with %d entries", v2Path, len(secrets))
		return secrets, nil
	}

	Debugf("vault: KV v2 read at %q failed: %v — falling back to KV v1 at %q", v2Path, err, logicalPath)
	ctx, cancel = context.WithTimeout(context.Background(), secretReadTimeout)
	secrets, err = readSecretsKvV1(ctx, client, logicalPath)
	cancel()
	if err != nil {
		return nil, err
	}
	Debugf("vault: KV v1 read at %q succeeded with %d entries", logicalPath, len(secrets))
	return secrets, nil
}

// readAndBuildSecrets reads path, records the secret-fetch metric and
// serialises the result via buildEnvBytes. ok=false (read error or no secrets,
// already logged via the caller's warnOnErr/warnOnEmpty and counted) means the
// caller must return without writing.
func readAndBuildSecrets(client *vault.Client, path string, warnOnErr func(err error), warnOnEmpty func()) (secrets map[string]string, content []byte, ok bool) {
	secrets, err := readSecretsV2ThenV1(client, path)
	if err != nil {
		warnOnErr(err)
		recordSecretFetch(false)
		return nil, nil, false
	}
	if len(secrets) == 0 {
		warnOnEmpty()
		recordSecretFetch(false)
		return nil, nil, false
	}
	recordSecretFetch(true)
	return secrets, buildEnvBytes(secrets), true
}

// flattenStringMap stringifies a Vault data map: scalars as text (json.Number
// included — vault-client-go decodes with UseNumber; extend the switch for a
// new scalar type), non-scalars as byte-stable JSON, so an unchanged secret
// never looks drifted. nil values and empty keys are skipped.
// Why: DOCS/CLAUDE.md § Vault secret fetch
func flattenStringMap(m map[string]interface{}) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		if k == "" {
			continue
		}
		switch x := v.(type) {
		case string:
			out[k] = x
		case json.Number:
			out[k] = x.String()
		case bool:
			out[k] = strconv.FormatBool(x)
		case float64:
			out[k] = strconv.FormatFloat(x, 'f', -1, 64)
		case int:
			out[k] = strconv.Itoa(x)
		case int64:
			out[k] = strconv.FormatInt(x, 10)
		case nil:
			// skip — nil cannot be expressed as a usable env value
		default:
			// Non-scalar (map, slice, …): JSON, never dropped.
			encoded, err := json.Marshal(v)
			if err != nil {
				Warningf("vault: cannot JSON-encode non-scalar secret value for key %q (type %T): %v — skipping", k, v, err)
				continue
			}
			out[k] = string(encoded)
			Debugf("vault: JSON-encoded non-scalar secret value for key %q (type %T)", k, v)
		}
	}
	return out
}

// buildEnvBytes serialises secrets as KEY=VALUE lines, sorted, strconv.Quote-ing
// values that need it. Deterministic, so callers compare it with the file on disk.
func buildEnvBytes(secrets map[string]string) []byte {
	keys := make([]string, 0, len(secrets))
	for k := range secrets {
		if k == "" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b bytes.Buffer
	for _, k := range keys {
		v := secrets[k]
		if needsEnvQuoting(v) {
			v = strconv.Quote(v)
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.Bytes()
}

// needsEnvQuoting reports whether v contains characters that must be quoted
// to remain safe in a shell-compatible env file. Empty values are also
// quoted so they round-trip as `KEY=""` rather than `KEY=`.
func needsEnvQuoting(v string) bool {
	if v == "" {
		return true
	}
	for _, c := range v {
		switch c {
		case ' ', '\t', '\n', '\r', '"', '\'', '#', '$', '\\', '`':
			return true
		}
	}
	return false
}

// ensureEnvFileMode chmods an existing path to 0600 if its mode differs
// (os.WriteFile sets the mode only on create). Callers run it every cycle and
// never autosync-gate it: metadata hardening, not a content sync. Fail-soft:
// Info only on a change, Warning on a failed chmod (e.g. EPERM, other owner).
// Why: DOCS/CLAUDE.md § Vault secret fetch
func ensureEnvFileMode(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	old := info.Mode().Perm()
	if old == 0o600 {
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		Warningf("vault: cannot tighten %q to 0600 (currently %04o): %v — secret file may be readable by other users", path, old, err)
		return
	}
	Infof("vault: tightened %q permissions %04o → 0600", path, old)
}

// ─── ResolveRefreshInterval ────────────────────────────────────────────────

// ResolveRefreshInterval returns the secret-refresh cadence from envValue
// (VAULT_REFRESH_INTERVAL), else configValue (vault.refresh_interval), else
// DefaultVaultRefreshInterval. Values are trimmed Go durations; invalid or
// negative → Warning + default; 0 disables (.env is then written at startup only).
func ResolveRefreshInterval(envValue, configValue string) time.Duration {
	s := strings.TrimSpace(envValue)
	source := "VAULT_REFRESH_INTERVAL env"
	if s == "" {
		s = strings.TrimSpace(configValue)
		source = "config.yml vault.refresh_interval"
	}
	if s == "" {
		Infof("vault: refresh interval defaults to %s (no override set)", DefaultVaultRefreshInterval)
		return DefaultVaultRefreshInterval
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		Warningf("vault: invalid refresh interval %q (source: %s): %v — using default %s",
			s, source, err, DefaultVaultRefreshInterval)
		return DefaultVaultRefreshInterval
	}
	if d < 0 {
		Warningf("vault: negative refresh interval %s (source: %s) — using default %s",
			d, source, DefaultVaultRefreshInterval)
		return DefaultVaultRefreshInterval
	}
	if d == 0 {
		Infof("vault: refresh interval set to 0 (source: %s) — periodic refresh disabled", source)
		return 0
	}
	Infof("vault: refresh interval set to %s (source: %s)", d, source)
	return d
}

// ─── Service secrets and refresh loops ────────────────────────────────────

// FetchServiceSecrets runs fetchServiceSecrets, autosync-gated and fail-soft,
// for every managed service: the scheduled path (startup, refresh tick); /sync
// calls fetchServiceSecrets with force. It must NOT take syncMu: the refresh
// tick already holds it.
func FetchServiceSecrets(appCfg *Config, vaultCfg *VaultConfig) {
	if appCfg == nil {
		return
	}
	managed := appCfg.managedEntries()
	if len(managed) == 0 {
		// Otherwise a host that manages nothing would be silent here.
		Debugf("vault: service secrets skipped — this host manages no services")
		return
	}
	for _, entry := range managed {
		fetchServiceSecrets(vaultCfg, entry, false)
	}
}

// fetchServiceSecrets syncs one service's ${workdir}/${service}/.env from
// entry.Secrets and, after a rewrite, restarts the unit if running. The write +
// restart are gated by autosync || force (/sync); the chmod and drift record always run.
func fetchServiceSecrets(vaultCfg *VaultConfig, entry ServiceEntry, force bool) {
	if vaultCfg == nil {
		return
	}

	state := GetVaultClient()
	if state == nil || state.Client == nil {
		return
	}

	svcName := entry.Service
	if svcName == "" {
		return
	}
	if entry.Secrets == "" {
		Debugf("vault: service secrets skipped for %q — secrets path not configured in continuous_deployment", svcName)
		return
	}
	// Gates only the write + restart; the read and drift record always run.
	autosync := serviceAutoSync(entry) || force

	destDir := serviceDir(entry)
	destFile := filepath.Join(destDir, ".env")

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		Warningf("vault: cannot create service dir %q: %v — skipping service secrets", destDir, err)
		return
	}

	// Every cycle, before the compare; never autosync-gated.
	ensureEnvFileMode(destFile)

	Debugf("vault: fetching service secrets for %q from %q (target %q)", svcName, entry.Secrets, destFile)

	secrets, newContent, ok := readAndBuildSecrets(state.Client, entry.Secrets,
		func(err error) {
			Warningf("vault: failed to read service secrets for %q at %q: %v — continuing",
				svcName, entry.Secrets, err)
		},
		func() {
			Warningf("vault: no usable service secrets for %q at %q — nothing written", svcName, entry.Secrets)
		},
	)
	if !ok {
		return
	}

	// Record the key diff (names only, never values) BEFORE any rewrite, so
	// oldPairs is the current on-disk state.
	oldPairs, perr := ParseEnvFile(destFile)
	if perr != nil {
		oldPairs = map[string]string{}
	}
	recordEnvDiff(svcName, oldPairs, secrets)

	if existing, readErr := os.ReadFile(destFile); readErr == nil && bytes.Equal(existing, newContent) {
		Debugf("vault: %q in sync", destFile)
		return
	}
	if !autosync {
		Infof("vault: %q out-of-date (autosync=false); not rewriting", destFile)
		return
	}
	// Atomic, never a plain write: /diff and /env read this file lock-free.
	if !writeSecretFileAtomic(destFile, newContent) {
		return
	}
	Infof("vault: wrote %d service secret(s) for %q to %q", len(secrets), svcName, destFile)

	// EnvironmentFile= is read only at unit start; a stopped unit stays
	// stopped (no-resurrect) and picks the file up at its next start.
	restartIfRunningForUpdate("vault", svcName, ".env update")
}

// StartAllSecretsRefresh starts the combined refresh loop: every interval,
// FetchAndWriteSecrets + FetchServiceSecrets under syncMu, then RenderTemplates.
// The first tick is one interval in (main.go covers t=0); it exits on ctx.Done.
// A nil vaultCfg, empty VAULT_ADDRESS or non-positive interval is a no-op.
// Why: DOCS/CLAUDE.md § Combined refresh loop
func StartAllSecretsRefresh(ctx context.Context, appCfg *Config, vaultCfg *VaultConfig, interval time.Duration) {
	if vaultCfg == nil {
		Infof("vault: secret refresh disabled — no Vault config")
		return
	}
	if vaultCfg.Address == "" {
		Infof("vault: secret refresh disabled — VAULT_ADDRESS not set")
		return
	}
	if interval <= 0 {
		Infof("vault: periodic secret refresh disabled (interval=%s)", interval)
		return
	}

	Infof("vault: periodic secret refresh started (interval=%s)", interval)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				Infof("vault: periodic secret refresh stopping (context cancelled)")
				return
			case <-ticker.C:
				// syncMu around the fetches: CD, /sync and this tick all write
				// managed-service files and run systemctl. The fetchers must not
				// re-take it; the deferred Unlock survives a panic.
				safeCycle("secrets-refresh", func() {
					func() {
						syncMu.Lock()
						defer syncMu.Unlock()
						FetchAndWriteSecrets(vaultCfg)
						FetchServiceSecrets(appCfg, vaultCfg)
					}()
					// Deliberately OUTSIDE syncMu: templates write only operator
					// files, so their Vault reads must not lengthen the lock.
					RenderTemplates(vaultCfg)
				})
			}
		}
	}()
}

// ─── .env hot-reload ────────────────────────────────────────────────────────

// authEnvKeys are the env keys whose change makes StartEnvReload re-run InitAuth.
var authEnvKeys = map[string]bool{
	"AUTH_ENABLED": true, "AUTH_PUBLIC_KEY_FILE": true,
	inlinePublicKeyEnv: true, legacyInlinePublicKeyEnv: true,
}

// ParseEnvFile parses a KEY=VALUE file, skipping blank lines, # comments and
// lines without a key before '='. Double-quoted values are strconv.Unquote'd
// (else just stripped); single quotes are stripped.
func ParseEnvFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 1 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := line[idx+1:]
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			// buildEnvBytes uses strconv.Quote; unquote to restore escape seqs.
			if unq, err := strconv.Unquote(val); err == nil {
				val = unq
			} else {
				val = val[1 : len(val)-1]
			}
		} else if len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'' {
			val = val[1 : len(val)-1]
		}
		result[key] = val
	}
	return result, nil
}

// StartEnvReload polls envPath every interval (non-positive disables). On an
// mtime change it Setenvs changed keys and Unsetenvs keys gone since the last
// parse (removing VAULT_TOKEN must really clear it). An authEnvKeys change with
// auth enabled re-runs InitAuth; an AUTH_ENABLED change only warns (restart).
// Why: DOCS/CLAUDE.md § Hot-reload contract
func StartEnvReload(ctx context.Context, envPath string, cfg *Config, interval time.Duration) {
	if interval <= 0 {
		Debugf("env: hot-reload disabled")
		return
	}

	var lastMod time.Time
	if fi, err := os.Stat(envPath); err == nil {
		lastMod = fi.ModTime()
	}

	prevKeys := make(map[string]bool)
	if pairs, err := ParseEnvFile(envPath); err == nil {
		for key := range pairs {
			prevKeys[key] = true
		}
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Per-cycle panic guard (.env is written by the refresh loop and
				// operators). Top-level `return` here = `continue`; nested loops'
				// continues are unaffected.
				safeCycle("env-reload", func() {
					fi, err := os.Stat(envPath)
					if err != nil {
						if !os.IsNotExist(err) {
							Warningf("env: hot-reload stat %s: %v", envPath, err)
						}
						return
					}
					if !fi.ModTime().After(lastMod) {
						return
					}
					prev := lastMod
					lastMod = fi.ModTime()
					Infof("env: %s changed (%s → %s), reloading",
						envPath, prev.Format(time.RFC3339), lastMod.Format(time.RFC3339))

					pairs, err := ParseEnvFile(envPath)
					if err != nil {
						Warningf("env: hot-reload parse %s: %v — keeping current env", envPath, err)
						lastMod = prev
						return
					}

					var changed []string
					authChanged := false
					authEnabledChanged := false
					for key, val := range pairs {
						if os.Getenv(key) != val {
							if err := os.Setenv(key, val); err != nil {
								Warningf("env: hot-reload setenv %s: %v", key, err)
								continue
							}
							changed = append(changed, key)
							if authEnvKeys[key] {
								authChanged = true
								if key == "AUTH_ENABLED" {
									authEnabledChanged = true
								}
							}
						}
					}
					for key := range prevKeys {
						if _, ok := pairs[key]; ok {
							continue
						}
						if err := os.Unsetenv(key); err != nil {
							Warningf("env: hot-reload unsetenv %s: %v", key, err)
							continue
						}
						changed = append(changed, key)
						if authEnvKeys[key] {
							authChanged = true
							if key == "AUTH_ENABLED" {
								authEnabledChanged = true
							}
						}
					}
					prevKeys = make(map[string]bool, len(pairs))
					for key := range pairs {
						prevKeys[key] = true
					}
					if len(changed) == 0 {
						Debugf("env: hot-reload: no values changed")
						return
					}
					sort.Strings(changed)
					Infof("env: hot-reload applied: %v", changed)

					if authEnabledChanged {
						Warningf("env: AUTH_ENABLED changed — JWT middleware binding requires a restart to take effect")
					}
					if authChanged {
						// authReloadMu serialises this read-InitAuth-write span with
						// StartConfigReload's (else a lost update); copy cfg.Auth only
						// after taking it.
						authReloadMu.Lock()
						cfg.mu.RLock()
						authCopy := cfg.Auth
						cfg.mu.RUnlock()
						if authCopy.Enabled {
							if err := InitAuth(&authCopy); err != nil {
								Warningf("env: hot-reload InitAuth: %v", err)
							} else {
								cfg.mu.Lock()
								cfg.Auth = authCopy
								cfg.mu.Unlock()
								Infof("env: hot-reload: auth re-initialised (key rotation applied)")
							}
						}
						authReloadMu.Unlock()
					}
				})
			}
		}
	}()
	Infof("env: hot-reload watching %s every %s", envPath, interval)
}
