package service

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RegisterHandlers registers usage and API endpoints on the default http mux.
func RegisterHandlers(cfg *Config) {
	RegisterHandlersOn(http.DefaultServeMux, cfg)
}

// RegisterHandlersOn registers usage and API endpoints on the provided mux.
// Prefer this in tests to avoid global-mux collisions; production uses RegisterHandlers.
func RegisterHandlersOn(mux *http.ServeMux, cfg *Config) {
	// Snapshot cfg.Auth under cfg.mu and pass the snapshot, never &cfg.Auth: the
	// hot-reload pollers already run and write it under cfg.mu.Lock. RequireJWT
	// reads it only at registration, so auth.enabled still needs a restart.
	cfg.mu.RLock()
	authCfg := cfg.Auth
	cfg.mu.RUnlock()

	// Usage handler: serve usage JSON at any path that isn't a defined endpoint
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		Debugf("usage page served: method=%s path=%q host=%s remote=%s user_agent=%q",
			r.Method, r.URL.Path, r.Host, r.RemoteAddr, r.UserAgent())
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.MarshalIndent(buildUsageJSON(r, cfg), "", "  ")
		_, _ = w.Write(data)
	})

	// Action endpoints (GET only, protected by JWT if enabled) — plain text
	mux.HandleFunc("/restart", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		HandleAction(w, r, cfg, wrapWithStatus(RestartService))
	}))
	mux.HandleFunc("/start", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		HandleAction(w, r, cfg, wrapWithStatus(StartService))
	}))
	mux.HandleFunc("/stop", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		HandleAction(w, r, cfg, wrapWithStatus(StopService))
	}))
	mux.HandleFunc("/enable", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		HandleAction(w, r, cfg, wrapWithStatus(EnableService))
	}))
	mux.HandleFunc("/disable", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		HandleAction(w, r, cfg, wrapWithStatus(DisableService))
	}))

	// Per-service file reads (GET only, JWT if enabled) — plain text. /env
	// returns KEY names only, never a value.
	mux.HandleFunc("/config", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		HandleAction(w, r, cfg, readServiceConfig(cfg))
	}))
	mux.HandleFunc("/env", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		HandleAction(w, r, cfg, readServiceEnvKeys(cfg))
	}))

	// Host-scoped, multi-service endpoints (JWT if enabled): own handlers, not
	// HandleAction — ?service= is ignored and one [AUDIT] line is emitted per
	// managed service. /sync forces a sync; /diff is live and read-only;
	// /lastdiff serves the cache.
	mux.HandleFunc("/sync", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		handleSync(w, r, cfg)
	}))
	mux.HandleFunc("/diff", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		handleDiff(w, r, cfg)
	}))
	mux.HandleFunc("/lastdiff", RequireJWT(&authCfg, func(w http.ResponseWriter, r *http.Request) {
		handleLastDiff(w, r, cfg)
	}))

	// Open JSON endpoints, sharing the statusVersionConcurrencySem cap.
	mux.HandleFunc("/status", withStatusVersionConcurrencyLimit(func(w http.ResponseWriter, r *http.Request) {
		HandleJSONAction(w, r, cfg, StatusService)
	}))
	mux.HandleFunc("/version", withStatusVersionConcurrencyLimit(func(w http.ResponseWriter, r *http.Request) {
		HandleJSONAction(w, r, cfg, VersionService)
	}))

	// Health endpoint: process-level liveness probe for load balancers
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			jsonError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		data, _ := json.Marshal(HealthJSON{Status: "ok"})
		_, _ = w.Write(data)
	})

	// Metrics endpoint: Prometheus text exposition (open, GET-only) — never a
	// secret value or a token.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		handleMetrics(w, r, cfg)
	})

	Infof("routes registered: / /restart /start /stop /enable /disable /config /env /sync /diff /lastdiff /status /version /health /metrics")
}

// ─── /status + /version concurrency limit ──────────────────────────────────

// statusVersionConcurrencyLimit caps concurrent /status + /version requests:
// ONE semaphore shared by both open endpoints, each of which can block ~85s on
// a wedged D-Bus or 10s on the rpmdb lock. Never reuse diffConcurrencySem or
// syncMu (non-reentrant) for it.
// Why: DOCS/CLAUDE.md § `/status` + `/version` concurrency cap
const statusVersionConcurrencyLimit = 8

var statusVersionConcurrencySem = make(chan struct{}, statusVersionConcurrencyLimit)

// withStatusVersionConcurrencyLimit wraps a /status or /version handler with a
// non-blocking try-acquire of statusVersionConcurrencySem (released by defer on
// every path). Over capacity it rejects at once: 503 + jsonError + exactly one
// FAILED audit line.
func withStatusVersionConcurrencyLimit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case statusVersionConcurrencySem <- struct{}{}:
			defer func() { <-statusVersionConcurrencySem }()
			next(w, r)
		default:
			start := time.Now()
			Warningf("%s: rejected — concurrency limit reached (%d active)", r.URL.Path, statusVersionConcurrencyLimit)
			svc := r.URL.Query().Get("service")
			switch {
			case svc == "":
				svc = "<none>"
			case !validServiceName.MatchString(svc):
				svc = "<invalid>"
			}
			auditLog(r, svc, r.URL.Path, "FAILED", time.Since(start))
			jsonError(w, http.StatusServiceUnavailable, fmt.Sprintf("too many concurrent %s requests; retry shortly", r.URL.Path))
		}
	}
}

// handleDiff serves /diff: a live, read-only diff of every managed service
// against git values and Vault (?service= ignored; audit via
// writeDiffResponse). Over diffConcurrencyLimit it returns busy-500 at once.
func handleDiff(w http.ResponseWriter, r *http.Request, cfg *Config) {
	start := time.Now()
	if r.Method != http.MethodGet {
		Warningf("method %s not allowed on %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		auditLog(r, "<none>", r.URL.Path, "DENIED", time.Since(start))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Non-blocking try-acquire: never queue behind a running /diff.
	select {
	case diffConcurrencySem <- struct{}{}:
		defer func() { <-diffConcurrencySem }()
	default:
		Warningf("diff: rejected — concurrency limit reached (%d active)", diffConcurrencyLimit)
		auditLog(r, "<none>", r.URL.Path, "FAILED", time.Since(start))
		jsonError(w, http.StatusInternalServerError, "a /diff is already in progress; retry shortly")
		return
	}

	results, err := ComputeCurrentDiffs(cfg)
	writeDiffResponse(w, r, start, results, err)
}

// handleLastDiff serves /lastdiff: the cached diff of every managed service,
// never cloning or reading Vault. A service not yet diffed reports empty
// config/env diffs and an empty version.
func handleLastDiff(w http.ResponseWriter, r *http.Request, cfg *Config) {
	start := time.Now()
	if r.Method != http.MethodGet {
		Warningf("method %s not allowed on %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		auditLog(r, "<none>", r.URL.Path, "DENIED", time.Since(start))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	managed := cfg.managedEntries()
	if len(managed) == 0 {
		Warningf("lastdiff: no managed services resolved for this host — nothing to report")
		auditLog(r, "<none>", r.URL.Path, "FAILED", time.Since(start))
		jsonError(w, http.StatusInternalServerError, "no managed service is configured for this host")
		return
	}
	results := getLastDiffs(serviceNames(managed))
	writeDiffResponse(w, r, start, results, nil)
}

// writeDiffResponse renders results as DiffResponseJSON and emits one [AUDIT]
// line per service (one "<none>" line when results is empty) — the host-scoped
// exception to "exactly one auditLog per request", shared with /sync.
func writeDiffResponse(w http.ResponseWriter, r *http.Request, start time.Time, results []ServiceDiffJSON, err error) {
	result := "ALLOWED"
	if err != nil {
		result = "FAILED"
	}
	if len(results) == 0 {
		auditLog(r, "<none>", r.URL.Path, result, time.Since(start))
	} else {
		for _, sd := range results {
			auditLog(r, sd.Service, r.URL.Path, result, time.Since(start))
		}
	}

	if err != nil {
		Errorf("action %s failed: %v", r.URL.Path, err)
		jsonError(w, http.StatusInternalServerError, fmt.Sprintf("%v", err))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	data, _ := json.MarshalIndent(DiffResponseJSON{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Services:    results,
	}, "", "  ")
	_, _ = w.Write(data)
}

// handleSync serves /sync (see RunServiceSync). ?service= is ignored; one
// [AUDIT] line is emitted per managed service acted on, so every mutated unit
// is recorded — or one "<none>" line on 405, busy or nothing managed (→ 500).
// ALLOWED means "authorised + executed", not "systemctl succeeded".
func handleSync(w http.ResponseWriter, r *http.Request, cfg *Config) {
	start := time.Now()
	if r.Method != http.MethodGet {
		Warningf("method %s not allowed on %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		auditLog(r, "<none>", r.URL.Path, "DENIED", time.Since(start))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	summary, synced, err := RunServiceSync(cfg)

	result := "ALLOWED"
	if err != nil {
		result = "FAILED"
	}
	if len(synced) == 0 {
		auditLog(r, "<none>", r.URL.Path, result, time.Since(start))
	} else {
		for _, svc := range synced {
			auditLog(r, svc, r.URL.Path, result, time.Since(start))
		}
	}

	if err != nil {
		Errorf("action %s failed: %v", r.URL.Path, err)
		http.Error(w, fmt.Sprintf("%v", err), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(summary))
}

// wrapWithStatus runs action, then appends the unit's StatusService JSON; only
// action's error is returned (StatusService never fails — a D-Bus failure comes
// back as a JSON error object).
func wrapWithStatus(action func(string) (string, error)) func(string) (string, error) {
	return func(service string) (string, error) {
		out1, err1 := action(service)
		out2, _ := StatusService(service)
		combined := out1 + "\n\n--- Status ---\n" + out2
		return combined, err1
	}
}

// resolveServiceDir returns ${workdir}/${svc} and the managed entry whose
// Service == svc (its own workdir and service_config, not the primary entry's);
// error when this host does not manage svc (the whitelist spans all hosts).
// svc must be whitelist-checked: validServiceName still admits a bare "..".
func resolveServiceDir(cfg *Config, svc string) (string, *ServiceEntry, error) {
	for _, e := range cfg.managedEntries() {
		if e.Service == svc {
			entry := e // copy; &entry is stable for the caller's use
			return serviceDir(entry), &entry, nil
		}
	}
	return "", nil, fmt.Errorf("this host does not manage service %q", svc)
}

// readServiceConfig returns an action serving the managed service's raw
// ${workdir}/${service}/${service_config} (default target.config.yml).
func readServiceConfig(cfg *Config) func(string) (string, error) {
	return func(svc string) (string, error) {
		dir, entry, err := resolveServiceDir(cfg, svc)
		if err != nil {
			return "", err
		}
		path := filepath.Join(dir, serviceServiceConfig(*entry))
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read service config %q: %w", path, err)
		}
		return string(data), nil
	}
}

// readServiceEnvKeys returns an action serving the sorted KEY names of the
// managed service's ${workdir}/${service}/.env, one per line — NEVER a value.
func readServiceEnvKeys(cfg *Config) func(string) (string, error) {
	return func(svc string) (string, error) {
		dir, _, err := resolveServiceDir(cfg, svc)
		if err != nil {
			return "", err
		}
		path := filepath.Join(dir, ".env")
		pairs, err := ParseEnvFile(path)
		if err != nil {
			return "", fmt.Errorf("cannot read service env file %q: %w", path, err)
		}
		keys := make([]string, 0, len(pairs))
		for k := range pairs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return strings.Join(keys, "\n"), nil
	}
}

// ─── Usage JSON ────────────────────────────────────────────────────────────

// HealthJSON is the JSON shape returned by the /health endpoint.
type HealthJSON struct {
	Status string `json:"status"`
}

// UsageJSON is the / (catch-all) response. Fields stay in JSON-tag order;
// instance identity lives under Server, so the top level is only the API
// documentation surface.
// Why: DOCS/CLAUDE.md § JSON output sorting
type UsageJSON struct {
	Endpoints []UsageEndpoint `json:"endpoints"`
	Examples  UsageExamples   `json:"examples"`
	Notes     []string        `json:"notes"`
	Server    UsageServer     `json:"server"`
}

// UsageServer describes this running instance: identity, build metadata and
// Vault status. Fields stay in JSON-tag order.
type UsageServer struct {
	BuildTime     string          `json:"build_time"`
	Commit        string          `json:"commit"`
	GeneratedAt   string          `json:"generated_at"`
	GoVersion     string          `json:"go_version"`
	Hostname      string          `json:"hostname"`
	IP            string          `json:"ip"`
	ListenAddress string          `json:"listen_address"`
	Service       string          `json:"service"`
	Vault         VaultStatusJSON `json:"vault"`
	Version       string          `json:"version"`
}

// UsageEndpoint describes one endpoint on the usage page. Fields stay in
// JSON-tag order; Examples must never be empty
// (TestBuildUsageJSON_EveryEndpointHasExamples).
type UsageEndpoint struct {
	AuthRequired bool     `json:"auth_required"`
	Description  string   `json:"description"`
	Examples     []string `json:"examples"`
	Method       string   `json:"method"`
	Path         string   `json:"path"`
	ResponseType string   `json:"response_type"`
}

// UsageExamples contains ready-to-run curl examples grouped by access path.
type UsageExamples struct {
	ViaLoadBalancer []string `json:"via_load_balancer"`
	ViaServerIP     []string `json:"via_server_ip"`
}

// buildUsageJSON constructs the UsageJSON payload from the incoming request
// and the running config.
func buildUsageJSON(r *http.Request, cfg *Config) UsageJSON {
	host := r.Host
	if host == "" {
		host = "localhost"
	}

	listen := cfg.ListenAddress
	if listen == "" {
		listen = ":50080"
	}

	serverIP := deriveLocalIP()

	// Normalize listen port for examples
	port := listen
	if strings.HasPrefix(port, ":") {
		port = port[1:]
	} else if strings.Contains(port, ":") {
		parts := strings.Split(port, ":")
		port = parts[len(parts)-1]
	}

	hostname, _ := os.Hostname()
	cfg.mu.RLock()
	svc := cfg.ServiceName
	cfg.mu.RUnlock()

	// exampleFor returns an endpoint's load-balancer (HTTPS) and direct-IP curl
	// examples: auth adds the Bearer header; serviceScoped adds ?service=<svc>
	// to the direct form when a primary service is resolved.
	exampleFor := func(path string, auth, serviceScoped bool) []string {
		authPrefix := ""
		if auth {
			authPrefix = `-H "Authorization: Bearer <token>" `
		}
		direct := fmt.Sprintf(`curl %s"http://%s:%s%s"`, authPrefix, serverIP, port, path)
		if serviceScoped && svc != "" {
			direct = fmt.Sprintf(`curl %s"http://%s:%s%s?service=%s"`, authPrefix, serverIP, port, path, svc)
		}
		return []string{
			fmt.Sprintf(`curl %s"https://%s%s"`, authPrefix, host, path),
			direct,
		}
	}

	return UsageJSON{
		Server: UsageServer{
			BuildTime:     BuildTime(),
			Commit:        Commit(),
			GeneratedAt:   time.Now().Format(time.RFC3339),
			GoVersion:     GoVersion(),
			Hostname:      hostname,
			IP:            serverIP,
			ListenAddress: listen,
			Service:       "cystemd",
			Vault:         BuildVaultStatus(),
			Version:       Version(),
		},
		Endpoints: []UsageEndpoint{
			{Method: "GET", Path: "/", AuthRequired: false, ResponseType: "application/json", Examples: exampleFor("/", false, false), Description: "This usage page — endpoint catalogue with per-endpoint curl examples; catch-all for any unregistered path"},
			{Method: "GET", Path: "/health", AuthRequired: false, ResponseType: "application/json", Examples: exampleFor("/health", false, false), Description: "Process liveness probe for load balancers — always returns {\"status\":\"ok\"} with HTTP 200"},
			{Method: "GET", Path: "/status", AuthRequired: false, ResponseType: "application/json", Examples: exampleFor("/status", false, true), Description: "Show systemctl status"},
			{Method: "GET", Path: "/version", AuthRequired: false, ResponseType: "application/json", Examples: exampleFor("/version", false, true), Description: "Show RPM package version"},
			{Method: "GET", Path: "/metrics", AuthRequired: false, ResponseType: "text/plain", Examples: exampleFor("/metrics", false, false), Description: "Prometheus metrics for the whole process — HTTP RED (rate/errors/duration), audit & JWT-auth outcomes, uptime, Go runtime, Vault/secret/template counters, and managed-unit health (ActiveState + restart count) sampled from systemd; OTel-Collector compatible (target_info)"},
			{Method: "GET", Path: "/restart", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/restart", true, true), Description: "Restart a systemd service"},
			{Method: "GET", Path: "/start", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/start", true, true), Description: "Start a systemd service"},
			{Method: "GET", Path: "/stop", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/stop", true, true), Description: "Stop a systemd service"},
			{Method: "GET", Path: "/enable", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/enable", true, true), Description: "Enable a systemd service (not recommended for Myproject applications controlled by schedulers)"},
			{Method: "GET", Path: "/disable", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/disable", true, true), Description: "Disable a systemd service (default behavior for Myproject applications)"},
			{Method: "GET", Path: "/config", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/config", true, true), Description: "Return the managed service's runtime config file (${workdir}/${service}/${service_config})"},
			{Method: "GET", Path: "/env", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/env", true, true), Description: "Return only the variable KEY names (never the values) from the managed service's ${workdir}/${service}/.env"},
			{Method: "GET", Path: "/sync", AuthRequired: true, ResponseType: "text/plain", Examples: exampleFor("/sync", true, false), Description: "Force a one-shot sync of this host's managed service (config, version, resource quota, and Vault .env secrets) regardless of the autosync setting; a service that is not running is never started (no-resurrect) — use /start"},
			{Method: "GET", Path: "/diff", AuthRequired: true, ResponseType: "application/json", Examples: exampleFor("/diff", true, false), Description: "Live, read-only diff of every managed service against its upstream sources: config (YAML properties, one line each), version, and .env (variable names only, never values)"},
			{Method: "GET", Path: "/lastdiff", AuthRequired: true, ResponseType: "application/json", Examples: exampleFor("/lastdiff", true, false), Description: "Most recently cached diff for every managed service (populated by the CD loop, secrets refresh, /sync, and /diff) — no new clone or Vault read"},
		},
		Examples: UsageExamples{
			ViaLoadBalancer: []string{
				fmt.Sprintf(`curl "https://%s/health"`, host),
				fmt.Sprintf(`curl "https://%s/version"`, host),
				fmt.Sprintf(`curl "https://%s/status"`, host),
				fmt.Sprintf(`curl "https://%s/restart"`, host),
				fmt.Sprintf(`curl "https://%s/stop"`, host),
				fmt.Sprintf(`curl "https://%s/start"`, host),
				fmt.Sprintf(`curl -H "Authorization: Bearer <token>" "https://%s/sync"`, host),
				fmt.Sprintf(`curl -H "Authorization: Bearer <token>" "https://%s/diff"`, host),
				fmt.Sprintf(`curl -H "Authorization: Bearer <token>" "https://%s/lastdiff"`, host),
			},
			ViaServerIP: []string{
				fmt.Sprintf(`curl "http://%s:%s/version?service=%s"`, serverIP, port, svc),
				fmt.Sprintf(`curl "http://%s:%s/status?service=%s"`, serverIP, port, svc),
				fmt.Sprintf(`curl "http://%s:%s/restart?service=%s"`, serverIP, port, svc),
				// Host-scoped endpoints ignore ?service=, so none is shown.
				fmt.Sprintf(`curl -H "Authorization: Bearer <token>" "http://%s:%s/sync"`, serverIP, port),
				fmt.Sprintf(`curl -H "Authorization: Bearer <token>" "http://%s:%s/diff"`, serverIP, port),
				fmt.Sprintf(`curl -H "Authorization: Bearer <token>" "http://%s:%s/lastdiff"`, serverIP, port),
			},
		},
		Notes: []string{
			"Authentication: provide the JWT via Authorization: Bearer <token> header.",
			"Every endpoints[] entry carries ready-to-run curl examples: an HTTPS load-balancer form and a direct server-IP form (?service= shown where the endpoint accepts it).",
			"Allowed services are derived from continuous_deployment[].service in config.yml (all entries, any host).",
			"All endpoints accept an optional ?service=<name> query parameter; if omitted, service_name (derived from this host's node_pool) is used.",
			"Audit logs are emitted to the systemd journal (journalctl -u cystemd.service -f).",
			"/health, /status, /version, and /metrics are open endpoints — no token required.",
			"/health is a lightweight process liveness probe: HTTP 200 means the server is up; no D-Bus or RPM calls are made.",
			"/restart, /start, /stop, /enable, /disable, /config, /env, /sync, /diff, and /lastdiff require a valid JWT when auth.enabled=true.",
			"/config returns the managed service's runtime config file; /env returns only the variable key names from its .env (never the values).",
			"/sync forces a deploy of this host's managed service (config, version, resource quota, Vault .env) regardless of autosync; it is host-scoped (the ?service= parameter is ignored) and rejects overlapping calls while one is in progress. Restarts follow the no-resurrect rule: only a currently-running service is restarted — a stopped service stays down until started via /start.",
			"/diff performs a live, read-only comparison of every managed service against its upstream sources (config as one-line YAML properties, version, and .env variable NAMES ONLY — never values); /lastdiff serves the most recently cached comparison (populated by the CD loop, secrets refresh, /sync, and /diff) without any new clone or Vault read. Both are host-scoped like /sync.",
			"/metrics exposes whole-process Prometheus metrics (HTTP RED, audit, auth, uptime, Go runtime, Vault/secret/template) and is OTel-Collector compatible: the Collector's prometheus receiver scrapes it and emits OTLP (target_info maps to resource attributes). See README for the pipeline config.",
		},
	}
}

// deriveLocalIP returns the first IPv4 address of an up, non-loopback
// interface, or "127.0.0.1" when there is none.
func deriveLocalIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "127.0.0.1"
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip = ip.To4()
			if ip == nil {
				continue
			}
			return ip.String()
		}
	}
	return "127.0.0.1"
}
