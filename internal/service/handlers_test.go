package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── deriveLocalIP ─────────────────────────────────────────────────────────

func TestDeriveLocalIP_ReturnsValidIP(t *testing.T) {
	ip := deriveLocalIP()
	if ip == "" {
		t.Error("deriveLocalIP returned empty string")
	}
	parts := strings.Split(ip, ".")
	if len(parts) != 4 {
		t.Errorf("deriveLocalIP returned non-IPv4: %q", ip)
	}
}

func TestDeriveLocalIP_NeverEmpty(t *testing.T) {
	if ip := deriveLocalIP(); ip == "" {
		t.Error("deriveLocalIP must never return empty string")
	}
}

// ─── buildUsageJSON ────────────────────────────────────────────────────────

func TestBuildUsageJSON_IsValidJSON(t *testing.T) {
	cfg := &Config{ServiceName: "myproject_testsvc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "lb.example.com"

	u := buildUsageJSON(r, cfg)
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("buildUsageJSON produced un-marshalable struct: %v", err)
	}
	if !json.Valid(data) {
		t.Errorf("buildUsageJSON output is not valid JSON: %s", string(data))
	}
}

func TestBuildUsageJSON_ContainsAllEndpoints(t *testing.T) {
	cfg := &Config{ServiceName: "myproject_testsvc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "lb.example.com"

	u := buildUsageJSON(r, cfg)

	paths := make(map[string]bool)
	for _, ep := range u.Endpoints {
		paths[ep.Path] = true
	}
	for _, ep := range []string{"/restart", "/start", "/stop", "/enable", "/disable", "/config", "/env", "/sync", "/status", "/version", "/health", "/metrics"} {
		if !paths[ep] {
			t.Errorf("buildUsageJSON missing endpoint %q in Endpoints array", ep)
		}
	}
}

func TestBuildUsageJSON_StatusAndVersionAreJSON(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	for _, ep := range u.Endpoints {
		if ep.Path == "/status" || ep.Path == "/version" || ep.Path == "/health" {
			if ep.ResponseType != "application/json" {
				t.Errorf("endpoint %s ResponseType: got %q, want application/json", ep.Path, ep.ResponseType)
			}
			if ep.AuthRequired {
				t.Errorf("endpoint %s should not require auth", ep.Path)
			}
		}
	}
}

func TestBuildUsageJSON_MutatingEndpointsRequireAuth(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	for _, ep := range u.Endpoints {
		switch ep.Path {
		case "/restart", "/start", "/stop", "/enable", "/disable", "/config", "/env", "/sync":
			if !ep.AuthRequired {
				t.Errorf("endpoint %s should have auth_required=true", ep.Path)
			}
		}
	}
}

// TestBuildUsageJSON_EveryEndpointHasExamples pins that every endpoints[] entry
// has examples naming its own path, with a Bearer header exactly when auth-required.
func TestBuildUsageJSON_EveryEndpointHasExamples(t *testing.T) {
	cfg := &Config{ServiceName: "myproject_testsvc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "lb.example.com"

	u := buildUsageJSON(r, cfg)

	if len(u.Endpoints) == 0 {
		t.Fatal("usage page lists no endpoints")
	}
	for _, ep := range u.Endpoints {
		if len(ep.Examples) == 0 {
			t.Errorf("endpoint %s has no examples", ep.Path)
			continue
		}
		for _, ex := range ep.Examples {
			if !strings.Contains(ex, ep.Path) {
				t.Errorf("endpoint %s example %q does not reference its own path", ep.Path, ex)
			}
			if ep.AuthRequired && !strings.Contains(ex, "Authorization: Bearer") {
				t.Errorf("auth-required endpoint %s example %q lacks the Authorization header", ep.Path, ex)
			}
			if !ep.AuthRequired && strings.Contains(ex, "Authorization") {
				t.Errorf("open endpoint %s example %q must not show an Authorization header", ep.Path, ex)
			}
		}
	}
}

// TestBuildUsageJSON_ServiceScopedExamplesShowServiceParam pins ?service=<name>
// on service-scoped direct-IP examples only (never /sync, /, /health, /metrics).
func TestBuildUsageJSON_ServiceScopedExamplesShowServiceParam(t *testing.T) {
	cfg := &Config{ServiceName: "myproject_testsvc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "lb.example.com"

	u := buildUsageJSON(r, cfg)

	scoped := map[string]bool{
		"/status": true, "/version": true, "/restart": true, "/start": true,
		"/stop": true, "/enable": true, "/disable": true, "/config": true, "/env": true,
	}
	for _, ep := range u.Endpoints {
		found := false
		for _, ex := range ep.Examples {
			if strings.Contains(ex, ep.Path+"?service=myproject_testsvc") {
				found = true
			}
		}
		if scoped[ep.Path] && !found {
			t.Errorf("service-scoped endpoint %s should have a ?service= example; got: %v", ep.Path, ep.Examples)
		}
		if !scoped[ep.Path] && found {
			t.Errorf("endpoint %s must not show ?service= in its examples; got: %v", ep.Path, ep.Examples)
		}
	}
}

func TestBuildUsageJSON_ContainsBuildMetadata(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	if u.Server.Service != "cystemd" {
		t.Errorf("Service: got %q, want cystemd", u.Server.Service)
	}
	if u.Server.Version == "" {
		t.Error("Version must not be empty")
	}
	if u.Server.Commit == "" {
		t.Error("Commit must not be empty")
	}
	if u.Server.BuildTime == "" {
		t.Error("BuildTime must not be empty")
	}
	if u.Server.GoVersion == "" {
		t.Error("GoVersion must not be empty")
	}
}

func TestBuildUsageJSON_ContainsServiceName(t *testing.T) {
	cfg := &Config{ServiceName: "myproject_myapp", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)
	data, _ := json.Marshal(u)
	if !strings.Contains(string(data), "myproject_myapp") {
		t.Errorf("buildUsageJSON should contain service name; got: %s", string(data))
	}
}

func TestBuildUsageJSON_LBExamplesUseRequestHost(t *testing.T) {
	cfg := &Config{ServiceName: "myproject_myapplication", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "myapp-dev.example.com"

	u := buildUsageJSON(r, cfg)

	found := false
	for _, ex := range u.Examples.ViaLoadBalancer {
		if strings.Contains(ex, "myapp-dev.example.com") {
			found = true
		}
		if strings.Contains(ex, "myapp-dev.example.com/myapplication/") {
			t.Errorf("LB example must not include legacy path prefix; got: %s", ex)
		}
	}
	if !found {
		t.Errorf("expected at least one LB example containing the request host; got: %v", u.Examples.ViaLoadBalancer)
	}
}

func TestBuildUsageJSON_EmptyHostFallsBackToLocalhost(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = ""

	u := buildUsageJSON(r, cfg)
	data, _ := json.Marshal(u)
	if !strings.Contains(string(data), "localhost") {
		t.Errorf("buildUsageJSON should use 'localhost' when Host header is empty; got: %s", string(data))
	}
}

func TestBuildUsageJSON_ContainsGeneratedAt(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)
	if u.Server.GeneratedAt == "" {
		t.Error("GeneratedAt must not be empty")
	}
}

func TestBuildUsageJSON_ServerFieldsPopulated(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)
	if u.Server.IP == "" {
		t.Error("Server.IP must not be empty")
	}
	if u.Server.ListenAddress == "" {
		t.Error("Server.ListenAddress must not be empty")
	}
}

func TestBuildUsageJSON_NotesNonEmpty(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)
	if len(u.Notes) == 0 {
		t.Error("Notes must not be empty")
	}
}

// ─── buildUsageJSON — Vault status ─────────────────────────────────────────

func TestBuildUsageJSON_VaultFieldPresent(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)
	data, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"vault"`) {
		t.Errorf("buildUsageJSON JSON must contain 'vault' field; got: %s", string(data))
	}
}

func TestBuildUsageJSON_VaultNotConfigured_ShowsFalse(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })
	t.Setenv("VAULT_ADDRESS", "")

	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	if u.Server.Vault.Configured {
		t.Error("Vault.Configured: got true, want false when vault is not configured")
	}
	if u.Server.Vault.Status != VaultStatusNotConfigured {
		t.Errorf("Vault.Status: got %q, want %q", u.Server.Vault.Status, VaultStatusNotConfigured)
	}
	if u.Server.Vault.AuthMethod != string(VaultAuthMethodNone) {
		t.Errorf("Vault.AuthMethod: got %q, want %q", u.Server.Vault.AuthMethod, VaultAuthMethodNone)
	}
}

func TestBuildUsageJSON_VaultConfigured_TokenAuth(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		Token:      "hvs.secret",
		AuthMethod: VaultAuthMethodToken,
		Address:    "https://vault.example.com",
		Accessor:   "acc-001",
		Policies:   []string{"default", "myapp"},
	}
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	if !u.Server.Vault.Configured {
		t.Error("Vault.Configured: got false, want true")
	}
	if u.Server.Vault.Status != VaultStatusAuthenticated {
		t.Errorf("Vault.Status: got %q, want %q", u.Server.Vault.Status, VaultStatusAuthenticated)
	}
	if u.Server.Vault.AuthMethod != string(VaultAuthMethodToken) {
		t.Errorf("Vault.AuthMethod: got %q, want %q", u.Server.Vault.AuthMethod, VaultAuthMethodToken)
	}
	if u.Server.Vault.Address != "https://vault.example.com" {
		t.Errorf("Vault.Address: got %q, want %q", u.Server.Vault.Address, "https://vault.example.com")
	}
	// The accessor/policies on the VaultState must NOT reach the open usage page.
	data, _ := json.Marshal(u)
	if strings.Contains(string(data), "acc-001") || strings.Contains(string(data), "myapp") {
		t.Errorf("usage page must not expose the Vault accessor/policies; got: %s", data)
	}
}

func TestBuildUsageJSON_VaultConfigured_AppRoleAuth(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		Token:         "hvs.approlesecret",
		AuthMethod:    VaultAuthMethodAppRole,
		Address:       "https://vault.example.com",
		Accessor:      "acc-approle-001",
		LeaseDuration: 3600,
		Renewable:     true,
	}
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	if !u.Server.Vault.Configured {
		t.Error("Vault.Configured: got false, want true")
	}
	if u.Server.Vault.AuthMethod != string(VaultAuthMethodAppRole) {
		t.Errorf("Vault.AuthMethod: got %q, want %q", u.Server.Vault.AuthMethod, VaultAuthMethodAppRole)
	}
	if u.Server.Vault.LeaseDuration != 3600 {
		t.Errorf("Vault.LeaseDuration: got %d, want 3600", u.Server.Vault.LeaseDuration)
	}
	if !u.Server.Vault.Renewable {
		t.Error("Vault.Renewable: got false, want true")
	}
}

func TestBuildUsageJSON_VaultStatus_NeverExposesRawToken(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		Token:      "hvs.supersecrettokenthatmustnotleak",
		AuthMethod: VaultAuthMethodToken,
		Address:    "https://vault.example.com",
	}
	t.Cleanup(func() { globalVaultState = prev })

	cfg := &Config{ServiceName: "svc", ListenAddress: ":50080"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)
	data, _ := json.Marshal(u)

	if strings.Contains(string(data), "hvs.supersecrettokenthatmustnotleak") {
		t.Errorf("raw Vault token must never appear in / JSON response; got: %s", string(data))
	}
}

// ─── wrapWithStatus ────────────────────────────────────────────────────────

func TestWrapWithStatus_CombinesOutputs(t *testing.T) {
	action := func(svc string) (string, error) {
		return fmt.Sprintf("restarted %s", svc), nil
	}
	wrapped := func(svc string) (string, error) {
		out1, err := action(svc)
		out2 := `{"active_state":"active"}`
		return out1 + "\n\n--- Status ---\n" + out2, err
	}

	out, err := wrapped("testsvc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, "restarted testsvc") {
		t.Errorf("expected primary action output; got: %s", out)
	}
	if !strings.Contains(out, "--- Status ---") {
		t.Errorf("expected status separator; got: %s", out)
	}
}

func TestWrapWithStatus_PropagatesActionError(t *testing.T) {
	failAction := func(svc string) (string, error) {
		return "partial", fmt.Errorf("systemctl error")
	}
	wrapped := wrapWithStatus(failAction)
	_, err := wrapped("svc")
	if err == nil {
		t.Error("expected error to propagate from action")
	}
}

func TestWrapWithStatus_ErrorStillReturnsOutput(t *testing.T) {
	failAction := func(svc string) (string, error) {
		return "partial output", fmt.Errorf("systemctl error")
	}
	wrapped := wrapWithStatus(failAction)
	out, _ := wrapped("svc")
	if !strings.Contains(out, "partial output") {
		t.Errorf("expected partial output even on error; got: %s", out)
	}
}

func TestWrapWithStatus_AppendsSeparator(t *testing.T) {
	action := func(svc string) (string, error) { return "action done", nil }
	wrapped := wrapWithStatus(action)
	out, _ := wrapped("svc")
	if !strings.Contains(out, "--- Status ---") {
		t.Errorf("expected '--- Status ---' separator in combined output; got: %s", out)
	}
}

// ─── RegisterHandlers ──────────────────────────────────────────────────────

func TestRegisterHandlers_UsagePageResponds(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "myproject_testsvc",
		HostSelector:    "myproject_testsvc",
		ListenAddress:   ":50080",
		AllowedServices: []string{"myproject_testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /: expected 200, got %d", resp.StatusCode)
	}
}

func TestRegisterHandlers_UsagePageContentTypeIsJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("/ Content-Type: got %q, want application/json", ct)
	}
}

func TestRegisterHandlers_UsagePageBodyIsValidJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/ response body is not valid JSON: %s", w.Body.String())
	}
}

func TestRegisterHandlers_UsagePageHasServiceField(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var u UsageJSON
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("cannot unmarshal / response: %v", err)
	}
	if u.Server.Service != "cystemd" {
		t.Errorf("/ JSON service field: got %q, want cystemd", u.Server.Service)
	}
}

func TestRegisterHandlers_UsagePageHasVaultField(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })
	t.Setenv("VAULT_ADDRESS", "")

	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var u UsageJSON
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("cannot unmarshal / response: %v", err)
	}
	if u.Server.Vault.AuthMethod == "" {
		t.Error("Vault.AuthMethod must not be empty in / response")
	}
	if u.Server.Vault.Configured {
		t.Error("Vault.Configured: got true, want false when vault not initialised")
	}
}

func TestRegisterHandlers_UsagePageVaultStatus_TokenAuth(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{
		Token:      "hvs.secret",
		AuthMethod: VaultAuthMethodToken,
		Address:    "https://vault.example.com",
		Accessor:   "acc-test",
	}
	t.Cleanup(func() { globalVaultState = prev })

	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var u UsageJSON
	if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
		t.Fatalf("cannot unmarshal / response: %v", err)
	}
	if !u.Server.Vault.Configured {
		t.Error("Vault.Configured: got false, want true")
	}
	if u.Server.Vault.AuthMethod != string(VaultAuthMethodToken) {
		t.Errorf("Vault.AuthMethod: got %q, want %q", u.Server.Vault.AuthMethod, VaultAuthMethodToken)
	}
	// Neither the raw token nor the accessor may appear in the open / response.
	body := w.Body.String()
	if strings.Contains(body, "hvs.secret") {
		t.Errorf("raw vault token must not appear in / response body; got: %s", body)
	}
	if strings.Contains(body, "acc-test") {
		t.Errorf("vault accessor must not appear in the open / response; got: %s", body)
	}
}

func TestRegisterHandlers_LogsRoutesRegistered(t *testing.T) {
	buf := captureLogs(t, LevelInfo)

	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	if !strings.Contains(buf.String(), "routes registered:") {
		t.Errorf("expected 'routes registered:' info log; got:\n%s", buf.String())
	}
	for _, path := range []string{"/restart", "/start", "/stop", "/enable", "/disable", "/status", "/version", "/health"} {
		if !strings.Contains(buf.String(), path) {
			t.Errorf("expected %q in routes-registered log; got:\n%s", path, buf.String())
		}
	}
}

func TestRegisterHandlers_RootEmitsDebugLog(t *testing.T) {
	buf := captureLogs(t, LevelDebug)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "myproject_testsvc",
		HostSelector:    "myproject_testsvc",
		ListenAddress:   ":50080",
		AllowedServices: []string{"myproject_testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Host = "svc.example.com"
	r.Header.Set("User-Agent", "curl/test")
	r.RemoteAddr = "10.1.2.3:4242"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("GET /: expected 200, got %d", w.Code)
	}
	rawOut := buf.String()
	if !strings.Contains(rawOut, `"level":"debug"`) {
		t.Errorf("expected debug-level JSON log line; got:\n%s", rawOut)
	}
	msgs := decodeLogMsgs(t, buf)
	for _, want := range []string{
		"usage page served", "method=GET", `path="/"`,
		"host=svc.example.com", "remote=10.1.2.3:4242", `user_agent="curl/test"`,
	} {
		if !strings.Contains(msgs, want) {
			t.Errorf("expected %q in decoded log msg; got msgs:\n%s\nraw:\n%s",
				want, msgs, rawOut)
		}
	}
}

func TestRegisterHandlers_RootSuppressedAtInfoLevel(t *testing.T) {
	buf := captureLogs(t, LevelInfo)

	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if strings.Contains(buf.String(), "usage page served") {
		t.Errorf("debug line must not appear at LevelInfo; got:\n%s", buf.String())
	}
}

func TestRegisterHandlers_RootIsCatchAll(t *testing.T) {
	buf := captureLogs(t, LevelDebug)

	mux := http.NewServeMux()
	cfg := &Config{ServiceName: "svc", AllowedServices: []string{"svc"}}
	RegisterHandlersOn(mux, cfg)

	for _, path := range []string{"/", "/does-not-exist", "/random/nested/path", "/restart-typo"} {
		t.Run(path, func(t *testing.T) {
			buf.Reset()
			r := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)

			if w.Code != http.StatusOK {
				t.Errorf("GET %s: expected 200 from catch-all, got %d", path, w.Code)
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Errorf("GET %s: expected valid JSON body; got: %s", path, w.Body.String())
			}
			var u UsageJSON
			if err := json.Unmarshal(w.Body.Bytes(), &u); err != nil {
				t.Errorf("GET %s: cannot unmarshal usage JSON: %v", path, err)
			}
			if u.Server.Service != "cystemd" {
				t.Errorf("GET %s: expected service=cystemd in JSON body", path)
			}
			msgs := decodeLogMsgs(t, buf)
			if !strings.Contains(msgs, "usage page served") {
				t.Errorf("GET %s: expected debug log msg; got:\n%s", path, msgs)
			}
			wantPath := fmt.Sprintf(`path=%q`, path)
			if !strings.Contains(msgs, wantPath) {
				t.Errorf("GET %s: expected %q in log msg; got:\n%s", path, wantPath, msgs)
			}
		})
	}
}

// ─── /status endpoint ──────────────────────────────────────────────────────

func TestStatusEndpoint_IsOpen_NoAuthRequired(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: true},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/status?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized {
		t.Error("/status should be an open endpoint — no 401 expected")
	}
}

func TestStatusEndpoint_ReturnsJSONContentType(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/status?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("/status Content-Type: got %q, want %q", ct, "application/json")
	}
}

func TestStatusEndpoint_ResponseBodyIsValidJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/status?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/status response body is not valid JSON: %s", w.Body.String())
	}
}

func TestStatusEndpoint_MethodNotAllowed_ReturnsJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodPost, "/status?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("/status POST: expected 405, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/status POST Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/status POST body is not valid JSON: %s", w.Body.String())
	}
}

func TestStatusEndpoint_ForbiddenService_ReturnsJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/status?service=forbidden_svc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("/status forbidden: expected 403, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/status forbidden Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/status forbidden body is not valid JSON: %s", w.Body.String())
	}
	var m map[string]string
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if _, ok := m["error"]; !ok {
		t.Errorf("/status forbidden JSON missing 'error' key; got: %v", m)
	}
}

// ─── /version endpoint ─────────────────────────────────────────────────────

func TestVersionEndpoint_IsOpen_NoAuthRequired(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: true},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/version?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized {
		t.Error("/version should be an open endpoint — no 401 expected")
	}
}

func TestVersionEndpoint_ReturnsJSONContentType_OnSuccess(t *testing.T) {
	if !hasRPM() {
		t.Skip("rpm not available; skipping live /version content-type test")
	}
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "rpm",
		HostSelector:    "rpm",
		AllowedServices: []string{"rpm"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/version?service=rpm", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Skipf("/version returned %d (rpm package may not be in DB); skipping", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/version Content-Type: got %q, want application/json", ct)
	}
}

func TestVersionEndpoint_MethodNotAllowed_ReturnsJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodPost, "/version?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("/version POST: expected 405, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/version POST Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/version POST body is not valid JSON: %s", w.Body.String())
	}
}

func TestVersionEndpoint_ForbiddenService_ReturnsJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/version?service=not_allowed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("/version forbidden: expected 403, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/version forbidden Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/version forbidden body is not valid JSON: %s", w.Body.String())
	}
}

// ─── /status + /version concurrency limit (503, not /diff's 500) ──────────

// drainStatusVersionSem empties statusVersionConcurrencySem after a test that
// filled it by hand to simulate saturation.
func drainStatusVersionSem() {
	for {
		select {
		case <-statusVersionConcurrencySem:
		default:
			return
		}
	}
}

func TestStatusEndpoint_SingleRequest_NotRejectedByConcurrencyLimit(t *testing.T) {
	t.Cleanup(drainStatusVersionSem)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/status?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("a single /status request should never be rejected by the concurrency limiter; got 503: %s", w.Body.String())
	}
}

func TestVersionEndpoint_SingleRequest_NotRejectedByConcurrencyLimit(t *testing.T) {
	t.Cleanup(drainStatusVersionSem)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/version?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code == http.StatusServiceUnavailable {
		t.Errorf("a single /version request should never be rejected by the concurrency limiter; got 503: %s", w.Body.String())
	}
}

// TestStatusVersionEndpoint_ConcurrentBeyondLimit_Returns503 fills the shared
// semaphore (deterministically, no goroutines) and pins 503 + a JSON busy body
// for BOTH /status and /version.
func TestStatusVersionEndpoint_ConcurrentBeyondLimit_Returns503(t *testing.T) {
	t.Cleanup(drainStatusVersionSem)

	for i := 0; i < statusVersionConcurrencyLimit; i++ {
		statusVersionConcurrencySem <- struct{}{} // simulate an acquired slot
	}

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	for _, path := range []string{"/status", "/version"} {
		r := httptest.NewRequest(http.MethodGet, path+"?service=testsvc", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)

		if w.Code != http.StatusServiceUnavailable {
			t.Errorf("%s over capacity: expected 503, got %d (body: %s)", path, w.Code, w.Body.String())
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s busy response Content-Type: got %q, want application/json", path, ct)
		}
		var errResp map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil {
			t.Errorf("%s busy response should be JSON: %v (body: %s)", path, err, w.Body.String())
		}
		if !strings.Contains(errResp["error"], "retry shortly") {
			t.Errorf("%s expected a 'retry shortly' busy message; got: %s", path, errResp["error"])
		}
	}
}

// TestStatusVersionEndpoint_ReleasesSlot_SubsequentRequestSucceeds pins that a
// freed slot admits a request whose deferred release frees it again (no leak
// on the success path).
func TestStatusVersionEndpoint_ReleasesSlot_SubsequentRequestSucceeds(t *testing.T) {
	t.Cleanup(drainStatusVersionSem)

	for i := 0; i < statusVersionConcurrencyLimit; i++ {
		statusVersionConcurrencySem <- struct{}{}
	}

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	// Semaphore full: rejected as busy.
	r := httptest.NewRequest(http.MethodGet, "/status?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("first /status (semaphore full): expected 503, got %d", w.Code)
	}

	// Free exactly one slot.
	<-statusVersionConcurrencySem

	// Takes the freed slot; its own defer releases it again.
	r2 := httptest.NewRequest(http.MethodGet, "/status?service=testsvc", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if w2.Code == http.StatusServiceUnavailable {
		t.Errorf("second /status after freeing a slot: unexpectedly still rejected as busy (body: %s)", w2.Body.String())
	}

	// A leaked slot would reject this one; the OTHER endpoint proves sharing.
	r3 := httptest.NewRequest(http.MethodGet, "/version?service=testsvc", nil)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, r3)
	if w3.Code == http.StatusServiceUnavailable {
		t.Errorf("third request (/version) immediately after the second: unexpectedly rejected as busy — the second request's slot was not released (body: %s)", w3.Body.String())
	}
}

// TestStatusVersionEndpoint_ReleaseOnDownstreamError_SubsequentRequestSucceeds
// pins that the slot is released even when the wrapped handler fails (403).
func TestStatusVersionEndpoint_ReleaseOnDownstreamError_SubsequentRequestSucceeds(t *testing.T) {
	t.Cleanup(drainStatusVersionSem)

	// Leave exactly one slot free.
	for i := 0; i < statusVersionConcurrencyLimit-1; i++ {
		statusVersionConcurrencySem <- struct{}{}
	}

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	// Takes the last slot, then fails downstream (not whitelisted), not busy.
	r := httptest.NewRequest(http.MethodGet, "/status?service=not_allowed", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("first /status (forbidden service): expected 403, got %d (body: %s)", w.Code, w.Body.String())
	}

	// The slot is free again: the other endpoint must not be rejected as busy.
	r2 := httptest.NewRequest(http.MethodGet, "/version?service=testsvc", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, r2)
	if w2.Code == http.StatusServiceUnavailable {
		t.Errorf("request after a downstream (non-busy) error on the prior request: unexpectedly rejected as busy — slot was not released (body: %s)", w2.Body.String())
	}
}

// ─── /health endpoint ──────────────────────────────────────────────────────

func TestHealthEndpoint_Returns200(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: true}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("/health: expected 200, got %d", w.Code)
	}
}

func TestHealthEndpoint_IsOpen_NoAuthRequired(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: true}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code == http.StatusUnauthorized {
		t.Error("/health should be an open endpoint — no 401 expected")
	}
}

func TestHealthEndpoint_ReturnsJSONContentType(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: false}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	ct := w.Header().Get("Content-Type")
	if ct != "application/json" {
		t.Errorf("/health Content-Type: got %q, want %q", ct, "application/json")
	}
}

func TestHealthEndpoint_BodyIsValidJSON(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: false}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/health response body is not valid JSON: %s", w.Body.String())
	}
}

func TestHealthEndpoint_BodyHasStatusOk(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: false}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	var h HealthJSON
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		t.Fatalf("/health: unmarshal error: %v", err)
	}
	if h.Status != "ok" {
		t.Errorf("/health: status field: got %q, want %q", h.Status, "ok")
	}
}

func TestHealthEndpoint_MethodNotAllowed(t *testing.T) {
	mux := http.NewServeMux()
	cfg := &Config{Auth: AuthConfig{Enabled: false}}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodPost, "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("/health POST: expected 405, got %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("/health POST Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("/health POST body is not valid JSON: %s", w.Body.String())
	}
}

// ─── /restart (and other mutating endpoints) return text/plain ────────────

func TestRestartEndpoint_PlainTextContentType_WhenAuth(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlersOn(mux, cfg)

	r := httptest.NewRequest(http.MethodGet, "/restart?service=testsvc", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)

	ct := w.Header().Get("Content-Type")
	if ct == "application/json" {
		t.Errorf("/restart must not return application/json; got %q", ct)
	}
}

// ─── Protected endpoints require JWT ──────────────────────────────────────

func TestRegisterHandlers_ProtectedEndpointsRequireJWT(t *testing.T) {
	pub, _, sshKey := mustGenerateSSHKeyPair(t)
	mustSetPublicKey(t, pub, sshKey)

	mux := http.NewServeMux()
	cfg := &Config{
		ServiceName:     "testsvc",
		HostSelector:    "testsvc",
		AllowedServices: []string{"testsvc"},
		Auth:            AuthConfig{Enabled: true},
	}
	RegisterHandlersOn(mux, cfg)

	for _, path := range []string{"/restart", "/start", "/stop", "/enable", "/disable"} {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path+"?service=testsvc", nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("%s without token: expected 401, got %d", path, w.Code)
			}
		})
	}
}

// ─── RegisterHandlers — default mux entry point ───────────────────────────
// These tests swap http.DefaultServeMux for a fresh mux and restore it.

func TestRegisterHandlers_DefaultMux_RegistersUsage(t *testing.T) {
	prev := http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
	t.Cleanup(func() { http.DefaultServeMux = prev })

	cfg := &Config{
		ServiceName:     "default_mux_svc",
		HostSelector:    "default_mux_svc",
		ListenAddress:   ":50080",
		AllowedServices: []string{"default_mux_svc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlers(cfg)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("default-mux GET /: expected 200, got %d", w.Code)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("default-mux GET / body is not valid JSON: %s", w.Body.String())
	}
}

func TestRegisterHandlers_DefaultMux_StatusEndpointResponds(t *testing.T) {
	prev := http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
	t.Cleanup(func() { http.DefaultServeMux = prev })

	cfg := &Config{
		ServiceName:     "dm_svc",
		HostSelector:    "dm_svc",
		ListenAddress:   ":50080",
		AllowedServices: []string{"dm_svc"},
		Auth:            AuthConfig{Enabled: false},
	}
	RegisterHandlers(cfg)

	r := httptest.NewRequest(http.MethodGet, "/status?service=dm_svc", nil)
	w := httptest.NewRecorder()
	http.DefaultServeMux.ServeHTTP(w, r)

	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("default-mux /status Content-Type: got %q, want application/json", ct)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("default-mux /status body is not valid JSON: %s", w.Body.String())
	}
}

// ─── buildUsageJSON — port-parsing branches ───────────────────────────────
// Other tests cover ":port"; these cover "host:port" (trailing field) and a bare port.

func TestBuildUsageJSON_HostPortListenAddress_ExtractsTrailingPort(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: "0.0.0.0:51234"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	found := false
	for _, ex := range u.Examples.ViaServerIP {
		if strings.Contains(ex, ":51234/") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ViaServerIP example with :51234; got: %v", u.Examples.ViaServerIP)
	}
	for _, ex := range u.Examples.ViaServerIP {
		if strings.Contains(ex, ":0.0.0.0:") {
			t.Errorf("port-parsing should strip the host; got: %s", ex)
		}
	}
}

func TestBuildUsageJSON_NoColonListenAddress_KeepsValueAsPort(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: "60000"}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)

	found := false
	for _, ex := range u.Examples.ViaServerIP {
		if strings.Contains(ex, ":60000/") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ViaServerIP example with :60000; got: %v", u.Examples.ViaServerIP)
	}
}

func TestBuildUsageJSON_EmptyListenAddress_FallsBackToDefault(t *testing.T) {
	cfg := &Config{ServiceName: "svc", ListenAddress: ""}
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	u := buildUsageJSON(r, cfg)
	if u.Server.ListenAddress != ":50080" {
		t.Errorf("Server.ListenAddress: got %q, want %q (default fallback)", u.Server.ListenAddress, ":50080")
	}
}

// ─── /config and /env (per-service file reads) ─────────────────────────────

// newServiceFileCfg creates ${workdir}/${svc}/ (plus the config file and .env
// when their content is non-empty) and returns a Config serving it: selector
// "sel", auth disabled.
func newServiceFileCfg(t *testing.T, svc, configContent, envContent string) *Config {
	t.Helper()
	workdir := mkTempDir(t)
	dir := filepath.Join(workdir, svc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if configContent != "" {
		if err := os.WriteFile(filepath.Join(dir, "target.config.yml"), []byte(configContent), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
	}
	if envContent != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(envContent), 0o600); err != nil {
			t.Fatalf("write env: %v", err)
		}
	}
	return &Config{
		ServiceName:          svc,
		HostSelector:         "sel",
		AllowedServices:      []string{svc},
		ContinuousDeployment: []ServiceEntry{{Selector: "sel", Service: svc, Workdir: workdir}},
	}
}

func getRec(t *testing.T, cfg *Config, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	RegisterHandlersOn(mux, cfg)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

// TestResolveServiceDir_MatchesServiceDirHelper pins resolveServiceDir to the
// shared serviceDir(entry) rule used by CD, the secrets fetch and /config, /env.
func TestResolveServiceDir_MatchesServiceDirHelper(t *testing.T) {
	entry := ServiceEntry{Service: "svc", Selector: "sel", Workdir: "/custom/base"}
	cfg := &Config{
		ServiceName:          "svc",
		HostSelector:         "sel",
		HostSelectors:        []string{"sel"},
		AllowedServices:      []string{"svc"},
		ContinuousDeployment: []ServiceEntry{entry},
	}

	dir, gotEntry, err := resolveServiceDir(cfg, "svc")
	if err != nil {
		t.Fatalf("resolveServiceDir: %v", err)
	}
	if want := serviceDir(entry); dir != want {
		t.Errorf("resolveServiceDir dir = %q, want serviceDir(entry) = %q", dir, want)
	}
	if gotEntry.Service != "svc" {
		t.Errorf("resolveServiceDir entry.Service = %q, want %q", gotEntry.Service, "svc")
	}
}

func TestConfigEndpoint_ReturnsServiceConfigFile(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "app:\n  port: 8080\n", "")
	w := getRec(t, cfg, http.MethodGet, "/config")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := w.Body.String(); got != "app:\n  port: 8080\n" {
		t.Errorf("/config body mismatch: got %q", got)
	}
}

func TestConfigEndpoint_RespectsCustomServiceConfigName(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "", "")
	// Custom service_config filename; write that file and point the entry at it.
	dir := filepath.Join(serviceWorkdir(cfg.ContinuousDeployment[0]), "svc")
	if err := os.WriteFile(filepath.Join(dir, "custom.yml"), []byte("k: v\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg.ContinuousDeployment[0].ServiceConfig = "custom.yml"

	w := getRec(t, cfg, http.MethodGet, "/config")
	if w.Code != http.StatusOK || w.Body.String() != "k: v\n" {
		t.Errorf("expected custom.yml content; got code=%d body=%q", w.Code, w.Body.String())
	}
}

// TestConfigEndpoint_MultiService_UsesRequestedServiceWorkdir pins that on a
// two-service node /config?service=X reads X's own workdir and config file.
func TestConfigEndpoint_MultiService_UsesRequestedServiceWorkdir(t *testing.T) {
	wd1 := mkTempDir(t)
	wd2 := mkTempDir(t)
	if err := os.MkdirAll(filepath.Join(wd1, "svc1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wd2, "svc2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd1, "svc1", "target.config.yml"), []byte("from: wd1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd2, "svc2", "app.yml"), []byte("from: wd2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		ServiceName:     "svc1",
		HostSelector:    "sel1",
		HostSelectors:   []string{"sel1", "sel2"},
		AllowedServices: []string{"svc1", "svc2"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "svc1", Selector: "sel1", Workdir: wd1},
			{Service: "svc2", Selector: "sel2", Workdir: wd2, ServiceConfig: "app.yml"},
		},
	}

	// ?service=svc2 → svc2's own workdir + custom config filename.
	if w := getRec(t, cfg, http.MethodGet, "/config?service=svc2"); w.Code != http.StatusOK || w.Body.String() != "from: wd2\n" {
		t.Errorf("/config?service=svc2 should read svc2's own workdir; got code=%d body=%q", w.Code, w.Body.String())
	}
	// Omitted ?service= → primary (svc1) in its own workdir.
	if w := getRec(t, cfg, http.MethodGet, "/config"); w.Code != http.StatusOK || w.Body.String() != "from: wd1\n" {
		t.Errorf("default /config should read primary svc1; got code=%d body=%q", w.Code, w.Body.String())
	}

	// Whitelisted (AllowedServices spans all hosts) but not managed here: a
	// clear error, never another service's directory.
	unmanaged := &Config{
		ServiceName:          "svc1",
		HostSelector:         "sel1",
		HostSelectors:        []string{"sel1"},
		AllowedServices:      []string{"svc1", "svc2"},
		ContinuousDeployment: []ServiceEntry{{Service: "svc1", Selector: "sel1", Workdir: wd1}},
	}
	if w := getRec(t, unmanaged, http.MethodGet, "/config?service=svc2"); w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "does not manage") {
		t.Errorf("whitelisted-but-unmanaged service should 500 'does not manage'; got code=%d body=%q", w.Code, w.Body.String())
	}
}

func TestConfigEndpoint_MissingFile_Returns500(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "", "") // dir exists, no config file written
	w := getRec(t, cfg, http.MethodGet, "/config")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("missing config file should 500; got %d (body=%s)", w.Code, w.Body.String())
	}
}

func TestConfigEndpoint_NoCDEntry_Returns500(t *testing.T) {
	cfg := &Config{ServiceName: "svc", HostSelector: "", AllowedServices: []string{"svc"}}
	w := getRec(t, cfg, http.MethodGet, "/config")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("no CD entry should 500; got %d", w.Code)
	}
}

func TestConfigEndpoint_NonGet_Returns405(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "x: y\n", "")
	w := getRec(t, cfg, http.MethodPost, "/config")
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /config should 405; got %d", w.Code)
	}
}

func TestEnvEndpoint_ReturnsSortedKeysNeverValues(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "", "DB_PASSWORD=supersecret\nAPI_KEY=abc123\n")
	w := getRec(t, cfg, http.MethodGet, "/env")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if body != "API_KEY\nDB_PASSWORD" {
		t.Errorf("/env should return sorted key names; got %q", body)
	}
	// CRITICAL: a value must NEVER appear in the /env response.
	if strings.Contains(body, "supersecret") || strings.Contains(body, "abc123") {
		t.Errorf("/env leaked a secret value: %q", body)
	}
}

func TestEnvEndpoint_QuotedValuesStayHidden(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "", "TOKEN=\"shh secret with spaces\"\nPLAIN=plainval\n")
	w := getRec(t, cfg, http.MethodGet, "/env")
	body := w.Body.String()
	if !strings.Contains(body, "TOKEN") || !strings.Contains(body, "PLAIN") {
		t.Errorf("/env should list TOKEN and PLAIN; got %q", body)
	}
	if strings.Contains(body, "shh secret") || strings.Contains(body, "plainval") {
		t.Errorf("/env leaked a value: %q", body)
	}
}

func TestEnvEndpoint_MissingFile_Returns500(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "", "") // no .env written
	w := getRec(t, cfg, http.MethodGet, "/env")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("missing .env should 500; got %d", w.Code)
	}
}

// TestConfigEnvEndpoints_RequireAuthWhenEnabled pins both routes behind
// RequireJWT: no token with auth enabled → 401, never served.
func TestConfigEnvEndpoints_RequireAuthWhenEnabled(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "x: y\n", "K=v\n")
	cfg.Auth = AuthConfig{Enabled: true}
	for _, path := range []string{"/config", "/env"} {
		w := getRec(t, cfg, http.MethodGet, path)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s with auth enabled + no token should 401; got %d", path, w.Code)
		}
	}
}

// TestConfigEnvEndpoints_ServiceParamCannotTraverse pins the traversal defence,
// which is HandleAction's pre-checks, not the readers: '/' → validServiceName
// 400; ".." passes the regex but not the isAllowedService whitelist → 403.
func TestConfigEnvEndpoints_ServiceParamCannotTraverse(t *testing.T) {
	cfg := newServiceFileCfg(t, "svc", "x: y\n", "K=v\n")
	cases := []struct {
		query string
		want  int
	}{
		{"service=..", http.StatusForbidden},                // passes regex, blocked by whitelist
		{"service=../../etc/passwd", http.StatusBadRequest}, // '/' → rejected by validServiceName
		{"service=foo/bar", http.StatusBadRequest},          // '/' → rejected by validServiceName
		{"service=%2e%2e%2f", http.StatusBadRequest},        // decoded "../" contains '/'
	}
	for _, ep := range []string{"/config", "/env"} {
		for _, tc := range cases {
			w := getRec(t, cfg, http.MethodGet, ep+"?"+tc.query)
			if w.Code != tc.want {
				t.Errorf("%s?%s: got %d, want %d (body=%s)", ep, tc.query, w.Code, tc.want, w.Body.String())
			}
		}
	}
}
