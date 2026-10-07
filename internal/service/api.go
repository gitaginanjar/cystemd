package service

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// validServiceName matches safe systemd unit names: alphanumerics and : . _ @ -
// (template instances like nginx@80). It admits a bare "..": only the whitelist
// (isAllowedService) stops that.
var validServiceName = regexp.MustCompile(`^[a-zA-Z0-9:._@-]+$`)

// isAllowedService reports whether service is whitelisted; an empty whitelist
// allows only cfg.ServiceName. Takes cfg.mu.RLock: StartConfigReload rewrites both.
func isAllowedService(cfg *Config, service string) bool {
	cfg.mu.RLock()
	svcName := cfg.ServiceName
	allowed := cfg.AllowedServices
	cfg.mu.RUnlock()

	if len(allowed) == 0 {
		return service == svcName
	}
	for _, s := range allowed {
		if s == service {
			return true
		}
	}
	return false
}

// auditLog emits one [AUDIT] line (Info, to journald) and counts it in
// cystemd_audit_total. result is ALLOWED, DENIED or FAILED:
// [AUDIT] 2026-02-20T11:25:00Z | Client=10.11.12.10 | Action=/restart | Service=myproject_myapplication5 | Result=ALLOWED | Duration=12ms
// plus "| RemoteAddr=<addr>" when a valid X-Forwarded-For disagrees with the peer.
// Why: DOCS/CLAUDE.md § `HandleAction` log/audit contract
func auditLog(r *http.Request, service string, action string, result string, duration time.Duration) {
	// Socket peer without the port; the raw string when it is not host:port.
	remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		remoteHost = r.RemoteAddr
	}

	// Client = the first X-Forwarded-For entry (unverified; the peer is logged
	// too when they differ), used only if it parses as an IP — a non-IP value
	// could inject "| Field=value" segments into the line.
	xff := r.Header.Get("X-Forwarded-For")
	clientIP := remoteHost
	xffValid := false
	if xff != "" {
		candidate := strings.TrimSpace(strings.Split(xff, ",")[0])
		if net.ParseIP(candidate) != nil {
			clientIP = candidate
			xffValid = true
		} else {
			Warningf("audit: ignoring malformed X-Forwarded-For value %q from %s", candidate, remoteHost)
		}
	}

	timestamp := time.Now().Format(time.RFC3339)

	if xffValid && clientIP != remoteHost {
		Infof("[AUDIT] %s | Client=%s | Action=%s | Service=%s | Result=%s | Duration=%dms | RemoteAddr=%s",
			timestamp, clientIP, action, service, result, duration.Milliseconds(), remoteHost)
	} else {
		Infof("[AUDIT] %s | Client=%s | Action=%s | Service=%s | Result=%s | Duration=%dms",
			timestamp, clientIP, action, service, result, duration.Milliseconds())
	}

	// Mirror into cystemd_audit_total{result}, lowercased.
	recordAudit(strings.ToLower(result))
}

// jsonError writes a JSON-encoded error body and the given HTTP status code.
func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}

// HandleAction is the generic HTTP handler for plain-text action endpoints.
func HandleAction(w http.ResponseWriter, r *http.Request, cfg *Config, action func(string) (string, error)) {
	handleAction(w, r, cfg, action, "text/plain")
}

// HandleJSONAction is the generic handler for JSON action endpoints (/status,
// /version): action returns JSON, and error responses are JSON too.
func HandleJSONAction(w http.ResponseWriter, r *http.Request, cfg *Config, action func(string) (string, error)) {
	handleAction(w, r, cfg, action, "application/json")
}

// handleAction implements HandleAction and HandleJSONAction: GET only, service
// resolution, charset, whitelist, then the action — exactly one auditLog on
// every path.
func handleAction(w http.ResponseWriter, r *http.Request, cfg *Config, action func(string) (string, error), contentType string) {
	start := time.Now()
	isJSON := contentType == "application/json"

	writeErr := func(status int, msg string) {
		if isJSON {
			jsonError(w, status, msg)
		} else {
			http.Error(w, msg, status)
		}
	}

	actionName := r.URL.Path

	// GET only. ?service= is unvalidated here: audit "<none>"/"<invalid>", never
	// the raw value (it could forge "| Field=value" segments).
	if r.Method != http.MethodGet {
		Warningf("method %s not allowed on %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
		svcForAudit := r.URL.Query().Get("service")
		switch {
		case svcForAudit == "":
			svcForAudit = "<none>"
		case !validServiceName.MatchString(svcForAudit):
			svcForAudit = "<invalid>"
		}
		auditLog(r, svcForAudit, actionName, "DENIED", time.Since(start))
		writeErr(http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	serviceName := r.URL.Query().Get("service")
	if serviceName == "" {
		cfg.mu.RLock()
		serviceName = cfg.ServiceName
		cfg.mu.RUnlock()
	}
	Debugf("request %s service=%s from %s", actionName, serviceName, r.RemoteAddr)
	if serviceName == "" {
		Warningf("denied %s: no service name provided", actionName)
		auditLog(r, "<none>", actionName, "DENIED", time.Since(start))
		writeErr(http.StatusBadRequest, "service name not provided")
		return
	}

	// Charset check before the whitelist: special characters never reach D-Bus,
	// even with a misconfigured whitelist.
	if !validServiceName.MatchString(serviceName) {
		Warningf("denied %s: service name %q contains invalid characters", actionName, serviceName)
		auditLog(r, "<invalid>", actionName, "DENIED", time.Since(start))
		writeErr(http.StatusBadRequest, "service name contains invalid characters")
		return
	}

	// Enforce whitelist
	if !isAllowedService(cfg, serviceName) {
		Warningf("denied %s: service %s not in whitelist", actionName, serviceName)
		auditLog(r, serviceName, actionName, "DENIED", time.Since(start))
		writeErr(http.StatusForbidden, fmt.Sprintf("service %s is not allowed", serviceName))
		return
	}

	output, err := action(serviceName)
	if err != nil {
		Errorf("action %s on %s failed: %v", actionName, serviceName, err)
		auditLog(r, serviceName, actionName, "FAILED", time.Since(start))
		writeErr(http.StatusInternalServerError, fmt.Sprintf("%v", err))
		return
	}

	Tracef("action %s on %s output: %s", actionName, serviceName, output)
	auditLog(r, serviceName, actionName, "ALLOWED", time.Since(start))
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write([]byte(output))
}
