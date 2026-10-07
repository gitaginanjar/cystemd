package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// DNFConfig holds the config.yml `dnf:` settings of the scoped pre-install
// metadata refresh (dnfcache.go). See ADR-0015.
type DNFConfig struct {
	// MakecacheRepos lists the repo IDs refreshed before each install. Empty
	// (the default) disables the refresh; no repo ID is ever compiled in.
	// Env CYSTEMD_DNF_MAKECACHE_REPOS (comma-separated) wins.
	MakecacheRepos []string `yaml:"makecache_repos"`

	// MakecacheTimeout is a Go duration bounding the refresh (default 60s;
	// non-positive → default). Fail-soft: past it the install proceeds on
	// cached metadata. Env CYSTEMD_DNF_MAKECACHE_TIMEOUT wins.
	MakecacheTimeout string `yaml:"makecache_timeout"`
}

// VaultYAMLConfig holds the config.yml `vault:` cadence knobs. Credentials stay
// env-only (VaultConfig); each knob is merged with its env var, env winning, by
// its Resolve* function.
type VaultYAMLConfig struct {
	// RefreshInterval is the secret-refresh cadence (Go duration; "0"
	// disables; empty → default). Env VAULT_REFRESH_INTERVAL wins.
	RefreshInterval string `yaml:"refresh_interval"`

	// RenewalInterval is the ceiling of the lease-aware renewal loop (Go
	// duration; "0" disables; empty → DefaultVaultRenewalInterval). Env
	// VAULT_RENEWAL_INTERVAL wins.
	RenewalInterval string `yaml:"renewal_interval"`

	// RenewalFloor is the renewal loop's floor and collapsed-lease threshold
	// (Go duration; empty or "0" → DefaultVaultRenewalFloor, as a zero floor
	// busy-loops). Env VAULT_RENEWAL_FLOOR wins. Unlike the intervals it is
	// genuinely hot-reloadable: it is read afresh per scheduling decision.
	RenewalFloor string `yaml:"renewal_floor"`
}

// NodePool groups a set of hostnames under a selector label.
// Declared in node_pools[] in config.yml.
type NodePool struct {
	Selector string   `yaml:"selector"`
	Nodes    []string `yaml:"nodes"`
}

// ServiceEntry is one continuous_deployment[] entry: a managed service and the
// node_pools selector that places it. Fields stay alphabetical by YAML tag.
// Service need not be unique (e.g. PT/POC variants under different Selectors);
// per host, managedEntries keeps the first entry per Service.
// Why: DOCS/CLAUDE.md § Continuous Deployment
type ServiceEntry struct {
	AutoSync            interface{} `yaml:"autosync"`               // tolerant truthy value (serviceAutoSync) enables; interface{} so a typo never aborts LoadConfig. Gates the .env / ${service_config} writes, dnf install, set-property quota and restart-if-running
	GitBranch           string      `yaml:"git_branch"`             // git branch for git_values clone; defaults to "master"
	GitValues           string      `yaml:"git_values"`             // path in repo to the values YAML file
	GitValuesConfigKey  string      `yaml:"git_values_config_key"`  // yq-style dot-path extracted from git_values into ${service_config}; ${selector} → effectiveSelector (SelectorKey, else Selector)
	GitValuesCPUKey     string      `yaml:"git_values_cpu_key"`     // dot-path to a scalar k8s CPU limit (e.g. "200m"), applied as CPUQuota via set-property; ${selector} as above
	GitValuesMemoryKey  string      `yaml:"git_values_memory_key"`  // dot-path to a scalar k8s memory limit (e.g. "1Gi"), applied as MemoryMax bytes via set-property; ${selector} as above
	GitValuesVersionKey string      `yaml:"git_values_version_key"` // dot-path to a scalar version tag in git_values; ${selector} as above
	Replica             int         `yaml:"replica"`
	Secrets             string      `yaml:"secrets"`        // Vault secret path for this service
	Selector            string      `yaml:"selector"`       // matches a node_pools[].selector (pool membership); also the ${selector} value when SelectorKey is empty
	SelectorKey         string      `yaml:"selectorkey"`    // optional ${selector} value inside every git_values_*_key; empty → Selector. Independent of pool resolution
	Service             string      `yaml:"service"`        // systemd unit name (need not be unique across entries)
	ServiceConfig       string      `yaml:"service_config"` // output filename under ${workdir}/${service}/; defaults to "target.config.yml"
	Workdir             string      `yaml:"workdir"`        // local base dir; defaults to "/opt"
}

// serviceWorkdir returns entry.Workdir, defaulting to "/opt".
func serviceWorkdir(entry ServiceEntry) string {
	if entry.Workdir == "" {
		return "/opt"
	}
	return entry.Workdir
}

// serviceServiceConfig returns entry.ServiceConfig, the extracted config's
// filename, defaulting to "target.config.yml".
func serviceServiceConfig(entry ServiceEntry) string {
	if entry.ServiceConfig == "" {
		return "target.config.yml"
	}
	return entry.ServiceConfig
}

// serviceGitBranch returns entry.GitBranch, defaulting to "master".
func serviceGitBranch(entry ServiceEntry) string {
	if entry.GitBranch == "" {
		return "master"
	}
	return entry.GitBranch
}

// serviceDir returns ${workdir}/${service}, the per-service state root
// (.installed_version, .applied_quota, .env, ${service_config}) — the single
// source of that path for CD, secrets, workdir creation and /config, /env.
func serviceDir(entry ServiceEntry) string {
	return filepath.Join(serviceWorkdir(entry), entry.Service)
}

// serviceNames returns the Service names of entries, in order. Used to log /
// summarise the set of services a host manages or a /sync touched.
func serviceNames(entries []ServiceEntry) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Service)
	}
	return names
}

// effectiveSelector returns the ${selector} substitution for values-file keys:
// SelectorKey, else Selector. Never use it for pool matching (always Selector).
func effectiveSelector(entry ServiceEntry) string {
	if entry.SelectorKey != "" {
		return entry.SelectorKey
	}
	return entry.Selector
}

// serviceAutoSync reports whether the entry may install its RPM, rewrite its
// .env / ${service_config}, apply quotas and restart the unit. True only for
// bool true, int 1, or "true"/"yes"/"1" (parseBoolEnv: case-insensitive,
// trimmed); anything else is false — never enable on ambiguity.
func serviceAutoSync(entry ServiceEntry) bool {
	switch v := entry.AutoSync.(type) {
	case bool:
		return v
	case string:
		b, err := parseBoolEnv(v)
		return err == nil && b
	case int:
		return v == 1
	case int64:
		return v == 1
	default:
		return false
	}
}

// managedEntries returns the entries whose Selector is in hostSelectors,
// deduplicated by Service, first wins — required: ${workdir}/${service} is the
// per-service state key and a host runs one instance of a unit (template
// instances such as "nginx@80"/"nginx@443" are distinct Services). Pure,
// returns copies; called every cycle, so LoadConfig logs collisions instead.
func managedEntries(entries []ServiceEntry, hostSelectors []string) []ServiceEntry {
	var managed []ServiceEntry
	seen := make(map[string]struct{})
	for _, e := range entriesForSelectors(entries, hostSelectors) {
		if _, dup := seen[e.Service]; dup {
			continue
		}
		seen[e.Service] = struct{}{}
		managed = append(managed, e)
	}
	return managed
}

// entriesForSelectors returns, in declaration order and without dedup, the
// entries with a non-empty Service whose Selector is in hostSelectors (nil if
// hostSelectors is empty) — the filter under managedEntries and serviceCollisions.
func entriesForSelectors(entries []ServiceEntry, hostSelectors []string) []ServiceEntry {
	if len(hostSelectors) == 0 {
		return nil
	}
	selSet := make(map[string]struct{}, len(hostSelectors))
	for _, s := range hostSelectors {
		selSet[s] = struct{}{}
	}
	var out []ServiceEntry
	for _, e := range entries {
		if e.Service == "" {
			continue
		}
		if _, ok := selSet[e.Selector]; !ok {
			continue
		}
		out = append(out, e)
	}
	return out
}

// serviceCollisions returns, in declaration order, the Services declared by more
// than one entry matching hostSelectors — those managedEntries silently drops,
// which LoadConfig reports once as a Warning.
func serviceCollisions(entries []ServiceEntry, hostSelectors []string) []string {
	counts := make(map[string]int)
	var order []string
	for _, e := range entriesForSelectors(entries, hostSelectors) {
		if counts[e.Service] == 0 {
			order = append(order, e.Service)
		}
		counts[e.Service]++
	}
	var dups []string
	for _, svc := range order {
		if counts[svc] > 1 {
			dups = append(dups, svc)
		}
	}
	return dups
}

// managedEntries returns this host's managed entries (copies), read under
// c.mu.RLock. Callers MUST NOT hold c.mu: RWMutex read locks are not reentrant
// while a writer waits. Falls back to HostSelector when HostSelectors is empty
// (hand-built configs; LoadConfig always sets both).
func (c *Config) managedEntries() []ServiceEntry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	sels := c.HostSelectors
	if len(sels) == 0 && c.HostSelector != "" {
		sels = []string{c.HostSelector}
	}
	return managedEntries(c.ContinuousDeployment, sels)
}

// EnsureServiceWorkdir creates ${workdir}/${service} (0755) for every managed
// service, at startup and after each hot-reload, before CD or the secrets
// fetch write there. Fail-soft: a failure logs a Warning.
func EnsureServiceWorkdir(cfg *Config) {
	managed := cfg.managedEntries()
	if len(managed) == 0 {
		Debugf("config: no managed services for this host; skipping workdir creation")
		return
	}
	for _, entry := range managed {
		dir := serviceDir(entry)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			Warningf("config: cannot create service workdir %q: %v", dir, err)
			continue
		}
		Debugf("config: service workdir ensured: %s", dir)
	}
}

// Config holds config.yml plus the host identity LoadConfig derives from the
// hostname. CD's own GIT_* settings are deliberately env-only: CD rewrites
// this very file every cycle.
// Why: DOCS/CLAUDE.md § Service whitelist
type Config struct {
	mu sync.RWMutex // guards every field applyReloaded writes (all but ListenAddress)

	// Derived by LoadConfig, not YAML keys. HostSelectors: every pool listing
	// this hostname (resolve entries via managedEntries). ServiceName /
	// HostSelector: the primary (first) managed entry, the ?service= default;
	// empty when nothing is managed. AllowedServices: the API whitelist.
	ServiceName     string
	HostSelector    string
	HostSelectors   []string
	AllowedServices []string

	Auth                 AuthConfig      `yaml:"auth"`
	ContinuousDeployment []ServiceEntry  `yaml:"continuous_deployment"`
	DNF                  DNFConfig       `yaml:"dnf"`
	ListenAddress        string          `yaml:"listen_address"`
	LogLevel             string          `yaml:"log_level"`
	NodePools            []NodePool      `yaml:"node_pools"`
	Vault                VaultYAMLConfig `yaml:"vault"`
	// Version is cystemd's own wanted RPM version (tag or commit hash). When it
	// matches neither Version() nor Commit(), RunSelfVersionInstall installs it
	// and restarts. Empty (the default) disables self-update. Hot-reloadable.
	Version string `yaml:"version"`
}

// applyReloaded copies every hot-reloadable field from a freshly loaded config
// under c.mu, then re-applies env precedence (auth, dnf refresh, renewal floor)
// and the log level. ListenAddress is excluded: the server is already bound.
func (c *Config) applyReloaded(from *Config) {
	c.mu.Lock()
	logLevelChanged := c.LogLevel != from.LogLevel
	c.ServiceName = from.ServiceName
	c.HostSelector = from.HostSelector
	c.HostSelectors = from.HostSelectors
	c.AllowedServices = from.AllowedServices
	c.NodePools = from.NodePools
	c.ContinuousDeployment = from.ContinuousDeployment
	c.LogLevel = from.LogLevel
	c.Auth = from.Auth
	// The parsed YAML is env-ignorant: re-apply the AUTH_* env precedence, or a
	// reload would revert an env-forced auth state to the config.yml value.
	applyAuthEnvOverrides(&c.Auth)
	c.Vault = from.Vault
	c.DNF = from.DNF
	c.Version = from.Version
	c.mu.Unlock()

	// Env keeps precedence on reload, as at startup — the trap
	// applyAuthEnvOverrides guards for auth.
	SetDNFCacheSettings(
		ResolveMakecacheRepos(os.Getenv("CYSTEMD_DNF_MAKECACHE_REPOS"), from.DNF.MakecacheRepos),
		ResolveMakecacheTimeout(os.Getenv("CYSTEMD_DNF_MAKECACHE_TIMEOUT"), from.DNF.MakecacheTimeout),
	)

	// The floor is read per scheduling decision, so reloading it takes effect.
	SetRenewalFloor(ResolveRenewalFloor(os.Getenv("VAULT_RENEWAL_FLOOR"), from.Vault.RenewalFloor))

	if logLevelChanged {
		if lvl, ok := ParseLevel(from.LogLevel); ok {
			SetLevel(lvl)
			Infof("config: log level updated to %q", from.LogLevel)
		} else {
			// Warn like startup does; otherwise a typo'd level is dropped silently.
			Warningf("config: hot-reload: unknown log_level %q — keeping current level", from.LogLevel)
		}
	}
}

// StartConfigReload polls path every interval (non-positive disables); on an
// mtime change it re-runs LoadConfig + applyReloaded, re-inits auth on a key
// change, then tries a self-update. A parse failure keeps cfg and retries.
// Why: DOCS/CLAUDE.md § Hot-reload contract
func StartConfigReload(ctx context.Context, path string, cfg *Config, interval time.Duration) {
	if interval <= 0 {
		Debugf("config: hot-reload disabled")
		return
	}

	var lastMod time.Time
	if fi, err := os.Stat(path); err == nil {
		lastMod = fi.ModTime()
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// Per-cycle panic guard (CD rewrites config.yml from git, so
				// malformed input reaches here). Inside it, `return` = `continue`.
				safeCycle("config-reload", func() {
					fi, err := os.Stat(path)
					if err != nil {
						Warningf("config: hot-reload stat %s: %v", path, err)
						return
					}
					if !fi.ModTime().After(lastMod) {
						return
					}
					prev := lastMod
					lastMod = fi.ModTime()
					Infof("config: %s changed (%s → %s), reloading",
						path, prev.Format(time.RFC3339), lastMod.Format(time.RFC3339))
					newCfg, err := LoadConfig(path)
					if err != nil {
						Warningf("config: hot-reload failed: %v — keeping current config", err)
						lastMod = prev // don't advance; retry on next tick
						return
					}
					cfg.mu.RLock()
					oldPublicKeyFile := cfg.Auth.PublicKeyFile
					oldAuthEnabled := cfg.Auth.Enabled
					cfg.mu.RUnlock()

					cfg.applyReloaded(newCfg)
					EnsureServiceWorkdir(cfg)

					cfg.mu.RLock()
					svcName := cfg.ServiceName
					hostSels := cfg.HostSelectors
					nAllowed := len(cfg.AllowedServices)
					newPublicKeyFile := cfg.Auth.PublicKeyFile
					newAuthEnabled := cfg.Auth.Enabled
					cfg.mu.RUnlock()
					Infof("config: hot-reload applied: primary_service=%q host_selectors=%v allowed_services=%d",
						svcName, hostSels, nAllowed)

					if oldAuthEnabled != newAuthEnabled {
						Warningf("config: auth.enabled changed — JWT middleware binding requires a restart to take effect")
					}
					if oldPublicKeyFile != newPublicKeyFile && newAuthEnabled {
						// authReloadMu serialises this read-InitAuth-write span with
						// StartEnvReload's (else a lost update); copy cfg.Auth only
						// after taking it.
						authReloadMu.Lock()
						cfg.mu.RLock()
						authCopy := cfg.Auth
						cfg.mu.RUnlock()
						if err := InitAuth(&authCopy); err != nil {
							Warningf("config: hot-reload InitAuth: %v", err)
						} else {
							cfg.mu.Lock()
							cfg.Auth = authCopy
							cfg.mu.Unlock()
							Infof("config: hot-reload: auth re-initialised (key rotation applied)")
						}
						authReloadMu.Unlock()
					}

					// Self-update only under syncMu.TryLock, skipping this tick if
					// busy: its restart SIGKILLs cystemd's cgroup, including a
					// managed-service dnf that CD or /sync may have in flight; the CD
					// cycle tail retries. The deferred Unlock survives a panic. Never
					// move the lock into RunSelfVersionInstall (syncMu is non-reentrant).
					if syncMu.TryLock() {
						func() {
							defer syncMu.Unlock()
							RunSelfVersionInstall(cfg)
						}()
					} else {
						Debugf("config: self-update deferred — a synchronisation is in progress; the CD cycle tail retries every tick")
					}
				})
			}
		}
	}()
	Infof("config: hot-reload watching %s every %s", path, interval)
}

// resolveFromHostname returns hostSelectors — every pool whose nodes include
// hostname (case-insensitive, deduped, declaration order; the host manages the
// union, no "last match wins") — and allowedServices, the deduped Services of
// all continuous_deployment[] entries regardless of host (the API whitelist).
func resolveFromHostname(hostname string, pools []NodePool, entries []ServiceEntry) (hostSelectors, allowedServices []string) {
	seen := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.Service == "" {
			continue
		}
		if _, ok := seen[e.Service]; ok {
			continue
		}
		seen[e.Service] = struct{}{}
		allowedServices = append(allowedServices, e.Service)
	}

	selSeen := make(map[string]struct{})
	for _, pool := range pools {
		for _, node := range pool.Nodes {
			if strings.EqualFold(node, hostname) {
				if _, ok := selSeen[pool.Selector]; !ok {
					selSeen[pool.Selector] = struct{}{}
					hostSelectors = append(hostSelectors, pool.Selector)
				}
				break // only one match per pool
			}
		}
	}
	return hostSelectors, allowedServices
}

// LoadConfig parses the YAML at path, defaults listen_address (":50080") and
// log_level ("debug"), and derives the host identity (HostSelectors,
// ServiceName, HostSelector, AllowedServices) from os.Hostname.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}

	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":50080"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "debug"
	}

	hostname, hostnameErr := os.Hostname()
	if hostnameErr != nil {
		Warningf("config: cannot resolve hostname: %v — service_name will be empty", hostnameErr)
	} else {
		cfg.HostSelectors, cfg.AllowedServices = resolveFromHostname(hostname, cfg.NodePools, cfg.ContinuousDeployment)
		managed := managedEntries(cfg.ContinuousDeployment, cfg.HostSelectors)

		// Primary = first managed entry: the ?service= default.
		if len(managed) > 0 {
			cfg.ServiceName = managed[0].Service
			cfg.HostSelector = managed[0].Selector
		}

		// Warn once here, not per cycle, about entries managedEntries drops.
		for _, svc := range serviceCollisions(cfg.ContinuousDeployment, cfg.HostSelectors) {
			Warningf("config: service %q is declared by multiple entries matching this host's pools — managing the first, ignoring the rest (a host runs one instance of a unit)", svc)
		}

		names := serviceNames(managed)
		if len(managed) == 0 && len(cfg.NodePools) > 0 {
			Warningf("config: hostname %q resolves to no managed services (host_selectors=%v) — not in any node_pool, or its pools have no continuous_deployment entry", hostname, cfg.HostSelectors)
		}
		Debugf("config: identity resolved: hostname=%q managed_services=%v host_selectors=%v allowed_services=%d",
			hostname, names, cfg.HostSelectors, len(cfg.AllowedServices))
	}

	return &cfg, nil
}
