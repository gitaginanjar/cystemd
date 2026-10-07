package service

// actions_test.go: pure helpers and JSON shapes run anywhere (StatusService
// answers JSON even without systemd); job-running actions are integration
// tests gated by hasSystemd(), dnf tests by hasDNF()/hasWorkingDNF().

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-systemd/v22/dbus"
)

// ─── environment probes ────────────────────────────────────────────────────

// hasSystemd reports whether a systemd D-Bus socket is reachable on this host.
func hasSystemd() bool {
	conn, err := dbus.NewSystemdConnectionContext(context.Background())
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// hasRPM reports whether the rpm binary is available on this host.
func hasRPM() bool {
	_, err := exec.LookPath("rpm")
	return err == nil
}

// hasDNF reports whether the dnf binary is available on this host. Presence
// only — use hasWorkingDNF when the test needs dnf to actually resolve.
func hasDNF() bool {
	_, err := exec.LookPath("dnf")
	return err == nil
}

// dnfProbeTimeout bounds the one-time hasWorkingDNF probe; a responsive host
// answers in ~2s, so anything slower cannot reach its repos.
const dnfProbeTimeout = 20 * time.Second

// dnfProbePackage cannot exist in any repo, so the probe never installs anything.
const dnfProbePackage = "cystemd-probe-nonexistent-xyz-99999"

var (
	workingDNFOnce sync.Once
	workingDNF     bool
)

// hasWorkingDNF reports whether dnf can RESOLVE a package here (a verdict within
// dnfProbeTimeout; the exit code is irrelevant), not merely whether it exists.
// Cached per test binary. See DOCS/CLAUDE.md § Testing.
func hasWorkingDNF() bool {
	workingDNFOnce.Do(func() {
		if !hasDNF() {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), dnfProbeTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, "dnf", "-y", "install", dnfProbePackage, "--setopt=tsflags=test")
		_ = cmd.Run() // a "no match" failure is a healthy answer
		workingDNF = ctx.Err() == nil
	})
	return workingDNF
}

// TestMain lowers dnfInstallTimeout for the suite: dnf tests assert a FAILED
// install, so failing fast keeps what they check without the 10-minute stall.
func TestMain(m *testing.M) {
	if hasWorkingDNF() {
		// Reachable repos: ample for a real resolve or install.
		dnfInstallTimeout = 60 * time.Second
	} else {
		// Unreachable repos: every call times out, so make that cheap.
		dnfInstallTimeout = 2 * time.Second
	}
	os.Exit(m.Run())
}

// ─── unitName ──────────────────────────────────────────────────────────────

func TestUnitName_AppendsSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mysvc", "mysvc.service"},
		{"mysvc.service", "mysvc.service"},
		{"mysvc.socket", "mysvc.socket"},
		{"mysvc.timer", "mysvc.timer"},
		{"multi.word.svc", "multi.word.svc"}, // already has dot → unchanged
	}
	for _, tc := range cases {
		got := unitName(tc.in)
		if got != tc.want {
			t.Errorf("unitName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ─── humanDuration ─────────────────────────────────────────────────────────

func TestHumanDuration_Seconds(t *testing.T) {
	if got := humanDuration(45 * time.Second); got != "45s ago" {
		t.Errorf("humanDuration(45s) = %q, want %q", got, "45s ago")
	}
}

func TestHumanDuration_Minutes(t *testing.T) {
	got := humanDuration(3*time.Minute + 20*time.Second)
	if !strings.Contains(got, "3min") {
		t.Errorf("humanDuration(3m20s) = %q, expected 3min", got)
	}
	if !strings.Contains(got, "20s") {
		t.Errorf("humanDuration(3m20s) = %q, expected 20s", got)
	}
}

func TestHumanDuration_Hours(t *testing.T) {
	got := humanDuration(1*time.Hour + 31*time.Minute)
	if !strings.HasPrefix(got, "1h") {
		t.Errorf("humanDuration(1h31m) = %q, want 1h prefix", got)
	}
	if !strings.Contains(got, "31min") {
		t.Errorf("humanDuration(1h31m) = %q, want 31min", got)
	}
}

func TestHumanDuration_AlwaysEndsWithAgo(t *testing.T) {
	for _, d := range []time.Duration{5 * time.Second, 3 * time.Minute, 2 * time.Hour} {
		got := humanDuration(d)
		if !strings.HasSuffix(got, "ago") {
			t.Errorf("humanDuration(%v) = %q, expected 'ago' suffix", d, got)
		}
	}
}

func TestHumanDuration_ZeroSeconds(t *testing.T) {
	if got := humanDuration(0); got != "0s ago" {
		t.Errorf("humanDuration(0) = %q, want %q", got, "0s ago")
	}
}

// ─── UnitStatusJSON struct ─────────────────────────────────────────────────

func TestUnitStatusJSON_MarshalRoundtrip(t *testing.T) {
	original := UnitStatusJSON{
		Unit:               "myapp.service",
		Description:        "My Application",
		LoadState:          "loaded",
		ActiveState:        "active",
		SubState:           "running",
		ActiveSince:        "2026-04-21T08:00:00Z",
		ActiveSinceHuman:   "Tue 21 Apr 2026 08:00:00 WIB",
		ActiveDuration:     "1h 0min ago",
		MainPID:            12345,
		MainPIDComm:        "myapp",
		TasksCurrent:       4,
		TasksMax:           1000,
		MemoryCurrentBytes: 100 * 1024 * 1024,
		NRestarts:          uint32Ptr(3),
		CGroup:             "/system.slice/myapp.service",
		Processes:          []ProcessEntry{{PID: 12345, Command: "/opt/myapp/bin/myapp"}},
	}

	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var decoded UnitStatusJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if decoded.Unit != original.Unit {
		t.Errorf("Unit: got %q, want %q", decoded.Unit, original.Unit)
	}
	if decoded.MainPID != original.MainPID {
		t.Errorf("MainPID: got %d, want %d", decoded.MainPID, original.MainPID)
	}
	if len(decoded.Processes) != 1 || decoded.Processes[0].PID != 12345 {
		t.Errorf("Processes: got %v, want [{12345 ...}]", decoded.Processes)
	}
	if decoded.MemoryCurrentBytes != original.MemoryCurrentBytes {
		t.Errorf("MemoryCurrentBytes: got %d, want %d", decoded.MemoryCurrentBytes, original.MemoryCurrentBytes)
	}
	if decoded.NRestarts == nil {
		t.Fatalf("NRestarts: got nil, want %d", *original.NRestarts)
	}
	if *decoded.NRestarts != *original.NRestarts {
		t.Errorf("NRestarts: got %d, want %d", *decoded.NRestarts, *original.NRestarts)
	}
}

func TestUnitStatusJSON_OmitsZeroFields(t *testing.T) {
	st := UnitStatusJSON{
		Unit:        "bare.service",
		ActiveState: "inactive",
		SubState:    "dead",
	}
	data, _ := json.Marshal(st)
	s := string(data)
	for _, absent := range []string{
		`"main_pid"`, `"tasks_current"`, `"memory_current_bytes"`,
		`"cpu_usage_ns"`, `"drop_ins"`, `"processes"`,
	} {
		if strings.Contains(s, absent) {
			t.Errorf("expected %s to be omitted for zero value; got: %s", absent, s)
		}
	}
	// n_restarts is omitted only for a failed read (nil), never for zero; the
	// other half is TestUnitStatusJSON_ZeroNRestartsIsEmitted.
	if st.NRestarts != nil {
		t.Fatalf("test setup: NRestarts should be nil here")
	}
	if strings.Contains(s, `"n_restarts"`) {
		t.Errorf("expected n_restarts to be omitted for an unread (nil) counter; got: %s", s)
	}
}

// TestUnitStatusJSON_ZeroNRestartsIsEmitted pins that a measured NRestarts=0
// (never auto-restarted) is emitted, not dropped by omitempty.
// See DOCS/MEMORY.md § `n_restarts` now reports a measured zero.
func TestUnitStatusJSON_ZeroNRestartsIsEmitted(t *testing.T) {
	st := UnitStatusJSON{
		Unit:        "growin_fixordermanagementservice.service",
		ActiveState: "inactive",
		SubState:    "dead",
		NRestarts:   uint32Ptr(0),
	}
	data, _ := json.Marshal(st)
	s := string(data)
	if !strings.Contains(s, `"n_restarts":0`) {
		t.Errorf("expected a measured zero to be emitted as \"n_restarts\":0; got: %s", s)
	}

	var decoded UnitStatusJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if decoded.NRestarts == nil {
		t.Fatalf("a measured zero must survive the roundtrip as a non-nil pointer")
	}
	if *decoded.NRestarts != 0 {
		t.Errorf("NRestarts: got %d, want 0", *decoded.NRestarts)
	}
}

func TestUnitStatusJSON_JSONKeyNames(t *testing.T) {
	st := UnitStatusJSON{
		Unit:               "svc.service",
		ActiveState:        "active",
		SubState:           "running",
		MainPID:            42,
		MemoryCurrentBytes: 1024,
		CPUUsageNS:         1000,
		NRestarts:          uint32Ptr(2),
	}
	data, _ := json.Marshal(st)
	s := string(data)
	for _, key := range []string{`"unit"`, `"active_state"`, `"sub_state"`, `"main_pid"`, `"memory_current_bytes"`, `"cpu_usage_ns"`, `"n_restarts"`} {
		if !strings.Contains(s, key) {
			t.Errorf("expected JSON key %s in output; got: %s", key, s)
		}
	}
}

// ─── PackageInfoJSON struct ────────────────────────────────────────────────

func TestPackageInfoJSON_MarshalRoundtrip(t *testing.T) {
	original := PackageInfoJSON{
		Name:         "myproject_myapplication",
		Version:      "abc123d",
		Release:      "20.el9",
		Architecture: "x86_64",
		InstallDate:  "Thu 08 Jan 2026 04:51:01 PM WIB",
		Size:         12306478,
		License:      "Proprietary",
		Signature:    "(none)",
		SourceRPM:    "myproject_myapplication-abc123d-20.el9.nosrc.rpm",
		Vendor:       "myproject",
		Summary:      "myapplication",
	}

	data, err := json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var decoded PackageInfoJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if decoded.Name != original.Name {
		t.Errorf("Name: got %q, want %q", decoded.Name, original.Name)
	}
	if decoded.Size != original.Size {
		t.Errorf("Size: got %d, want %d", decoded.Size, original.Size)
	}
	if decoded.Vendor != original.Vendor {
		t.Errorf("Vendor: got %q, want %q", decoded.Vendor, original.Vendor)
	}
	if decoded.SourceRPM != original.SourceRPM {
		t.Errorf("SourceRPM: got %q, want %q", decoded.SourceRPM, original.SourceRPM)
	}
}

func TestPackageInfoJSON_OmitsEmptyInstallDate(t *testing.T) {
	pkg := PackageInfoJSON{Name: "mypkg", Version: "1.0", Size: 1000}
	data, _ := json.Marshal(pkg)
	if strings.Contains(string(data), `"install_date"`) {
		t.Errorf("install_date should be omitted when empty; got: %s", string(data))
	}
}

func TestPackageInfoJSON_JSONKeyNames(t *testing.T) {
	pkg := PackageInfoJSON{
		Name: "p", Version: "1", Release: "1.el9",
		Architecture: "x86_64", Size: 100,
		License: "MIT", Signature: "(none)",
		SourceRPM: "p-1-1.el9.nosrc.rpm",
		Vendor:    "acme", Summary: "test pkg",
	}
	data, _ := json.Marshal(pkg)
	s := string(data)
	for _, key := range []string{
		`"name"`, `"version"`, `"release"`, `"architecture"`,
		`"size"`, `"license"`, `"signature"`, `"source_rpm"`, `"vendor"`, `"summary"`,
	} {
		if !strings.Contains(s, key) {
			t.Errorf("expected JSON key %s; got: %s", key, s)
		}
	}
}

// ─── StatusService — always returns valid JSON ──────────────────────────────

func TestStatusService_LogsAtDebug(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	_, _ = StatusService("cystemd_test_nonexistent")
	if !strings.Contains(buf.String(), "systemd: status cystemd_test_nonexistent") {
		t.Errorf("expected debug log for StatusService; got:\n%s", buf.String())
	}
}

func TestStatusService_NeverReturnsError(t *testing.T) {
	_, err := StatusService("nonexistent_service_xyz_12345")
	if err != nil {
		t.Errorf("StatusService must never return an error; got: %v", err)
	}
}

func TestStatusService_ReturnsValidJSON(t *testing.T) {
	out, _ := StatusService("cystemd_test_nonexistent")
	if !json.Valid([]byte(out)) {
		t.Errorf("StatusService output is not valid JSON; got:\n%s", out)
	}
}

func TestStatusService_JSONAlwaysHasUnitKey(t *testing.T) {
	out, _ := StatusService("cystemd_test_svc")
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("cannot unmarshal StatusService JSON: %v\noutput: %s", err, out)
	}
	if _, ok := m["unit"]; !ok {
		t.Errorf("StatusService JSON missing 'unit' key; got: %s", out)
	}
}

func TestStatusService_JSONUnitContainsServiceName(t *testing.T) {
	out, _ := StatusService("cystemd_test_svc")
	// Whether systemd is available or not, the service name appears in "unit".
	if !strings.Contains(out, "cystemd_test_svc") {
		t.Errorf("StatusService JSON should contain service name; got:\n%s", out)
	}
}

func TestStatusService_NoSystemd_JSONHasErrorKey(t *testing.T) {
	if hasSystemd() {
		t.Skip("systemd is available; this test targets no-systemd environments")
	}
	out, err := StatusService("anything")
	if err != nil {
		t.Fatalf("StatusService must not return a Go error; got: %v", err)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out)
	}
	if _, ok := m["error"]; !ok {
		t.Errorf("no-systemd JSON response should contain 'error' key; got: %s", out)
	}
}

func TestStatusService_WithSystemd_ActiveStatePresent(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	out, err := StatusService("systemd-journald")
	if err != nil {
		t.Fatalf("StatusService must not return error: %v", err)
	}
	var st UnitStatusJSON
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("cannot unmarshal StatusService JSON: %v\noutput: %s", err, out)
	}
	if st.ActiveState == "" {
		t.Error("active_state should be non-empty for systemd-journald")
	}
	if st.Unit == "" {
		t.Error("unit field should be non-empty")
	}
}

// ─── VersionService ────────────────────────────────────────────────────────

func TestVersionService_LogsAtDebug(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	_, _ = VersionService("cystemd_test_nonexistent")
	if !strings.Contains(buf.String(), "rpm: query cystemd_test_nonexistent") {
		t.Errorf("expected debug log for VersionService; got:\n%s", buf.String())
	}
}

func TestVersionService_ReturnsErrorWhenRPMDBAbsent(t *testing.T) {
	if hasRPM() {
		t.Skip("RPM database present on this host; skipping absence test")
	}
	_, err := VersionService("some_package")
	if err == nil {
		t.Error("expected error when RPM database is not available")
	}
}

func TestVersionService_ReturnsErrorForUnknownPackage(t *testing.T) {
	if !hasRPM() {
		t.Skip("RPM database not available; skipping RPM query test")
	}
	_, err := VersionService("nonexistent_package_xyz_99999")
	if err == nil {
		t.Error("expected error for unknown RPM package")
	}
}

func TestVersionService_ReturnsValidJSONOnSuccess(t *testing.T) {
	if !hasRPM() {
		t.Skip("RPM database not available")
	}
	out, err := VersionService("rpm")
	if err != nil {
		t.Skipf("'rpm' package not found in database: %v", err)
	}
	if !json.Valid([]byte(out)) {
		t.Errorf("VersionService output is not valid JSON; got:\n%s", out)
	}
}

func TestVersionService_JSONHasRequiredFields(t *testing.T) {
	if !hasRPM() {
		t.Skip("RPM database not available")
	}
	out, err := VersionService("rpm")
	if err != nil {
		t.Skipf("'rpm' package not found: %v", err)
	}
	var pkg PackageInfoJSON
	if err := json.Unmarshal([]byte(out), &pkg); err != nil {
		t.Fatalf("cannot unmarshal VersionService JSON: %v\noutput: %s", err, out)
	}
	if pkg.Name == "" {
		t.Error("JSON 'name' field is empty")
	}
	if pkg.Version == "" {
		t.Error("JSON 'version' field is empty")
	}
	if pkg.Architecture == "" {
		t.Error("JSON 'architecture' field is empty")
	}
	if pkg.Size == 0 {
		t.Error("JSON 'size' field is zero")
	}
}

// ─── parseRPMInfo ──────────────────────────────────────────────────────────

func TestParseRPMInfo_AllFields(t *testing.T) {
	// Mirrors real `rpm -qi cystemd` output from HOST005.
	input := `Name        : cystemd
Version     : v4.6.1
Release     : 1.el9
Architecture: x86_64
Install Date: Tue 05 May 2026 01:52:29 PM WIB
Group       : Unspecified
Size        : 15285821
License     : Proprietary
Signature   : (none)
Source RPM  : cystemd-v4.6.1-1.el9.nosrc.rpm
Build Date  : Tue 05 May 2026 01:39:48 PM WIB
Build Host  : vm-development-runner-0.asia-southeast2-b.c.cicd-dev-0108.internal
Packager    : MYCO - My Company DevOps <devops@mycompany.com>
Vendor      : cystemd
URL         : https://bitbucket.org/mycompany/cystemd
Summary     : api
`
	info := parseRPMInfo(input)

	cases := []struct{ field, got, want string }{
		{"Name", info.Name, "cystemd"},
		{"Version", info.Version, "v4.6.1"},
		{"Release", info.Release, "1.el9"},
		{"Architecture", info.Architecture, "x86_64"},
		{"InstallDate", info.InstallDate, "Tue 05 May 2026 01:52:29 PM WIB"},
		{"Group", info.Group, "Unspecified"},
		{"License", info.License, "Proprietary"},
		{"Signature", info.Signature, "(none)"},
		{"SourceRPM", info.SourceRPM, "cystemd-v4.6.1-1.el9.nosrc.rpm"},
		{"BuildDate", info.BuildDate, "Tue 05 May 2026 01:39:48 PM WIB"},
		{"BuildHost", info.BuildHost, "vm-development-runner-0.asia-southeast2-b.c.cicd-dev-0108.internal"},
		{"Vendor", info.Vendor, "cystemd"},
		{"URL", info.URL, "https://bitbucket.org/mycompany/cystemd"},
		{"Summary", info.Summary, "api"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: got %q, want %q", c.field, c.got, c.want)
		}
	}
	if info.Size != 15285821 {
		t.Errorf("Size: got %d, want 15285821", info.Size)
	}
}

func TestParseRPMInfo_EmptyInput(t *testing.T) {
	info := parseRPMInfo("")
	if info.Name != "" || info.Version != "" || info.Size != 0 {
		t.Errorf("empty input should yield zero-value struct, got: %+v", info)
	}
}

func TestParseRPMInfo_UnknownLinesIgnored(t *testing.T) {
	info := parseRPMInfo("Name        : mypkg\nXXX_Unknown : value\nVersion     : 1.0\n")
	if info.Name != "mypkg" {
		t.Errorf("Name: got %q, want %q", info.Name, "mypkg")
	}
	if info.Version != "1.0" {
		t.Errorf("Version: got %q, want %q", info.Version, "1.0")
	}
}

// ─── Integration: systemd-dependent actions ────────────────────────────────

func TestRestartService_ReturnsErrorForNonexistentService(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	_, err := RestartService("nonexistent_service_xyz_12345")
	if err == nil {
		t.Error("expected error when restarting a nonexistent service")
	}
}

func TestStartService_ReturnsErrorForNonexistentService(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	_, err := StartService("nonexistent_service_xyz_12345")
	if err == nil {
		t.Error("expected error when starting a nonexistent service")
	}
}

func TestStopService_ReturnsErrorForNonexistentService(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	_, err := StopService("nonexistent_service_xyz_12345")
	if err == nil {
		t.Error("expected error when stopping a nonexistent service")
	}
}

func TestEnableService_ReturnsErrorForNonexistentService(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	_, err := EnableService("nonexistent_service_xyz_12345")
	if err == nil {
		t.Error("expected error when enabling a nonexistent service")
	}
}

func TestDisableService_ReturnsErrorForNonexistentService(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	_, err := DisableService("nonexistent_service_xyz_12345")
	if err == nil {
		t.Error("expected error when disabling a nonexistent service")
	}
}

// ─── SetServiceResourceQuota ───────────────────────────────────────────────

// Empty props return before any systemctl call, so this runs without systemd.
func TestSetServiceResourceQuota_NoProps_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	out, err := SetServiceResourceQuota("anything", nil)
	if err != nil || out != "" {
		t.Errorf("SetServiceResourceQuota(no props) = %q, %v; want \"\", nil", out, err)
	}
	if msgs := strings.TrimSpace(decodeLogMsgs(t, buf)); msgs != "" {
		t.Errorf("expected no logs for empty props; got:\n%s", msgs)
	}
}

func TestSetServiceResourceQuota_ReturnsErrorForNonexistentService(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	out, err := SetServiceResourceQuota("nonexistent_service_xyz_12345", []string{"CPUQuota=20%"})
	if err == nil {
		t.Errorf("expected error when setting properties on a nonexistent service; output: %s", out)
	}
}

// TestSetServiceResourceQuota_AppliesToTransientUnit pins set-property end to
// end on a throwaway transient unit: CPUQuota=20% and MemoryMax=1073741824
// (200m/1Gi) are live and persisted under /etc/systemd/system.control/.
// Cleanup reverts only this run's pid-suffixed unit.
func TestSetServiceResourceQuota_AppliesToTransientUnit(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not available on this host")
	}

	svc := fmt.Sprintf("cystemd_test_quota_%d", os.Getpid())
	unit := svc + ".service"

	// Needs manager privileges: an environment limit, so skip, don't fail.
	if out, err := exec.Command("systemd-run", "--unit="+svc, "--quiet", "/bin/sleep", "60").CombinedOutput(); err != nil {
		t.Skipf("cannot launch transient unit (insufficient privileges?): %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "revert", unit).Run()
		_ = exec.Command("systemctl", "stop", unit).Run()
		_ = exec.Command("systemctl", "reset-failed", unit).Run()
		_ = os.RemoveAll("/etc/systemd/system.control/" + unit + ".d")
	})

	if out, err := SetServiceResourceQuota(svc, []string{"CPUQuota=20%", "MemoryMax=1073741824"}); err != nil {
		t.Fatalf("SetServiceResourceQuota: %v\noutput: %s", err, out)
	}

	// Live: CPUQuota=20% reads back as 200ms per second, MemoryMax in bytes.
	show, err := exec.Command("systemctl", "show", unit,
		"-p", "CPUQuotaPerSecUSec", "-p", "MemoryMax").CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl show: %v\n%s", err, show)
	}
	got := string(show)
	if !strings.Contains(got, "CPUQuotaPerSecUSec=200ms") {
		t.Errorf("CPUQuota not applied live; systemctl show:\n%s", got)
	}
	if !strings.Contains(got, "MemoryMax=1073741824") {
		t.Errorf("MemoryMax not applied live; systemctl show:\n%s", got)
	}

	// Persisted: set-property (no --runtime) wrote a drop-in.
	dropInDir := "/etc/systemd/system.control/" + unit + ".d"
	if entries, err := os.ReadDir(dropInDir); err != nil || len(entries) == 0 {
		t.Errorf("expected persistent drop-in(s) in %s; err=%v entries=%d", dropInDir, err, len(entries))
	}
}

// ─── awaitJobResult — bounded job-result receive ───────────────────────────

func TestAwaitJobResult_Done_Succeeds(t *testing.T) {
	ch := make(chan string, 1)
	ch <- "done"
	err := awaitJobResult(ch, "start", "svc", "", time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAwaitJobResult_NotDone_ReturnsError(t *testing.T) {
	ch := make(chan string, 1)
	ch <- "failed"
	if err := awaitJobResult(ch, "start", "svc", "", time.Second); err == nil {
		t.Fatal("expected error for a non-done job result")
	}
}

// TestAwaitJobResult_Timeout_DoesNotHang pins that a job that never answers
// yields a prompt timeout error instead of blocking the request forever.
func TestAwaitJobResult_Timeout_DoesNotHang(t *testing.T) {
	ch := make(chan string) // never receives
	start := time.Now()
	err := awaitJobResult(ch, "start", "svc", "", 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error should mention timeout; got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("awaitJobResult should return promptly on timeout; took %s", elapsed)
	}
}

// TestKillDrainTimeout_Killed_UsesShortPostKillWindow pins postKillDrainTimeout
// after an actual SIGKILL.
func TestKillDrainTimeout_Killed_UsesShortPostKillWindow(t *testing.T) {
	timeout, note := killDrainTimeout(true)
	if timeout != postKillDrainTimeout {
		t.Errorf("killDrainTimeout(true) timeout = %s, want %s (postKillDrainTimeout)", timeout, postKillDrainTimeout)
	}
	if !strings.Contains(note, "post-watchdog") {
		t.Errorf("killDrainTimeout(true) note = %q, want mention of post-watchdog", note)
	}
}

// TestKillDrainTimeout_NotKilled_UsesFullJobTimeout pins systemdJobTimeout when
// no kill was sent, so a slow-but-healthy restart gets no spurious timeout and
// no false FAILED audit line.
func TestKillDrainTimeout_NotKilled_UsesFullJobTimeout(t *testing.T) {
	timeout, note := killDrainTimeout(false)
	if timeout != systemdJobTimeout {
		t.Errorf("killDrainTimeout(false) timeout = %s, want %s (systemdJobTimeout)", timeout, systemdJobTimeout)
	}
	if strings.Contains(note, "post-watchdog") {
		t.Errorf("killDrainTimeout(false) note = %q, must NOT claim post-watchdog when no kill was sent", note)
	}
}

// ─── runSystemdJob: no-systemd path ────────────────────────────────────────

func TestRunSystemdJob_NoSystemd_ReturnsError(t *testing.T) {
	if hasSystemd() {
		t.Skip("systemd is reachable; this test targets no-systemd environments")
	}
	_, err := runSystemdJob("start", "anything")
	if err == nil {
		t.Error("expected error when systemd D-Bus is not reachable")
	}
	if !strings.Contains(err.Error(), "systemd") {
		t.Errorf("error should mention systemd; got: %v", err)
	}
}

func TestRunSystemdWithKillWatchdog_NoSystemd_ReturnsError(t *testing.T) {
	if hasSystemd() {
		t.Skip("systemd is reachable; this test targets no-systemd environments")
	}
	_, err := runSystemdWithKillWatchdog("stop", "anything")
	if err == nil {
		t.Error("expected error when systemd D-Bus is not reachable")
	}
	if !strings.Contains(err.Error(), "systemd") {
		t.Errorf("error should mention systemd; got: %v", err)
	}
}

// TestRunSystemdJob_RejectsNonStartAction pins that runSystemdJob handles only
// "start", with an "unknown action" error pointing stop/restart at
// runSystemdWithKillWatchdog.
func TestRunSystemdJob_RejectsNonStartAction(t *testing.T) {
	if !hasSystemd() {
		// Without systemd openSystemdConn fails before the switch.
		t.Skip("systemd not available; this test exercises the post-connect switch")
	}
	for _, action := range []string{"stop", "restart", "reload", ""} {
		t.Run(action, func(t *testing.T) {
			_, err := runSystemdJob(action, "anything")
			if err == nil {
				t.Errorf("runSystemdJob(%q) expected error, got nil", action)
			}
			if !strings.Contains(err.Error(), "unknown action") {
				t.Errorf("runSystemdJob(%q) error should say 'unknown action'; got: %v", action, err)
			}
		})
	}
}

// TestRunSystemdWithKillWatchdog_RejectsNonStopRestart pins the symmetric guard:
// the watchdog variant accepts only stop, restart and try-restart.
func TestRunSystemdWithKillWatchdog_RejectsNonStopRestart(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available; this test exercises the post-connect switch")
	}
	for _, action := range []string{"start", "reload", ""} {
		t.Run(action, func(t *testing.T) {
			_, err := runSystemdWithKillWatchdog(action, "anything")
			if err == nil {
				t.Errorf("runSystemdWithKillWatchdog(%q) expected error, got nil", action)
			}
			if !strings.Contains(err.Error(), "unknown watchdog action") {
				t.Errorf("runSystemdWithKillWatchdog(%q) error should say 'unknown watchdog action'; got: %v", action, err)
			}
		})
	}
}

// ─── /proc helpers ─────────────────────────────────────────────────────────

func TestReadComm_ReturnsProcessName(t *testing.T) {
	got := readComm(uint32(os.Getpid()))
	if got == "" {
		t.Errorf("readComm(self) returned empty string for current pid")
	}
	if strings.Contains(got, "\n") {
		t.Errorf("readComm should strip newline; got %q", got)
	}
}

func TestReadComm_NonexistentPID_ReturnsEmpty(t *testing.T) {
	// PID 0 has no /proc/0/comm entry on Linux.
	if got := readComm(0); got != "" {
		t.Errorf("readComm(0) = %q, want empty for missing /proc entry", got)
	}
}

func TestReadCmdline_ReturnsCommandLine(t *testing.T) {
	got := readCmdline(uint32(os.Getpid()))
	if got == "" {
		t.Errorf("readCmdline(self) returned empty string for current pid")
	}
}

func TestReadCmdline_NonexistentPID_ReturnsEmpty(t *testing.T) {
	// No /proc/0/cmdline, and the readComm fallback is missing too.
	if got := readCmdline(0); got != "" {
		t.Errorf("readCmdline(0) = %q, want empty when /proc entries absent", got)
	}
}

// ─── InstallServiceVersion ─────────────────────────────────────────────────

func TestInstallServiceVersion_LogsPackageAtDebug(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	_, _ = InstallServiceVersion("test_svc", "1.0.0")
	if !strings.Contains(buf.String(), "dnf: install test_svc-1.0.0") {
		t.Errorf("expected 'dnf: install test_svc-1.0.0' debug log; got:\n%s", buf.String())
	}
}

func TestInstallServiceVersion_PackageNameIsServiceDashVersion(t *testing.T) {
	// The constructed ${svc}-${version} name reaches the debug log.
	buf := captureLogs(t, LevelDebug)
	_, _ = InstallServiceVersion("myproject_myapplication", "abc123def456abc123def456abc123def456abc123")
	want := "dnf: install myproject_myapplication-abc123def456abc123def456abc123def456abc123"
	if !strings.Contains(buf.String(), want) {
		t.Errorf("expected %q in debug log; got:\n%s", want, buf.String())
	}
}

func TestInstallServiceVersion_NoDNF_ReturnsError(t *testing.T) {
	if hasDNF() {
		t.Skip("dnf is available on this host; skipping no-dnf error path test")
	}
	_, err := InstallServiceVersion("some_svc", "1.0.0")
	if err == nil {
		t.Error("expected error when dnf binary is not installed")
	}
	if !strings.Contains(err.Error(), "dnf install") {
		t.Errorf("error should mention 'dnf install'; got: %v", err)
	}
}

func TestInstallServiceVersion_UnknownPackage_ReturnsError(t *testing.T) {
	if !hasWorkingDNF() {
		t.Skip("dnf cannot reach its repos on this host; the error would be a timeout, not a resolution failure")
	}
	_, err := InstallServiceVersion("nonexistent_cystemd_pkg_xyz_99999", "0.0.0.0.0")
	if err == nil {
		t.Error("expected error for a package that does not exist in any configured repo")
	}
}

func TestInstallServiceVersion_UnknownPackage_ReturnsOutput(t *testing.T) {
	if !hasWorkingDNF() {
		t.Skip("dnf cannot reach its repos on this host; a timed-out call produces no dnf output to assert on")
	}
	out, _ := InstallServiceVersion("nonexistent_cystemd_pkg_xyz_99999", "0.0.0.0.0")
	// dnf always produces some output — "No match" or "Error" or similar.
	if out == "" {
		t.Error("expected non-empty output from dnf even on failure")
	}
}

// TestInstallServiceVersion_FailureIsNotLoggedAtErrorLevel pins that a failed
// install logs at Debug, never Error (the caller reports Warning). Not dnf-gated:
// without dnf, exec.ErrNotFound takes the same path.
// See DOCS/MEMORY.md § RESOLVED — `dnf install` failure severity: the README was right.
func TestInstallServiceVersion_FailureIsNotLoggedAtErrorLevel(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	_, err := InstallServiceVersion("nonexistent_cystemd_pkg_xyz_99999", "0.0.0")
	if err == nil {
		t.Fatal("expected an error installing a package that cannot exist")
	}
	if strings.Contains(buf.String(), `"level":"error"`) {
		t.Errorf("a fail-soft, retried install failure must not log at error level "+
			"(the caller reports it at Warning); got:\n%s", buf.String())
	}
	// Downgraded, not deleted — the detail must remain for debugging.
	if !strings.Contains(buf.String(), "install") {
		t.Errorf("the failure should still be logged at debug; got:\n%s", buf.String())
	}
}

func TestCGroupProcessEntries_PathNotFound_FallsBackToMainPID(t *testing.T) {
	// /sys/fs/cgroup + this path is extremely unlikely to exist.
	entries := cgroupProcessEntries("/cystemd_no_such_cgroup_xyz_999", uint32(os.Getpid()))
	if len(entries) != 1 {
		t.Fatalf("expected 1 fallback entry from MainPID; got %d entries: %v", len(entries), entries)
	}
	if entries[0].PID != uint32(os.Getpid()) {
		t.Errorf("entry PID: got %d, want %d (current pid)", entries[0].PID, os.Getpid())
	}
}

func TestCGroupProcessEntries_NoFile_NoMainPID_ReturnsEmpty(t *testing.T) {
	entries := cgroupProcessEntries("/cystemd_no_such_cgroup_xyz_999", 0)
	if len(entries) != 0 {
		t.Errorf("expected empty entries with no file and no MainPID; got %v", entries)
	}
}

// TestCGroupProcessEntries_CommandIsShortCommNotFullCmdline pins the argv-leak
// fix: Command is the short comm, never argv (secrets on an open endpoint). The
// test binary's argv carries "-test." flags its comm lacks.
func TestCGroupProcessEntries_CommandIsShortCommNotFullCmdline(t *testing.T) {
	pid := uint32(os.Getpid())
	entries := cgroupProcessEntries("/cystemd_no_such_cgroup_xyz_999", pid)
	if len(entries) != 1 {
		t.Fatalf("expected 1 fallback entry from MainPID; got %d entries: %v", len(entries), entries)
	}
	wantComm := readComm(pid)
	if wantComm == "" {
		t.Fatal("readComm(self) returned empty; cannot verify the fix")
	}
	if entries[0].Command != wantComm {
		t.Errorf("Command: got %q, want short comm %q", entries[0].Command, wantComm)
	}
	fullCmdline := readCmdline(pid)
	if fullCmdline == wantComm {
		t.Fatal("readCmdline(self) == readComm(self); test cannot distinguish the two paths in this environment")
	}
	if strings.Contains(entries[0].Command, "-test.") {
		t.Errorf("Command leaked full cmdline/argv content; got %q (full cmdline was %q)", entries[0].Command, fullCmdline)
	}
}

// ─── unitIsRunning / ServiceActiveState / TryRestartService ────────────────

// TestUnitIsRunning_States pins the no-resurrect table: only live or starting
// states run; "deactivating" and "" (never loaded, or systemd unreachable) do not.
func TestUnitIsRunning_States(t *testing.T) {
	for _, s := range []string{"active", "activating", "reloading", "refreshing"} {
		if !unitIsRunning(s) {
			t.Errorf("unitIsRunning(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"inactive", "failed", "deactivating", ""} {
		if unitIsRunning(s) {
			t.Errorf("unitIsRunning(%q) = true, want false", s)
		}
	}
}

// TestServiceActiveState_NeverLoadedUnit_NotRunning pins that a unit systemd has
// never loaded reads as not running, so no-resurrect leaves it alone.
func TestServiceActiveState_NeverLoadedUnit_NotRunning(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available")
	}
	if state := ServiceActiveState("cystemd-test-no-such-unit-xyz"); unitIsRunning(state) {
		t.Errorf("nonexistent unit reads as running (ActiveState=%q)", state)
	}
}

// TestServiceActiveState_RunningUnit_IsRunning pins the running side against
// init.scope — active on every systemd host — without mutating anything.
func TestServiceActiveState_RunningUnit_IsRunning(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available")
	}
	if state := ServiceActiveState("init.scope"); !unitIsRunning(state) {
		t.Errorf("init.scope should read as running; ActiveState=%q", state)
	}
}

// TestTryRestartService_NeverLoadedUnit_Errors pins the try-restart wiring: a
// job against a nonexistent unit errors and mutates nothing.
func TestTryRestartService_NeverLoadedUnit_Errors(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available")
	}
	if _, err := TryRestartService("cystemd-test-no-such-unit-xyz"); err == nil {
		t.Error("expected error try-restarting a nonexistent unit")
	}
}

// TestReloadSystemdDaemon runs a real, idempotent daemon-reload when systemd is present.
func TestReloadSystemdDaemon(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available")
	}
	if err := ReloadSystemdDaemon(); err != nil {
		t.Fatalf("ReloadSystemdDaemon: %v", err)
	}
}

// ─── D-Bus unit-type interface derivation (2026-08-13 fix; pure mapping) ───

func TestUnitTypeInterface_DerivesFromSuffix(t *testing.T) {
	cases := []struct {
		unit string
		want string
	}{
		{"sshd.service", "Service"},
		{"myapp.service", "Service"},
		{"cockpit.socket", "Socket"},
		{"data.mount", "Mount"},
		{"swapfile.swap", "Swap"},
		{"user.slice", "Slice"},
		{"session-1.scope", "Scope"},
		{"multi-user.target", "Target"},
		{"logrotate.timer", "Timer"},
		{"cups.path", "Path"},
		{"proc-sys.automount", "Automount"},
		{"dev-sda.device", "Device"},
		// Case-insensitive suffix match.
		{"weird.SERVICE", "Service"},
		// Not a unit-type suffix ("my.app" is a legal name): Service, never "…App".
		{"my.app", "Service"},
		{"foo.bar.baz", "Service"},
		// No suffix at all (unitName would normally have appended .service).
		{"bare", "Service"},
		{"", "Service"},
	}
	for _, c := range cases {
		if got := unitTypeInterface(c.unit); got != c.want {
			t.Errorf("unitTypeInterface(%q) = %q, want %q", c.unit, got, c.want)
		}
	}
}

func TestUnitTypeInterface_NeverReturnsEmpty(t *testing.T) {
	// "" would build the bare "org.freedesktop.systemd1." and fail every read.
	for _, u := range []string{"", ".", "..", "a.", ".service", "x.unknown"} {
		if got := unitTypeInterface(u); got == "" {
			t.Errorf("unitTypeInterface(%q) returned empty interface suffix", u)
		}
	}
}

func TestKnownUnitTypes_ValuesAreCapitalisedGoStyle(t *testing.T) {
	// go-systemd appends the value to "org.freedesktop.systemd1.": match the casing.
	for suffix, iface := range knownUnitTypes {
		if iface == "" || iface[0] < 'A' || iface[0] > 'Z' {
			t.Errorf("knownUnitTypes[%q] = %q; want a capitalised interface name", suffix, iface)
		}
		if strings.ToLower(iface) != suffix {
			t.Errorf("knownUnitTypes[%q] = %q; value should be the capitalised form of the key", suffix, iface)
		}
	}
}

// ─── kill-watchdog grace derivation (pure arithmetic) ──────────────────────

func TestWatchdogGrace_DerivesFromUnitStopTimeout(t *testing.T) {
	const us = uint64(time.Second / time.Microsecond) // 1s expressed in µs

	cases := []struct {
		name      string
		stopUSec  uint64
		wantGrace time.Duration
		wantKill  bool
	}{
		// 0 (also what an unreadable read yields) → 3s default, kill allowed.
		{"unreadable", 0, watchdogGraceDefault, true},
		// infinity: the operator said never force-stop.
		{"infinity", maxUint64, watchdogGraceCap, false},
		// Real fleet values, each + margin.
		{"ouchinterface 1s", 1 * us, 1*time.Second + watchdogGraceMargin, true},
		{"fixorderms 5s", 5 * us, 5*time.Second + watchdogGraceMargin, true},
		{"cystemd 10s", 10 * us, 10*time.Second + watchdogGraceMargin, true},
		{"alloy 20s", 20 * us, 20*time.Second + watchdogGraceMargin, true},
		// Above the cap → clamped.
		{"120s clamps to cap", 120 * us, watchdogGraceCap, true},
		{"just over cap", uint64(watchdogGraceCap/time.Microsecond) + 1, watchdogGraceCap, true},
	}
	for _, c := range cases {
		gotGrace, gotKill := watchdogGrace(c.stopUSec)
		if gotGrace != c.wantGrace || gotKill != c.wantKill {
			t.Errorf("%s: watchdogGrace(%d) = (%s, %t), want (%s, %t)",
				c.name, c.stopUSec, gotGrace, gotKill, c.wantGrace, c.wantKill)
		}
	}
}

func TestWatchdogGrace_NeverExceedsCapOrReturnsZero(t *testing.T) {
	// Zero would fire before the unit could stop; above the cap outlives the job bound.
	for _, us := range []uint64{0, 1, 1000, 3_000_000, 59_000_000, 60_000_000,
		600_000_000, maxUint64 - 1, maxUint64} {
		g, _ := watchdogGrace(us)
		if g <= 0 {
			t.Errorf("watchdogGrace(%d) returned non-positive grace %s", us, g)
		}
		if g > watchdogGraceCap {
			t.Errorf("watchdogGrace(%d) = %s exceeds cap %s", us, g, watchdogGraceCap)
		}
	}
}

func TestWatchdogGrace_InfinityIsTheOnlyNoKillCase(t *testing.T) {
	for _, us := range []uint64{0, 1, 1_000_000, 60_000_000, maxUint64 - 1} {
		if _, mayKill := watchdogGrace(us); !mayKill {
			t.Errorf("watchdogGrace(%d) disallowed kill; only infinity should", us)
		}
	}
	if _, mayKill := watchdogGrace(maxUint64); mayKill {
		t.Error("watchdogGrace(maxUint64/infinity) allowed kill; operator policy is never force-stop")
	}
}

// formatUnitFileChanges is pure, so it is tested here although its callers need
// live D-Bus; the empty branch is what /enable on an enabled unit returns.

func TestFormatUnitFileChanges_OneLinePerChange(t *testing.T) {
	got := formatUnitFileChanges([]unitFileChange{
		{Type: "symlink", Filename: "/etc/systemd/system/multi-user.target.wants/x.service", Destination: "/usr/lib/systemd/system/x.service"},
		{Type: "unlink", Filename: "/etc/systemd/system/y.service", Destination: ""},
	}, "x.service", "enable")

	want := "symlink: /etc/systemd/system/multi-user.target.wants/x.service → /usr/lib/systemd/system/x.service\n" +
		"unlink: /etc/systemd/system/y.service → \n"
	if got != want {
		t.Errorf("formatUnitFileChanges mismatch:\ngot:  %q\nwant: %q", got, want)
	}
	// The svc/verb arguments must not leak into the non-empty rendering.
	if strings.Contains(got, "no symlink changes") {
		t.Error("non-empty changes must not emit the fallback line")
	}
}

func TestFormatUnitFileChanges_EmptyEmitsFallbackNamingServiceAndVerb(t *testing.T) {
	// Zero changes (already in the requested state) must not yield an empty
	// body, which would read as a failure rather than a no-op.
	for _, tc := range []struct {
		name    string
		changes []unitFileChange
	}{
		{"nil", nil},
		{"empty slice", []unitFileChange{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := formatUnitFileChanges(tc.changes, "x.service", "enable")
			want := "systemd: x.service enable (no symlink changes)\n"
			if got != want {
				t.Errorf("got %q, want %q", got, want)
			}
		})
	}

	// The verb is not hardcoded — /disable shares this helper.
	if got := formatUnitFileChanges(nil, "y.service", "disable"); got != "systemd: y.service disable (no symlink changes)\n" {
		t.Errorf("disable fallback did not carry svc/verb through: %q", got)
	}
}
