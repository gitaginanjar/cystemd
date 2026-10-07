package service

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func scrapeMetrics(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handleMetrics(rec, req, nil)
	return rec
}

// allMetricFamilies is every metric family name the exposition must emit — one
// HELP + one TYPE line each, on every scrape (including degraded mode).
var allMetricFamilies = []string{
	"cystemd_build_info",
	"target_info",
	"process_start_time_seconds",
	"go_goroutines",
	"go_memstats_alloc_bytes",
	"go_memstats_sys_bytes",
	"go_memstats_heap_inuse_bytes",
	"go_memstats_next_gc_bytes",
	"cystemd_http_requests_total",
	"cystemd_http_request_duration_seconds",
	"cystemd_audit_total",
	"cystemd_panic_recovered_total",
	"cystemd_auth_total",
	"cystemd_vault_up",
	"cystemd_vault_token_lease_duration_seconds",
	"cystemd_vault_token_renewable",
	"cystemd_vault_token_renewal_total",
	"cystemd_vault_token_last_renewal_timestamp_seconds",
	"cystemd_secret_fetch_total",
	"cystemd_secret_last_fetch_timestamp_seconds",
	"cystemd_template_render_total",
	"cystemd_template_last_render_timestamp_seconds",
	"cystemd_template_reload_total",
	"cystemd_cd_cycle_total",
	"cystemd_cd_last_success_timestamp_seconds",
	"cystemd_cd_clone_total",
	"cystemd_cd_install_total",
	"cystemd_cd_restart_total",
	// Managed-unit health: HELP/TYPE even with no units or systemd unreachable.
	"cystemd_unit_health_scrape_ok",
	"cystemd_unit_active_state",
	"cystemd_unit_restarts_total",
}

// seriesPrefixes are the series lines (up to the value) every scrape must carry.
// Only presence is asserted: other tests' background goroutines can bump these
// package-global counters.
var seriesPrefixes = []string{
	`target_info{service_name="cystemd",`,
	"process_start_time_seconds ",
	"go_goroutines ",
	"go_memstats_alloc_bytes ",
	"go_memstats_sys_bytes ",
	"go_memstats_heap_inuse_bytes ",
	"go_memstats_next_gc_bytes ",
	"cystemd_vault_up ",
	"cystemd_vault_token_lease_duration_seconds ",
	"cystemd_vault_token_renewable ",
	`cystemd_vault_token_renewal_total{result="success"} `,
	`cystemd_vault_token_renewal_total{result="reauth"} `,
	`cystemd_vault_token_renewal_total{result="failure"} `,
	"cystemd_vault_token_last_renewal_timestamp_seconds ",
	`cystemd_secret_fetch_total{result="success"} `,
	`cystemd_secret_fetch_total{result="failure"} `,
	"cystemd_secret_last_fetch_timestamp_seconds ",
	`cystemd_template_render_total{result="success"} `,
	`cystemd_template_render_total{result="failure"} `,
	"cystemd_template_last_render_timestamp_seconds ",
	`cystemd_template_reload_total{result="success"} `,
	`cystemd_template_reload_total{result="failure"} `,
	`cystemd_cd_cycle_total{result="success"} `,
	`cystemd_cd_cycle_total{result="failure"} `,
	"cystemd_cd_last_success_timestamp_seconds ",
	`cystemd_cd_clone_total{result="success"} `,
	`cystemd_cd_clone_total{result="failure"} `,
	`cystemd_cd_install_total{result="success"} `,
	`cystemd_cd_install_total{result="failure"} `,
	`cystemd_cd_restart_total{result="success"} `,
	`cystemd_cd_restart_total{result="failure"} `,
	// Only the flag is unconditional; per-unit series: unit_metrics_test.go.
	"cystemd_unit_health_scrape_ok ",
}

func TestHandleMetrics_ExpositionFormat(t *testing.T) {
	rec := scrapeMetrics(t)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics: got %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Errorf("Content-Type: got %q want the classic 0.0.4 exposition type", ct)
	}
	body := rec.Body.String()

	// build_info carries identity in labels and a constant 1 value.
	if !strings.Contains(body, "cystemd_build_info{version=") || !strings.Contains(body, "go_version=") {
		t.Errorf("build_info line malformed:\n%s", body)
	}

	// Exactly one HELP + one TYPE per family (strict parsers reject duplicates).
	for _, fam := range allMetricFamilies {
		if n := strings.Count(body, "# HELP "+fam+" "); n != 1 {
			t.Errorf("family %q: got %d HELP lines, want 1", fam, n)
		}
		if n := strings.Count(body, "# TYPE "+fam+" "); n != 1 {
			t.Errorf("family %q: got %d TYPE lines, want 1", fam, n)
		}
	}

	// Every series is present on every scrape (no holes when degraded).
	for _, p := range seriesPrefixes {
		if !strings.Contains(body, p) {
			t.Errorf("missing series %q\n---\n%s", p, body)
		}
	}

	// Classic exposition (0.0.4) has no OpenMetrics EOF trailer.
	if strings.Contains(body, "# EOF") {
		t.Errorf("classic exposition must not contain # EOF")
	}
}

func TestHandleMetrics_RejectsNonGET(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	rec := httptest.NewRecorder()
	handleMetrics(rec, req, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics: got %d want 405", rec.Code)
	}
}

// TestHandleMetrics_VaultGaugesReflectState pins the scrape-time Vault gauges to
// globalVaultState: nil → 0/0/0; a renewable lease → 1/lease/1 (exact values).
func TestHandleMetrics_VaultGaugesReflectState(t *testing.T) {
	prev := globalVaultState

	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })
	body := scrapeMetrics(t).Body.String()
	for _, w := range []string{
		"cystemd_vault_up 0\n",
		"cystemd_vault_token_lease_duration_seconds 0\n",
		"cystemd_vault_token_renewable 0\n",
	} {
		if !strings.Contains(body, w) {
			t.Errorf("degraded: missing %q\n%s", w, body)
		}
	}

	globalVaultState = &VaultState{
		Address:       "https://vault.example.com",
		AuthMethod:    VaultAuthMethodToken,
		LeaseDuration: 3600,
		Renewable:     true,
	}
	body = scrapeMetrics(t).Body.String()
	for _, w := range []string{
		"cystemd_vault_up 1\n",
		"cystemd_vault_token_lease_duration_seconds 3600\n",
		"cystemd_vault_token_renewable 1\n",
	} {
		if !strings.Contains(body, w) {
			t.Errorf("live: missing %q\n%s", w, body)
		}
	}
}

// TestMetricsRecorders_Increment pins that each recorder bumps its atomic and
// stamps its last-event gauge (>= deltas tolerate other tests' goroutines).
func TestMetricsRecorders_Increment(t *testing.T) {
	s0, r0, f0 := metVaultRenewSuccess.Load(), metVaultRenewReauth.Load(), metVaultRenewFailure.Load()
	recordVaultRenewSuccess()
	recordVaultRenewReauth()
	recordVaultRenewFailure()
	if metVaultRenewSuccess.Load()-s0 < 1 || metVaultRenewReauth.Load()-r0 < 1 || metVaultRenewFailure.Load()-f0 < 1 {
		t.Errorf("renewal recorders did not increment")
	}
	if metVaultLastRenewUnix.Load() == 0 {
		t.Errorf("last-renewal timestamp not stamped")
	}

	sf0, ff0 := metSecretFetchSuccess.Load(), metSecretFetchFailure.Load()
	recordSecretFetch(true)
	recordSecretFetch(false)
	if metSecretFetchSuccess.Load()-sf0 < 1 || metSecretFetchFailure.Load()-ff0 < 1 {
		t.Errorf("secret-fetch recorder did not increment")
	}
	if metSecretLastFetchUnix.Load() == 0 {
		t.Errorf("last-fetch timestamp not stamped")
	}

	rs0, rf0 := metTemplateRenderSuccess.Load(), metTemplateRenderFailure.Load()
	recordTemplateRender(true)
	recordTemplateRender(false)
	if metTemplateRenderSuccess.Load()-rs0 < 1 || metTemplateRenderFailure.Load()-rf0 < 1 {
		t.Errorf("template-render recorder did not increment")
	}

	ls0, lf0 := metTemplateReloadSuccess.Load(), metTemplateReloadFailure.Load()
	recordTemplateReload(true)
	recordTemplateReload(false)
	if metTemplateReloadSuccess.Load()-ls0 < 1 || metTemplateReloadFailure.Load()-lf0 < 1 {
		t.Errorf("template-reload recorder did not increment")
	}
}

// TestMetricsRecorders_CD pins the four CD recorders' atomics and the
// last-success gauge stamped by recordCDCycle (>= deltas, as above).
func TestMetricsRecorders_CD(t *testing.T) {
	cs0, cf0 := metCDCycleSuccess.Load(), metCDCycleFailure.Load()
	recordCDCycle(true)
	recordCDCycle(false)
	if metCDCycleSuccess.Load()-cs0 < 1 || metCDCycleFailure.Load()-cf0 < 1 {
		t.Errorf("cd-cycle recorder did not increment")
	}
	if metCDLastSuccessUnix.Load() == 0 {
		t.Errorf("cd last-success timestamp not stamped")
	}

	cls0, clf0 := metCDCloneSuccess.Load(), metCDCloneFailure.Load()
	recordCDClone(true)
	recordCDClone(false)
	if metCDCloneSuccess.Load()-cls0 < 1 || metCDCloneFailure.Load()-clf0 < 1 {
		t.Errorf("cd-clone recorder did not increment")
	}

	is0, if0 := metCDInstallSuccess.Load(), metCDInstallFailure.Load()
	recordCDInstall(true)
	recordCDInstall(false)
	if metCDInstallSuccess.Load()-is0 < 1 || metCDInstallFailure.Load()-if0 < 1 {
		t.Errorf("cd-install recorder did not increment")
	}

	rs0, rf0 := metCDRestartSuccess.Load(), metCDRestartFailure.Load()
	recordCDRestart(true)
	recordCDRestart(false)
	if metCDRestartSuccess.Load()-rs0 < 1 || metCDRestartFailure.Load()-rf0 < 1 {
		t.Errorf("cd-restart recorder did not increment")
	}
}

// TestMetricsRecorders_WholeApp pins the HTTP/audit/auth labeled series and the
// duration histogram's cumulative _bucket/_sum/_count lines.
func TestMetricsRecorders_WholeApp(t *testing.T) {
	resetMetricsForTest()

	recordHTTPRequest("/status", "GET", 200)
	recordHTTPRequest("/status", "GET", 500)
	recordHTTPRequest("/restart", "GET", 403)
	recordHTTPDuration("/status", 12*time.Millisecond) // 0.012s → le="0.025" bucket
	recordAudit("allowed")
	recordAudit("denied")
	recordAuth("accepted")
	recordAuth("invalid")

	body := scrapeMetrics(t).Body.String()

	// Labels render in declaration order: path, method, status.
	for _, want := range []string{
		`cystemd_http_requests_total{path="/status",method="GET",status="200"} 1`,
		`cystemd_http_requests_total{path="/status",method="GET",status="500"} 1`,
		`cystemd_http_requests_total{path="/restart",method="GET",status="403"} 1`,
		`cystemd_audit_total{result="allowed"} 1`,
		`cystemd_audit_total{result="denied"} 1`,
		`cystemd_auth_total{result="accepted"} 1`,
		`cystemd_auth_total{result="invalid"} 1`,
		// 12ms falls in the 0.025 bucket; cumulative there is 1, +Inf is the total.
		`cystemd_http_request_duration_seconds_bucket{path="/status",le="0.025"} 1`,
		`cystemd_http_request_duration_seconds_bucket{path="/status",le="+Inf"} 1`,
		`cystemd_http_request_duration_seconds_count{path="/status"} 1`,
		`cystemd_http_request_duration_seconds_sum{path="/status"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n---\n%s", want, body)
		}
	}

	// 12ms must NOT have landed in a smaller bucket (cumulative stays 0 below 0.025).
	if strings.Contains(body, `cystemd_http_request_duration_seconds_bucket{path="/status",le="0.01"} 1`) {
		t.Errorf("12ms wrongly counted in the 0.01 bucket\n%s", body)
	}
}

// TestWriteMetrics_LabeledFamiliesAlwaysEmitHelpType pins one HELP + TYPE per
// labeled family on a fresh scrape with no observations (never headerless).
func TestWriteMetrics_LabeledFamiliesAlwaysEmitHelpType(t *testing.T) {
	resetMetricsForTest()
	body := scrapeMetrics(t).Body.String()
	for _, fam := range []string{
		"cystemd_http_requests_total",
		"cystemd_http_request_duration_seconds",
		"cystemd_audit_total",
		"cystemd_auth_total",
	} {
		if strings.Count(body, "# HELP "+fam+" ") != 1 {
			t.Errorf("family %q: got %d HELP lines, want 1\n%s", fam, strings.Count(body, "# HELP "+fam+" "), body)
		}
		if strings.Count(body, "# TYPE "+fam+" ") != 1 {
			t.Errorf("family %q: got %d TYPE lines, want 1", fam, strings.Count(body, "# TYPE "+fam+" "))
		}
	}
}

func TestResetMetricsForTest_Zeroes(t *testing.T) {
	recordVaultRenewSuccess()
	recordTemplateReload(false)
	recordCDCycle(true)
	recordCDClone(false)
	recordCDInstall(true)
	recordCDRestart(false)
	resetMetricsForTest()
	if metVaultRenewSuccess.Load() != 0 || metTemplateReloadFailure.Load() != 0 || metVaultLastRenewUnix.Load() != 0 {
		t.Errorf("resetMetricsForTest did not zero all counters/gauges")
	}
	if metCDCycleSuccess.Load() != 0 || metCDCloneFailure.Load() != 0 || metCDInstallSuccess.Load() != 0 ||
		metCDRestartFailure.Load() != 0 || metCDLastSuccessUnix.Load() != 0 {
		t.Errorf("resetMetricsForTest did not zero the CD-pipeline counters/gauges")
	}
}

func TestEscapeLabelValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{`plain`, `plain`},
		{`a"b`, `a\"b`},
		{`a\b`, `a\\b`},
		{"line1\nline2", `line1\nline2`},
	}
	for _, c := range cases {
		if got := escapeLabelValue(c.in); got != c.want {
			t.Errorf("escapeLabelValue(%q): got %q want %q", c.in, got, c.want)
		}
	}
}

// TestRegisterHandlers_MetricsRoute pins /metrics on the mux, open, serving the
// exposition.
func TestRegisterHandlers_MetricsRoute(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	RegisterHandlersOn(mux, cfg)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics via mux: got %d want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type: got %q want text/plain…", ct)
	}
	if !strings.Contains(rec.Body.String(), "cystemd_build_info") {
		t.Errorf("mux-served /metrics missing build_info")
	}
}
