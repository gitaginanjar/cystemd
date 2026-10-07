package service

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestRouteLabel(t *testing.T) {
	cases := map[string]string{
		"/":             "/",
		"/restart":      "/restart",
		"/metrics":      "/metrics",
		"/status":       "/status",
		"/sync":         "/sync",
		"/api/v1/foo":   "other",
		"/random-xyz":   "other",
		"":              "other",
		"/health/probe": "other",
	}
	for in, want := range cases {
		if got := routeLabel(in); got != want {
			t.Errorf("routeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMethodLabel(t *testing.T) {
	cases := map[string]string{
		http.MethodGet:     http.MethodGet,
		http.MethodPost:    http.MethodPost,
		http.MethodPut:     http.MethodPut,
		http.MethodPatch:   http.MethodPatch,
		http.MethodDelete:  http.MethodDelete,
		http.MethodHead:    http.MethodHead,
		http.MethodOptions: http.MethodOptions,
		"BOGUS":            "other",
		"get":              "other", // lowercase is not a valid HTTP method token
		"":                 "other",
		"TRACE":            "other",
		"CONNECT":          "other",
	}
	for in, want := range cases {
		if got := methodLabel(in); got != want {
			t.Errorf("methodLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestStatusRecorder_CapturesWrittenCode pins the status from the first
// WriteHeader; a second one does not overwrite it.
func TestStatusRecorder_CapturesWrittenCode(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	rec.WriteHeader(http.StatusForbidden)
	if rec.status != http.StatusForbidden {
		t.Errorf("after WriteHeader(403): status=%d want 403", rec.status)
	}
	// A second WriteHeader must not overwrite the captured code.
	rec.WriteHeader(http.StatusInternalServerError)
	if rec.status != http.StatusForbidden {
		t.Errorf("second WriteHeader overwrote status: got %d want 403", rec.status)
	}
}

// TestStatusRecorder_DefaultsTo200 pins 200 for a handler that only Writes, as
// net/http does.
func TestStatusRecorder_DefaultsTo200(t *testing.T) {
	rec := &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: http.StatusOK}
	if _, err := rec.Write([]byte("hi")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if rec.status != http.StatusOK {
		t.Errorf("Write without WriteHeader: status=%d want 200", rec.status)
	}
}

// TestInstrumentHTTP_RecordsRED pins rate/status and duration per route, with an
// unknown path bucketed as "other".
func TestInstrumentHTTP_RecordsRED(t *testing.T) {
	resetMetricsForTest()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/restart", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := InstrumentHTTP(mux)

	// Known open endpoint → 200.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health: got %d want 200", rec.Code)
	}
	// Known endpoint returning an error status.
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/restart", nil))
	// Unknown path → catch-all "/" serves it, but the route label is "other".
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/probe/xyz", nil))

	body := scrapeMetrics(t).Body.String()
	for _, want := range []string{
		`cystemd_http_requests_total{path="/health",method="GET",status="200"} 1`,
		`cystemd_http_requests_total{path="/restart",method="GET",status="403"} 1`,
		`cystemd_http_requests_total{path="other",method="GET",status="200"} 1`,
		`cystemd_http_request_duration_seconds_count{path="/health"} 1`,
		`cystemd_http_request_duration_seconds_count{path="other"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q\n---\n%s", want, body)
		}
	}

	// One "other" series, however many distinct paths are probed.
	if c := strings.Count(body, `cystemd_http_requests_total{path="other"`); c != 1 {
		t.Errorf("expected exactly 1 other-path series, got %d\n%s", c, body)
	}
}

// TestInstrumentHTTP_ArbitraryMethod_CollapsesToOther pins an arbitrary
// client-sent method collapsing to method="other", never a raw label.
func TestInstrumentHTTP_ArbitraryMethod_CollapsesToOther(t *testing.T) {
	resetMetricsForTest()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := InstrumentHTTP(mux)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	req.Method = "GARBAGE-METHOD-XYZ"
	h.ServeHTTP(httptest.NewRecorder(), req)

	body := scrapeMetrics(t).Body.String()
	if !strings.Contains(body, `cystemd_http_requests_total{path="/health",method="other",status="200"} 1`) {
		t.Errorf("expected arbitrary method to collapse to method=\"other\"; got:\n%s", body)
	}
	if strings.Contains(body, `method="GARBAGE-METHOD-XYZ"`) {
		t.Errorf("raw arbitrary method must never reach a label; got:\n%s", body)
	}
}

// TestInstrumentHTTP_SingleIncrementPerRequest pins exactly one request-counter
// increment per request.
func TestInstrumentHTTP_SingleIncrementPerRequest(t *testing.T) {
	resetMetricsForTest()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	h := InstrumentHTTP(mux)

	for i := 0; i < 3; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/health", nil))
	}
	body := scrapeMetrics(t).Body.String()
	if !strings.Contains(body, `cystemd_http_requests_total{path="/health",method="GET",status="200"} 3`) {
		t.Errorf("expected 3 health requests, got:\n%s", body)
	}
}

// TestKnownHTTPRoutes_MatchesRegisteredRoutes pins knownHTTPRoutes to the routes
// RegisterHandlersOn registers, both ways: a forgotten entry fails nothing else
// (its RED metrics just land in path="other"); a stale one wastes a label.
// Why: DOCS/CLAUDE.md § Adding an endpoint
func TestKnownHTTPRoutes_MatchesRegisteredRoutes(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	// ── map → mux ──────────────────────────────────────────────────────────
	// An unregistered path resolves to the "/" catch-all's pattern, not its own.
	for route := range knownHTTPRoutes {
		if route == "/" {
			continue // the catch-all matches itself trivially
		}
		req := httptest.NewRequest(http.MethodGet, route, nil)
		_, pattern := mux.Handler(req)
		if pattern != route {
			t.Errorf("knownHTTPRoutes contains %q but RegisterHandlersOn does not register it "+
				"(request matched pattern %q instead) — remove the stale entry from metrics_http.go",
				route, pattern)
		}
	}

	// ── mux → map ──────────────────────────────────────────────────────────
	// net/http cannot enumerate a ServeMux, so scan handlers.go's literal
	// registrations; the floor below fails loudly if that style ever changes.
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	registered := regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(registered) < len(knownHTTPRoutes) {
		t.Fatalf("scanned only %d mux.HandleFunc registrations in handlers.go but knownHTTPRoutes has %d entries — "+
			"the registration style changed and this test is no longer checking anything",
			len(registered), len(knownHTTPRoutes))
	}
	for _, m := range registered {
		route := m[1]
		if !knownHTTPRoutes[route] {
			t.Errorf("handlers.go registers %q but knownHTTPRoutes does not list it — "+
				"add it to metrics_http.go or its RED metrics land in path=\"other\"", route)
		}
	}
}
