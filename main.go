package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"cystemd/internal/service"
)

// printVersionAndExit prints the build metadata and exits 0.
func printVersionAndExit() {
	fmt.Printf("version: %s\n", service.Version())
	fmt.Printf("commit: %s\n", service.Commit())
	fmt.Printf("build_time: %s\n", service.BuildTime())
	fmt.Printf("go_version: %s\n", service.GoVersion())
	os.Exit(0)
}

// main wires startup in a strict order; only LoadConfig, InitAuth and the
// listener are fatal. See DOCS/CLAUDE.md § Startup sequence.
func main() {
	showVersion := flag.Bool("version", false, "print version information and exit")
	flag.Parse()

	if *showVersion {
		printVersionAndExit()
	}

	// Logged before the level is resolved, so it shows at the default Info.
	service.Infof("cystemd starting: version=%s commit=%s build_time=%s go_version=%s pid=%d",
		service.Version(), service.Commit(), service.BuildTime(), service.GoVersion(), os.Getpid())

	configPath := os.Getenv("CYSTEMD_CONFIG")
	if configPath == "" {
		configPath = "/opt/cystemd/config.yml"
	}
	service.Infof("loading config from %s", configPath)

	cfg, err := service.LoadConfig(configPath)
	if err != nil {
		service.Emergencyf("Error loading config: %v", err)
		os.Exit(1)
	}
	service.Infof("config loaded: listen_address=%s default_service=%q allowed_services=%d log_level=%s auth_enabled=%t",
		cfg.ListenAddress, cfg.ServiceName, len(cfg.AllowedServices), cfg.LogLevel, cfg.Auth.Enabled)

	// Must precede every writer into a managed service's ${workdir}/${service}.
	service.EnsureServiceWorkdir(cfg)

	// Self-update, fail-soft: a failed install logs Warning and startup goes on;
	// an installed update restarts cystemd, so the call then never returns.
	service.RunSelfVersionInstall(cfg)

	// CYSTEMD_LOG_LEVEL wins over config.yml log_level (default "debug").
	levelStr := os.Getenv("CYSTEMD_LOG_LEVEL")
	levelSource := "CYSTEMD_LOG_LEVEL env"
	if levelStr == "" {
		levelStr = cfg.LogLevel
		levelSource = "config"
	}
	if lvl, ok := service.ParseLevel(levelStr); ok {
		service.SetLevel(lvl)
		service.Infof("log level set to %q (source: %s)", levelStr, levelSource)
	} else {
		service.SetLevel(service.LevelDebug)
		service.Warningf("unknown log level %q (source: %s) — reverting to default %q", levelStr, levelSource, "debug")
	}

	if err := service.InitAuth(&cfg.Auth); err != nil {
		service.Emergencyf("Error initializing authentication: %v", err)
		os.Exit(1)
	}

	// Vault is non-fatal: a failure logs Warning and the API runs degraded with
	// a nil client; empty VAULT_ADDRESS is a no-op. EnsureEnvFileSecure chmods
	// the bootstrap .env to 0600 even when degraded.
	service.Infof("vault: reading configuration from environment")
	vaultCfg := service.LoadVaultConfig()
	service.EnsureEnvFileSecure(configPath)
	if err := service.InitVault(vaultCfg); err != nil {
		service.Warningf("vault: initialisation failed — continuing without Vault client (degraded mode): %v", err)
	}

	// Fail-soft (no-op without a Vault client): VAULT_PATH → VAULT_ENV_FILE.
	service.FetchAndWriteSecrets(vaultCfg)

	// Fail-soft: ServiceEntry.Secrets → ${workdir}/${service}/.env, then templates.
	service.FetchServiceSecrets(cfg, vaultCfg)
	service.RenderTemplates(vaultCfg)

	// dnf pre-install metadata refresh: env wins over config.yml; an empty repo
	// list (the default) disables it. applyReloaded re-applies it on reload.
	service.SetDNFCacheSettings(
		service.ResolveMakecacheRepos(
			os.Getenv("CYSTEMD_DNF_MAKECACHE_REPOS"),
			cfg.DNF.MakecacheRepos,
		),
		service.ResolveMakecacheTimeout(
			os.Getenv("CYSTEMD_DNF_MAKECACHE_TIMEOUT"),
			cfg.DNF.MakecacheTimeout,
		),
	)

	// VAULT_REFRESH_INTERVAL → vault.refresh_interval → 1m; 0 disables the
	// refresh (negative or invalid → default).
	refreshInterval := service.ResolveRefreshInterval(
		os.Getenv("VAULT_REFRESH_INTERVAL"),
		cfg.Vault.RefreshInterval,
	)
	service.StartAllSecretsRefresh(context.Background(), cfg, vaultCfg, refreshInterval)

	// In-process Vault Agent, fail-soft (renew-self, else a fresh InitVault).
	// VAULT_RENEWAL_INTERVAL → vault.renewal_interval → 30m is the ceiling of
	// a lease-aware schedule; 0 disables renewal.
	renewalInterval := service.ResolveRenewalInterval(
		os.Getenv("VAULT_RENEWAL_INTERVAL"),
		cfg.Vault.RenewalInterval,
	)
	// Install the floor (minimum delay and collapsed-lease re-auth threshold)
	// before the loop starts: its first scheduling decision reads it.
	service.SetRenewalFloor(service.ResolveRenewalFloor(
		os.Getenv("VAULT_RENEWAL_FLOOR"),
		cfg.Vault.RenewalFloor,
	))
	service.StartVaultRenewal(context.Background(), vaultCfg, renewalInterval)

	// CD knobs come from .env only (GIT_*): CD overwrites config.yml, so they
	// can never live there.
	cdCfg := service.LoadCDConfig(configPath)
	service.StartContinuousDeployment(context.Background(), cdCfg, cfg)

	// Hot-reload config.yml (rewritten by CD) and .env (rewritten by the refresh
	// loop or an operator), polling every 30s.
	service.StartConfigReload(context.Background(), configPath, cfg, 30*time.Second)
	envPath := filepath.Join(filepath.Dir(configPath), ".env")
	service.StartEnvReload(context.Background(), envPath, cfg, 30*time.Second)

	service.RegisterHandlers(cfg)

	// RED metrics for every route; RegisterHandlers used http.DefaultServeMux.
	handler := service.InstrumentHTTP(http.DefaultServeMux)

	// ReadHeaderTimeout guards against slow-loris. Never set Read/WriteTimeout:
	// action handlers may block up to systemdJobTimeout (120s) on a systemd job.
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	service.Infof("API server listening on %s", cfg.ListenAddress)
	if err := server.ListenAndServe(); err != nil {
		service.Emergencyf("HTTP server stopped: %v", err)
		os.Exit(1)
	}
}
