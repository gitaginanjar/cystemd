package service

// Vault paths run against setupFakeVault's one shared in-memory client (never a
// per-test server); live Vault is gated by hasVaultConfig(); temp dirs come from
// mkTempDir, not t.TempDir(). Why: DOCS/CLAUDE.md § Testing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	vault "github.com/hashicorp/vault-client-go"
)

// mkTempDir returns a temp dir removed at cleanup; use it instead of
// t.TempDir() here and in cd_test.go. Why: DOCS/CLAUDE.md § Testing
func mkTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cystemd_secrets_*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// ─── buildKvV2Path ─────────────────────────────────────────────────────────

func TestBuildKvV2Path_DefaultUserPath(t *testing.T) {
	got := buildKvV2Path("kubernetes/devtools/cystemd/cystemd")
	want := "kubernetes/data/devtools/cystemd/cystemd"
	if got != want {
		t.Errorf("buildKvV2Path: got %q, want %q", got, want)
	}
}

func TestBuildKvV2Path_StripsLeadingSlash(t *testing.T) {
	got := buildKvV2Path("/secret/foo/bar")
	want := "secret/data/foo/bar"
	if got != want {
		t.Errorf("buildKvV2Path with leading slash: got %q, want %q", got, want)
	}
}

func TestBuildKvV2Path_TwoSegments(t *testing.T) {
	got := buildKvV2Path("secret/myapp")
	if got != "secret/data/myapp" {
		t.Errorf("buildKvV2Path two-segment: got %q, want %q", got, "secret/data/myapp")
	}
}

func TestBuildKvV2Path_NoSlashUnchanged(t *testing.T) {
	if got := buildKvV2Path("kubernetes"); got != "kubernetes" {
		t.Errorf("mount-only path should be unchanged; got %q", got)
	}
}

func TestBuildKvV2Path_EmptyStringUnchanged(t *testing.T) {
	if got := buildKvV2Path(""); got != "" {
		t.Errorf("empty path should be unchanged; got %q", got)
	}
}

// ─── flattenStringMap ──────────────────────────────────────────────────────

func TestFlattenStringMap_StringValues(t *testing.T) {
	in := map[string]interface{}{
		"API_KEY":  "secret123",
		"DATABASE": "postgres://example/db",
	}
	got := flattenStringMap(in)
	if got["API_KEY"] != "secret123" {
		t.Errorf("API_KEY: got %q, want %q", got["API_KEY"], "secret123")
	}
	if got["DATABASE"] != "postgres://example/db" {
		t.Errorf("DATABASE: got %q", got["DATABASE"])
	}
}

func TestFlattenStringMap_NumericValues(t *testing.T) {
	in := map[string]interface{}{
		"INT":   42,
		"INT64": int64(42_000_000_000),
		"FLOAT": 3.14,
	}
	got := flattenStringMap(in)
	if got["INT"] != "42" {
		t.Errorf("INT: got %q, want %q", got["INT"], "42")
	}
	if got["INT64"] != "42000000000" {
		t.Errorf("INT64: got %q, want %q", got["INT64"], "42000000000")
	}
	if !strings.HasPrefix(got["FLOAT"], "3.14") {
		t.Errorf("FLOAT: got %q, want prefix 3.14", got["FLOAT"])
	}
}

func TestFlattenStringMap_BoolValues(t *testing.T) {
	in := map[string]interface{}{"DEBUG": true, "VERBOSE": false}
	got := flattenStringMap(in)
	if got["DEBUG"] != "true" {
		t.Errorf("DEBUG: got %q", got["DEBUG"])
	}
	if got["VERBOSE"] != "false" {
		t.Errorf("VERBOSE: got %q", got["VERBOSE"])
	}
}

func TestFlattenStringMap_JSONNumberValues(t *testing.T) {
	// vault-client-go decodes JSON numbers as json.Number (UseNumber()).
	in := map[string]interface{}{
		"PORT":  json.Number("5432"),
		"RATIO": json.Number("0.99"),
	}
	got := flattenStringMap(in)
	if got["PORT"] != "5432" {
		t.Errorf("PORT: got %q, want %q", got["PORT"], "5432")
	}
	if got["RATIO"] != "0.99" {
		t.Errorf("RATIO: got %q, want %q", got["RATIO"], "0.99")
	}
}

func TestFlattenStringMap_SkipsNil(t *testing.T) {
	in := map[string]interface{}{"NIL_VAL": nil, "OK": "yes"}
	got := flattenStringMap(in)
	if _, ok := got["NIL_VAL"]; ok {
		t.Errorf("nil value should be skipped; got %v", got)
	}
	if got["OK"] != "yes" {
		t.Errorf("OK: got %q, want yes", got["OK"])
	}
}

func TestFlattenStringMap_JSONEncodesNestedAndArrays(t *testing.T) {
	in := map[string]interface{}{
		"NESTED": map[string]interface{}{"x": "y"},
		"ARRAY":  []interface{}{"a", "b"},
		"STR":    "ok",
	}
	got := flattenStringMap(in)
	if got["NESTED"] != `{"x":"y"}` {
		t.Errorf("nested map should be JSON-encoded, not dropped; got %q", got["NESTED"])
	}
	if got["ARRAY"] != `["a","b"]` {
		t.Errorf("array should be JSON-encoded, not dropped; got %q", got["ARRAY"])
	}
	if got["STR"] != "ok" {
		t.Errorf("STR: got %q, want ok", got["STR"])
	}
}

func TestFlattenStringMap_NonScalarEncodingIsByteStable(t *testing.T) {
	// Despite random map order the encoding must be byte-identical, or the
	// unchanged-content compare sees phantom drift and restarts every cycle.
	in := map[string]interface{}{
		"NESTED": map[string]interface{}{"zebra": "1", "alpha": "2", "mid": "3"},
		"ARRAY":  []interface{}{"a", "b", "c"},
	}
	first := flattenStringMap(in)
	for i := 0; i < 50; i++ {
		got := flattenStringMap(in)
		if got["NESTED"] != first["NESTED"] {
			t.Fatalf("NESTED encoding not stable across calls: %q vs %q", got["NESTED"], first["NESTED"])
		}
		if got["ARRAY"] != first["ARRAY"] {
			t.Fatalf("ARRAY encoding not stable across calls: %q vs %q", got["ARRAY"], first["ARRAY"])
		}
	}
	if first["NESTED"] != `{"alpha":"2","mid":"3","zebra":"1"}` {
		t.Errorf("NESTED: got %q, want sorted-key JSON", first["NESTED"])
	}
	if first["ARRAY"] != `["a","b","c"]` {
		t.Errorf("ARRAY: got %q", first["ARRAY"])
	}
}

func TestFlattenStringMap_UnquotedJSONArrayRoundTrips(t *testing.T) {
	// JSON with no env-special chars (`[1,2,3]`) is written UNQUOTED; it must
	// still round-trip verbatim (the no-quoting branch).
	enc := flattenStringMap(map[string]interface{}{"NUMS": []interface{}{1, 2, 3}})
	if enc["NUMS"] != "[1,2,3]" {
		t.Fatalf("NUMS encoding: got %q, want [1,2,3]", enc["NUMS"])
	}
	if needsEnvQuoting(enc["NUMS"]) {
		t.Errorf("[1,2,3] should not need env-quoting (no special chars)")
	}
	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "arr.env")
	if err := os.WriteFile(envPath, buildEnvBytes(enc), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	parsed, err := ParseEnvFile(envPath)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	if parsed["NUMS"] != "[1,2,3]" {
		t.Errorf("round-trip NUMS: got %q, want [1,2,3]", parsed["NUMS"])
	}
}

func TestFlattenStringMap_SkipsEmptyKey(t *testing.T) {
	in := map[string]interface{}{"": "should be skipped", "OK": "kept"}
	got := flattenStringMap(in)
	if _, ok := got[""]; ok {
		t.Error("empty key should be skipped")
	}
	if got["OK"] != "kept" {
		t.Errorf("OK: got %q", got["OK"])
	}
}

// ─── needsEnvQuoting ───────────────────────────────────────────────────────

func TestNeedsEnvQuoting_PlainAlphaNumeric(t *testing.T) {
	for _, s := range []string{"simple", "value123", "abc_DEF-123"} {
		if needsEnvQuoting(s) {
			t.Errorf("plain value %q should not need quoting", s)
		}
	}
}

func TestNeedsEnvQuoting_Empty(t *testing.T) {
	if !needsEnvQuoting("") {
		t.Error("empty value should need quoting")
	}
}

func TestNeedsEnvQuoting_SpecialCharacters(t *testing.T) {
	cases := []struct {
		in   string
		name string
	}{
		{"has space", "space"},
		{"has\ttab", "tab"},
		{"has\nnewline", "newline"},
		{"has\"quote", "double-quote"},
		{"has'apostrophe", "single-quote"},
		{"has#hash", "hash"},
		{"has$dollar", "dollar"},
		{"has\\backslash", "backslash"},
		{"has`backtick", "backtick"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !needsEnvQuoting(c.in) {
				t.Errorf("value %q (%s) should need quoting", c.in, c.name)
			}
		})
	}
}

// ─── buildEnvBytes ─────────────────────────────────────────────────────────

func TestBuildEnvBytes_WritesSortedKeys(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "out.env")
	secrets := map[string]string{
		"Z_LAST":  "last",
		"A_FIRST": "first",
		"M_MID":   "mid",
	}
	if err := os.WriteFile(path, buildEnvBytes(secrets), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := strings.TrimSpace(string(data))
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines; got %d (%q)", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "A_FIRST=") {
		t.Errorf("first line should be A_FIRST=...; got %q", lines[0])
	}
	if !strings.HasPrefix(lines[2], "Z_LAST=") {
		t.Errorf("last line should be Z_LAST=...; got %q", lines[2])
	}
}

func TestBuildEnvBytes_QuotesValuesWithSpecialChars(t *testing.T) {
	out := string(buildEnvBytes(map[string]string{
		"PLAIN":      "simple",
		"WITH_SPACE": "value with space",
		"EMPTY":      "",
	}))
	if !strings.Contains(out, "PLAIN=simple\n") {
		t.Errorf("plain value should not be quoted; got: %s", out)
	}
	if !strings.Contains(out, `WITH_SPACE="value with space"`) {
		t.Errorf("space-containing value should be quoted; got: %s", out)
	}
	if !strings.Contains(out, `EMPTY=""`) {
		t.Errorf("empty value should be quoted as \"\"; got: %s", out)
	}
}

func TestBuildEnvBytes_PermissionsAre0600(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "out.env")
	if err := os.WriteFile(path, buildEnvBytes(map[string]string{"K": "v"}), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("permissions: got %o, want 0600 (secret file must not be world-readable)", mode)
	}
}

func TestBuildEnvBytes_EmptyMap_ReturnsEmpty(t *testing.T) {
	if got := buildEnvBytes(map[string]string{}); len(got) != 0 {
		t.Errorf("expected empty bytes; got %q", got)
	}
}

func TestBuildEnvBytes_OverwritesExisting(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "out.env")

	if err := os.WriteFile(path, buildEnvBytes(map[string]string{"OLD": "value"}), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := os.WriteFile(path, buildEnvBytes(map[string]string{"NEW": "value"}), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	data, _ := os.ReadFile(path)
	out := string(data)
	if strings.Contains(out, "OLD=") {
		t.Errorf("file should be overwritten, OLD must not remain; got: %s", out)
	}
	if !strings.Contains(out, "NEW=") {
		t.Errorf("file should contain NEW=; got: %s", out)
	}
}

// ─── FetchAndWriteSecrets — fail-soft no-op guards ────────────────────────

func TestFetchAndWriteSecrets_NilCfg_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(nil)
	if !strings.Contains(buf.String(), "no Vault config") {
		t.Errorf("expected 'no Vault config' info log; got:\n%s", buf.String())
	}
}

func TestFetchAndWriteSecrets_NoVaultClient_NoOp(t *testing.T) {
	prev := globalVaultState
	globalVaultState = nil
	t.Cleanup(func() { globalVaultState = prev })

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "should_not_exist.env")

	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(&VaultConfig{Path: "anything", EnvFile: envPath})

	if !strings.Contains(buf.String(), "Vault client not initialised") {
		t.Errorf("expected 'Vault client not initialised' log; got:\n%s", buf.String())
	}
	if _, err := os.Stat(envPath); err == nil {
		t.Error("env file should not have been written when Vault client is nil")
	}
}

func TestFetchAndWriteSecrets_StateWithNilClient_NoOp(t *testing.T) {
	prev := globalVaultState
	globalVaultState = &VaultState{Client: nil} // state present but no client
	t.Cleanup(func() { globalVaultState = prev })

	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(&VaultConfig{Path: "anything"})

	if !strings.Contains(buf.String(), "Vault client not initialised") {
		t.Errorf("expected 'Vault client not initialised' log; got:\n%s", buf.String())
	}
}

// ─── FetchAndWriteSecrets — in-memory mock Vault ──────────────────────────

// The one process-wide mock client: in-memory RoundTripper, built once,
// RetryMax 0. Why: DOCS/CLAUDE.md § Testing
var (
	fakeVaultOnce    sync.Once
	fakeVaultClient  *vault.Client
	fakeVaultHandler http.HandlerFunc
)

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// setupFakeVault routes the shared mock client to handler and installs it as
// globalVaultState (token auth) until cleanup.
func setupFakeVault(t *testing.T, handler http.HandlerFunc) {
	t.Helper()

	fakeVaultOnce.Do(func() {
		httpClient := &http.Client{
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				rec := httptest.NewRecorder()
				if fakeVaultHandler == nil {
					http.NotFound(rec, r)
				} else {
					fakeVaultHandler(rec, r)
				}
				resp := rec.Result()
				body, _ := io.ReadAll(resp.Body)
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.Request = r
				return resp, nil
			}),
		}
		var err error
		fakeVaultClient, err = vault.New(
			vault.WithAddress("http://fake-vault.invalid"),
			vault.WithHTTPClient(httpClient),
			vault.WithRetryConfiguration(vault.RetryConfiguration{RetryMax: 0}),
		)
		if err != nil {
			panic("vault.New: " + err.Error())
		}
	})

	fakeVaultHandler = handler
	prev := globalVaultState
	globalVaultState = &VaultState{
		Client:     fakeVaultClient,
		AuthMethod: VaultAuthMethodToken,
		Address:    "http://fake-vault.invalid",
	}
	t.Cleanup(func() {
		globalVaultState = prev
		fakeVaultHandler = nil
	})
}

// vaultEnvelope wraps the inner Data in Vault's outer "data" JSON envelope
// (the SDK unwraps the outer "data" automatically).
func vaultEnvelope(inner interface{}) []byte {
	b, _ := json.Marshal(map[string]interface{}{
		"request_id":     "fake-id",
		"lease_id":       "",
		"renewable":      false,
		"lease_duration": 0,
		"data":           inner,
	})
	return b
}

func TestFetchAndWriteSecrets_EmptyPath_NoOp(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("handler should not be invoked when path is empty (got %s)", r.URL.Path)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "no_path.env")

	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(&VaultConfig{Path: "", EnvFile: envPath})

	if !strings.Contains(buf.String(), "VAULT_PATH is empty") {
		t.Errorf("expected 'VAULT_PATH is empty' log; got:\n%s", buf.String())
	}
	if _, err := os.Stat(envPath); err == nil {
		t.Error("env file should not have been written when path is empty")
	}
}

func TestFetchAndWriteSecrets_KvV2_Success_WritesEnv(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/kubernetes/data/devtools/cystemd/cystemd") {
			http.NotFound(w, r)
			return
		}
		body := vaultEnvelope(map[string]interface{}{
			"data": map[string]interface{}{
				"DB_PASSWORD": "supersecret",
				"API_KEY":     "abc-123",
			},
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "v2.env")

	FetchAndWriteSecrets(&VaultConfig{
		Path:    "kubernetes/devtools/cystemd/cystemd",
		EnvFile: envPath,
	})

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("env file should be written: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "DB_PASSWORD=supersecret\n") {
		t.Errorf("expected DB_PASSWORD=supersecret line; got:\n%s", out)
	}
	if !strings.Contains(out, "API_KEY=abc-123\n") {
		t.Errorf("expected API_KEY=abc-123 line; got:\n%s", out)
	}
	// Freshly created secret file must be owner-only.
	if info, statErr := os.Stat(envPath); statErr == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("written .env must be 0600; got %04o", info.Mode().Perm())
	}
}

// ─── .env permission enforcement (ensureEnvFileMode) ───────────────────────

func TestEnsureEnvFileMode_TightensLooseFile(t *testing.T) {
	dir := mkTempDir(t)
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("X=y\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	buf := captureLogs(t, LevelInfo)
	ensureEnvFileMode(p)

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("got %04o, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(buf.String(), "tightened") {
		t.Errorf("expected Info 'tightened' log; got:\n%s", buf.String())
	}
}

func TestEnsureEnvFileMode_AlreadyRestrictive_NoChangeNoLog(t *testing.T) {
	dir := mkTempDir(t)
	p := filepath.Join(dir, ".env")
	if err := os.WriteFile(p, []byte("X=y\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	buf := captureLogs(t, LevelDebug)
	ensureEnvFileMode(p)

	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("got %04o, want 0600", info.Mode().Perm())
	}
	if strings.Contains(buf.String(), "tightened") {
		t.Errorf("a 0600 file should produce no log; got:\n%s", buf.String())
	}
}

func TestEnsureEnvFileMode_MissingFile_NoOp(t *testing.T) {
	dir := mkTempDir(t)
	buf := captureLogs(t, LevelDebug)
	ensureEnvFileMode(filepath.Join(dir, "does-not-exist.env"))
	if strings.Contains(buf.String(), "tightened") || strings.Contains(buf.String(), "cannot tighten") {
		t.Errorf("a missing file should be a silent no-op; got:\n%s", buf.String())
	}
}

// TestEnsureEnvFileMode_ChmodFails_Warns uses /proc/self/status (not 0600; chmod
// is EPERM even for root) as a stand-in for a .env owned by another uid.
func TestEnsureEnvFileMode_ChmodFails_Warns(t *testing.T) {
	const procPath = "/proc/self/status"
	info, err := os.Stat(procPath)
	if err != nil || info.Mode().Perm() == 0o600 {
		t.Skip("requires /proc/self/status present and not already 0600")
	}
	buf := captureLogs(t, LevelWarning)
	ensureEnvFileMode(procPath)
	if !strings.Contains(buf.String(), "cannot tighten") {
		t.Errorf("expected chmod-failure Warning; got:\n%s", buf.String())
	}
}

// TestFetchAndWriteSecrets_PreexistingLooseEnv_TightenedOnSkipPath pins that an
// unchanged 0644 .env is tightened to 0600 without a rewrite.
// See DOCS/MEMORY.md § World-readable .env on the unchanged-content path — FIXED
func TestFetchAndWriteSecrets_PreexistingLooseEnv_TightenedOnSkipPath(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/v1/kubernetes/data/devtools/cystemd/cystemd") {
			http.NotFound(w, r)
			return
		}
		body := vaultEnvelope(map[string]interface{}{
			"data":     map[string]interface{}{"DB_PASSWORD": "supersecret"},
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, ".env")
	// Seed the file at 0644 with content IDENTICAL to what the fetch will
	// serialise, so the unchanged-content skip path fires.
	if err := os.WriteFile(envPath, []byte("DB_PASSWORD=supersecret\n"), 0o644); err != nil {
		t.Fatalf("seed env: %v", err)
	}

	buf := captureLogs(t, LevelDebug)
	FetchAndWriteSecrets(&VaultConfig{
		Path:    "kubernetes/devtools/cystemd/cystemd",
		EnvFile: envPath,
	})

	if !strings.Contains(buf.String(), "secrets unchanged") {
		t.Fatalf("precondition: expected the unchanged-content skip path to fire; got:\n%s", buf.String())
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("pre-existing .env must be tightened to 0600 even on the skip path; got %04o", info.Mode().Perm())
	}
}

func TestFetchAndWriteSecrets_KvV2_FallsBackToKvV1(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/v1/secret/data/myapp"):
			http.NotFound(w, r)
		case strings.Contains(r.URL.Path, "/v1/secret/myapp"):
			body := vaultEnvelope(map[string]interface{}{"FLAT_KEY": "flat-value"})
			w.Header().Set("Content-Type", "application/json")
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "v1fallback.env")

	buf := captureLogs(t, LevelDebug)
	FetchAndWriteSecrets(&VaultConfig{Path: "secret/myapp", EnvFile: envPath})

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("env file should be written via KV v1 fallback: %v", err)
	}
	if !strings.Contains(string(data), "FLAT_KEY=flat-value\n") {
		t.Errorf("expected FLAT_KEY=flat-value; got:\n%s", string(data))
	}
	if !strings.Contains(buf.String(), "falling back to KV v1") {
		t.Errorf("expected fallback debug log; got:\n%s", buf.String())
	}
}

func TestFetchAndWriteSecrets_BothPathsFail_NoCrashNoFile(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "missing.env")

	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(&VaultConfig{Path: "nonexistent/path/foo", EnvFile: envPath})

	if _, err := os.Stat(envPath); err == nil {
		t.Error("env file should not be written when both v2 and v1 return 404")
	}
	if !strings.Contains(buf.String(), "failed to read secrets") {
		t.Errorf("expected 'failed to read secrets' warning; got:\n%s", buf.String())
	}
}

func TestFetchAndWriteSecrets_EmptySecretMap_NoFileWritten(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		body := vaultEnvelope(map[string]interface{}{
			"data":     map[string]interface{}{},
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "empty.env")

	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})

	if _, err := os.Stat(envPath); err == nil {
		t.Error("env file should not be written when secret map is empty")
	}
	if !strings.Contains(buf.String(), "no usable secrets") {
		t.Errorf("expected 'no usable secrets' warning; got:\n%s", buf.String())
	}
}

func TestFetchAndWriteSecrets_KvV2_JSONEncodesNonScalarValues(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		body := vaultEnvelope(map[string]interface{}{
			"data": map[string]interface{}{
				"PLAIN":  "ok",
				"NUMBER": 42,
				"FLAG":   true,
				"NESTED": map[string]interface{}{"x": "y"},
				"LIST":   []interface{}{"a"},
				"NIL":    nil,
			},
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "scalar.env")

	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("env file should be written: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "PLAIN=ok\n") {
		t.Errorf("expected PLAIN=ok; got:\n%s", out)
	}
	if !strings.Contains(out, "NUMBER=42\n") {
		t.Errorf("expected NUMBER=42; got:\n%s", out)
	}
	if !strings.Contains(out, "FLAG=true\n") {
		t.Errorf("expected FLAG=true; got:\n%s", out)
	}
	// Non-scalars are JSON-encoded (quoted: the JSON contains double quotes).
	if !strings.Contains(out, `NESTED="{\"x\":\"y\"}"`+"\n") {
		t.Errorf("expected NESTED JSON-encoded; got:\n%s", out)
	}
	if !strings.Contains(out, `LIST="[\"a\"]"`+"\n") {
		t.Errorf("expected LIST JSON-encoded; got:\n%s", out)
	}
	// nil has no env representation → skipped.
	if strings.Contains(out, "NIL=") {
		t.Errorf("nil value should be skipped; got:\n%s", out)
	}
	// Round-trip: the written file parses back and the non-scalar values decode
	// to their original JSON text (proves the quoting survives ParseEnvFile).
	parsed, err := ParseEnvFile(envPath)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	if parsed["NESTED"] != `{"x":"y"}` {
		t.Errorf("round-trip NESTED: got %q", parsed["NESTED"])
	}
	if parsed["LIST"] != `["a"]` {
		t.Errorf("round-trip LIST: got %q", parsed["LIST"])
	}
}

func TestFetchAndWriteSecrets_WriteFailure_NoCrash(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		body := vaultEnvelope(map[string]interface{}{
			"data":     map[string]interface{}{"K": "v"},
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	// A missing parent dir makes writeSecretFileAtomic's tmp-file creation fail.
	envPath := filepath.Join(mkTempDir(t), "no_such_subdir", "out.env")

	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})

	if !strings.Contains(buf.String(), "cannot create temp file") {
		t.Errorf("expected 'cannot create temp file' warning from writeSecretFileAtomic; got:\n%s", buf.String())
	}
}

// ─── LoadVaultConfig — VAULT_PATH and VAULT_ENV_FILE ──────────────────────

func TestLoadVaultConfig_DefaultPath(t *testing.T) {
	os.Unsetenv("VAULT_PATH")
	cfg := LoadVaultConfig()
	if cfg.Path != DefaultVaultPath {
		t.Errorf("Path default: got %q, want %q", cfg.Path, DefaultVaultPath)
	}
	if DefaultVaultPath != "kubernetes/devtools/cystemd/cystemd" {
		t.Errorf("DefaultVaultPath constant has unexpected value: %q", DefaultVaultPath)
	}
}

func TestLoadVaultConfig_CustomPath(t *testing.T) {
	t.Setenv("VAULT_PATH", "secret/myapp/db")
	cfg := LoadVaultConfig()
	if cfg.Path != "secret/myapp/db" {
		t.Errorf("Path: got %q, want %q", cfg.Path, "secret/myapp/db")
	}
}

func TestLoadVaultConfig_DefaultEnvFile(t *testing.T) {
	os.Unsetenv("VAULT_ENV_FILE")
	cfg := LoadVaultConfig()
	if cfg.EnvFile != DefaultVaultEnvFile {
		t.Errorf("EnvFile default: got %q, want %q", cfg.EnvFile, DefaultVaultEnvFile)
	}
}

func TestLoadVaultConfig_CustomEnvFile(t *testing.T) {
	t.Setenv("VAULT_ENV_FILE", "/tmp/custom_secrets.env")
	cfg := LoadVaultConfig()
	if cfg.EnvFile != "/tmp/custom_secrets.env" {
		t.Errorf("EnvFile: got %q, want %q", cfg.EnvFile, "/tmp/custom_secrets.env")
	}
}

func TestLoadVaultConfig_LogsPathAndEnvFile(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	t.Setenv("VAULT_PATH", "logged/path/here")
	t.Setenv("VAULT_ENV_FILE", "/tmp/logged.env")

	LoadVaultConfig()

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, `path="logged/path/here"`) {
		t.Errorf("expected path in debug log msg; got:\n%s", msgs)
	}
	if !strings.Contains(msgs, `env_file="/tmp/logged.env"`) {
		t.Errorf("expected env_file in debug log msg; got:\n%s", msgs)
	}
}

// ─── FetchAndWriteSecrets — unchanged-content optimisation ────────────────

func TestFetchAndWriteSecrets_UnchangedContent_DoesNotRewrite(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		body := vaultEnvelope(map[string]interface{}{
			"data":     map[string]interface{}{"K": "v"},
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "stable.env")

	// First call: writes the file.
	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})
	info1, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}
	// Sleep enough that any new write would change the mtime (filesystems
	// vary in mtime resolution; 50 ms is comfortably above 1 ms / 10 ms).
	time.Sleep(50 * time.Millisecond)

	// Second call: same secrets → must skip rewrite.
	buf := captureLogs(t, LevelDebug)
	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})

	info2, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("file should still exist after second call: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("mtime changed (%v → %v); expected unchanged-skip to leave the file alone",
			info1.ModTime(), info2.ModTime())
	}
	if !strings.Contains(buf.String(), "secrets unchanged") {
		t.Errorf("expected 'secrets unchanged' debug log; got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "wrote 1 secret(s)") {
		t.Errorf("Info 'wrote N secret(s)' should NOT appear on unchanged refresh; got:\n%s", buf.String())
	}
}

func TestFetchAndWriteSecrets_ChangedContent_Rewrites(t *testing.T) {
	var callCount atomic.Int32
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		body := vaultEnvelope(map[string]interface{}{
			"data":     map[string]interface{}{"K": fmt.Sprintf("v%d", n)},
			"metadata": map[string]interface{}{"version": int(n)},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "changing.env")

	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath}) // K=v1
	first, _ := os.ReadFile(envPath)

	buf := captureLogs(t, LevelInfo)
	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath}) // K=v2

	second, _ := os.ReadFile(envPath)
	if bytes.Equal(first, second) {
		t.Errorf("file should be rewritten when content changes; first=%q second=%q", first, second)
	}
	if !strings.Contains(string(second), "K=v2\n") {
		t.Errorf("expected K=v2 in second-write content; got:\n%s", string(second))
	}
	if !strings.Contains(buf.String(), "wrote 1 secret(s)") {
		t.Errorf("expected Info 'wrote 1 secret(s)' on change; got:\n%s", buf.String())
	}
}

// ─── FetchAndWriteSecrets — atomic write ──────────────────────────────────

// TestFetchAndWriteSecrets_WriteFailure_PreservesExistingContent pins that a
// failed write leaves the old .env intact (a plain os.WriteFile truncates first).
// The failure is forced root-safely: a non-empty directory at "<envFile>.tmp"
// defeats both os.Remove and the O_EXCL create.
func TestFetchAndWriteSecrets_WriteFailure_PreservesExistingContent(t *testing.T) {
	var callCount atomic.Int32
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		n := callCount.Add(1)
		body := vaultEnvelope(map[string]interface{}{
			"data":     map[string]interface{}{"K": fmt.Sprintf("v%d", n)},
			"metadata": map[string]interface{}{"version": int(n)},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "out.env")

	// First call succeeds and establishes "last known good" content.
	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})
	oldContent, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("seed write should have succeeded: %v", err)
	}
	if !strings.Contains(string(oldContent), "K=v1\n") {
		t.Fatalf("seed content unexpected: %q", oldContent)
	}

	// Poison the tmp path so writeSecretFileAtomic fails before touching envPath.
	tmpPath := envPath + ".tmp"
	if err := os.MkdirAll(filepath.Join(tmpPath, "blocker"), 0o755); err != nil {
		t.Fatalf("seed: poison tmp path: %v", err)
	}

	// Vault content has changed (K=v2 now), so a real rewrite is attempted —
	// the unchanged-content skip must not short-circuit before the write.
	buf := captureLogs(t, LevelWarning)
	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})

	newContent, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("envPath must still exist after a failed write: %v", err)
	}
	if !bytes.Equal(oldContent, newContent) {
		t.Errorf("destination was mutated despite a failed write — torn/partial write gap NOT closed.\nold=%q\nnew=%q",
			oldContent, newContent)
	}
	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "cannot create temp file") && !strings.Contains(msgs, "cannot") {
		t.Errorf("expected a writeSecretFileAtomic failure warning; got:\n%s", msgs)
	}
}

// TestFetchAndWriteSecrets_StaleTmpFile_RemovedAndNotReused pins that a
// crash-left "<envFile>.tmp" (loose mode, stale content) is removed, never reused.
func TestFetchAndWriteSecrets_StaleTmpFile_RemovedAndNotReused(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		body := vaultEnvelope(map[string]interface{}{
			"data":     map[string]interface{}{"K": "fresh"},
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	})

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "out.env")
	tmpPath := envPath + ".tmp"

	// As if the process died mid-write on its very first fetch.
	if err := os.WriteFile(tmpPath, []byte("GARBAGE-FROM-A-PRIOR-CRASH"), 0o644); err != nil {
		t.Fatalf("seed: write stale tmp: %v", err)
	}

	FetchAndWriteSecrets(&VaultConfig{Path: "any/path", EnvFile: envPath})

	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("envPath should have been written: %v", err)
	}
	if strings.Contains(string(got), "GARBAGE") {
		t.Errorf("stale tmp content leaked into envPath; got:\n%s", got)
	}
	if !strings.Contains(string(got), "K=fresh\n") {
		t.Errorf("expected fresh secret content; got:\n%s", got)
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("Stat envPath: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("expected envPath mode 0600 (not the stale tmp's 0644); got %04o", perm)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("expected %q to be gone after a successful write; stat err=%v", tmpPath, err)
	}
}

// ─── ResolveRefreshInterval ────────────────────────────────────────────────

func TestResolveRefreshInterval_Default_BothEmpty(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRefreshInterval("", ""); got != DefaultVaultRefreshInterval {
		t.Errorf("default: got %v, want %v", got, DefaultVaultRefreshInterval)
	}
	if DefaultVaultRefreshInterval != time.Minute {
		t.Errorf("DefaultVaultRefreshInterval: got %v, want 1m", DefaultVaultRefreshInterval)
	}
}

func TestResolveRefreshInterval_EnvWinsOverConfig(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRefreshInterval("30s", "1h"); got != 30*time.Second {
		t.Errorf("got %v, want 30s (env should win over config)", got)
	}
}

func TestResolveRefreshInterval_ConfigUsedWhenEnvEmpty(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRefreshInterval("", "5m"); got != 5*time.Minute {
		t.Errorf("got %v, want 5m (config when env empty)", got)
	}
}

func TestResolveRefreshInterval_TrimsWhitespace(t *testing.T) {
	captureLogs(t, LevelInfo)
	if got := ResolveRefreshInterval("  2m  ", ""); got != 2*time.Minute {
		t.Errorf("got %v, want 2m (whitespace should be trimmed)", got)
	}
}

func TestResolveRefreshInterval_Zero_DisablesPeriodic(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	if got := ResolveRefreshInterval("0", ""); got != 0 {
		t.Errorf("got %v, want 0 (periodic disabled)", got)
	}
	if !strings.Contains(buf.String(), "periodic refresh disabled") {
		t.Errorf("expected disable log; got:\n%s", buf.String())
	}
}

func TestResolveRefreshInterval_InvalidFallsBackToDefault(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	if got := ResolveRefreshInterval("not-a-duration", ""); got != DefaultVaultRefreshInterval {
		t.Errorf("invalid env: got %v, want default %v", got, DefaultVaultRefreshInterval)
	}
	if !strings.Contains(buf.String(), "invalid refresh interval") {
		t.Errorf("expected warning log; got:\n%s", buf.String())
	}
}

func TestResolveRefreshInterval_NegativeFallsBackToDefault(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	if got := ResolveRefreshInterval("-30s", ""); got != DefaultVaultRefreshInterval {
		t.Errorf("negative env: got %v, want default %v", got, DefaultVaultRefreshInterval)
	}
	if !strings.Contains(buf.String(), "negative refresh interval") {
		t.Errorf("expected warning log; got:\n%s", buf.String())
	}
}

func TestResolveRefreshInterval_LogsSourceEnv(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ResolveRefreshInterval("45s", "ignored")
	if !strings.Contains(buf.String(), "VAULT_REFRESH_INTERVAL env") {
		t.Errorf("expected source=env in log; got:\n%s", buf.String())
	}
}

func TestResolveRefreshInterval_LogsSourceConfig(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ResolveRefreshInterval("", "45s")
	if !strings.Contains(buf.String(), "vault.refresh_interval") {
		t.Errorf("expected source=config in log; got:\n%s", buf.String())
	}
}

// ─── live Vault integration ───────────────────────────────────────────────

// TestFetchAndWriteSecrets_LiveVault_WritesEnvFile runs the happy path against a
// real Vault (AppRole env + a readable VAULT_PATH); skipped without one.
func TestFetchAndWriteSecrets_LiveVault_WritesEnvFile(t *testing.T) {
	if !hasVaultConfig() {
		t.Skip("VAULT_ADDRESS / VAULT_ROLE_ID / VAULT_SECRET_ID not set; skipping live Vault test")
	}

	prev := globalVaultState
	t.Cleanup(func() { globalVaultState = prev })

	cfg := LoadVaultConfig()
	if err := InitVault(cfg); err != nil {
		t.Fatalf("live Vault InitVault failed: %v", err)
	}

	dir := mkTempDir(t)
	envPath := filepath.Join(dir, "live.env")
	cfg.EnvFile = envPath

	FetchAndWriteSecrets(cfg)

	data, err := os.ReadFile(envPath)
	if err != nil {
		t.Skipf("env file not written (path may be missing in Vault): %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		if !strings.Contains(line, "=") {
			t.Errorf("malformed env line: %q", line)
		}
	}
}

// ─── ParseEnvFile ──────────────────────────────────────────────────────────

func TestParseEnvFile_BasicKeyValue(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "test.env")
	content := "KEY_A=value_a\nKEY_B=value_b\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	pairs, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	if pairs["KEY_A"] != "value_a" {
		t.Errorf("KEY_A: got %q, want %q", pairs["KEY_A"], "value_a")
	}
	if pairs["KEY_B"] != "value_b" {
		t.Errorf("KEY_B: got %q, want %q", pairs["KEY_B"], "value_b")
	}
}

func TestParseEnvFile_SkipsCommentsAndBlanks(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "test.env")
	content := "# comment\n\nKEY=val\n# another\n"
	os.WriteFile(path, []byte(content), 0600)
	pairs, _ := ParseEnvFile(path)
	if len(pairs) != 1 || pairs["KEY"] != "val" {
		t.Errorf("got %v, want map[KEY:val]", pairs)
	}
}

func TestParseEnvFile_DoubleQuotedValue(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "test.env")
	// buildEnvBytes uses strconv.Quote, e.g. "my value" → `"my value"`
	os.WriteFile(path, []byte(`KEY="my value"`+"\n"), 0600)
	pairs, _ := ParseEnvFile(path)
	if pairs["KEY"] != "my value" {
		t.Errorf("KEY: got %q, want %q", pairs["KEY"], "my value")
	}
}

func TestParseEnvFile_SingleQuotedValue(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "test.env")
	os.WriteFile(path, []byte("KEY='single quoted'\n"), 0600)
	pairs, _ := ParseEnvFile(path)
	if pairs["KEY"] != "single quoted" {
		t.Errorf("KEY: got %q, want %q", pairs["KEY"], "single quoted")
	}
}

func TestParseEnvFile_FileNotFound(t *testing.T) {
	_, err := ParseEnvFile("/nonexistent/path/test.env")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseEnvFile_EmptyFile(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "empty.env")
	os.WriteFile(path, []byte(""), 0600)
	pairs, err := ParseEnvFile(path)
	if err != nil {
		t.Fatalf("ParseEnvFile: %v", err)
	}
	if len(pairs) != 0 {
		t.Errorf("expected empty map, got %v", pairs)
	}
}

// ─── StartEnvReload ────────────────────────────────────────────────────────

func TestStartEnvReload_PicksUpNewValues(t *testing.T) {
	dir := mkTempDir(t)
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("TEST_RELOAD_KEY=initial\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Ensure key starts at known state.
	os.Setenv("TEST_RELOAD_KEY", "initial")
	t.Cleanup(func() { os.Unsetenv("TEST_RELOAD_KEY") })

	cfg := &Config{Auth: AuthConfig{Enabled: false}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartEnvReload(ctx, envPath, cfg, 20*time.Millisecond)

	time.Sleep(5 * time.Millisecond)
	os.WriteFile(envPath, []byte("TEST_RELOAD_KEY=updated\n"), 0600)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if os.Getenv("TEST_RELOAD_KEY") == "updated" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("TEST_RELOAD_KEY not updated; got %q", os.Getenv("TEST_RELOAD_KEY"))
}

// TestStartEnvReload_RemovedKeyIsUnset pins that a key deleted from .env is
// Unsetenv'd, not left stale (a removed VAULT_TOKEN must stop being a fallback).
func TestStartEnvReload_RemovedKeyIsUnset(t *testing.T) {
	dir := mkTempDir(t)
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("TEST_RELOAD_KEEP=keep\nTEST_RELOAD_REMOVE=removeme\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	os.Setenv("TEST_RELOAD_KEEP", "keep")
	os.Setenv("TEST_RELOAD_REMOVE", "removeme")
	t.Cleanup(func() {
		os.Unsetenv("TEST_RELOAD_KEEP")
		os.Unsetenv("TEST_RELOAD_REMOVE")
	})

	cfg := &Config{Auth: AuthConfig{Enabled: false}}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartEnvReload(ctx, envPath, cfg, 20*time.Millisecond)

	time.Sleep(5 * time.Millisecond)
	os.WriteFile(envPath, []byte("TEST_RELOAD_KEEP=keep\n"), 0600)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if os.Getenv("TEST_RELOAD_REMOVE") == "" {
			if got := os.Getenv("TEST_RELOAD_KEEP"); got != "keep" {
				t.Errorf("TEST_RELOAD_KEEP must be untouched; got %q", got)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("TEST_RELOAD_REMOVE not unset; got %q", os.Getenv("TEST_RELOAD_REMOVE"))
}

func TestStartEnvReload_ZeroIntervalIsNoop(t *testing.T) {
	cfg := &Config{}
	// Should not panic or start a goroutine.
	StartEnvReload(context.Background(), "/nonexistent/.env", cfg, 0)
}

func TestStartEnvReload_MissingFileIsSilent(t *testing.T) {
	cfg := &Config{}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	// Should not panic even if the file does not exist.
	StartEnvReload(ctx, "/nonexistent/path/.env", cfg, 20*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
}

// ─── FetchServiceSecrets ───────────────────────────────────────────────────

func TestFetchServiceSecrets_NilCfg_NoOp(t *testing.T) {
	FetchServiceSecrets(nil, &VaultConfig{}) // must not panic
}

func TestFetchServiceSecrets_NilVaultCfg_NoOp(t *testing.T) {
	FetchServiceSecrets(&Config{}, nil) // must not panic
}

func TestFetchServiceSecrets_NoVaultClient_NoOp(t *testing.T) {
	// No mock installed: with no live client this must be a no-op.
	cfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_myapp", Selector: "mypool",
				Secrets: "kubernetes/myapp/dev", Workdir: mkTempDir(t)},
		},
	}
	FetchServiceSecrets(cfg, &VaultConfig{Address: "http://vault:8200"}) // client nil → no-op
}

func TestFetchServiceSecrets_EmptyServiceName_NoOp(t *testing.T) {
	FetchServiceSecrets(&Config{}, &VaultConfig{Address: "http://vault:8200"})
}

func TestFetchServiceSecrets_NoEntry_NoOp(t *testing.T) {
	// Nothing is managed, so FetchServiceSecrets must never touch Vault.
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("vault should not be called when no CD entry exists (got %s)", r.URL.Path)
	})
	cfg := &Config{
		ServiceName:  "missing",
		HostSelector: "missing_pool",
		// no ContinuousDeployment entries → managedEntries() is empty
	}
	FetchServiceSecrets(cfg, &VaultConfig{Address: "http://fake-vault.invalid"})
}

func TestFetchServiceSecrets_EmptySecrets_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("vault should not be called when Secrets path is empty (got %s)", r.URL.Path)
	})
	cfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_myapp", Selector: "mypool"}, // no Secrets field
		},
	}
	FetchServiceSecrets(cfg, &VaultConfig{Address: "http://fake-vault.invalid"})
	if !strings.Contains(buf.String(), "secrets path not configured") {
		t.Errorf("expected 'secrets path not configured' debug log; got:\n%s", buf.String())
	}
}

func TestFetchServiceSecrets_WritesEnvFile(t *testing.T) {
	workbase := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_myapp", Selector: "mypool",
				Secrets: "kubernetes/myapp/dev", Workdir: workbase},
		},
	}

	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		// Serve both KV v2 and KV v1 paths.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"APP_KEY":"secret123"},"metadata":{}},"request_id":"x"}`)
	})

	vaultCfg := &VaultConfig{Address: "http://fake-vault"}
	FetchServiceSecrets(appCfg, vaultCfg)

	destFile := filepath.Join(workbase, "myproject_myapp", ".env")
	data, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", destFile, err)
	}
	if !strings.Contains(string(data), "APP_KEY=secret123") {
		t.Errorf("expected APP_KEY=secret123 in %q; got %q", destFile, string(data))
	}
}

func TestFetchServiceSecrets_UnchangedSkipsRewrite(t *testing.T) {
	workbase := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_myapp", Selector: "mypool",
				Secrets: "kubernetes/myapp/dev", Workdir: workbase},
		},
	}

	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"KEY":"val"},"metadata":{}},"request_id":"x"}`)
	})

	vaultCfg := &VaultConfig{Address: "http://fake-vault"}
	FetchServiceSecrets(appCfg, vaultCfg)

	destFile := filepath.Join(workbase, "myproject_myapp", ".env")
	info1, err := os.Stat(destFile)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	time.Sleep(20 * time.Millisecond)
	FetchServiceSecrets(appCfg, vaultCfg)
	info2, _ := os.Stat(destFile)
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Error("mtime changed on unchanged content; expected skip")
	}
}

// TestFetchServiceSecrets_EnvChanged_NoResurrect pins that a rewritten .env runs
// the restart gate, which leaves a stopped (here nonexistent) unit alone and logs it.
func TestFetchServiceSecrets_EnvChanged_NoResurrect(t *testing.T) {
	workbase := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_myapp", Selector: "mypool",
				Secrets: "kubernetes/myapp/dev", Workdir: workbase},
		},
	}

	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"APP_KEY":"rotated-secret-xyz"},"metadata":{}},"request_id":"x"}`)
	})

	buf := captureLogs(t, LevelInfo)
	FetchServiceSecrets(appCfg, &VaultConfig{Address: "http://fake-vault"})

	out := buf.String()
	if !strings.Contains(out, "not restarting after .env update") {
		t.Errorf("expected no-resurrect skip after .env write; got:\n%s", out)
	}
	if strings.Contains(out, "restarting myproject_myapp after") {
		t.Errorf("must NOT attempt a restart of a non-running unit; got:\n%s", out)
	}
}

// TestFetchServiceSecrets_EnvUnchanged_NoRestart pins that unchanged content
// skips the restart gate along with the rewrite.
func TestFetchServiceSecrets_EnvUnchanged_NoRestart(t *testing.T) {
	workbase := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_myapp", Selector: "mypool",
				Secrets: "kubernetes/myapp/dev", Workdir: workbase},
		},
	}

	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"STABLE_KEY":"stable-value"},"metadata":{}},"request_id":"x"}`)
	})

	vaultCfg := &VaultConfig{Address: "http://fake-vault"}
	FetchServiceSecrets(appCfg, vaultCfg) // first call writes .env

	// Second call: same secrets → unchanged → must NOT restart.
	buf := captureLogs(t, LevelInfo)
	FetchServiceSecrets(appCfg, vaultCfg)

	if strings.Contains(buf.String(), "restarting") {
		t.Errorf("restart gate must not run when .env is unchanged; got:\n%s", buf.String())
	}
}

// TestFetchServiceSecrets_StoppedService_EnvStillWritten pins the ordering: the
// .env is written BEFORE the restart gate, so a stopped unit gets it at next start.
func TestFetchServiceSecrets_StoppedService_EnvStillWritten(t *testing.T) {
	workbase := mkTempDir(t)

	appCfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_myapp", Selector: "mypool",
				Secrets: "kubernetes/myapp/dev", Workdir: workbase},
		},
	}

	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"K":"v"},"metadata":{}},"request_id":"x"}`)
	})

	buf := captureLogs(t, LevelInfo)
	FetchServiceSecrets(appCfg, &VaultConfig{Address: "http://fake-vault"})

	// The unit is not running → the gate must log the skip, not a job.
	if !strings.Contains(buf.String(), "not restarting after .env update") {
		t.Errorf("expected no-resurrect skip; got:\n%s", buf.String())
	}

	// The .env must have been written even though nothing was restarted.
	destFile := filepath.Join(workbase, "myproject_myapp", ".env")
	if _, err := os.Stat(destFile); err != nil {
		t.Errorf(".env must be written before the restart gate; stat error: %v", err)
	}
}

// ─── autosync gate (FetchServiceSecrets) ───────────────────────────────────

// TestFetchServiceSecrets_AutoSyncDisabled_DetectsAndLogsDrift pins that a falsy
// autosync still reads Vault and logs "out-of-date (autosync=false); not
// rewriting", but writes and restarts nothing. See ADR-0003.
func TestFetchServiceSecrets_AutoSyncDisabled_DetectsAndLogsDrift(t *testing.T) {
	cases := []struct {
		name        string
		autosyncVal interface{}
	}{
		{"bool_false", false},
		{"omitted", nil},
		{"string_false", "false"},
		{"string_NO", "NO"},
		{"string_0", "0"},
		{"int_0", 0},
		{"int_2", 2},
		{"invalid_string", "notabool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workbase := mkTempDir(t)
			buf := captureLogs(t, LevelInfo)
			// Vault is read even with autosync off (drift detection).
			setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprintf(w, `{"data":{"data":{"APP_KEY":"new-rotated-secret"},"metadata":{}},"request_id":"x"}`)
			})
			appCfg := &Config{
				ServiceName:  "myproject_myapp",
				HostSelector: "mypool",
				ContinuousDeployment: []ServiceEntry{
					{
						AutoSync: tc.autosyncVal,
						Service:  "myproject_myapp",
						Selector: "mypool",
						Secrets:  "kubernetes/myapp/dev",
						Workdir:  workbase,
					},
				},
			}
			FetchServiceSecrets(appCfg, &VaultConfig{Address: "http://fake-vault"})

			out := buf.String()
			if !strings.Contains(out, "out-of-date (autosync=false); not rewriting") {
				t.Errorf("expected 'out-of-date (autosync=false); not rewriting' log; got:\n%s", out)
			}
			destFile := filepath.Join(workbase, "myproject_myapp", ".env")
			if _, err := os.Stat(destFile); err == nil {
				t.Errorf(".env must NOT be written when autosync disabled")
			}
			if strings.Contains(out, "restarting myproject_myapp") {
				t.Errorf("restart must NOT happen when autosync disabled; got:\n%s", out)
			}
		})
	}
}

// TestFetchServiceSecrets_AutoSyncDisabled_InSync_NoDriftLog pins that an in-sync
// .env with autosync=false logs no "out-of-date": only real drift does.
func TestFetchServiceSecrets_AutoSyncDisabled_InSync_NoDriftLog(t *testing.T) {
	workbase := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync: true, // first call writes the file
				Service:  "myproject_myapp",
				Selector: "mypool",
				Secrets:  "kubernetes/myapp/dev",
				Workdir:  workbase,
			},
		},
	}
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"STABLE":"v1"},"metadata":{}},"request_id":"x"}`)
	})
	vaultCfg := &VaultConfig{Address: "http://fake-vault"}
	FetchServiceSecrets(appCfg, vaultCfg) // writes the .env

	// Autosync off, same secret: the on-disk .env matches.
	appCfg.ContinuousDeployment[0].AutoSync = false
	buf := captureLogs(t, LevelInfo)
	FetchServiceSecrets(appCfg, vaultCfg)
	if strings.Contains(buf.String(), "out-of-date") {
		t.Errorf("in-sync .env must NOT log out-of-date; got:\n%s", buf.String())
	}
}

// ─── StartAllSecretsRefresh ────────────────────────────────────────────────
// The no-op guards must log and return without starting a goroutine (no leaked ticker).

func TestStartAllSecretsRefresh_NilVaultCfg_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartAllSecretsRefresh(ctx, nil, nil, time.Hour)

	if !strings.Contains(buf.String(), "secret refresh disabled — no Vault config") {
		t.Errorf("expected 'no Vault config' info log; got:\n%s", buf.String())
	}
}

func TestStartAllSecretsRefresh_EmptyAddress_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartAllSecretsRefresh(ctx, nil, &VaultConfig{}, time.Hour)

	if !strings.Contains(buf.String(), "VAULT_ADDRESS not set") {
		t.Errorf("expected 'VAULT_ADDRESS not set' info log; got:\n%s", buf.String())
	}
}

func TestStartAllSecretsRefresh_ZeroInterval_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartAllSecretsRefresh(ctx, nil, &VaultConfig{Address: "http://x"}, 0)

	if !strings.Contains(buf.String(), "periodic secret refresh disabled") {
		t.Errorf("expected zero-interval disabled log; got:\n%s", buf.String())
	}
}

func TestStartAllSecretsRefresh_NegativeInterval_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartAllSecretsRefresh(ctx, nil, &VaultConfig{Address: "http://x"}, -10*time.Second)

	if !strings.Contains(buf.String(), "periodic secret refresh disabled") {
		t.Errorf("expected negative-interval disabled log; got:\n%s", buf.String())
	}
}

// TestStartAllSecretsRefresh_PeriodicLoopFires pins that ticks reach Vault: at
// least two reads within the window (each tick reads for both fetchers).
func TestStartAllSecretsRefresh_PeriodicLoopFires(t *testing.T) {
	var calls atomic.Int32
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"K":"v"},"metadata":{}},"request_id":"x"}`)
	})

	workbase := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "svc",
		HostSelector: "pool",
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "svc", Selector: "pool", Secrets: "kubernetes/x", Workdir: workbase},
		},
	}
	// FetchAndWriteSecrets needs a non-empty Path to fire.
	vaultCfg := &VaultConfig{
		Address: "http://fake-vault.invalid",
		Path:    "kubernetes/devtools/cystemd",
		EnvFile: filepath.Join(workbase, "main.env"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartAllSecretsRefresh(ctx, appCfg, vaultCfg, 30*time.Millisecond)

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if calls.Load() >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	time.Sleep(50 * time.Millisecond)

	got := calls.Load()
	if got < 2 {
		t.Errorf("expected at least 2 Vault hits across two cycles (interval=30ms); got %d", got)
	}
}

// TestStartAllSecretsRefresh_ContextCancel_StopsLoop pins the exit on ctx.Done
// and its "stopping" Info line.
func TestStartAllSecretsRefresh_ContextCancel_StopsLoop(t *testing.T) {
	setupFakeVault(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"data":{"data":{"K":"v"},"metadata":{}},"request_id":"x"}`)
	})

	workbase := mkTempDir(t)
	vaultCfg := &VaultConfig{
		Address: "http://fake-vault.invalid",
		Path:    "kubernetes/devtools/cystemd",
		EnvFile: filepath.Join(workbase, "main.env"),
	}

	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	StartAllSecretsRefresh(ctx, nil, vaultCfg, 25*time.Millisecond)

	time.Sleep(80 * time.Millisecond)
	cancel()
	time.Sleep(120 * time.Millisecond)

	if !strings.Contains(buf.String(), "periodic secret refresh stopping") {
		t.Errorf("expected 'periodic secret refresh stopping' log after cancel; got:\n%s", buf.String())
	}
}

// TestStartAllSecretsRefresh_LogsStartLine pins that the "started" line carries
// the resolved interval.
func TestStartAllSecretsRefresh_LogsStartLine(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartAllSecretsRefresh(ctx, nil, &VaultConfig{Address: "http://x"}, 2*time.Hour)

	if !strings.Contains(buf.String(), "periodic secret refresh started (interval=2h0m0s)") {
		t.Errorf("expected start log with interval; got:\n%s", buf.String())
	}
}
