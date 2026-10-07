package service

import (
	"context"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// ─── dnf repository metadata refresh ───────────────────────────────────────

// DefaultDNFMakecacheTimeout bounds the refresh when none is configured — far
// below dnfInstallTimeout: it is one small mirror fetch holding up an install,
// and proceeding on a stale cache beats stalling the CD cycle.
const DefaultDNFMakecacheTimeout = 60 * time.Second

// dnfCacheSettings is the resolved, immutable refresh configuration, swapped
// atomically on config reload.
type dnfCacheSettings struct {
	Repos   []string
	Timeout time.Duration
}

// dnfCache holds the active settings; nil (not yet set) means disabled.
// Package-level because InstallServiceVersion's callers carry no *Config.
var dnfCache atomic.Pointer[dnfCacheSettings]

// SetDNFCacheSettings installs the resolved refresh settings (non-positive
// timeout → default). Called at startup and on every config reload.
func SetDNFCacheSettings(repos []string, timeout time.Duration) {
	if timeout <= 0 {
		timeout = DefaultDNFMakecacheTimeout
	}
	dnfCache.Store(&dnfCacheSettings{Repos: repos, Timeout: timeout})
}

// currentDNFCacheSettings returns the active settings, or the disabled zero
// value when nothing has been configured.
func currentDNFCacheSettings() dnfCacheSettings {
	if s := dnfCache.Load(); s != nil {
		return *s
	}
	return dnfCacheSettings{Timeout: DefaultDNFMakecacheTimeout}
}

// ResolveMakecacheRepos returns the repo list from envValue
// (CYSTEMD_DNF_MAKECACHE_REPOS, comma-separated), else configValue
// (dnf.makecache_repos); entries trimmed, empties dropped. Empty (the default)
// disables the refresh. Never name a site's repo ID in this package, even in a comment.
func ResolveMakecacheRepos(envValue string, configValue []string) []string {
	source := "CYSTEMD_DNF_MAKECACHE_REPOS env"
	parts := strings.Split(envValue, ",")
	if strings.TrimSpace(envValue) == "" {
		parts = configValue
		source = "config.yml dnf.makecache_repos"
	}

	repos := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			repos = append(repos, t)
		}
	}
	if len(repos) == 0 {
		Infof("dnf: pre-install metadata refresh disabled (no repos configured) — a build published after the last cache write stays invisible until metadata_expire elapses")
		return nil
	}
	Infof("dnf: pre-install metadata refresh enabled for repo(s) %s (source: %s)",
		strings.Join(repos, ","), source)
	return repos
}

// ResolveMakecacheTimeout returns the refresh timeout from envValue
// (CYSTEMD_DNF_MAKECACHE_TIMEOUT), else configValue (dnf.makecache_timeout),
// else DefaultDNFMakecacheTimeout. Invalid or non-positive → Warning + default:
// 0 does not disable (the empty repo list does).
func ResolveMakecacheTimeout(envValue, configValue string) time.Duration {
	s := strings.TrimSpace(envValue)
	source := "CYSTEMD_DNF_MAKECACHE_TIMEOUT env"
	if s == "" {
		s = strings.TrimSpace(configValue)
		source = "config.yml dnf.makecache_timeout"
	}
	if s == "" {
		return DefaultDNFMakecacheTimeout
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		Warningf("dnf: invalid makecache timeout %q (source: %s): %v — using default %s",
			s, source, err, DefaultDNFMakecacheTimeout)
		return DefaultDNFMakecacheTimeout
	}
	if d <= 0 {
		Warningf("dnf: non-positive makecache timeout %s (source: %s) — using default %s",
			d, source, DefaultDNFMakecacheTimeout)
		return DefaultDNFMakecacheTimeout
	}
	return d
}

// makecacheCommand is the exec seam for the refresh (like openSystemdConn):
// tests stub it rather than spawn a real dnf, which costs a timeout per test
// on a repo-less host.
var makecacheCommand = func(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "dnf", args...).CombinedOutput()
}

// refreshRepoMetadata refreshes each configured repo's dnf metadata so the
// install that follows sees builds published since the last cache write. A
// no-op when unconfigured (the default). Fail-soft by construction: it returns
// nothing and can never fail the install. See ADR-0015.
func refreshRepoMetadata() {
	s := currentDNFCacheSettings()
	if len(s.Repos) == 0 {
		return
	}

	// One call per repo, never a joined --enablerepo: dnf rejects an unknown ID
	// and then refreshes NOTHING, and hosts name the mirror differently.
	for _, repo := range s.Repos {
		refreshOneRepo(repo, s.Timeout)
	}
}

// refreshOneRepo refreshes one repo's metadata; errors are logged at Debug
// and dropped, so one unknown or unreachable repo blocks neither the others
// nor the install.
func refreshOneRepo(repo string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Scoped, never a blanket --refresh: every other repo keeps its cache, so
	// an unreachable upstream cannot fail this call or the install behind it.
	start := time.Now()
	raw, err := makecacheCommand(ctx, "makecache", "--disablerepo=*", "--enablerepo="+repo)
	if err != nil {
		Debugf("dnf: makecache for repo %s failed after %s: %v — continuing with cached metadata\n%s",
			repo, time.Since(start).Round(time.Millisecond), err, strings.TrimSpace(string(raw)))
		return
	}
	Debugf("dnf: makecache for repo %s completed in %s",
		repo, time.Since(start).Round(time.Millisecond))
}
