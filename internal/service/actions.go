package service

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

// ─── D-Bus helpers ─────────────────────────────────────────────────────────

// unitName appends ".service" to svc if it carries no unit-type suffix already.
func unitName(svc string) string {
	if strings.Contains(svc, ".") {
		return svc
	}
	return svc + ".service"
}

// dbusConnectTimeout bounds openRealSystemdConn's WAIT for the dial, not the
// dial itself: godbus makes the dial ctx the connection's lifetime (auto-Close
// on Done), so that ctx must never carry a deadline or a live connection would
// be torn down mid-request. On timeout the dial goroutine (and any late
// connection) leaks, accepted over an unrecoverable hang.
const dbusConnectTimeout = 10 * time.Second

// systemdConn is the subset of *dbus.Conn this package uses: a test seam
// (tests swap openSystemdConn for a fake that embeds it and implements only
// what is called; anything else nil-panics). Keep it minimal: every method is
// one a fake may have to grow. See DOCS/CLAUDE.md § Testing.
type systemdConn interface {
	Close()
	GetUnitPropertyContext(ctx context.Context, unit, propertyName string) (*dbus.Property, error)
	GetUnitTypePropertyContext(ctx context.Context, unit, unitType, propertyName string) (*dbus.Property, error)
	StartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	StopUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	RestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	TryRestartUnitContext(ctx context.Context, name, mode string, ch chan<- string) (int, error)
	ReloadContext(ctx context.Context) error
	EnableUnitFilesContext(ctx context.Context, files []string, runtime, force bool) (bool, []dbus.EnableUnitFileChange, error)
	DisableUnitFilesContext(ctx context.Context, files []string, runtime bool) ([]dbus.DisableUnitFileChange, error)
}

// A godbus signature change breaks the build here, not at a call site.
var _ systemdConn = (*dbus.Conn)(nil)

// openSystemdConn opens a system D-Bus connection to systemd, bounded by
// dbusConnectTimeout. A var so tests can substitute a fake (restore it via
// t.Cleanup).
var openSystemdConn = openRealSystemdConn

func openRealSystemdConn() (systemdConn, error) {
	type dialResult struct {
		conn systemdConn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := dbus.NewSystemdConnectionContext(context.Background())
		resultCh <- dialResult{conn, err}
	}()
	select {
	case r := <-resultCh:
		// Return a nil INTERFACE on failure: r.conn would be a non-nil
		// systemdConn holding a nil *dbus.Conn, passing a conn != nil check.
		if r.err != nil {
			return nil, r.err
		}
		return r.conn, nil
	case <-time.After(dbusConnectTimeout):
		return nil, fmt.Errorf("systemd dbus connect: timed out after %s", dbusConnectTimeout)
	}
}

// dbusPropertyTimeout bounds each get*Prop read. It is a per-call ctx, so
// expiry fails only that read and never tears down the connection. Without it a
// wedged systemd pins goroutines forever (the watchdog's post-grace reads,
// StatusService's ~15 reads per /status).
const dbusPropertyTimeout = 5 * time.Second

func getStringProp(conn systemdConn, unit, prop string) string {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitPropertyContext(ctx, unit, prop)
	if err != nil {
		return ""
	}
	v, _ := p.Value.Value().(string)
	return v
}

// NOTE: deliberately no getUint32Prop (.Unit uint32 reader): its only user,
// MainPID, lives on the per-type interface (the silent-zero bug). Use
// getTypedUint32Prop; verify any Unit-interface property with busctl first.
// Why: DOCS/MEMORY.md § Dead helper `getUint32Prop` removed

// knownUnitTypes maps unit-name suffixes to their per-type D-Bus interface
// (…systemd1.Service, …Socket). Any other suffix is not a unit type (a unit may
// be named "my.app"), so unitTypeInterface falls back to Service.
var knownUnitTypes = map[string]string{
	"automount": "Automount",
	"device":    "Device",
	"mount":     "Mount",
	"path":      "Path",
	"scope":     "Scope",
	"service":   "Service",
	"slice":     "Slice",
	"socket":    "Socket",
	"swap":      "Swap",
	"target":    "Target",
	"timer":     "Timer",
}

// unitTypeInterface maps a unit name to the D-Bus interface suffix carrying its
// type-specific properties: "sshd.service" → "Service", "foo.socket" → "Socket".
// Defaults to "Service" for an unknown or absent suffix, matching unitName's
// intent (it appends ".service" to a bare name).
func unitTypeInterface(unit string) string {
	if i := strings.LastIndex(unit, "."); i >= 0 {
		if t, ok := knownUnitTypes[strings.ToLower(unit[i+1:])]; ok {
			return t
		}
	}
	return "Service"
}

// getTypedStringProp and the other getTyped*Prop helpers read the unit's
// PER-TYPE interface (…systemd1.Service etc.); getStringProp, getUint64Prop and
// getStringSliceProp read …systemd1.Unit. sd-bus has no cross-interface
// fallback and all of them swallow errors into zero, so a per-type property
// (MainPID, ControlGroup, Tasks*, Memory*, CPU*, NRestarts, TimeoutStopUSec) read
// through a .Unit helper silently yields 0. A unit without the interface (a
// .target has no MainPID) errors → zero → omitted, as intended.
// Why: DOCS/CLAUDE.md § Continuous Deployment
func getTypedStringProp(conn systemdConn, unit, prop string) string {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitTypePropertyContext(ctx, unit, unitTypeInterface(unit), prop)
	if err != nil {
		return ""
	}
	v, _ := p.Value.Value().(string)
	return v
}

func getTypedUint32Prop(conn systemdConn, unit, prop string) uint32 {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitTypePropertyContext(ctx, unit, unitTypeInterface(unit), prop)
	if err != nil {
		return 0
	}
	v, _ := p.Value.Value().(uint32)
	return v
}

// getTypedUint32PropOK is getTypedUint32Prop keeping the read outcome, for a
// property where zero is a measurement (NRestarts=0: never auto-restarted),
// distinct from an unreadable one.
func getTypedUint32PropOK(conn systemdConn, unit, prop string) (uint32, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitTypePropertyContext(ctx, unit, unitTypeInterface(unit), prop)
	if err != nil {
		return 0, false
	}
	v, ok := p.Value.Value().(uint32)
	if !ok {
		return 0, false
	}
	return v, true
}

// getTypedUint64PropOK is getTypedUint64Prop keeping the read outcome, for the
// quota check: a cap gone from the live unit (re-apply) must not collapse with
// a failed read (skip the cycle; see currentServiceQuota).
func getTypedUint64PropOK(conn systemdConn, unit, prop string) (uint64, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitTypePropertyContext(ctx, unit, unitTypeInterface(unit), prop)
	if err != nil {
		return 0, false
	}
	v, ok := p.Value.Value().(uint64)
	if !ok {
		return 0, false
	}
	return v, true
}

func getTypedUint64Prop(conn systemdConn, unit, prop string) uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitTypePropertyContext(ctx, unit, unitTypeInterface(unit), prop)
	if err != nil {
		return 0
	}
	v, _ := p.Value.Value().(uint64)
	return v
}

func getUint64Prop(conn systemdConn, unit, prop string) uint64 {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitPropertyContext(ctx, unit, prop)
	if err != nil {
		return 0
	}
	v, _ := p.Value.Value().(uint64)
	return v
}

func getStringSliceProp(conn systemdConn, unit, prop string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), dbusPropertyTimeout)
	defer cancel()
	p, err := conn.GetUnitPropertyContext(ctx, unit, prop)
	if err != nil {
		return nil
	}
	switch v := p.Value.Value().(type) {
	case []string:
		return v
	case []interface{}:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// ─── formatting helpers ────────────────────────────────────────────────────

const maxUint64 = ^uint64(0)

// humanDuration formats a duration like systemctl: "1h 31min ago".
func humanDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) - h*60
	s := int(d.Seconds()) - h*3600 - m*60
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dmin ago", h, m)
	case m > 0:
		return fmt.Sprintf("%dmin %ds ago", m, s)
	default:
		return fmt.Sprintf("%ds ago", s)
	}
}

// ─── /proc helpers ─────────────────────────────────────────────────────────

// readComm reads the short process name from /proc/<pid>/comm.
func readComm(pid uint32) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// readCmdline returns a process's argv joined with spaces (readComm on error).
// argv can carry secrets (--db-password=...), so it must NEVER back a field of
// the open /status or /version endpoints: use readComm there.
func readCmdline(pid uint32) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return readComm(pid)
	}
	parts := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	return strings.Join(parts, " ")
}

// ─── systemctl job runners ─────────────────────────────────────────────────

// systemdJobTimeout bounds the wait for a job result. It sits above systemd's
// own job timeout (DefaultTimeoutStartSec, ~90s), so it fires only when systemd
// is wedged and never answers; a slow legitimate start resolves first.
const systemdJobTimeout = 120 * time.Second

// postKillDrainTimeout bounds the job-result wait AFTER the watchdog SIGKILLed a
// unit. Never use it when no kill was issued (see killDrainTimeout).
const postKillDrainTimeout = 5 * time.Second

// killDrainTimeout picks the post-grace awaitJobResult timeout and note:
// postKillDrainTimeout after a SIGKILL, else systemdJobTimeout, so a unit still
// transitioning on its own (a slow ExecStartPre) never gets a spurious timeout
// and a false FAILED audit line.
func killDrainTimeout(killed bool) (timeout time.Duration, note string) {
	if killed {
		return postKillDrainTimeout, " (post-watchdog)"
	}
	return systemdJobTimeout, " (still transitioning, no kill sent)"
}

// awaitJobResult waits up to timeout for the job result on ch: nil for "done",
// an error for any other result or no result in time. note (e.g.
// " (post-watchdog)") is appended to the log and error text.
func awaitJobResult(ch <-chan string, action, svc, note string, timeout time.Duration) error {
	select {
	case result := <-ch:
		Tracef("systemd %s %s: job result=%s%s", action, svc, result, note)
		if result != "done" {
			return fmt.Errorf("systemd %s %s: job result=%q%s", action, svc, result, note)
		}
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("systemd %s %s: timed out after %s waiting for job result%s", action, svc, timeout, note)
	}
}

func runSystemdJob(action, svc string) (string, error) {
	Debugf("systemd: %s %s", action, svc)
	conn, err := openSystemdConn()
	if err != nil {
		return "", fmt.Errorf("systemd connect: %w", err)
	}
	defer conn.Close()

	unit := unitName(svc)
	ch := make(chan string, 1)
	ctx := context.Background()

	switch action {
	case "start":
		_, err = conn.StartUnitContext(ctx, unit, "replace", ch)
	default:
		return "", fmt.Errorf("unknown action: %s (use runSystemdWithKillWatchdog for stop/restart)", action)
	}
	if err != nil {
		return "", fmt.Errorf("systemd %s %s: %w", action, svc, err)
	}

	if err := awaitJobResult(ch, action, svc, "", systemdJobTimeout); err != nil {
		return "", err
	}
	return fmt.Sprintf("systemd: %s %s → done\n", action, svc), nil
}

// Kill-watchdog grace: the unit's own TimeoutStopUSec + watchdogGraceMargin,
// capped at watchdogGraceCap. systemd SIGKILLs at its own deadline, so ours is
// a backstop for a wedged systemd, never a competing stop policy. See ADR-0007.
const (
	watchdogGraceMargin  = 2 * time.Second
	watchdogGraceDefault = 3 * time.Second
	watchdogGraceCap     = 60 * time.Second
)

// watchdogGrace returns the wait before a SIGKILL is considered and whether a
// kill is permitted: 0 → watchdogGraceDefault; infinity (maxUint64) → never
// kill. An unreadable value also arrives as 0 and so fails open (ADR-0007's
// known defect).
func watchdogGrace(stopTimeoutUSec uint64) (time.Duration, bool) {
	switch stopTimeoutUSec {
	case 0:
		return watchdogGraceDefault, true
	case maxUint64:
		return watchdogGraceCap, false
	}
	d := time.Duration(stopTimeoutUSec)*time.Microsecond + watchdogGraceMargin
	if d > watchdogGraceCap {
		d = watchdogGraceCap
	}
	return d, true
}

func runSystemdWithKillWatchdog(action, svc string) (string, error) {
	Debugf("systemd: %s %s (with kill watchdog)", action, svc)
	conn, err := openSystemdConn()
	if err != nil {
		return "", fmt.Errorf("systemd connect: %w", err)
	}
	defer conn.Close()

	unit := unitName(svc)
	ch := make(chan string, 1)
	ctx := context.Background()

	switch action {
	case "stop":
		_, err = conn.StopUnitContext(ctx, unit, "replace", ch)
	case "restart":
		_, err = conn.RestartUnitContext(ctx, unit, "replace", ch)
	case "try-restart":
		_, err = conn.TryRestartUnitContext(ctx, unit, "replace", ch)
	default:
		return "", fmt.Errorf("unknown watchdog action: %s", action)
	}
	if err != nil {
		return "", fmt.Errorf("systemd %s %s: %w", action, svc, err)
	}

	// TimeoutStopUSec is per-type: a .Unit read would silently yield 0 → 3s.
	grace, mayKill := watchdogGrace(getTypedUint64Prop(conn, unit, "TimeoutStopUSec"))
	Debugf("systemd: %s %s watchdog grace=%s may_kill=%t", action, svc, grace, mayKill)

	select {
	case result := <-ch:
		Tracef("systemd %s %s: job result=%s", action, svc, result)
		if result != "done" {
			return "", fmt.Errorf("systemd %s %s: job result=%q", action, svc, result)
		}
		return fmt.Sprintf("systemd: %s %s → done\n", action, svc), nil

	case <-time.After(grace):
		state := getStringProp(conn, unit, "ActiveState")
		Tracef("systemd %s %s: ActiveState=%s after %s (watchdog)", action, svc, state, grace)
		killed := false
		if state == "deactivating" && !mayKill {
			Infof("watchdog: %s still deactivating after %s but TimeoutStopUSec=infinity — not killing (operator policy)", svc, grace)
		}
		if state == "deactivating" && mayKill {
			// MainPID is per-type: read via .Unit it is 0 and the kill never fires.
			if pid := getTypedUint32Prop(conn, unit, "MainPID"); pid > 0 {
				Warningf("watchdog: SIGKILL → %s MainPID=%d (stuck deactivating)", svc, pid)
				_ = syscall.Kill(int(pid), syscall.SIGKILL)
				killed = true
			}
		}
		timeout, note := killDrainTimeout(killed)
		if err := awaitJobResult(ch, action, svc, note, timeout); err != nil {
			return "", err
		}
		if killed {
			return fmt.Sprintf("systemd: %s %s → done (watchdog fired)\n", action, svc), nil
		}
		return fmt.Sprintf("systemd: %s %s → done\n", action, svc), nil
	}
}

// ─── public service actions ────────────────────────────────────────────────

func RestartService(svc string) (string, error) { return runSystemdWithKillWatchdog("restart", svc) }
func StartService(svc string) (string, error)   { return runSystemdJob("start", svc) }
func StopService(svc string) (string, error)    { return runSystemdWithKillWatchdog("stop", svc) }

// TryRestartService restarts the unit ONLY if it is running (a stopped unit's
// no-op job reports "done"). systemd checks at job time, so there is no
// read-then-act race: the enforcement half of no-resurrect. See ADR-0004.
func TryRestartService(svc string) (string, error) {
	return runSystemdWithKillWatchdog("try-restart", svc)
}

// ReloadSystemdDaemon is `systemctl daemon-reload` over D-Bus. CD calls it
// after an RPM install so a unit file the package added or replaced is known
// before any later job: not every package daemon-reloads in %post, and a job
// on an unseen unit fails "unit not found".
func ReloadSystemdDaemon() error {
	Debugf("systemd: daemon-reload")
	conn, err := openSystemdConn()
	if err != nil {
		return fmt.Errorf("systemd connect: %w", err)
	}
	defer conn.Close()
	if err := conn.ReloadContext(context.Background()); err != nil {
		return fmt.Errorf("systemd daemon-reload: %w", err)
	}
	return nil
}

// unitIsRunning reports whether an ActiveState has a live or starting process;
// no-resurrect restarts only such units. "deactivating" is NOT running (an
// operator stop in progress must not be undone), nor are inactive, failed and
// "" (unreadable).
func unitIsRunning(state string) bool {
	switch state {
	case "active", "activating", "reloading", "refreshing":
		return true
	}
	return false
}

// ServiceActiveState returns the unit's live ActiveState ("active",
// "inactive", "failed", "activating", …), or "" when it cannot be read (no
// systemd, connection failure, or a unit systemd has never loaded).
func ServiceActiveState(svc string) string {
	conn, err := openSystemdConn()
	if err != nil {
		return ""
	}
	defer conn.Close()
	return getStringProp(conn, unitName(svc), "ActiveState")
}

// unitFileChange is the field set shared by dbus.EnableUnitFileChange and
// dbus.DisableUnitFileChange, so both reports share formatUnitFileChanges.
type unitFileChange struct {
	Type        string
	Filename    string
	Destination string
}

// formatUnitFileChanges renders one "Type: Filename → Destination" line per
// change, or a "no symlink changes" line naming svc and verb.
func formatUnitFileChanges(changes []unitFileChange, svc, verb string) string {
	var b strings.Builder
	for _, c := range changes {
		fmt.Fprintf(&b, "%s: %s → %s\n", c.Type, c.Filename, c.Destination)
	}
	if b.Len() == 0 {
		fmt.Fprintf(&b, "systemd: %s %s (no symlink changes)\n", svc, verb)
	}
	return b.String()
}

func EnableService(svc string) (string, error) {
	Debugf("systemd: enable %s", svc)
	conn, err := openSystemdConn()
	if err != nil {
		return "", fmt.Errorf("systemd connect: %w", err)
	}
	defer conn.Close()

	unit := unitName(svc)
	_, changes, err := conn.EnableUnitFilesContext(context.Background(), []string{unit}, false, true)
	if err != nil {
		return "", fmt.Errorf("systemd enable %s: %w", svc, err)
	}
	Tracef("systemd enable %s: %d symlink changes", svc, len(changes))

	converted := make([]unitFileChange, len(changes))
	for i, c := range changes {
		converted[i] = unitFileChange(c)
	}
	return formatUnitFileChanges(converted, svc, "enabled"), nil
}

func DisableService(svc string) (string, error) {
	Debugf("systemd: disable %s", svc)
	conn, err := openSystemdConn()
	if err != nil {
		return "", fmt.Errorf("systemd connect: %w", err)
	}
	defer conn.Close()

	unit := unitName(svc)
	changes, err := conn.DisableUnitFilesContext(context.Background(), []string{unit}, false)
	if err != nil {
		return "", fmt.Errorf("systemd disable %s: %w", svc, err)
	}
	Tracef("systemd disable %s: %d symlink changes", svc, len(changes))

	converted := make([]unitFileChange, len(changes))
	for i, c := range changes {
		converted[i] = unitFileChange(c)
	}
	return formatUnitFileChanges(converted, svc, "disabled"), nil
}

// ProcessEntry is one cgroup process in StatusService (fields in JSON-tag
// order). Command is the short comm (readComm), NEVER argv: /status is open and
// an ExecStart may pass a secret as a flag.
type ProcessEntry struct {
	Command string `json:"command"`
	PID     uint32 `json:"pid"`
}

// UnitStatusJSON is the StatusService shape; keep fields in JSON-tag
// alphabetical order. Numeric fields use omitempty (zero = unavailable), except
// NRestarts (*uint32): a measured 0 is emitted, only a failed read is omitted.
// See DOCS/DESIGN-N-RESTARTS-ZERO-VS-ABSENT.md.
type UnitStatusJSON struct {
	ActiveDuration     string         `json:"active_duration,omitempty"`
	ActiveSince        string         `json:"active_since,omitempty"`       // RFC3339
	ActiveSinceHuman   string         `json:"active_since_human,omitempty"` // e.g. "Tue 5 May 2026 07:45:00 WIB"
	ActiveState        string         `json:"active_state"`
	CGroup             string         `json:"cgroup,omitempty"`
	CPUQuotaPerSecUSec uint64         `json:"cpu_quota_per_sec_usec,omitempty"`
	CPUQuotaPercent    string         `json:"cpu_quota_percent,omitempty"`
	CPUUsageNS         uint64         `json:"cpu_usage_ns,omitempty"`
	Description        string         `json:"description,omitempty"`
	DropIns            []string       `json:"drop_ins,omitempty"`
	FragmentPath       string         `json:"fragment_path,omitempty"`
	LoadState          string         `json:"load_state"`
	MainPID            uint32         `json:"main_pid,omitempty"`
	MainPIDComm        string         `json:"main_pid_comm,omitempty"`
	MemoryCurrentBytes uint64         `json:"memory_current_bytes,omitempty"`
	MemoryMaxBytes     uint64         `json:"memory_max_bytes,omitempty"`
	MemoryPeakBytes    uint64         `json:"memory_peak_bytes,omitempty"`
	NRestarts          *uint32        `json:"n_restarts,omitempty"`
	Processes          []ProcessEntry `json:"processes,omitempty"`
	SubState           string         `json:"sub_state"`
	TasksCurrent       uint64         `json:"tasks_current,omitempty"`
	TasksMax           uint64         `json:"tasks_max,omitempty"`
	Unit               string         `json:"unit"`
	UnitFilePreset     string         `json:"unit_file_preset,omitempty"`
	UnitFileState      string         `json:"unit_file_state,omitempty"`
}

// cgroupProcessEntries lists the cgroup's processes from cgroupfs (MainPID
// alone when none can be read); Command is readComm, never argv.
func cgroupProcessEntries(cgroupPath string, mainPID uint32) []ProcessEntry {
	var pids []uint32
	procsFile := "/sys/fs/cgroup" + cgroupPath + "/cgroup.procs"
	if data, err := os.ReadFile(procsFile); err == nil {
		for _, s := range strings.Fields(strings.TrimSpace(string(data))) {
			var pid uint32
			if _, err2 := fmt.Sscanf(s, "%d", &pid); err2 == nil && pid > 0 {
				pids = append(pids, pid)
			}
		}
	}
	if len(pids) == 0 && mainPID > 0 {
		pids = []uint32{mainPID}
	}
	entries := make([]ProcessEntry, 0, len(pids))
	for _, pid := range pids {
		entries = append(entries, ProcessEntry{PID: pid, Command: readComm(pid)})
	}
	return entries
}

// cpuQuotaPercentString renders CPUQuotaPerSecUSec (µs of CPU per second;
// 100% = one core = 1_000_000) as the CPUQuota string, minimal decimals:
// 200000 → "20%", 1000 → "0.1%". 0 or unset (maxUint64) → "".
func cpuQuotaPercentString(perSecUSec uint64) string {
	if perSecUSec == 0 || perSecUSec == maxUint64 {
		return ""
	}
	pct := float64(perSecUSec) / 10000.0
	return strconv.FormatFloat(pct, 'f', -1, 64) + "%"
}

// StatusService returns JSON unit status fetched via D-Bus, mirroring
// `systemctl status`. It never returns an error — on D-Bus failure it returns
// a JSON error object so callers always get valid JSON output.
func StatusService(svc string) (string, error) {
	Debugf("systemd: status %s", svc)
	conn, err := openSystemdConn()
	if err != nil {
		// Use a map so encoding/json marshals keys alphabetically ("error" < "unit").
		out, _ := json.Marshal(map[string]string{
			"error": fmt.Sprintf("systemd unavailable: %v", err),
			"unit":  unitName(svc),
		})
		return string(out), nil
	}
	defer conn.Close()

	unit := unitName(svc)

	activeState := getStringProp(conn, unit, "ActiveState")
	subState := getStringProp(conn, unit, "SubState")
	description := getStringProp(conn, unit, "Description")
	loadState := getStringProp(conn, unit, "LoadState")
	fragmentPath := getStringProp(conn, unit, "FragmentPath")
	uFileState := getStringProp(conn, unit, "UnitFileState")
	uFilePreset := getStringProp(conn, unit, "UnitFilePreset")
	dropInPaths := getStringSliceProp(conn, unit, "DropInPaths")
	enterTS := getUint64Prop(conn, unit, "ActiveEnterTimestamp")
	// Per-type-interface properties from here on: getTyped*Prop, never .Unit.
	mainPID := getTypedUint32Prop(conn, unit, "MainPID")
	tasksCurrent := getTypedUint64Prop(conn, unit, "TasksCurrent")
	tasksMax := getTypedUint64Prop(conn, unit, "TasksMax")
	memCurrent := getTypedUint64Prop(conn, unit, "MemoryCurrent")
	memMax := getTypedUint64Prop(conn, unit, "MemoryMax")
	memPeak := getTypedUint64Prop(conn, unit, "MemoryPeak")
	cpuNSec := getTypedUint64Prop(conn, unit, "CPUUsageNSec")
	cpuQuotaPerSec := getTypedUint64Prop(conn, unit, "CPUQuotaPerSecUSec")
	cgroupPath := getTypedStringProp(conn, unit, "ControlGroup")
	nRestarts, nRestartsOK := getTypedUint32PropOK(conn, unit, "NRestarts")

	st := UnitStatusJSON{
		ActiveState:    activeState,
		CGroup:         cgroupPath,
		Description:    description,
		DropIns:        dropInPaths,
		FragmentPath:   fragmentPath,
		LoadState:      loadState,
		SubState:       subState,
		Unit:           unit,
		UnitFilePreset: uFilePreset,
		UnitFileState:  uFileState,
	}

	if enterTS > 0 {
		t := time.Unix(0, int64(enterTS)*int64(time.Microsecond)).Local()
		st.ActiveSince = t.Format(time.RFC3339)
		st.ActiveSinceHuman = t.Format("Mon 2 Jan 2006 15:04:05 MST")
		st.ActiveDuration = humanDuration(time.Since(t))
	}
	if mainPID > 0 {
		st.MainPID = mainPID
		st.MainPIDComm = readComm(mainPID)
	}
	if tasksCurrent > 0 && tasksCurrent != maxUint64 {
		st.TasksCurrent = tasksCurrent
	}
	if tasksMax > 0 && tasksMax != maxUint64 {
		st.TasksMax = tasksMax
	}
	if memCurrent > 0 && memCurrent != maxUint64 {
		st.MemoryCurrentBytes = memCurrent
	}
	if memMax > 0 && memMax != maxUint64 {
		st.MemoryMaxBytes = memMax
	}
	if memPeak > 0 && memPeak != maxUint64 {
		st.MemoryPeakBytes = memPeak
	}
	if cpuNSec > 0 {
		st.CPUUsageNS = cpuNSec
	}
	if cpuQuotaPerSec > 0 && cpuQuotaPerSec != maxUint64 {
		st.CPUQuotaPerSecUSec = cpuQuotaPerSec
		st.CPUQuotaPercent = cpuQuotaPercentString(cpuQuotaPerSec)
	}
	// Emit any answer, including 0; omit only an unreadable property.
	if nRestartsOK {
		st.NRestarts = &nRestarts
	}
	if cgroupPath != "" {
		st.Processes = cgroupProcessEntries(cgroupPath, mainPID)
	}

	Tracef("systemd status %s: %s (%s)", svc, activeState, subState)
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		// "error" < "unit" alphabetically — maintain sorted output in the fallback string.
		return fmt.Sprintf(`{"error":"json marshal failed: %v","unit":%q}`, err, unit), nil
	}
	return string(out), nil
}

// PackageInfoJSON is the VersionService shape: the `rpm -qi` fields, declared
// in JSON-tag alphabetical order.
type PackageInfoJSON struct {
	Architecture string `json:"architecture"`
	BuildDate    string `json:"build_date,omitempty"`
	BuildHost    string `json:"build_host,omitempty"`
	Group        string `json:"group,omitempty"`
	InstallDate  string `json:"install_date,omitempty"`
	License      string `json:"license"`
	Name         string `json:"name"`
	Release      string `json:"release"`
	Signature    string `json:"signature"`
	Size         int    `json:"size"`
	SourceRPM    string `json:"source_rpm"`
	Summary      string `json:"summary"`
	URL          string `json:"url,omitempty"`
	Vendor       string `json:"vendor"`
	Version      string `json:"version"`
}

// parseRPMInfo extracts package metadata from `rpm -qi` output.
// Each line has the form "Field Name<spaces>: value"; unknown lines are
// silently ignored so future rpm versions cannot break the parser.
func parseRPMInfo(output string) PackageInfoJSON {
	var info PackageInfoJSON
	for _, line := range strings.Split(output, "\n") {
		idx := strings.Index(line, ": ")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+2:])
		switch key {
		case "Name":
			info.Name = val
		case "Version":
			info.Version = val
		case "Release":
			info.Release = val
		case "Architecture":
			info.Architecture = val
		case "Install Date":
			info.InstallDate = val
		case "Group":
			info.Group = val
		case "Size":
			if n, err := strconv.Atoi(val); err == nil {
				info.Size = n
			}
		case "License":
			info.License = val
		case "Signature":
			info.Signature = val
		case "Source RPM":
			info.SourceRPM = val
		case "Build Date":
			info.BuildDate = val
		case "Build Host":
			info.BuildHost = val
		case "Vendor":
			info.Vendor = val
		case "URL":
			info.URL = val
		case "Summary":
			info.Summary = val
		}
	}
	return info
}

// dnfInstallTimeout bounds every dnf install (InstallServiceVersion, the scoped
// self-install). They run under syncMu, so a hung mirror or rpmdb lock must not
// freeze CD, the secrets refresh and /sync; generous, since a real install is
// slow. A var only so TestMain can lower it (dnf with no repo route blocks on
// metadata); production never reassigns it.
var dnfInstallTimeout = 10 * time.Minute

// InstallServiceVersion runs `dnf -y install ${svc}-${version}` (the CD
// delivery path) after refreshRepoMetadata and returns dnf's combined output;
// an error means dnf exited non-zero. A failure logs at Debug, never Error: the
// caller owns severity and the next cycle retries (the same holds for
// SetServiceResourceQuota and installSelfVersion). Pinned by
// TestInstallServiceVersion_FailureIsNotLoggedAtErrorLevel.
func InstallServiceVersion(svc, version string) (string, error) {
	pkg := svc + "-" + version
	// Refresh first so a build published since the last cache write is
	// visible; no-op unless an operator configured dnf.makecache_repos.
	refreshRepoMetadata()
	Debugf("dnf: install %s", pkg)
	ctx, cancel := context.WithTimeout(context.Background(), dnfInstallTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "dnf", "-y", "install", pkg).CombinedOutput()
	output := strings.TrimSpace(string(raw))
	if err != nil {
		Debugf("dnf: install %s failed: %v", pkg, err)
		return output, fmt.Errorf("dnf install %s: %w", pkg, err)
	}
	Tracef("dnf: install %s output:\n%s", pkg, output)
	return output, nil
}

// rpmQueryTimeout bounds the read-only rpm queries. rpmInstalledVersions runs
// every cycle per managed service under syncMu, so a contended rpmdb must not
// stall it: a timeout is inconclusive (one skipped check, never a hang).
const rpmQueryTimeout = 10 * time.Second

// rpmEraseTimeout bounds only removeDuplicateRPMPackages' rpm -e: an erase is a
// write transaction (scriptlets, rpmdb commit, maybe queued behind a dnf lock),
// so it gets more than rpmQueryTimeout but far less than dnfInstallTimeout.
const rpmEraseTimeout = 60 * time.Second

// rpmInstalledVersions returns every %{VERSION} the rpm database holds for pkg:
// the ONLY source of truth for what is installed, never .installed_version.
// See ADR-0011.
//   - (nil, false): rpm could not be consulted (no binary, rpmdb error,
//     timeout). Inconclusive: the caller must not decide.
//   - (nil, true): definitively not installed.
//   - (vers, true): installed; several means duplicates, the caller decides.
//
// rpm -q exits 1 both for "not installed" (stderr empty) and for an rpmdb
// failure ("error: ..." on stderr), so only a SILENT exit 1 means absent.
// Inconclusive must never read as missing: that would reinstall and restart a
// healthy service.
func rpmInstalledVersions(pkg string) ([]string, bool) {
	if _, err := exec.LookPath("rpm"); err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpmQueryTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "rpm", "-q", "--queryformat", "%{VERSION}\n", pkg)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		Warningf("cd: rpm -q %s timed out after %s — rpmdb busy/contended; treating as inconclusive", pkg, rpmQueryTimeout)
		return nil, false
	}
	if err != nil {
		if errText := strings.TrimSpace(stderr.String()); errText != "" {
			Warningf("cd: rpm -q %s failed with rpmdb error (treating as inconclusive): %s", pkg, errText)
			return nil, false
		}
		Debugf("cd: rpm -q %s: package not installed", pkg)
		return nil, true
	}

	var versions []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		// Exit 0 with nothing parseable should not happen; refuse to guess.
		Warningf("cd: rpm -q %s exited 0 but returned no version — treating as inconclusive", pkg)
		return nil, false
	}
	return versions, true
}

// systemctlSetPropertyTimeout bounds SetServiceResourceQuota, which runs under
// syncMu (the CD quota step); set-property is normally a near-instant local
// D-Bus call, hence far shorter than dnfInstallTimeout.
const systemctlSetPropertyTimeout = 15 * time.Second

// SetServiceResourceQuota runs `systemctl set-property <unit> props...` (e.g.
// "CPUQuota=20%", "MemoryMax=1073741824"): persisted as a drop-in under
// /etc/systemd/system.control/ AND applied live, so no restart is needed. props
// are separate argv elements (no shell). The unit must already exist (CD
// applies quota after the version install). Returns the combined output; an
// error means a non-zero exit, logged at Debug (the caller owns severity).
func SetServiceResourceQuota(svc string, props []string) (string, error) {
	if len(props) == 0 {
		return "", nil
	}
	unit := unitName(svc)
	args := append([]string{"set-property", unit}, props...)
	Debugf("systemctl set-property %s %s", unit, strings.Join(props, " "))
	ctx, cancel := context.WithTimeout(context.Background(), systemctlSetPropertyTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
	output := strings.TrimSpace(string(raw))
	if err != nil {
		Debugf("systemctl set-property %s failed: %v", unit, err)
		return output, fmt.Errorf("systemctl set-property %s: %w", unit, err)
	}
	Tracef("systemctl set-property %s output:\n%s", unit, output)
	return output, nil
}

// selfPackageName is cystemd's own RPM package name.
const selfPackageName = "cystemd"

// installSelfVersion installs cystemd-<version> via `systemd-run --scope dnf -y
// install`, outside cystemd's cgroup: the %post restart would otherwise SIGKILL
// dnf mid-transaction and leave two RPMs installed. cystemd itself dies in that
// restart (no SIGTERM handler), which is fine: the new binary is starting.
// Without systemd-run it falls back to InstallServiceVersion. A failure logs
// at Debug (the caller owns severity).
func installSelfVersion(version string) (string, error) {
	pkg := selfPackageName + "-" + version

	if _, err := exec.LookPath("systemd-run"); err != nil {
		Warningf("cd: systemd-run not found; self-update runs dnf directly (may leave duplicate RPMs if restart interrupts dnf)")
		return InstallServiceVersion(selfPackageName, version)
	}

	// Here, not above: the fallback refreshes inside InstallServiceVersion.
	refreshRepoMetadata()
	Debugf("dnf: installing %s via systemd-run --scope (dnf survives cystemd restart)", pkg)
	ctx, cancel := context.WithTimeout(context.Background(), dnfInstallTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "systemd-run", "--scope", "dnf", "-y", "install", pkg).CombinedOutput()
	output := strings.TrimSpace(string(raw))
	if err != nil {
		Debugf("dnf: scoped install %s failed: %v", pkg, err)
		return output, fmt.Errorf("scoped dnf install %s: %w", pkg, err)
	}
	Tracef("dnf: scoped install %s output:\n%s", pkg, output)
	return output, nil
}

// removeDuplicateRPMPackages `rpm -e --nodeps`es every installed cystemd RPM
// whose Version matches neither Version() nor Commit(): the leftovers of a
// self-update whose dnf died mid-transaction. Commit() keeps a per-commit CI
// RPM (versioned by the full hash) of the running build from being removed
// every cycle. --nodeps is safe (nothing depends on cystemd) and the running
// binary is already in memory. Versions come from --queryformat, never NEVRA
// splitting (epochs, hyphenated tags). Fail-soft: query errors skip; a package
// a concurrent dnf already removed logs at Info, other failures at Warning.
// Skipped for dev/unknown builds. See DOCS/CLAUDE.md § Self-update.
func removeDuplicateRPMPackages() {
	current := Version()
	if isDevVersion(current) {
		return // cannot determine running version — skip subprocess entirely
	}
	currentCommit := Commit()

	// One structured query: Version to compare, NEVRA to erase.
	queryCtx, queryCancel := context.WithTimeout(context.Background(), rpmQueryTimeout)
	raw, err := exec.CommandContext(queryCtx, "rpm", "-q", "--queryformat", "%{VERSION} %{NEVRA}\n", selfPackageName).Output()
	queryCancel()
	if err != nil {
		return // package not installed, rpm error, or timeout — not our concern here
	}

	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) <= 1 {
		return // 0 or 1 packages — clean
	}

	var toRemove []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Each line is "VERSION NEVRA", e.g. "v5.2.7 cystemd-v5.2.7-1.el9.x86_64".
		parts := strings.SplitN(line, " ", 2)
		if len(parts) < 2 {
			continue
		}
		rpmVersion, nevra := parts[0], parts[1]

		if rpmVersion == current || rpmVersion == currentCommit {
			Debugf("cd: keeping RPM %s (matches running version %s / commit %s)", nevra, current, currentCommit)
			continue
		}

		Infof("cd: marking old RPM %s for removal (version=%s, running=%s/%s)", nevra, rpmVersion, current, currentCommit)
		toRemove = append(toRemove, nevra)
	}

	if len(toRemove) == 0 {
		return
	}

	// Re-query just before rpm -e to shrink the race with a concurrent dnf
	// (installSelfVersion's scope) that may have removed a candidate. If the
	// re-query fails, keep the list: logRPMRemovalFailure still classifies.
	requeryCtx, requeryCancel := context.WithTimeout(context.Background(), rpmQueryTimeout)
	stillInstalled, requeryErr := exec.CommandContext(requeryCtx, "rpm", "-q", "--queryformat", "%{NEVRA}\n", selfPackageName).Output()
	requeryCancel()
	if requeryErr == nil {
		present := make(map[string]bool)
		for _, l := range strings.Split(strings.TrimSpace(string(stillInstalled)), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				present[l] = true
			}
		}
		filtered := make([]string, 0, len(toRemove))
		for _, nevra := range toRemove {
			if present[nevra] {
				filtered = append(filtered, nevra)
			} else {
				Debugf("cd: skipping %s — no longer present on re-query (already removed)", nevra)
			}
		}
		toRemove = filtered
	}

	if len(toRemove) == 0 {
		Debugf("cd: all candidate duplicate RPM package(s) already removed by re-query; skipping rpm -e")
		return
	}

	Warningf("cd: removing %d duplicate cystemd RPM package(s): %s", len(toRemove), strings.Join(toRemove, ", "))
	args := append([]string{"-e", "--nodeps"}, toRemove...)
	removeCtx, removeCancel := context.WithTimeout(context.Background(), rpmEraseTimeout)
	defer removeCancel()
	if out, err := exec.CommandContext(removeCtx, "rpm", args...).CombinedOutput(); err != nil {
		logRPMRemovalFailure(err, strings.TrimSpace(string(out)), toRemove)
	}
}

// rpmRemovalAlreadyGone reports whether a failed rpm -e's output says the
// package was already absent ("is not installed") rather than a real failure.
func rpmRemovalAlreadyGone(combinedOutput string) bool {
	return strings.Contains(combinedOutput, "is not installed")
}

// logRPMRemovalFailure logs a failed rpm -e: already removed by the concurrent
// install transaction → Info (the end state is reached); anything else → Warning.
func logRPMRemovalFailure(err error, combinedOutput string, toRemove []string) {
	if rpmRemovalAlreadyGone(combinedOutput) {
		Infof("cd: old RPM package(s) already removed by the install transaction: %s", strings.Join(toRemove, ", "))
		return
	}
	Warningf("cd: failed to remove old RPM packages: %v (%s)", err, combinedOutput)
}

// VersionService runs `rpm -qi <svc>` and returns its fields as JSON. Bounded by
// rpmQueryTimeout: /version is open, and an unbounded call would pile up
// goroutines and rpm children behind a contended rpmdb lock (e.g. a dnf install).
func VersionService(svc string) (string, error) {
	Debugf("rpm: query %s", svc)

	ctx, cancel := context.WithTimeout(context.Background(), rpmQueryTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "rpm", "-qi", svc).Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("rpm -qi %s: timed out after %s", svc, rpmQueryTimeout)
		}
		if exitErr, ok := err.(*exec.ExitError); ok {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if msg == "" {
				msg = strings.TrimSpace(string(raw))
			}
			return "", fmt.Errorf("rpm -qi %s: %s", svc, msg)
		}
		return "", fmt.Errorf("rpm -qi %s: %w", svc, err)
	}

	Tracef("rpm: %s output:\n%s", svc, string(raw))
	info := parseRPMInfo(string(raw))

	out, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", fmt.Errorf("json marshal failed: %w", err)
	}
	return string(out), nil
}
