package service

// metrics_http.go — RED middleware (Rate / Errors / Duration): one observation
// per HTTP transaction. Path and method labels are normalised to a known value
// or "other", so no client-chosen string can grow the label space.

import (
	"net/http"
	"time"
)

// knownHTTPRoutes is the set of route labels emitted; any other path becomes
// "other". Must match RegisterHandlersOn (TestKnownHTTPRoutes_MatchesRegisteredRoutes).
var knownHTTPRoutes = map[string]bool{
	"/":         true,
	"/restart":  true,
	"/start":    true,
	"/stop":     true,
	"/enable":   true,
	"/disable":  true,
	"/config":   true,
	"/env":      true,
	"/sync":     true,
	"/diff":     true,
	"/lastdiff": true,
	"/status":   true,
	"/version":  true,
	"/health":   true,
	"/metrics":  true,
}

// routeLabel maps a registered route to itself and any other path (a catch-all
// hit or a probe) to "other".
func routeLabel(path string) string {
	if knownHTTPRoutes[path] {
		return path
	}
	return "other"
}

// knownHTTPMethods is the set of method labels emitted verbatim; anything else
// becomes "other". Every request is labeled whatever the handler answers (405
// included), so an arbitrary client-sent method must never reach the label.
var knownHTTPMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodHead:    true,
	http.MethodOptions: true,
}

// methodLabel maps a known method to itself and anything else to "other".
func methodLabel(method string) string {
	if knownHTTPMethods[method] {
		return method
	}
	return "other"
}

// statusRecorder captures the status the inner handler wrote: the first
// WriteHeader or Write wins, and 200 (net/http's default) when neither runs.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// InstrumentHTTP wraps inner (the root mux) with the RED metrics: after inner
// returns it records cystemd_http_requests_total{path,method,status} and one
// cystemd_http_request_duration_seconds{path} observation (/metrics included).
func InstrumentHTTP(inner http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		inner.ServeHTTP(rec, r)
		route := routeLabel(r.URL.Path)
		recordHTTPRequest(route, methodLabel(r.Method), rec.status)
		recordHTTPDuration(route, time.Since(start))
	})
}
