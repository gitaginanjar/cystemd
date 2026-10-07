package service

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ─── isAllowedService ──────────────────────────────────────────────────────

func TestIsAllowedService_WithWhitelist(t *testing.T) {
	cfg := &Config{
		ServiceName:     "default_svc",
		HostSelector:    "default_svc",
		AllowedServices: []string{"svc_a", "svc_b", "svc_c"},
	}

	for _, svc := range []string{"svc_a", "svc_b", "svc_c"} {
		if !isAllowedService(cfg, svc) {
			t.Errorf("isAllowedService(%q): expected true, got false", svc)
		}
	}
	for _, svc := range []string{"svc_d", "default_svc", "", "SVC_A"} {
		if isAllowedService(cfg, svc) {
			t.Errorf("isAllowedService(%q): expected false, got true", svc)
		}
	}
}

func TestIsAllowedService_EmptyWhitelistFallsBackToServiceName(t *testing.T) {
	cfg := &Config{ServiceName: "only_svc", AllowedServices: nil}
	if !isAllowedService(cfg, "only_svc") {
		t.Error("isAllowedService with empty whitelist: expected default service to be allowed")
	}
	if isAllowedService(cfg, "other_svc") {
		t.Error("isAllowedService with empty whitelist: expected non-default service to be denied")
	}
}

func TestIsAllowedService_EmptyWhitelistAndEmptyServiceName(t *testing.T) {
	cfg := &Config{ServiceName: "", AllowedServices: nil}
	if isAllowedService(cfg, "anything") {
		t.Error("expected false when no whitelist and no service_name")
	}
}

func TestIsAllowedService_CaseSensitive(t *testing.T) {
	cfg := &Config{AllowedServices: []string{"MySvc"}}
	if isAllowedService(cfg, "mysvc") {
		t.Error("isAllowedService should be case-sensitive")
	}
	if !isAllowedService(cfg, "MySvc") {
		t.Error("exact case match should be allowed")
	}
}

// ─── auditLog ──────────────────────────────────────────────────────────────

func TestAuditLog_EmitsEntryViaLogger(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.RemoteAddr = "192.168.1.100:12345"

	auditLog(r, "test_service", "/restart", "ALLOWED", 0)

	line := buf.String()
	for _, want := range []string{"[AUDIT]", "192.168.1.100", "/restart", "test_service", "ALLOWED", "Duration="} {
		if !strings.Contains(line, want) {
			t.Errorf("audit log missing %q; got: %s", want, line)
		}
	}
}

func TestAuditLog_AllResultValues(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/stop", nil)
	r.RemoteAddr = "10.0.0.1:9999"

	for _, result := range []string{"ALLOWED", "DENIED", "FAILED"} {
		auditLog(r, "svc", "/stop", result, 0)
	}

	content := buf.String()
	for _, result := range []string{"ALLOWED", "DENIED", "FAILED"} {
		if !strings.Contains(content, result) {
			t.Errorf("audit log missing result %q", result)
		}
	}
}

func TestAuditLog_UsesXForwardedFor(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.Header.Set("X-Forwarded-For", "10.11.12.5, 10.0.0.1")
	r.RemoteAddr = "127.0.0.1:8080"

	auditLog(r, "svc", "/status", "ALLOWED", 0)

	if !strings.Contains(buf.String(), "10.11.12.5") {
		t.Errorf("audit log should use X-Forwarded-For first entry; got: %s", buf.String())
	}
}

// TestAuditLog_XFFMismatchWithRemoteAddr_LogsBoth pins both values in the audit
// line when X-Forwarded-For disagrees with the socket peer (a spoofed header).
func TestAuditLog_XFFMismatchWithRemoteAddr_LogsBoth(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("X-Forwarded-For", "10.11.12.5")
	r.RemoteAddr = "203.0.113.9:54321"

	auditLog(r, "svc", "/restart", "ALLOWED", 0)

	out := buf.String()
	if !strings.Contains(out, "Client=10.11.12.5") {
		t.Errorf("audit log should keep XFF-derived Client; got: %s", out)
	}
	if !strings.Contains(out, "RemoteAddr=203.0.113.9") {
		t.Errorf("audit log should also surface the raw socket RemoteAddr on XFF/RemoteAddr mismatch; got: %s", out)
	}
}

// TestAuditLog_XFFMatchesRemoteAddr_NoRemoteAddrField pins no RemoteAddr= field
// when X-Forwarded-For agrees with the socket peer.
func TestAuditLog_XFFMatchesRemoteAddr_NoRemoteAddrField(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.Header.Set("X-Forwarded-For", "192.168.10.20")
	r.RemoteAddr = "192.168.10.20:54321"

	auditLog(r, "svc", "/restart", "ALLOWED", 0)

	out := buf.String()
	if strings.Contains(out, "RemoteAddr=") {
		t.Errorf("audit log should not append RemoteAddr= when XFF matches RemoteAddr; got: %s", out)
	}
}

func TestAuditLog_NoRemoteAddr_DoesNotPanic(t *testing.T) {
	captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "127.0.0.1:8080"
	auditLog(r, "svc", "/status", "ALLOWED", 0)
}

func TestAuditLog_StripPortFromRemoteAddr(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	r.RemoteAddr = "192.168.10.20:54321"

	auditLog(r, "svc", "/restart", "ALLOWED", 0)

	out := buf.String()
	if strings.Contains(out, ":54321") {
		t.Errorf("audit log should strip port from RemoteAddr; got: %s", out)
	}
	if !strings.Contains(out, "Client=192.168.10.20") {
		t.Errorf("audit log should contain IP; got: %s", out)
	}
}

// TestAuditLog_StripPortFromIPv6RemoteAddr pins the port stripped from a
// bracketed IPv6 RemoteAddr ("[::1]:54321", the form net/http emits).
func TestAuditLog_StripPortFromIPv6RemoteAddr(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "[::1]:54321"

	auditLog(r, "svc", "/status", "ALLOWED", 0)

	out := buf.String()
	if strings.Contains(out, ":54321") {
		t.Errorf("audit log should strip port from IPv6 RemoteAddr; got: %s", out)
	}
	if !strings.Contains(out, "Client=::1") {
		t.Errorf("audit log should contain bracket-stripped IPv6 address; got: %s", out)
	}
}

// TestAuditLog_UnparseableRemoteAddr_FallsBackToRaw pins the raw RemoteAddr
// when it is not host:port (e.g. a Unix socket peer).
func TestAuditLog_UnparseableRemoteAddr_FallsBackToRaw(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "@" // not a host:port

	auditLog(r, "svc", "/status", "ALLOWED", 0)

	if !strings.Contains(buf.String(), "Client=@") {
		t.Errorf("audit log should preserve raw RemoteAddr on parse failure; got:\n%s", buf.String())
	}
}

// ─── jsonError ─────────────────────────────────────────────────────────────

func TestJsonError_SetsContentType(t *testing.T) {
	w := httptest.NewRecorder()
	jsonError(w, http.StatusBadRequest, "missing field")
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want %q", ct, "application/json")
	}
}

func TestJsonError_WritesStatusCode(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusInternalServerError, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			w := httptest.NewRecorder()
			jsonError(w, code, "some error")
			if w.Code != code {
				t.Errorf("status: got %d, want %d", w.Code, code)
			}
		})
	}
}

func TestJsonError_BodyIsValidJSON(t *testing.T) {
	w := httptest.NewRecorder()
	jsonError(w, http.StatusForbidden, "not allowed")
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("jsonError body is not valid JSON: %s", w.Body.String())
	}
}

func TestJsonError_BodyHasErrorKey(t *testing.T) {
	w := httptest.NewRecorder()
	jsonError(w, http.StatusBadRequest, "service name not provided")
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("cannot unmarshal jsonError body: %v", err)
	}
	if m["error"] != "service name not provided" {
		t.Errorf("jsonError body: got %v, want error='service name not provided'", m)
	}
}

// ─── service name validation ───────────────────────────────────────────────

func TestValidServiceName_AcceptsValidNames(t *testing.T) {
	valid := []string{
		"nginx", "my-service", "my_service", "svc.service",
		"nginx@80", "myproject_myapplication5", "dbus", "cystemd",
		"MyService", "svc123", "a:b",
	}
	for _, name := range valid {
		if !validServiceName.MatchString(name) {
			t.Errorf("validServiceName should accept %q", name)
		}
	}
}

func TestValidServiceName_RejectsInvalidNames(t *testing.T) {
	invalid := []string{
		"", "svc name", "svc;name", "svc&name", "svc|name",
		"svc`name", "svc$name", "svc/name", "svc\\name",
		"../etc/passwd", "svc\nname",
	}
	for _, name := range invalid {
		if validServiceName.MatchString(name) {
			t.Errorf("validServiceName should reject %q", name)
		}
	}
}

func TestHandleAction_InvalidServiceName_ReturnsBadRequest(t *testing.T) {
	cfg := &Config{ServiceName: "", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/restart?service=svc;rm+-rf", nil)
	w := httptest.NewRecorder()
	HandleAction(w, r, cfg, func(s string) (string, error) { return "ok", nil })
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid service name, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "rm") {
		t.Error("response body must not echo injected content")
	}
}

func TestHandleAction_InvalidServiceName_EmitsAuditLog(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	cfg := &Config{ServiceName: "", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/restart?service=bad|name", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	w := httptest.NewRecorder()
	HandleAction(w, r, cfg, func(s string) (string, error) { return "ok", nil })
	if !strings.Contains(buf.String(), "DENIED") {
		t.Errorf("expected DENIED audit entry for invalid service name; got:\n%s", buf.String())
	}
}

// ─── HandleAction (plain text) ─────────────────────────────────────────────

func TestHandleAction_MethodNotAllowed(t *testing.T) {
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			r := httptest.NewRequest(method, "/restart", nil)
			w := httptest.NewRecorder()
			HandleAction(w, r, cfg, func(s string) (string, error) { return "ok", nil })
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("method %s: expected 405, got %d", method, w.Code)
			}
		})
	}
}

// TestHandleAction_MethodNotAllowed_EmitsAuditLog pins exactly one DENIED audit
// line on the 405 path, like every other rejection.
// Why: DOCS/CLAUDE.md § `HandleAction` log/audit contract
func TestHandleAction_MethodNotAllowed_EmitsAuditLog(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodPost, "/restart?service=svc", nil)
	w := httptest.NewRecorder()
	HandleAction(w, r, cfg, func(s string) (string, error) { return "ok", nil })

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405, got %d", w.Code)
	}
	msgs := decodeLogMsgs(t, buf)
	auditCount := strings.Count(msgs, "[AUDIT]")
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 [AUDIT] line for a 405 request, got %d; log:\n%s", auditCount, msgs)
	}
	if !strings.Contains(msgs, "[AUDIT]") || !strings.Contains(msgs, "Result=DENIED") {
		t.Errorf("expected a DENIED audit entry for method-not-allowed; got:\n%s", msgs)
	}
}

func TestHandleAction_NoServiceName(t *testing.T) {
	cfg := &Config{ServiceName: "", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	w := httptest.NewRecorder()
	HandleAction(w, r, cfg, func(s string) (string, error) { return "ok", nil })
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing service name, got %d", w.Code)
	}
}

func TestHandleAction_ServiceFromQuery(t *testing.T) {
	cfg := &Config{ServiceName: "", AllowedServices: []string{"query_svc"}}
	r := httptest.NewRequest(http.MethodGet, "/status?service=query_svc", nil)
	w := httptest.NewRecorder()
	called := false
	HandleAction(w, r, cfg, func(s string) (string, error) {
		called = true
		if s != "query_svc" {
			t.Errorf("action called with %q, want %q", s, "query_svc")
		}
		return "output", nil
	})
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Error("action function was never called")
	}
}

func TestHandleAction_DefaultServiceFromConfig(t *testing.T) {
	cfg := &Config{ServiceName: "default_svc", AllowedServices: []string{"default_svc"}}
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	var invokedWith string
	HandleAction(w, r, cfg, func(s string) (string, error) {
		invokedWith = s
		return "status output", nil
	})
	if invokedWith != "default_svc" {
		t.Errorf("action invoked with %q, want default_svc", invokedWith)
	}
}

func TestHandleAction_ServiceNotInWhitelist(t *testing.T) {
	cfg := &Config{ServiceName: "allowed", AllowedServices: []string{"allowed"}}
	r := httptest.NewRequest(http.MethodGet, "/restart?service=forbidden", nil)
	w := httptest.NewRecorder()
	HandleAction(w, r, cfg, func(s string) (string, error) { return "ok", nil })
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403 for service not in whitelist, got %d", w.Code)
	}
}

func TestHandleAction_ActionError(t *testing.T) {
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/restart", nil)
	w := httptest.NewRecorder()
	HandleAction(w, r, cfg, func(s string) (string, error) {
		return "partial output", &actionError{msg: "systemctl failed"}
	})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on action error, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "systemctl failed") {
		t.Errorf("expected error message in body, got: %s", w.Body.String())
	}
}

func TestHandleAction_SuccessWritesOutput(t *testing.T) {
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	HandleAction(w, r, cfg, func(s string) (string, error) {
		return "service is running", nil
	})
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "service is running") {
		t.Errorf("expected output in response body, got: %s", w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("HandleAction Content-Type: got %q, want %q", ct, "text/plain")
	}
}

func TestHandleAction_QueryServiceOverridesDefault(t *testing.T) {
	cfg := &Config{ServiceName: "default", AllowedServices: []string{"default", "override"}}
	r := httptest.NewRequest(http.MethodGet, "/status?service=override", nil)
	w := httptest.NewRecorder()
	var invokedWith string
	HandleAction(w, r, cfg, func(s string) (string, error) {
		invokedWith = s
		return "ok", nil
	})
	if invokedWith != "override" {
		t.Errorf("expected ?service= to override default; invoked with %q", invokedWith)
	}
}

// ─── HandleJSONAction ──────────────────────────────────────────────────────

func TestHandleJSONAction_MethodNotAllowed_ReturnsJSON(t *testing.T) {
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			r := httptest.NewRequest(method, "/status", nil)
			w := httptest.NewRecorder()
			HandleJSONAction(w, r, cfg, func(s string) (string, error) { return "{}", nil })
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("method %s: expected 405, got %d", method, w.Code)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("method %s: Content-Type: got %q, want application/json", method, ct)
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Errorf("method %s: 405 body is not valid JSON: %s", method, w.Body.String())
			}
		})
	}
}

func TestHandleJSONAction_NoServiceName_ReturnsJSON(t *testing.T) {
	cfg := &Config{ServiceName: "", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	HandleJSONAction(w, r, cfg, func(s string) (string, error) { return "{}", nil })
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("400 body is not valid JSON: %s", w.Body.String())
	}
}

func TestHandleJSONAction_ServiceNotInWhitelist_ReturnsJSON(t *testing.T) {
	cfg := &Config{ServiceName: "allowed", AllowedServices: []string{"allowed"}}
	r := httptest.NewRequest(http.MethodGet, "/status?service=forbidden", nil)
	w := httptest.NewRecorder()
	HandleJSONAction(w, r, cfg, func(s string) (string, error) { return "{}", nil })
	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("403 body is not valid JSON: %s", w.Body.String())
	}
}

func TestHandleJSONAction_ActionError_ReturnsJSON(t *testing.T) {
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	HandleJSONAction(w, r, cfg, func(s string) (string, error) {
		return "", &actionError{msg: "dbus unavailable"}
	})
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500 on action error, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("500 body is not valid JSON: %s", w.Body.String())
	}
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if !strings.Contains(m["error"], "dbus unavailable") {
		t.Errorf("500 JSON error body should contain the error message; got: %v", m)
	}
}

func TestHandleJSONAction_SuccessWritesJSON(t *testing.T) {
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	w := httptest.NewRecorder()
	payload := `{"unit":"svc.service","active_state":"active"}`
	HandleJSONAction(w, r, cfg, func(s string) (string, error) {
		return payload, nil
	})
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}
	if w.Body.String() != payload {
		t.Errorf("body: got %q, want %q", w.Body.String(), payload)
	}
}

func TestHandleJSONAction_EmitsAuditLog(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.RemoteAddr = "10.0.0.5:1234"
	w := httptest.NewRecorder()
	HandleJSONAction(w, r, cfg, func(s string) (string, error) {
		return `{"unit":"svc.service"}`, nil
	})
	if !strings.Contains(buf.String(), "[AUDIT]") {
		t.Errorf("HandleJSONAction should emit an audit log; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "ALLOWED") {
		t.Errorf("audit log should record ALLOWED; got:\n%s", buf.String())
	}
}

func TestHandleJSONAction_DeniedEmitsAuditLog(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	cfg := &Config{ServiceName: "allowed", AllowedServices: []string{"allowed"}}
	r := httptest.NewRequest(http.MethodGet, "/status?service=bad", nil)
	w := httptest.NewRecorder()
	HandleJSONAction(w, r, cfg, func(s string) (string, error) { return "{}", nil })
	if !strings.Contains(buf.String(), "DENIED") {
		t.Errorf("audit log should record DENIED; got:\n%s", buf.String())
	}
}

// ─── helpers ───────────────────────────────────────────────────────────────

// actionError is a simple error type for tests.
type actionError struct{ msg string }

func (e *actionError) Error() string { return e.msg }
