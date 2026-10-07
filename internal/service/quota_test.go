package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The operator's key paths, selector-templated.
const (
	sampleCPUKey    = ".${selector}.services.${selector}.deployment.resources.limits.cpu"
	sampleMemoryKey = ".${selector}.services.${selector}.deployment.resources.limits.memory"
)

func TestK8sCPUToCPUQuota(t *testing.T) {
	ok := []struct{ in, want string }{
		{"200m", "20%"},   // the sample value
		{"2", "200%"},     // multi-core: >100% is valid
		{"1", "100%"},     // 1 core
		{"0.5", "50%"},    // fractional cores
		{"1500m", "150%"}, // 1.5 cores
		{"250m", "25%"},
		{"100m", "10%"},
		{"1m", "0.1%"},    // one-decimal output
		{" 200m ", "20%"}, // surrounding whitespace tolerated
	}
	for _, tc := range ok {
		got, err := k8sCPUToCPUQuota(tc.in)
		if err != nil {
			t.Errorf("k8sCPUToCPUQuota(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("k8sCPUToCPUQuota(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Non-finite inputs that strconv.ParseFloat accepts ("Inf"/"NaN") are rejected
	// explicitly (int64(float) saturation is arch-dependent).
	for _, in := range []string{"", "abc", "0", "0m", "-1", "-200m", "1.2.3", "Inf", "NaN"} {
		if got, err := k8sCPUToCPUQuota(in); err == nil {
			t.Errorf("k8sCPUToCPUQuota(%q) = %q, want error", in, got)
		}
	}
}

func TestK8sMemoryToBytes(t *testing.T) {
	ok := []struct{ in, want string }{
		{"1Gi", "1073741824"},  // the sample value; matches set-property drop-in
		{"512Mi", "536870912"}, // binary boundary …
		{"512M", "512000000"},  // … vs decimal boundary
		{"1G", "1000000000"},
		{"1Ti", "1099511627776"},       // binary tebi …
		{"1T", "1000000000000"},        // … vs decimal tera
		{"1Pi", "1125899906842624"},    // binary pebi …
		{"1P", "1000000000000000"},     // … vs decimal peta
		{"1Ei", "1152921504606846976"}, // binary exbi (< maxQuotaScalar)
		{"1E", "1000000000000000000"},  // decimal exa
		{"1Ki", "1024"},
		{"1k", "1000"},
		{"1073741824", "1073741824"}, // bare byte count passes through
		{"1.1Gi", "1181116006"},      // fractional mantissa rounded
		{" 1Gi ", "1073741824"},
	}
	for _, tc := range ok {
		got, err := k8sMemoryToBytes(tc.in)
		if err != nil {
			t.Errorf("k8sMemoryToBytes(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("k8sMemoryToBytes(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// "Inf"/"NaN" parse as floats and "8Ei" overflows int64: all rejected before conversion.
	for _, in := range []string{"", "abc", "1Gii", "0", "0Mi", "-5Mi", "Gi", "Inf", "NaN", "8Ei"} {
		if got, err := k8sMemoryToBytes(in); err == nil {
			t.Errorf("k8sMemoryToBytes(%q) = %q, want error", in, got)
		}
	}
}

func readResourcesFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "sample_resources_values.yaml"))
	if err != nil {
		t.Fatalf("read resources fixture: %v", err)
	}
	return data
}

// TestExtractScalarString_ResourceLimits: the operator's key paths end to end,
// extraction then unit conversion.
func TestExtractScalarString_ResourceLimits(t *testing.T) {
	data := readResourcesFixture(t)

	cpu, err := extractScalarString(data, sampleCPUKey, "application")
	if err != nil {
		t.Fatalf("extract cpu: %v", err)
	}
	if cpu != "200m" {
		t.Fatalf("cpu = %q, want %q", cpu, "200m")
	}
	if q, err := k8sCPUToCPUQuota(cpu); err != nil || q != "20%" {
		t.Errorf("cpu quota = %q, %v; want \"20%%\", nil", q, err)
	}

	mem, err := extractScalarString(data, sampleMemoryKey, "application")
	if err != nil {
		t.Fatalf("extract memory: %v", err)
	}
	if mem != "1Gi" {
		t.Fatalf("memory = %q, want %q", mem, "1Gi")
	}
	if b, err := k8sMemoryToBytes(mem); err != nil || b != "1073741824" {
		t.Errorf("memory bytes = %q, %v; want \"1073741824\", nil", b, err)
	}
}

// stubLiveQuota makes the D-Bus seam report a LOADED unit with the given live caps
// (noCPUCap / noMemCap for an absent cap).
func stubLiveQuota(t *testing.T, cpuPerSecUSec, memMax uint64) {
	t.Helper()
	stubSystemdConn(t, &fakeSystemdConn{
		unitProps: map[string]interface{}{"LoadState": "loaded"},
		typeProps: map[string]interface{}{
			"CPUQuotaPerSecUSec": cpuPerSecUSec,
			"MemoryMax":          memMax,
		},
	}, nil)
}

// Live values matching the fixture's 200m / 1Gi, and systemd's two "no cap" sentinels.
const (
	sampleCPUPerSecUSec = uint64(200000)     // CPUQuota=20%
	sampleMemMaxBytes   = uint64(1073741824) // MemoryMax=1Gi
	noCPUCap            = uint64(0)
	noMemCap            = maxUint64
)

func quotaTestEntry() *ServiceEntry {
	return &ServiceEntry{
		Service:            "sample_application",
		Selector:           "application",
		GitValues:          "values.yaml",
		GitValuesCPUKey:    sampleCPUKey,
		GitValuesMemoryKey: sampleMemoryKey,
	}
}

// autosync off, no tracking file: drift logged at Info, no set-property, no file written.
func TestRunServiceQuotaApply_AutoSyncDisabled_LogsDriftNoApply(t *testing.T) {
	data := readResourcesFixture(t)
	destDir := mkTempDir(t)
	stubLiveQuota(t, noCPUCap, noMemCap)
	buf := captureLogs(t, LevelDebug)

	runServiceQuotaApply("sample_application", quotaTestEntry(), data, destDir, false)

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "resource quota out-of-date") || !strings.Contains(msgs, "autosync=false") {
		t.Errorf("expected autosync-disabled drift log; got:\n%s", msgs)
	}
	if !strings.Contains(msgs, "CPUQuota=20% MemoryMax=1073741824") {
		t.Errorf("expected desired quota (both props) in drift log; got:\n%s", msgs)
	}
	if _, err := os.Stat(filepath.Join(destDir, appliedQuotaFilename)); !os.IsNotExist(err) {
		t.Errorf("expected no .applied_quota when autosync=false; stat err=%v", err)
	}
}

// Live caps already match: a no-op decided with NO tracking file present — live state
// decides (ADR-0016). autosync=true proves the skip precedes the apply gate.
func TestRunServiceQuotaApply_AlreadyApplied_NoOp(t *testing.T) {
	data := readResourcesFixture(t)
	destDir := mkTempDir(t)
	stubLiveQuota(t, sampleCPUPerSecUSec, sampleMemMaxBytes)
	buf := captureLogs(t, LevelDebug)

	runServiceQuotaApply("sample_application", quotaTestEntry(), data, destDir, true)

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "already applied") {
		t.Errorf("expected 'already applied' debug log; got:\n%s", msgs)
	}
	if strings.Contains(msgs, "applying resource quota") || strings.Contains(msgs, "change detected") {
		t.Errorf("unexpected apply attempt; got:\n%s", msgs)
	}
}

// An entry with neither key set produces no props, no logs, no file.
func TestRunServiceQuotaApply_NoKeys_NoOp(t *testing.T) {
	data := readResourcesFixture(t)
	destDir := mkTempDir(t)
	buf := captureLogs(t, LevelDebug)

	entry := &ServiceEntry{Service: "sample_application", Selector: "application", GitValues: "values.yaml"}
	runServiceQuotaApply("sample_application", entry, data, destDir, true)

	if msgs := strings.TrimSpace(decodeLogMsgs(t, buf)); msgs != "" {
		t.Errorf("expected no logs when no quota keys set; got:\n%s", msgs)
	}
}

// An invalid cpu value warns and is skipped while memory still resolves; the drift
// line shows ONLY MemoryMax (per-property granularity). autosync=false: no set-property.
func TestRunServiceQuotaApply_BadCPU_SkipsCPUKeepsMemory(t *testing.T) {
	badYAML := []byte("application:\n" +
		"  services:\n" +
		"    application:\n" +
		"      deployment:\n" +
		"        resources:\n" +
		"          limits:\n" +
		"            cpu: notacpu\n" +
		"            memory: 1Gi\n")
	destDir := mkTempDir(t)
	stubLiveQuota(t, noCPUCap, noMemCap)
	buf := captureLogs(t, LevelDebug)

	runServiceQuotaApply("sample_application", quotaTestEntry(), badYAML, destDir, false)

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "invalid cpu limit") {
		t.Errorf("expected invalid-cpu warning; got:\n%s", msgs)
	}
	if !strings.Contains(msgs, "wanted=\"MemoryMax=1073741824\"") {
		t.Errorf("expected memory-only desired quota; got:\n%s", msgs)
	}
	if strings.Contains(msgs, "CPUQuota=") {
		t.Errorf("CPUQuota must not appear when cpu is invalid; got:\n%s", msgs)
	}
}

// TestRunContinuousDeployment_QuotaReachedWhenConfigKeyExtractionFails: a failed
// git_values_config_key extraction must not skip the quota step (it is deferred).
// autosync=false: the drift line proves the step ran.
func TestRunContinuousDeployment_QuotaReachedWhenConfigKeyExtractionFails(t *testing.T) {
	values := "myapplication:\n" +
		"  services:\n" +
		"    myapplication:\n" +
		"      deployment:\n" +
		"        resources:\n" +
		"          limits:\n" +
		"            cpu: 200m\n" +
		"            memory: 1Gi\n"
	repo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": values,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync:  false, // drift-only: never reaches systemctl set-property
				Service:   "myapplication",
				Selector:  "myapplication",
				GitValues: "values.yaml",
				// Not in values.yaml: extraction fails and takes the early return.
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
				GitValuesCPUKey:    sampleCPUKey,
				GitValuesMemoryKey: sampleMemoryKey,
				Workdir:            workbase,
			},
		},
	}

	stubLiveQuota(t, noCPUCap, noMemCap)
	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	msgs := decodeLogMsgs(t, buf)
	// Sanity: the config-key extraction really did fail (the early-return path).
	if !strings.Contains(msgs, "cannot extract") {
		t.Fatalf("expected a config-key extraction failure; got:\n%s", msgs)
	}
	// The fix: quota was still evaluated and its drift logged.
	if !strings.Contains(msgs, "resource quota out-of-date") ||
		!strings.Contains(msgs, "CPUQuota=20% MemoryMax=1073741824") {
		t.Errorf("quota step was skipped after config-key extraction failed (regression); got:\n%s", msgs)
	}
	// The config sub-document must NOT have been written (extraction failed).
	if _, err := os.Stat(filepath.Join(workbase, "myapplication", "target.config.yml")); !os.IsNotExist(err) {
		t.Errorf("target.config.yml should not exist when extraction failed; stat err=%v", err)
	}
}

// TestCPUQuotaPercentString: CPUQuotaPerSecUSec rendered as the operator-facing
// percentage for /status (inverse of k8sCPUToCPUQuota): 100% = 1_000_000 µs/s.
func TestCPUQuotaPercentString(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{200000, "20%"},   // 200m → 20% round-trips
		{2000000, "200%"}, // 2 cores
		{1000000, "100%"}, // 1 core
		{500000, "50%"},
		{1000, "0.1%"}, // 1m → 0.1% (minimal decimal, no trailing zeros)
		{100, "0.01%"}, // permyriad granularity
		{1250000, "125%"},
	}
	for _, tc := range cases {
		if got := cpuQuotaPercentString(tc.in); got != tc.want {
			t.Errorf("cpuQuotaPercentString(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// Unset (0) and "infinity"/no-quota (max uint64) yield "" so /status omits the field.
	for _, in := range []uint64{0, maxUint64} {
		if got := cpuQuotaPercentString(in); got != "" {
			t.Errorf("cpuQuotaPercentString(%d) = %q, want \"\"", in, got)
		}
	}
}

// ─── Live-state convergence (ADR-0016) ─────────────────────────────────────────────────────

// TestRunServiceQuotaApply_CapClearedOutOfBand_IgnoresMatchingFile pins ADR-0016: a
// tracking file saying "applied" must not win over a live unit with no caps.
// autosync=false keeps it hermetic (the out-of-band notice precedes the apply gate).
func TestRunServiceQuotaApply_CapClearedOutOfBand_IgnoresMatchingFile(t *testing.T) {
	data := readResourcesFixture(t)
	destDir := mkTempDir(t)
	writeAppliedQuota(destDir, "CPUQuota=20% MemoryMax=1073741824")
	stubLiveQuota(t, noCPUCap, noMemCap)
	buf := captureLogs(t, LevelDebug)

	runServiceQuotaApply("sample_application", quotaTestEntry(), data, destDir, false)

	msgs := decodeLogMsgs(t, buf)
	if strings.Contains(msgs, "already applied") {
		t.Fatalf("a matching .applied_quota must NOT satisfy the check when the live unit has no cap; got:\n%s", msgs)
	}
	if !strings.Contains(msgs, "cleared out of band") {
		t.Errorf("expected the out-of-band drift notice naming the tracking file; got:\n%s", msgs)
	}
	if !strings.Contains(msgs, "resource quota out-of-date") {
		t.Errorf("expected drift to be reported so the cap is re-applied; got:\n%s", msgs)
	}
}

// A partially cleared unit (memory cap intact, CPU cap gone) is still drift:
// props compare as a joined string for per-property granularity.
func TestRunServiceQuotaApply_OneCapClearedIsStillDrift(t *testing.T) {
	data := readResourcesFixture(t)
	destDir := mkTempDir(t)
	stubLiveQuota(t, noCPUCap, sampleMemMaxBytes)
	buf := captureLogs(t, LevelDebug)

	runServiceQuotaApply("sample_application", quotaTestEntry(), data, destDir, false)

	msgs := decodeLogMsgs(t, buf)
	if strings.Contains(msgs, "already applied") {
		t.Fatalf("a half-applied quota must not read as applied; got:\n%s", msgs)
	}
	// The live side must name the missing cap so the log says WHICH one went.
	if !strings.Contains(msgs, `current="CPUQuota= MemoryMax=1073741824"`) {
		t.Errorf("drift log must show the empty CPUQuota against the intact MemoryMax; got:\n%s", msgs)
	}
}

// systemd unreachable: the cycle is skipped, never decided from the file (the
// currentServiceVersion contract); a matching file must not short-circuit it.
func TestRunServiceQuotaApply_UnreadableLiveState_DeclinesToDecide(t *testing.T) {
	data := readResourcesFixture(t)
	destDir := mkTempDir(t)
	writeAppliedQuota(destDir, "CPUQuota=20% MemoryMax=1073741824")
	stubSystemdConn(t, nil, errors.New("dial unix /run/systemd/private: connect: no such file"))
	buf := captureLogs(t, LevelDebug)

	runServiceQuotaApply("sample_application", quotaTestEntry(), data, destDir, true)

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "quota check skipped") {
		t.Errorf("expected the check to be skipped when systemd is unreachable; got:\n%s", msgs)
	}
	if strings.Contains(msgs, "already applied") {
		t.Errorf("an unreadable live state must not be reported as applied; got:\n%s", msgs)
	}
}

// A unit systemd does not know is inconclusive, not drifted.
func TestCurrentServiceQuota_UnitNotLoaded_IsInconclusive(t *testing.T) {
	stubSystemdConn(t, &fakeSystemdConn{
		unitProps: map[string]interface{}{"LoadState": "not-found"},
	}, nil)

	_, atWanted, known := currentServiceQuota("ghost", []string{"CPUQuota=20%"})
	if known || atWanted {
		t.Errorf("a not-found unit must be inconclusive; got atWanted=%t known=%t", atWanted, known)
	}
}

// liveQuotaValue renders live properties in the desired string's form and tells
// "no cap" from "cannot read".
func TestLiveQuotaValue_RendersDesiredForm(t *testing.T) {
	conn := &fakeSystemdConn{typeProps: map[string]interface{}{
		"CPUQuotaPerSecUSec": uint64(800000),
		"MemoryMax":          uint64(4294967296),
	}}
	if got, ok := liveQuotaValue(conn, "x.service", "CPUQuota"); !ok || got != "80%" {
		t.Errorf("CPUQuota = (%q, %t), want (\"80%%\", true)", got, ok)
	}
	if got, ok := liveQuotaValue(conn, "x.service", "MemoryMax"); !ok || got != "4294967296" {
		t.Errorf("MemoryMax = (%q, %t), want (\"4294967296\", true)", got, ok)
	}

	// systemd's "infinity" is a successful read of an absent cap, NOT a failure.
	uncapped := &fakeSystemdConn{typeProps: map[string]interface{}{
		"CPUQuotaPerSecUSec": maxUint64,
		"MemoryMax":          maxUint64,
	}}
	for _, prop := range []string{"CPUQuota", "MemoryMax"} {
		if got, ok := liveQuotaValue(uncapped, "x.service", prop); !ok || got != "" {
			t.Errorf("uncapped %s = (%q, %t), want (\"\", true)", prop, got, ok)
		}
	}

	// An unreadable property is the false case, so the caller can decline.
	unreadable := &fakeSystemdConn{typeErr: errors.New("Unknown interface or property")}
	for _, prop := range []string{"CPUQuota", "MemoryMax"} {
		if _, ok := liveQuotaValue(unreadable, "x.service", prop); ok {
			t.Errorf("an unreadable %s must report ok=false", prop)
		}
	}

	// An unmanaged property is also ok=false, so a new managed prop fails closed
	// until liveQuotaValue learns it.
	if _, ok := liveQuotaValue(conn, "x.service", "TasksMax"); ok {
		t.Error("an unmanaged property must report ok=false")
	}
}

// An uncapped unit logs first-application wording, not a change: it renders as
// "CPUQuota= MemoryMax=", hence quotaAllUnset.
func TestRunServiceQuotaApply_FreshUnit_LogsFirstApplicationNotChange(t *testing.T) {
	data := readResourcesFixture(t)
	destDir := mkTempDir(t)
	stubLiveQuota(t, noCPUCap, noMemCap)
	buf := captureLogs(t, LevelDebug)

	// autosync=false stops before set-property, so assert the drift line's shape.
	runServiceQuotaApply("sample_application", quotaTestEntry(), data, destDir, false)

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, `current="CPUQuota= MemoryMax="`) {
		t.Errorf("an uncapped unit must render both props as empty; got:\n%s", msgs)
	}
}

func TestQuotaAllUnset(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"CPUQuota= MemoryMax=", true},
		{"CPUQuota=", true},
		{"", true},
		{"CPUQuota=20% MemoryMax=", false},
		{"CPUQuota= MemoryMax=1073741824", false},
		{"CPUQuota=20% MemoryMax=1073741824", false},
	}
	for _, c := range cases {
		if got := quotaAllUnset(c.in); got != c.want {
			t.Errorf("quotaAllUnset(%q) = %t, want %t", c.in, got, c.want)
		}
	}
}

// Guard branches return "cannot tell", never a wrong answer, so a malformed input
// is never mistaken for "no cap set".
func TestCurrentServiceQuota_GuardBranches(t *testing.T) {
	t.Run("no props wanted is trivially satisfied", func(t *testing.T) {
		// No connection is opened, so a nil seam proves the early return.
		stubSystemdConn(t, nil, errors.New("must not be dialled"))
		cur, atWanted, known := currentServiceQuota("svc", nil)
		if cur != "" || !atWanted || !known {
			t.Errorf("got (%q, %t, %t), want (\"\", true, true)", cur, atWanted, known)
		}
	})

	t.Run("prop with no = is inconclusive", func(t *testing.T) {
		stubLiveQuota(t, sampleCPUPerSecUSec, sampleMemMaxBytes)
		if _, _, known := currentServiceQuota("svc", []string{"CPUQuota"}); known {
			t.Error("a malformed prop must be inconclusive, not treated as a value")
		}
	})

	t.Run("unmanaged prop name is inconclusive", func(t *testing.T) {
		stubLiveQuota(t, sampleCPUPerSecUSec, sampleMemMaxBytes)
		if _, _, known := currentServiceQuota("svc", []string{"TasksMax=100"}); known {
			t.Error("a property liveQuotaValue cannot read must be inconclusive")
		}
	})
}

// A property of the wrong wire type is "cannot tell", not zero (as in
// getTypedUint32PropOK): a cap is never re-applied on a misread.
func TestGetTypedUint64PropOK_WrongWireTypeIsNotZero(t *testing.T) {
	wrong := &fakeSystemdConn{typeProps: map[string]interface{}{"MemoryMax": uint32(5)}}
	if v, ok := getTypedUint64PropOK(wrong, "x.service", "MemoryMax"); ok || v != 0 {
		t.Errorf("wrong wire type: got (%d, %t), want (0, false)", v, ok)
	}
}
