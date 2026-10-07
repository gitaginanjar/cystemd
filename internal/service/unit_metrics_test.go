package service

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// stubUnitHealth swaps in a collector returning snap (no systemd needed) and
// restores the collector and clock on cleanup; it returns the collector's call
// count.
func stubUnitHealth(t *testing.T, snap unitHealthSnapshot) *int {
	t.Helper()
	calls := 0
	prevCollect, prevNow := unitHealthCollect, unitHealthNow
	unitHealthCollect = func([]string) unitHealthSnapshot {
		calls++
		return snap
	}
	resetUnitHealthCacheForTest()
	t.Cleanup(func() {
		unitHealthCollect, unitHealthNow = prevCollect, prevNow
		resetUnitHealthCacheForTest()
	})
	return &calls
}

func renderUnitMetrics(t *testing.T, cfg *Config) string {
	t.Helper()
	var buf bytes.Buffer
	writeUnitMetrics(&buf, cfg)
	return buf.String()
}

func mustContain(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, want := range lines {
		if !strings.Contains(out, want) {
			t.Errorf("exposition missing %q\n--- got ---\n%s", want, out)
		}
	}
}

func mustNotContain(t *testing.T, out string, lines ...string) {
	t.Helper()
	for _, unwanted := range lines {
		if strings.Contains(out, unwanted) {
			t.Errorf("exposition unexpectedly contains %q\n--- got ---\n%s", unwanted, out)
		}
	}
}

// TestWriteUnitMetricsStateSetHasExactlyOneOne pins exactly one 1 per readable
// unit and a 0 for every other state (the zeros make recovery visible).
func TestWriteUnitMetricsStateSetHasExactlyOneOne(t *testing.T) {
	stubUnitHealth(t, unitHealthSnapshot{
		SystemdOK: true,
		Units: []unitHealth{
			{Service: "svcA", ActiveState: "active", NRestarts: 3, NRestartsOK: true},
			{Service: "svcB", ActiveState: "failed", NRestarts: 0, NRestartsOK: true},
		},
	})

	out := renderUnitMetrics(t, nil)

	mustContain(t, out,
		"cystemd_unit_health_scrape_ok 1",
		`cystemd_unit_active_state{service="svcA",state="active"} 1`,
		`cystemd_unit_active_state{service="svcA",state="failed"} 0`,
		`cystemd_unit_active_state{service="svcB",state="failed"} 1`,
		`cystemd_unit_active_state{service="svcB",state="inactive"} 0`,
		// NRestarts=0 is a real reading: emitted, not omitted.
		`cystemd_unit_restarts_total{service="svcA"} 3`,
		`cystemd_unit_restarts_total{service="svcB"} 0`,
	)

	for _, svc := range []string{"svcA", "svcB"} {
		ones := 0
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, `cystemd_unit_active_state{service="`+svc+`"`) && strings.HasSuffix(line, " 1") {
				ones++
			}
		}
		if ones != 1 {
			t.Errorf("service %s: got %d state series set to 1, want exactly 1", svc, ones)
		}
	}

	// Every state in the closed set is present for every unit.
	for _, state := range unitActiveStates {
		mustContain(t, out, `cystemd_unit_active_state{service="svcA",state="`+state+`"}`)
	}
}

// TestWriteUnitMetricsUnreadableUnitIsAllZeros pins all-zeros for an unreadable
// unit (a healthy one has exactly one 1) and no restarts series.
func TestWriteUnitMetricsUnreadableUnitIsAllZeros(t *testing.T) {
	stubUnitHealth(t, unitHealthSnapshot{
		SystemdOK: true,
		Units:     []unitHealth{{Service: "ghost", ActiveState: "", NRestartsOK: false}},
	})

	out := renderUnitMetrics(t, nil)

	for _, state := range unitActiveStates {
		mustContain(t, out, `cystemd_unit_active_state{service="ghost",state="`+state+`"} 0`)
	}
	// NRestarts absent is not zero: the series is omitted entirely.
	mustNotContain(t, out, `cystemd_unit_restarts_total{service="ghost"}`)
}

// TestWriteUnitMetricsSystemdUnavailable pins scrape_ok 0 and no unit series
// when systemd is unreachable (distinct from "everything is down").
func TestWriteUnitMetricsSystemdUnavailable(t *testing.T) {
	stubUnitHealth(t, unitHealthSnapshot{SystemdOK: false})

	out := renderUnitMetrics(t, nil)

	mustContain(t, out, "cystemd_unit_health_scrape_ok 0")
	mustNotContain(t, out, "cystemd_unit_active_state{", "cystemd_unit_restarts_total{")
}

// TestWriteUnitMetricsNoManagedServicesReportsOK pins scrape_ok 1 and no unit
// series when nothing is managed: not a failure.
func TestWriteUnitMetricsNoManagedServicesReportsOK(t *testing.T) {
	stubUnitHealth(t, unitHealthSnapshot{SystemdOK: true})

	out := renderUnitMetrics(t, nil)

	mustContain(t, out, "cystemd_unit_health_scrape_ok 1")
	mustNotContain(t, out, "cystemd_unit_active_state{")
}

// TestUnitHealthSampleCachesWithinTTL pins one collection per TTL window and a
// refresh exactly at the boundary (the open-route D-Bus guard).
func TestUnitHealthSampleCachesWithinTTL(t *testing.T) {
	calls := stubUnitHealth(t, unitHealthSnapshot{
		SystemdOK: true,
		Units:     []unitHealth{{Service: "svcA", ActiveState: "active"}},
	})

	now := time.Now()
	unitHealthNow = func() time.Time { return now }

	unitHealthSample(nil)
	unitHealthSample(nil)
	unitHealthSample(nil)
	if *calls != 1 {
		t.Fatalf("collector called %d times within the TTL, want 1", *calls)
	}

	// Just inside the window: still cached.
	now = now.Add(unitHealthCacheTTL - time.Millisecond)
	unitHealthSample(nil)
	if *calls != 1 {
		t.Fatalf("collector called %d times just inside the TTL, want 1", *calls)
	}

	// At the boundary the sample must refresh.
	now = now.Add(time.Millisecond)
	unitHealthSample(nil)
	if *calls != 2 {
		t.Fatalf("collector called %d times after the TTL expired, want 2", *calls)
	}
}

func TestManagedServiceNamesSortedAndNilSafe(t *testing.T) {
	if got := managedServiceNames(nil); got != nil {
		t.Errorf("managedServiceNames(nil) = %v, want nil", got)
	}

	cfg := &Config{
		HostSelectors: []string{"selA", "selB"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "zeta", Selector: "selB"},
			{Service: "alpha", Selector: "selA"},
			{Service: "alpha", Selector: "selB"}, // deduped by managedEntries
			{Service: "ignored", Selector: "otherpool"},
		},
	}

	got := managedServiceNames(cfg)
	want := []string{"alpha", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("managedServiceNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("managedServiceNames = %v, want %v", got, want)
		}
	}
}

// TestHandleMetricsIncludesUnitBlock pins the unit block inside the full
// writeMetrics exposition, not only writeUnitMetrics.
func TestHandleMetricsIncludesUnitBlock(t *testing.T) {
	stubUnitHealth(t, unitHealthSnapshot{
		SystemdOK: true,
		Units:     []unitHealth{{Service: "svcA", ActiveState: "active", NRestarts: 7, NRestartsOK: true}},
	})

	var buf bytes.Buffer
	writeMetrics(&buf, nil)
	out := buf.String()

	mustContain(t, out,
		"# TYPE cystemd_unit_active_state gauge",
		"# TYPE cystemd_unit_restarts_total counter",
		`cystemd_unit_restarts_total{service="svcA"} 7`,
		// still emits the pre-existing surface
		"cystemd_build_info{",
	)
}

// ─── unitActiveStates ↔ systemd's own enum ─────────────────────────────────
// Checked against sources other than the list itself, from two directions, so
// at least one test runs everywhere.

// systemctlActiveStates returns the "Available unit active states:" block of
// `systemctl --state=help` (no running systemd or D-Bus needed), or ok=false
// when systemctl is not installed.
func systemctlActiveStates(t *testing.T) ([]string, bool) {
	t.Helper()
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Exit status is ignored (help output has exited both 0 and non-zero); an
	// empty parsed section is what fails, never a vacuous pass.
	raw, _ := exec.CommandContext(ctx, "systemctl", "--state=help").CombinedOutput()

	var states []string
	inSection := false
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Available unit active states:") {
			inSection = true
			continue
		}
		if inSection {
			// The block ends at the next blank line or the next "Available …:" heading.
			if line == "" || strings.HasPrefix(line, "Available ") {
				break
			}
			states = append(states, line)
		}
	}
	return states, len(states) > 0
}

// TestUnitActiveStates_MatchesSystemdEnum pins every ActiveState the installed
// systemd reports into unitActiveStates; skips only without systemctl.
func TestUnitActiveStates_MatchesSystemdEnum(t *testing.T) {
	systemdStates, ok := systemctlActiveStates(t)
	if !ok {
		t.Skip("systemctl not installed — cannot consult the ActiveState enum")
	}

	known := make(map[string]bool, len(unitActiveStates))
	for _, s := range unitActiveStates {
		known[s] = true
	}
	for _, s := range systemdStates {
		if !known[s] {
			t.Errorf("systemd reports ActiveState %q but unitActiveStates does not carry it — "+
				"a unit in that state would emit all-zeros, which this package documents as "+
				"\"the state could not be read\". Add it to unitActiveStates.\n"+
				"systemd enum: %v\nunitActiveStates: %v", s, systemdStates, unitActiveStates)
		}
	}
}

// TestUnitActiveStates_CoversEveryRunningState pins every state unitIsRunning
// treats as live into unitActiveStates; needs nothing installed.
func TestUnitActiveStates_CoversEveryRunningState(t *testing.T) {
	known := make(map[string]bool, len(unitActiveStates))
	for _, s := range unitActiveStates {
		known[s] = true
	}
	// The closed set unitIsRunning switches on; keep in step with actions.go.
	for _, s := range []string{"active", "activating", "reloading", "refreshing"} {
		if !unitIsRunning(s) {
			t.Fatalf("unitIsRunning(%q) = false — this test's copy of the running-state "+
				"set has drifted from actions.go; update it", s)
		}
		if !known[s] {
			t.Errorf("unitIsRunning treats %q as a live state but unitActiveStates cannot "+
				"express it — a running unit would report as unreadable in "+
				"cystemd_unit_active_state", s)
		}
	}
}

// TestUnitActiveStates_IsSortedAndUnique pins the list sorted and unique: the
// exposition follows slice order, so an out-of-order insert reorders every scrape.
func TestUnitActiveStates_IsSortedAndUnique(t *testing.T) {
	for i := 1; i < len(unitActiveStates); i++ {
		if unitActiveStates[i-1] >= unitActiveStates[i] {
			t.Errorf("unitActiveStates must be sorted and unique: %q at %d precedes %q at %d",
				unitActiveStates[i-1], i-1, unitActiveStates[i], i)
		}
	}
}
