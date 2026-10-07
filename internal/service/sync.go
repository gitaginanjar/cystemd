package service

import (
	"fmt"
	"os"
	"strings"
	"sync"
)

// syncMu serialises everything that mutates managed files or runs dnf/systemctl:
// the CD cycle and the secrets-refresh tick (blocking Lock), RunServiceSync
// (TryLock → busy, never queued) and StartConfigReload's self-update (TryLock,
// skip the tick). NON-reentrant: RunServiceSync must never call
// RunContinuousDeployment (self-deadlock).
var syncMu sync.Mutex

// RunServiceSync is the /sync engine: under syncMu.TryLock it FORCES every
// autosync-gated surface (config, version, quota, Vault .env; no-resurrect
// still holds) for every managed service — never the config.yml clone or
// RunSelfVersionInstall. It returns a summary of what was attempted (outcomes
// go to the journal) and the managed names, one [AUDIT] line each; err only
// for a nil config, busy or nothing managed (names then nil).
// Why: DOCS/CLAUDE.md § `/sync` — on-demand forced sync
func RunServiceSync(appCfg *Config) (string, []string, error) {
	if appCfg == nil {
		return "", nil, fmt.Errorf("no configuration loaded")
	}

	// Never block: a mid-cycle /sync is told to retry, not held open for a clone.
	if !syncMu.TryLock() {
		Warningf("sync: rejected — a synchronisation is already in progress")
		return "", nil, fmt.Errorf("a synchronisation is already in progress; retry shortly")
	}
	defer syncMu.Unlock()

	// Every managed service, whatever ?service= says; none → error (HTTP 500).
	managed := appCfg.managedEntries()
	if len(managed) == 0 {
		Warningf("sync: no managed services resolved for this host — nothing to sync")
		return "", nil, fmt.Errorf("no managed service is configured for this host")
	}

	names := serviceNames(managed)
	Infof("sync: forced synchronisation requested for %v (autosync override)", names)

	var summary strings.Builder
	fmt.Fprintf(&summary, "forced sync of %d service(s) on this host (autosync override): %s\n",
		len(managed), strings.Join(names, ", "))

	// ── values + version + quota (forced per-service deployment) ────────────
	// Resolve git auth ONCE and reuse it across every managed service.
	gitRepo := strings.TrimSpace(os.Getenv("GIT_REPOSITORY"))
	switch {
	case gitRepo == "":
		Warningf("sync: GIT_REPOSITORY is empty — skipping values/version/quota deployment")
		summary.WriteString("- values/version/quota: skipped (GIT_REPOSITORY not set)\n")
	default:
		auth, err := resolveCloneSSHAuth()
		if err != nil {
			Warningf("sync: %v — skipping values/version/quota deployment", err)
			fmt.Fprintf(&summary, "- values/version/quota: skipped (%v)\n", err)
		} else {
			// One clone per distinct branch, force=true.
			deployManagedServices(gitRepo, auth, managed, true)
			// "attempted", not "synced": the helpers are fail-soft and return no
			// status; per-surface outcomes are in the journal.
			summary.WriteString("- values/version/quota: attempted for all managed services (see journal for details)\n")
		}
	}

	// ── Vault .env secrets (forced) ─────────────────────────────────────────
	if state := GetVaultClient(); state == nil || state.Client == nil {
		Warningf("sync: Vault client not available (degraded or not configured) — skipping service secrets")
		summary.WriteString("- secrets: skipped (Vault not available)\n")
	} else {
		vaultCfg := LoadVaultConfig()
		for _, entry := range managed {
			fetchServiceSecrets(vaultCfg, entry, true)
		}
		summary.WriteString("- secrets: attempted for all managed services (see journal for details)\n")
	}

	Infof("sync: forced synchronisation complete for %v", names)
	return summary.String(), names, nil
}
