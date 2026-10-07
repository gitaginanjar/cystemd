package service

// unit_metrics.go — managed-unit health, SAMPLED from systemd at scrape time
// (cystemd is not in the loop for a crash, a manual stop or an auto-restart)
// and exposed as a state set: cystemd_unit_active_state{state="failed"} == 1.
// See ADR-0010.

import (
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// unitActiveStates is the closed set of systemd ActiveState values, sorted for
// a byte-stable exposition. Every state is emitted for every unit on every
// scrape (the zeros make recovery visible); an unreadable unit ("") emits all 0.
// Update it only from `systemctl --state=help`, never from memory, and keep
// every unitIsRunning state in it (TestUnitActiveStates_*).
// Why: DOCS/CLAUDE.md § Metrics / observability
var unitActiveStates = []string{
	"activating", "active", "deactivating", "failed", "inactive",
	"maintenance", "refreshing", "reloading",
}

// unitHealth is one managed unit's sample. NRestartsOK=false: systemd had no
// NRestarts (only .Service carries it) — the series is omitted, never a made-up 0.
type unitHealth struct {
	Service     string
	ActiveState string
	NRestarts   uint32
	NRestartsOK bool
}

// unitHealthSnapshot is one whole sample. SystemdOK=false means systemd was
// unreachable — otherwise indistinguishable from every unit being down.
type unitHealthSnapshot struct {
	Units     []unitHealth
	SystemdOK bool
}

// unitHealthCacheTTL bounds how often a scrape reaches D-Bus: /metrics is open,
// so without it anyone could drive unbounded D-Bus traffic. A 15–60s scrape
// still samples fresh.
const unitHealthCacheTTL = 5 * time.Second

var (
	unitHealthMu       sync.Mutex
	unitHealthCache    *unitHealthSnapshot
	unitHealthCachedAt time.Time

	// Test seams: a stub collector (no systemd needed) and a hand-driven clock.
	unitHealthNow     = time.Now
	unitHealthCollect = collectUnitHealthFromSystemd
)

// managedServiceNames returns the managed units' Service names, sorted; nil for
// a nil cfg. Safe across hot-reloads: applyReloaded mutates this same *Config
// under cfg.mu and managedEntries takes the read lock.
func managedServiceNames(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	entries := cfg.managedEntries()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Service)
	}
	sort.Strings(names)
	return names
}

// collectUnitHealthFromSystemd samples every managed unit over ONE D-Bus
// connection. Fail-soft: a connection error → SystemdOK=false, no units; a
// property error → its zero value, rendered as "unreadable", not a state.
func collectUnitHealthFromSystemd(services []string) unitHealthSnapshot {
	// Nothing to sample is not a failure: don't dial systemd just to prove it.
	if len(services) == 0 {
		return unitHealthSnapshot{SystemdOK: true}
	}

	conn, err := openSystemdConn()
	if err != nil {
		Debugf("metrics: managed-unit sample skipped, systemd unavailable: %v", err)
		return unitHealthSnapshot{SystemdOK: false}
	}
	defer conn.Close()

	snap := unitHealthSnapshot{SystemdOK: true, Units: make([]unitHealth, 0, len(services))}
	for _, svc := range services {
		unit := unitName(svc)
		u := unitHealth{Service: svc, ActiveState: getStringProp(conn, unit, "ActiveState")}
		u.NRestarts, u.NRestartsOK = getTypedUint32PropOK(conn, unit, "NRestarts")
		snap.Units = append(snap.Units, u)
	}
	return snap
}

// unitHealthSample returns a snapshot no older than unitHealthCacheTTL. The lock
// is held across collection so concurrent scrapes coalesce onto one sample; a
// wedged systemd stalls only the first scrape per TTL (failures are cached too).
func unitHealthSample(cfg *Config) unitHealthSnapshot {
	unitHealthMu.Lock()
	defer unitHealthMu.Unlock()

	if unitHealthCache != nil && unitHealthNow().Sub(unitHealthCachedAt) < unitHealthCacheTTL {
		return *unitHealthCache
	}

	snap := unitHealthCollect(managedServiceNames(cfg))
	unitHealthCache = &snap
	unitHealthCachedAt = unitHealthNow()
	return snap
}

// resetUnitHealthCacheForTest drops the memoized snapshot so a test's stub
// collector is actually consulted. Called from resetMetricsForTest.
func resetUnitHealthCacheForTest() {
	unitHealthMu.Lock()
	defer unitHealthMu.Unlock()
	unitHealthCache = nil
	unitHealthCachedAt = time.Time{}
}

// writeUnitMetrics appends the managed-unit health block to the exposition.
func writeUnitMetrics(w io.Writer, cfg *Config) {
	snap := unitHealthSample(cfg)

	fmt.Fprintln(w, "# HELP cystemd_unit_health_scrape_ok Whether the last managed-unit sample reached systemd over D-Bus; 1 also when there was nothing to sample, 0 when the connection failed (the unit series are then absent, not zero).")
	fmt.Fprintln(w, "# TYPE cystemd_unit_health_scrape_ok gauge")
	scrapeOK := 0
	if snap.SystemdOK {
		scrapeOK = 1
	}
	fmt.Fprintf(w, "cystemd_unit_health_scrape_ok %d\n", scrapeOK)

	// HELP/TYPE on every scrape, even with no units or systemd unreachable
	// (ADR-0002 contract); only the series are conditional.
	fmt.Fprintln(w, "# HELP cystemd_unit_active_state Managed unit systemd ActiveState as a state set; exactly one state carries 1 for a readable unit, and all states carry 0 when the unit's state could not be read.")
	fmt.Fprintln(w, "# TYPE cystemd_unit_active_state gauge")
	for _, u := range snap.Units {
		for _, state := range unitActiveStates {
			value := 0
			if u.ActiveState == state {
				value = 1
			}
			fmt.Fprintf(w, "cystemd_unit_active_state{service=\"%s\",state=\"%s\"} %d\n",
				escapeLabelValue(u.Service), state, value)
		}
	}

	fmt.Fprintln(w, "# HELP cystemd_unit_restarts_total Times systemd has auto-restarted the managed unit (its NRestarts property). Resets to 0 on systemctl reset-failed and on daemon state loss, which Prometheus handles as a normal counter reset; omitted entirely for units whose type has no NRestarts.")
	fmt.Fprintln(w, "# TYPE cystemd_unit_restarts_total counter")
	for _, u := range snap.Units {
		if !u.NRestartsOK {
			continue
		}
		fmt.Fprintf(w, "cystemd_unit_restarts_total{service=\"%s\"} %d\n",
			escapeLabelValue(u.Service), u.NRestarts)
	}
}
