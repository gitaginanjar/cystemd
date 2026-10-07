package service

// metrics.go — hand-rolled Prometheus text exposition (0.0.4) for the whole
// process: no client library, no OTel SDK (the OTel Collector scrapes it).
// Every value is an atomic (labeled families: sync.Map → *atomic.Int64) because
// many goroutines write while /metrics reads — keep the suite -race clean.
// See ADR-0002.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ─── counters & gauges ──────────────────────────────────────────────────────
//
// Counters only ever increase (Prometheus `counter`); the *Unix gauges hold the
// wall-clock second of the last successful event (0 until the first one).
var (
	// Vault token renewal, one outcome per RenewVaultToken call — except a
	// collapsed lease, whose successful pre-emptive re-auth adds reauth after
	// success: success = renew-self accepted; reauth = a fresh InitVault login
	// produced a live token; failure = no live token.
	metVaultRenewSuccess  atomic.Int64
	metVaultRenewReauth   atomic.Int64
	metVaultRenewFailure  atomic.Int64
	metVaultLastRenewUnix atomic.Int64 // gauge: unix seconds of the last success/reauth

	// Vault secret fetch, per fetch operation: FetchAndWriteSecrets plus one
	// fetchServiceSecrets per managed entry, so N consumers → N+1 per tick.
	metSecretFetchSuccess  atomic.Int64
	metSecretFetchFailure  atomic.Int64
	metSecretLastFetchUnix atomic.Int64

	// Template render + reload-hook outcomes.
	metTemplateRenderSuccess  atomic.Int64
	metTemplateRenderFailure  atomic.Int64
	metTemplateLastRenderUnix atomic.Int64
	metTemplateReloadSuccess  atomic.Int64
	metTemplateReloadFailure  atomic.Int64

	// Continuous Deployment (cd.go), genuine attempts only — a disabled loop,
	// autosync=false drift or a stopped unit records nothing: cycle = the main
	// git_configuration → config.yml pipeline; clone = the main clone and every
	// per-branch git_values clone (CD and /sync); install = every dnf install
	// (managed services and self-update); restart = restartIfRunningForUpdate.
	metCDCycleSuccess    atomic.Int64
	metCDCycleFailure    atomic.Int64
	metCDLastSuccessUnix atomic.Int64 // gauge: unix seconds of the last successful CD cycle

	metCDCloneSuccess atomic.Int64
	metCDCloneFailure atomic.Int64

	metCDInstallSuccess atomic.Int64
	metCDInstallFailure atomic.Int64

	metCDRestartSuccess atomic.Int64
	metCDRestartFailure atomic.Int64

	// Unix second of package init (≈ process start): process_start_time_seconds.
	metProcessStartUnix atomic.Int64

	// Labeled families: only label combinations that occurred are emitted (the
	// fixed-label families above emit every series, zeros included).
	metHTTPRequests *labelCounter // {path, method, status}
	metHTTPDuration *histogramMap // path → histogram
	metAudit        *labelCounter // {result}
	metAuth         *labelCounter // {result}

	// metPanicRecovered counts panics recovered at a background-loop cycle
	// boundary, by loop. Unlike the rest (a degraded dependency) it means a
	// cystemd BUG: alert on any non-zero value. See ADR-0014.
	metPanicRecovered *labelCounter // {loop}
)

// httpDurationBuckets are the HTTP duration bucket bounds in seconds, from
// sub-millisecond open endpoints to multi-second actions; anything slower than
// 10s lands only in +Inf.
var httpDurationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// metricsHostname is target_info's host_name, resolved once at init ("unknown"
// on error).
var metricsHostname = func() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "unknown"
	}
	return h
}()

func init() {
	metProcessStartUnix.Store(time.Now().Unix())
	metHTTPRequests = newLabelCounter("path", "method", "status")
	metHTTPDuration = &histogramMap{buckets: httpDurationBuckets}
	metAudit = newLabelCounter("result")
	metAuth = newLabelCounter("result")
	metPanicRecovered = newLabelCounter("loop")
}

// ─── labeled primitives ─────────────────────────────────────────────────────

// labelSep (NUL) joins label values into a map key; NUL never occurs in a
// recorded value, so the join/split round-trip is lossless.
const labelSep = "\x00"

// labelCounter is a counter over fixed label names: one *atomic.Int64 per
// joined label-value key in a sync.Map, exposed one series per observed
// combination, sorted.
type labelCounter struct {
	labelNames []string
	store      sync.Map // map[string]*atomic.Int64
}

func newLabelCounter(names ...string) *labelCounter {
	return &labelCounter{labelNames: names}
}

// observe increments the series for values, which must match labelNames (a
// missing value is exposed as "").
func (lc *labelCounter) observe(values ...string) {
	key := strings.Join(values, labelSep)
	v, _ := lc.store.LoadOrStore(key, new(atomic.Int64))
	v.(*atomic.Int64).Add(1)
}

// reset drops every observed series (test-only).
func (lc *labelCounter) reset() {
	lc.store.Range(func(k, _ any) bool { lc.store.Delete(k); return true })
}

// histogram is a hand-rolled Prometheus histogram: non-cumulative atomic
// per-bucket counts, overflow above the last bound, and the sum in integer ns.
// Cumulative buckets, +Inf and _count are derived at exposition — never add a
// separately-incremented total.
// Why: DOCS/CLAUDE.md § Metrics / observability
type histogram struct {
	bucket   []float64      // upper bounds, ascending, excluding +Inf
	count    []atomic.Int64 // per-bucket count (non-cumulative), len == len(bucket)
	overflow atomic.Int64   // observations exceeding every explicit bucket (implicit +Inf-only)
	sumNs    atomic.Int64   // sum of observations in nanoseconds
}

func newHistogram(buckets []float64) *histogram {
	return &histogram{bucket: buckets, count: make([]atomic.Int64, len(buckets))}
}

// observe increments exactly one counter — the first bucket whose bound d does
// not exceed, else overflow — and adds d to the nanosecond sum.
func (h *histogram) observe(d time.Duration) {
	secs := d.Seconds()
	for i, le := range h.bucket {
		if secs <= le {
			h.count[i].Add(1)
			h.sumNs.Add(int64(d))
			return
		}
	}
	h.overflow.Add(1)
	h.sumNs.Add(int64(d))
}

// histogramMap holds one histogram per label value (the path-labeled HTTP
// duration).
type histogramMap struct {
	buckets []float64
	store   sync.Map // map[string]*histogram
}

func (hm *histogramMap) observe(label string, d time.Duration) {
	v, _ := hm.store.LoadOrStore(label, newHistogram(hm.buckets))
	v.(*histogram).observe(d)
}

// reset drops every histogram (test-only).
func (hm *histogramMap) reset() {
	hm.store.Range(func(k, _ any) bool { hm.store.Delete(k); return true })
}

// formatBucket renders a bucket upper bound as Prometheus expects the `le`
// value: "0.005", "1", "2.5", …
func formatBucket(le float64) string {
	return strconv.FormatFloat(le, 'f', -1, 64)
}

// formatSeconds renders an integer-nanosecond sum as a seconds float string.
func formatSeconds(ns int64) string {
	return strconv.FormatFloat(float64(ns)/1e9, 'f', 9, 64)
}

func metricsNowUnix() int64 { return time.Now().Unix() }

// ─── recorders (called from the instrumented code paths) ────────────────────

func recordVaultRenewSuccess() {
	metVaultRenewSuccess.Add(1)
	metVaultLastRenewUnix.Store(metricsNowUnix())
}

func recordVaultRenewReauth() {
	metVaultRenewReauth.Add(1)
	metVaultLastRenewUnix.Store(metricsNowUnix())
}

func recordVaultRenewFailure() { metVaultRenewFailure.Add(1) }

func recordSecretFetch(ok bool) {
	if ok {
		metSecretFetchSuccess.Add(1)
		metSecretLastFetchUnix.Store(metricsNowUnix())
		return
	}
	metSecretFetchFailure.Add(1)
}

func recordTemplateRender(ok bool) {
	if ok {
		metTemplateRenderSuccess.Add(1)
		metTemplateLastRenderUnix.Store(metricsNowUnix())
		return
	}
	metTemplateRenderFailure.Add(1)
}

func recordTemplateReload(ok bool) {
	if ok {
		metTemplateReloadSuccess.Add(1)
		return
	}
	metTemplateReloadFailure.Add(1)
}

// recordCDCycle records one RunContinuousDeployment cycle of the main
// git_configuration → config.yml pipeline; called only past the skip checks,
// so a disabled loop records nothing.
func recordCDCycle(ok bool) {
	if ok {
		metCDCycleSuccess.Add(1)
		metCDLastSuccessUnix.Store(metricsNowUnix())
		return
	}
	metCDCycleFailure.Add(1)
}

// recordCDClone records one git clone by cd.go: the main config.yml clone or a
// per-branch git_values clone (CD loop and /sync; /diff passes nil).
func recordCDClone(ok bool) {
	if ok {
		metCDCloneSuccess.Add(1)
		return
	}
	metCDCloneFailure.Add(1)
}

// recordCDInstall records one dnf install attempted by runServiceVersionInstall
// or RunSelfVersionInstall; a current version or autosync=false drift is not an
// attempt. A failed self-install has no other symptom: alert on "failure".
func recordCDInstall(ok bool) {
	if ok {
		metCDInstallSuccess.Add(1)
		return
	}
	metCDInstallFailure.Add(1)
}

// recordCDRestart records one restart attempted by restartIfRunningForUpdate
// (version, config or .env update); a unit left alone by no-resurrect is not.
func recordCDRestart(ok bool) {
	if ok {
		metCDRestartSuccess.Add(1)
		return
	}
	metCDRestartFailure.Add(1)
}

// recordHTTPRequest counts one HTTP transaction (RED rate/errors). route is
// already normalised (a known route or "other").
func recordHTTPRequest(route, method string, status int) {
	metHTTPRequests.observe(route, method, strconv.Itoa(status))
}

// recordHTTPDuration records one HTTP request's duration into the per-route
// histogram (RED duration).
func recordHTTPDuration(route string, d time.Duration) {
	metHTTPDuration.observe(route, d)
}

// recordAudit increments the audit-outcome counter. result is the lowercased
// audit result: "allowed", "denied", or "failed".
func recordAudit(result string) { metAudit.observe(result) }

// recordPanicRecovered increments the recovered-panic counter for a background
// loop. loop is the loop's stable name ("cd", "vault-renewal", …).
func recordPanicRecovered(loop string) { metPanicRecovered.observe(loop) }

// recordAuth increments the JWT-auth-outcome counter. result is "accepted",
// "missing", or "invalid".
func recordAuth(result string) { metAuth.observe(result) }

// resetMetricsForTest zeroes every metric and drops the unit-health cache.
// Test-only: package-global state would leak across tests.
func resetMetricsForTest() {
	for _, m := range []*atomic.Int64{
		&metVaultRenewSuccess, &metVaultRenewReauth, &metVaultRenewFailure, &metVaultLastRenewUnix,
		&metSecretFetchSuccess, &metSecretFetchFailure, &metSecretLastFetchUnix,
		&metTemplateRenderSuccess, &metTemplateRenderFailure, &metTemplateLastRenderUnix,
		&metTemplateReloadSuccess, &metTemplateReloadFailure,
		&metCDCycleSuccess, &metCDCycleFailure, &metCDLastSuccessUnix,
		&metCDCloneSuccess, &metCDCloneFailure,
		&metCDInstallSuccess, &metCDInstallFailure,
		&metCDRestartSuccess, &metCDRestartFailure,
	} {
		m.Store(0)
	}
	metHTTPRequests.reset()
	metHTTPDuration.reset()
	metAudit.reset()
	metAuth.reset()
	metPanicRecovered.reset()
	resetUnitHealthCacheForTest()
}

// ─── exposition ─────────────────────────────────────────────────────────────

// escapeLabelValue escapes a Prometheus label value per the exposition format:
// backslash, double-quote, and newline are the only special characters.
func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// writeLabelCounter emits HELP + TYPE, then one escaped series per observed
// label combination, sorted; with no observations only the header.
func writeLabelCounter(w io.Writer, name, help string, lc *labelCounter) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)

	type row struct {
		values []string
		n      int64
	}
	var rows []row
	lc.store.Range(func(k, v any) bool {
		rows = append(rows, row{strings.Split(k.(string), labelSep), v.(*atomic.Int64).Load()})
		return true
	})
	sort.Slice(rows, func(i, j int) bool {
		return strings.Join(rows[i].values, labelSep) < strings.Join(rows[j].values, labelSep)
	})
	for _, r := range rows {
		labels := make([]string, len(lc.labelNames))
		for i, ln := range lc.labelNames {
			val := ""
			if i < len(r.values) {
				val = r.values[i]
			}
			labels[i] = fmt.Sprintf("%s=\"%s\"", ln, escapeLabelValue(val))
		}
		fmt.Fprintf(w, "%s{%s} %d\n", name, strings.Join(labels, ","), r.n)
	}
}

// writeHistogramMap emits HELP + TYPE, then per label value (sorted) the
// cumulative _bucket{le=…} lines, +Inf, _sum and _count; with no observations
// only the header.
func writeHistogramMap(w io.Writer, name, help, labelName string, hm *histogramMap) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s histogram\n", name)

	type rec struct {
		label string
		h     *histogram
	}
	var recs []rec
	hm.store.Range(func(k, v any) bool {
		recs = append(recs, rec{k.(string), v.(*histogram)})
		return true
	})
	sort.Slice(recs, func(i, j int) bool { return recs[i].label < recs[j].label })

	for _, r := range recs {
		lbl := fmt.Sprintf("%s=\"%s\"", labelName, escapeLabelValue(r.label))
		// cum only grows and +Inf/_count = cum + overflow (read after), so
		// bucket <= +Inf holds however a concurrent observe interleaves.
		var cum int64
		for i, le := range r.h.bucket {
			cum += r.h.count[i].Load()
			fmt.Fprintf(w, "%s_bucket{%s,le=\"%s\"} %d\n", name, lbl, formatBucket(le), cum)
		}
		total := cum + r.h.overflow.Load()
		fmt.Fprintf(w, "%s_bucket{%s,le=\"+Inf\"} %d\n", name, lbl, total)
		fmt.Fprintf(w, "%s_sum{%s} %s\n", name, lbl, formatSeconds(r.h.sumNs.Load()))
		fmt.Fprintf(w, "%s_count{%s} %d\n", name, lbl, total)
	}
}

// writeMetrics streams the 0.0.4 exposition: one HELP + TYPE per family; every
// fixed-label series on every scrape (zeros included, degraded too); labeled
// families only observed combinations; no OpenMetrics # EOF.
func writeMetrics(w io.Writer, cfg *Config) {
	// Scrape-time Vault auth gauges — always current, read from the singleton.
	var vaultUp, leaseDuration, renewable int64
	if st := GetVaultClient(); st != nil {
		vaultUp = 1
		leaseDuration = int64(st.LeaseDuration)
		if st.Renewable {
			renewable = 1
		}
	}

	// ── identity / resource ────────────────────────────────────────────────
	fmt.Fprintln(w, "# HELP cystemd_build_info Build metadata; constant value 1, identity carried in labels.")
	fmt.Fprintln(w, "# TYPE cystemd_build_info gauge")
	fmt.Fprintf(w, "cystemd_build_info{version=\"%s\",commit=\"%s\",go_version=\"%s\"} 1\n",
		escapeLabelValue(Version()), escapeLabelValue(Commit()), escapeLabelValue(GoVersion()))

	// target_info: the Collector's prometheus receiver maps it onto the OTLP
	// Resource (rename underscores to dotted OTel names there if needed).
	fmt.Fprintln(w, "# HELP target_info OTel resource attributes for the scrape target; the OTel Collector prometheus receiver maps this to OTLP resource attributes.")
	fmt.Fprintln(w, "# TYPE target_info gauge")
	fmt.Fprintf(w, "target_info{service_name=\"cystemd\",service_version=\"%s\",host_name=\"%s\"} 1\n",
		escapeLabelValue(Version()), escapeLabelValue(metricsHostname))

	// ── process / runtime ──────────────────────────────────────────────────
	fmt.Fprintln(w, "# HELP process_start_time_seconds Start time of the process since the unix epoch in seconds.")
	fmt.Fprintln(w, "# TYPE process_start_time_seconds gauge")
	fmt.Fprintf(w, "process_start_time_seconds %d\n", metProcessStartUnix.Load())

	// ReadMemStats briefly stops the world — negligible at a 15–60s scrape.
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	fmt.Fprintln(w, "# HELP go_goroutines Number of goroutines that currently exist.")
	fmt.Fprintln(w, "# TYPE go_goroutines gauge")
	fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())

	fmt.Fprintln(w, "# HELP go_memstats_alloc_bytes Number of bytes allocated and still in use.")
	fmt.Fprintln(w, "# TYPE go_memstats_alloc_bytes gauge")
	fmt.Fprintf(w, "go_memstats_alloc_bytes %d\n", ms.Alloc)

	fmt.Fprintln(w, "# HELP go_memstats_sys_bytes Number of bytes obtained from the system.")
	fmt.Fprintln(w, "# TYPE go_memstats_sys_bytes gauge")
	fmt.Fprintf(w, "go_memstats_sys_bytes %d\n", ms.Sys)

	fmt.Fprintln(w, "# HELP go_memstats_heap_inuse_bytes Number of heap bytes in use spans.")
	fmt.Fprintln(w, "# TYPE go_memstats_heap_inuse_bytes gauge")
	fmt.Fprintf(w, "go_memstats_heap_inuse_bytes %d\n", ms.HeapInuse)

	fmt.Fprintln(w, "# HELP go_memstats_next_gc_bytes Target heap size of the next GC cycle (GOGC-driven).")
	fmt.Fprintln(w, "# TYPE go_memstats_next_gc_bytes gauge")
	fmt.Fprintf(w, "go_memstats_next_gc_bytes %d\n", ms.NextGC)

	// ── HTTP RED / audit / auth (labeled) ──────────────────────────────────
	writeLabelCounter(w, "cystemd_http_requests_total",
		"HTTP requests processed by route, HTTP method, and status code (RED: Rate + Errors). Only combinations that have occurred are emitted.",
		metHTTPRequests)
	writeHistogramMap(w, "cystemd_http_request_duration_seconds",
		"HTTP request duration in seconds by route (RED: Duration).",
		"path", metHTTPDuration)
	writeLabelCounter(w, "cystemd_audit_total",
		"Audited control-plane actions by outcome: allowed (authorised + executed), denied, failed.",
		metAudit)
	writeLabelCounter(w, "cystemd_panic_recovered_total",
		"Panics recovered at a background-loop cycle boundary, by loop. The daemon survived and the next cycle retries, but a non-zero value is a bug and should alert.",
		metPanicRecovered)
	writeLabelCounter(w, "cystemd_auth_total",
		"JWT authentication outcomes by result: accepted, missing (no Authorization header), invalid (bad/expired token). Recorded only when auth is enabled.",
		metAuth)

	// ── Vault ──────────────────────────────────────────────────────────────
	fmt.Fprintln(w, "# HELP cystemd_vault_up 1 if cystemd currently holds a live Vault client, 0 if degraded/unconfigured.")
	fmt.Fprintln(w, "# TYPE cystemd_vault_up gauge")
	fmt.Fprintf(w, "cystemd_vault_up %d\n", vaultUp)

	fmt.Fprintln(w, "# HELP cystemd_vault_token_lease_duration_seconds Lease duration granted to the Vault token at its last renewal (NOT remaining TTL; compute remaining as this minus (now - last_renewal_timestamp)).")
	fmt.Fprintln(w, "# TYPE cystemd_vault_token_lease_duration_seconds gauge")
	fmt.Fprintf(w, "cystemd_vault_token_lease_duration_seconds %d\n", leaseDuration)

	fmt.Fprintln(w, "# HELP cystemd_vault_token_renewable 1 if the current Vault token is renewable, else 0.")
	fmt.Fprintln(w, "# TYPE cystemd_vault_token_renewable gauge")
	fmt.Fprintf(w, "cystemd_vault_token_renewable %d\n", renewable)

	fmt.Fprintln(w, "# HELP cystemd_vault_token_renewal_total Vault token renewal cycles by outcome (success=renew-self, reauth=re-login recovered, failure=both failed).")
	fmt.Fprintln(w, "# TYPE cystemd_vault_token_renewal_total counter")
	fmt.Fprintf(w, "cystemd_vault_token_renewal_total{result=\"success\"} %d\n", metVaultRenewSuccess.Load())
	fmt.Fprintf(w, "cystemd_vault_token_renewal_total{result=\"reauth\"} %d\n", metVaultRenewReauth.Load())
	fmt.Fprintf(w, "cystemd_vault_token_renewal_total{result=\"failure\"} %d\n", metVaultRenewFailure.Load())

	fmt.Fprintln(w, "# HELP cystemd_vault_token_last_renewal_timestamp_seconds Unix time of the last successful token renewal or re-auth (0 if none yet).")
	fmt.Fprintln(w, "# TYPE cystemd_vault_token_last_renewal_timestamp_seconds gauge")
	fmt.Fprintf(w, "cystemd_vault_token_last_renewal_timestamp_seconds %d\n", metVaultLastRenewUnix.Load())

	fmt.Fprintln(w, "# HELP cystemd_secret_fetch_total Vault secret fetch operations by outcome (one per fetch op: cystemd's own .env plus one per managed service per refresh tick).")
	fmt.Fprintln(w, "# TYPE cystemd_secret_fetch_total counter")
	fmt.Fprintf(w, "cystemd_secret_fetch_total{result=\"success\"} %d\n", metSecretFetchSuccess.Load())
	fmt.Fprintf(w, "cystemd_secret_fetch_total{result=\"failure\"} %d\n", metSecretFetchFailure.Load())

	fmt.Fprintln(w, "# HELP cystemd_secret_last_fetch_timestamp_seconds Unix time of the last successful Vault secret read (0 if none yet).")
	fmt.Fprintln(w, "# TYPE cystemd_secret_last_fetch_timestamp_seconds gauge")
	fmt.Fprintf(w, "cystemd_secret_last_fetch_timestamp_seconds %d\n", metSecretLastFetchUnix.Load())

	fmt.Fprintln(w, "# HELP cystemd_template_render_total Template render operations by outcome (success includes an unchanged in-sync render).")
	fmt.Fprintln(w, "# TYPE cystemd_template_render_total counter")
	fmt.Fprintf(w, "cystemd_template_render_total{result=\"success\"} %d\n", metTemplateRenderSuccess.Load())
	fmt.Fprintf(w, "cystemd_template_render_total{result=\"failure\"} %d\n", metTemplateRenderFailure.Load())

	fmt.Fprintln(w, "# HELP cystemd_template_last_render_timestamp_seconds Unix time of the last successful template render (0 if none yet).")
	fmt.Fprintln(w, "# TYPE cystemd_template_last_render_timestamp_seconds gauge")
	fmt.Fprintf(w, "cystemd_template_last_render_timestamp_seconds %d\n", metTemplateLastRenderUnix.Load())

	fmt.Fprintln(w, "# HELP cystemd_template_reload_total Template reload-hook executions by outcome.")
	fmt.Fprintln(w, "# TYPE cystemd_template_reload_total counter")
	fmt.Fprintf(w, "cystemd_template_reload_total{result=\"success\"} %d\n", metTemplateReloadSuccess.Load())
	fmt.Fprintf(w, "cystemd_template_reload_total{result=\"failure\"} %d\n", metTemplateReloadFailure.Load())

	// ── Continuous Deployment ──────────────────────────────────────────────
	fmt.Fprintln(w, "# HELP cystemd_cd_cycle_total Continuous-deployment cycles by outcome for the main git_configuration to config.yml pipeline (success/failure); a disabled/unconfigured loop records neither.")
	fmt.Fprintln(w, "# TYPE cystemd_cd_cycle_total counter")
	fmt.Fprintf(w, "cystemd_cd_cycle_total{result=\"success\"} %d\n", metCDCycleSuccess.Load())
	fmt.Fprintf(w, "cystemd_cd_cycle_total{result=\"failure\"} %d\n", metCDCycleFailure.Load())

	fmt.Fprintln(w, "# HELP cystemd_cd_last_success_timestamp_seconds Unix time of the last successful continuous-deployment cycle (0 if none yet).")
	fmt.Fprintln(w, "# TYPE cystemd_cd_last_success_timestamp_seconds gauge")
	fmt.Fprintf(w, "cystemd_cd_last_success_timestamp_seconds %d\n", metCDLastSuccessUnix.Load())

	fmt.Fprintln(w, "# HELP cystemd_cd_clone_total Git clone attempts by outcome, across the main config.yml clone and every per-branch git_values clone (the latter shared by the periodic CD loop and the on-demand /sync endpoint).")
	fmt.Fprintln(w, "# TYPE cystemd_cd_clone_total counter")
	fmt.Fprintf(w, "cystemd_cd_clone_total{result=\"success\"} %d\n", metCDCloneSuccess.Load())
	fmt.Fprintf(w, "cystemd_cd_clone_total{result=\"failure\"} %d\n", metCDCloneFailure.Load())

	fmt.Fprintln(w, "# HELP cystemd_cd_install_total RPM install attempts (dnf install) by outcome, one per git_values_version_key drift actually applied (autosync-gated), plus cystemd's own self-update attempts.")
	fmt.Fprintln(w, "# TYPE cystemd_cd_install_total counter")
	fmt.Fprintf(w, "cystemd_cd_install_total{result=\"success\"} %d\n", metCDInstallSuccess.Load())
	fmt.Fprintf(w, "cystemd_cd_install_total{result=\"failure\"} %d\n", metCDInstallFailure.Load())

	fmt.Fprintln(w, "# HELP cystemd_cd_restart_total Automated service restarts attempted under the no-resurrect rule (restartIfRunningForUpdate), triggered by a CD version/config change or a Vault .env update; a stopped unit left alone by that rule is not counted.")
	fmt.Fprintln(w, "# TYPE cystemd_cd_restart_total counter")
	fmt.Fprintf(w, "cystemd_cd_restart_total{result=\"success\"} %d\n", metCDRestartSuccess.Load())
	fmt.Fprintf(w, "cystemd_cd_restart_total{result=\"failure\"} %d\n", metCDRestartFailure.Load())

	// ── managed units (sampled at scrape time; see unit_metrics.go) ───────
	writeUnitMetrics(w, cfg)
}

// handleMetrics serves /metrics: open, GET-only; only counts, timestamps and
// the token lease duration — never a secret value or the token itself.
func handleMetrics(w http.ResponseWriter, r *http.Request, cfg *Config) {
	if r.Method != http.MethodGet {
		jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writeMetrics(w, cfg)
}
