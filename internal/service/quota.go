package service

import (
	"strconv"
	"strings"
)

// Live-state resolution for managed resource quotas: the live unit decides,
// never .applied_quota (a record of intent, not state).

// liveQuotaValue reads the live value behind a managed quota prop, rendered in
// runServiceQuotaApply's desired form so the two compare literally and drift
// logs quote configured units. "" = no cap live; false = unreadable.
// Why: DOCS/CLAUDE.md § Resource-quota live-state resolution
func liveQuotaValue(conn systemdConn, unit, prop string) (string, bool) {
	switch prop {
	case "CPUQuota":
		// Reported as runtime per second; cpuQuotaPercentString (as in /status)
		// converts back to the percentage.
		v, ok := getTypedUint64PropOK(conn, unit, "CPUQuotaPerSecUSec")
		if !ok {
			return "", false
		}
		return cpuQuotaPercentString(v), true
	case "MemoryMax":
		v, ok := getTypedUint64PropOK(conn, unit, "MemoryMax")
		if !ok {
			return "", false
		}
		// maxUint64 is systemd's "infinity"; 0 is no real cap either: both absent.
		if v == 0 || v == maxUint64 {
			return "", true
		}
		// systemd 252 reports it exactly as configured (no page rounding), so the
		// literal compare cannot re-apply forever.
		return strconv.FormatUint(v, 10), true
	}
	return "", false
}

// currentServiceQuota renders the quota live on svcName's unit in wanted's form
// (an uncapped prop as "Name=") and reports whether it matches. known=false
// (D-Bus unreachable, unit not loaded, prop unreadable or malformed): the
// caller must not decide — never fall back to .applied_quota. See ADR-0016.
func currentServiceQuota(svcName string, wanted []string) (current string, atWanted, known bool) {
	if len(wanted) == 0 {
		return "", true, true
	}
	conn, err := openSystemdConn()
	if err != nil {
		return "", false, false
	}
	defer conn.Close()

	unit := unitName(svcName)
	if getStringProp(conn, unit, "LoadState") != "loaded" {
		return "", false, false
	}

	live := make([]string, 0, len(wanted))
	for _, w := range wanted {
		name, _, ok := strings.Cut(w, "=")
		if !ok {
			// A caller bug, not a host condition: decline rather than guess.
			return "", false, false
		}
		val, ok := liveQuotaValue(conn, unit, name)
		if !ok {
			return "", false, false
		}
		live = append(live, name+"="+val)
	}

	cur := strings.Join(live, " ")
	return cur, cur == strings.Join(wanted, " "), true
}

// quotaAllUnset reports whether a currentServiceQuota string carries no live
// cap. An uncapped loaded unit renders "CPUQuota= MemoryMax=", never "", so
// this is what tells a first application from a change.
func quotaAllUnset(current string) bool {
	for _, f := range strings.Fields(current) {
		if _, v, ok := strings.Cut(f, "="); ok && v != "" {
			return false
		}
	}
	return true
}
