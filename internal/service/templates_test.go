package service

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── parseTemplateSpecs ────────────────────────────────────────────────────

func TestParseTemplateSpecs(t *testing.T) {
	captureLogs(t, LevelWarning)
	cases := []struct {
		name string
		in   string
		want []TemplateSpec
	}{
		{"empty", "", nil},
		{"whitespace", "   ", nil},
		{"single", "/a.tmpl:/out/a", []TemplateSpec{{Source: "/a.tmpl", Dest: "/out/a"}}},
		{"semicolon", "/a.tmpl:/out/a;/b.tmpl:/out/b", []TemplateSpec{{Source: "/a.tmpl", Dest: "/out/a"}, {Source: "/b.tmpl", Dest: "/out/b"}}},
		{"newline", "/a.tmpl:/out/a\n/b.tmpl:/out/b", []TemplateSpec{{Source: "/a.tmpl", Dest: "/out/a"}, {Source: "/b.tmpl", Dest: "/out/b"}}},
		{"trims", " /a.tmpl : /out/a ", []TemplateSpec{{Source: "/a.tmpl", Dest: "/out/a"}}},
		{"reload", "/a.tmpl:/out/a:systemctl reload nginx", []TemplateSpec{{Source: "/a.tmpl", Dest: "/out/a", Reload: "systemctl reload nginx"}}},
		{"reload-with-colon", "/a.tmpl:/out/a:sh -c foo:bar", []TemplateSpec{{Source: "/a.tmpl", Dest: "/out/a", Reload: "sh -c foo:bar"}}},
		{"reload-then-next-entry", "/a.tmpl:/out/a:reloadA;/b.tmpl:/out/b", []TemplateSpec{{Source: "/a.tmpl", Dest: "/out/a", Reload: "reloadA"}, {Source: "/b.tmpl", Dest: "/out/b"}}},
		{"malformed-no-colon", "noseparator", nil},
		{"malformed-empty-side", ":/out/a", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseTemplateSpecs(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("len: got %d (%v), want %d (%v)", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("[%d]: got %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestLoadVaultConfig_Templates(t *testing.T) {
	captureLogs(t, LevelDebug)
	t.Setenv("VAULT_TEMPLATES", "/x.tmpl:/out/x;/y.tmpl:/out/y")
	cfg := LoadVaultConfig()
	if len(cfg.Templates) != 2 {
		t.Fatalf("expected 2 templates, got %d", len(cfg.Templates))
	}
	if cfg.Templates[0].Source != "/x.tmpl" || cfg.Templates[0].Dest != "/out/x" {
		t.Errorf("template[0]: got %+v", cfg.Templates[0])
	}
}

// ─── RenderTemplates ───────────────────────────────────────────────────────

// kvHandler returns a KV v2 handler serving the given fields at any path.
func kvHandler(fields map[string]interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := vaultEnvelope(map[string]interface{}{
			"data":     fields,
			"metadata": map[string]interface{}{"version": 1},
		})
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}
}

func writeTemplateFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}
	return p
}

func TestRenderTemplates_HappyPath_RendersSecretValue(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{
		"DB_PASSWORD": "s3cr3t",
	}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "app.conf.tmpl", "password={{ secretField \"kv/app\" \"DB_PASSWORD\" }}\nalt={{ (secret \"kv/app\").DB_PASSWORD }}\n")
	dest := filepath.Join(dir, "app.conf")

	buf := captureLogs(t, LevelInfo)
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})

	out, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("dest should be written: %v", err)
	}
	got := string(out)
	if !strings.Contains(got, "password=s3cr3t") {
		t.Errorf("expected rendered secret value; got:\n%s", got)
	}
	if !strings.Contains(got, "alt=s3cr3t") {
		t.Errorf("expected map-access value; got:\n%s", got)
	}
	if strings.Contains(got, "<no value>") {
		t.Errorf("rendered output must not contain <no value>; got:\n%s", got)
	}
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("dest mode: got %o, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(buf.String(), "rendered") {
		t.Errorf("expected a 'rendered' info log; got:\n%s", buf.String())
	}
}

func TestRenderTemplates_PEMFromKV(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\nMIIBabc\n-----END CERTIFICATE-----\n"
	setupFakeVault(t, kvHandler(map[string]interface{}{"certificate": pem}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "cert.tmpl", "{{ secretField \"pki-kv/app\" \"certificate\" }}")
	dest := filepath.Join(dir, "app.pem")
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})
	out, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("dest: %v", err)
	}
	if string(out) != pem {
		t.Errorf("PEM round-trip mismatch:\n got %q\nwant %q", string(out), pem)
	}
}

// TestRenderTemplates_MissingField_FailsClosed_SecretField — an absent field via
// secretField must FAIL: no file written, Warning logged, no <no value>.
func TestRenderTemplates_MissingField_FailsClosed_SecretField(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"PRESENT": "x"}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "bad.tmpl", "value={{ secretField \"kv/app\" \"ABSENT\" }}")
	dest := filepath.Join(dir, "bad.conf")
	buf := captureLogs(t, LevelWarning)
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		out, _ := os.ReadFile(dest)
		t.Fatalf("missing field must NOT write the destination; file exists with:\n%s", out)
	}
	if !strings.Contains(buf.String(), "render") {
		t.Errorf("expected a render Warning; got:\n%s", buf.String())
	}
}

// TestRenderTemplates_MissingField_FailsClosed_MapAccess — map access on an absent
// key must FAIL via missingkey=error (not emit <no value>).
func TestRenderTemplates_MissingField_FailsClosed_MapAccess(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"PRESENT": "x"}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "bad2.tmpl", "value={{ (secret \"kv/app\").ABSENT }}")
	dest := filepath.Join(dir, "bad2.conf")
	captureLogs(t, LevelWarning)
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		out, _ := os.ReadFile(dest)
		t.Fatalf("map access on absent key must FAIL (missingkey=error); file exists with:\n%s", out)
	}
}

func TestRenderTemplates_UnchangedContent_NoRewrite(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"K": "v"}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "stable.tmpl", "{{ secretField \"kv/app\" \"K\" }}")
	dest := filepath.Join(dir, "stable.out")
	spec := []TemplateSpec{{Source: src, Dest: dest}}
	RenderTemplates(&VaultConfig{Templates: spec})
	st1, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("first render: %v", err)
	}
	RenderTemplates(&VaultConfig{Templates: spec})
	st2, _ := os.Stat(dest)
	if !st1.ModTime().Equal(st2.ModTime()) {
		t.Errorf("unchanged content should not rewrite (mtime changed: %v → %v)", st1.ModTime(), st2.ModTime())
	}
}

func TestRenderTemplates_Deterministic(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"A": "1", "B": "2", "C": "3"}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "det.tmpl", "{{ secretField \"kv/app\" \"A\" }}{{ secretField \"kv/app\" \"B\" }}{{ secretField \"kv/app\" \"C\" }}")
	dest := filepath.Join(dir, "det.out")
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})
	first, _ := os.ReadFile(dest)
	for i := 0; i < 10; i++ {
		RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})
		got, _ := os.ReadFile(dest)
		if string(got) != string(first) {
			t.Fatalf("render not deterministic: %q vs %q", got, first)
		}
	}
}

func TestRenderTemplates_SourceMissing_FailSoft(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"K": "v"}))
	dir := mkTempDir(t)
	dest := filepath.Join(dir, "out")
	buf := captureLogs(t, LevelWarning)
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: filepath.Join(dir, "nope.tmpl"), Dest: dest}}})
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("missing source must not write dest")
	}
	if !strings.Contains(buf.String(), "cannot read template source") {
		t.Errorf("expected source-read Warning; got:\n%s", buf.String())
	}
}

func TestRenderTemplates_NoVaultClient_NoOp(t *testing.T) {
	prev := GetVaultClient()
	setGlobalVaultState(nil)
	t.Cleanup(func() { setGlobalVaultState(prev) })
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "x.tmpl", "{{ secretField \"kv/app\" \"K\" }}")
	dest := filepath.Join(dir, "x.out")
	buf := captureLogs(t, LevelInfo)
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Error("no vault client → no render")
	}
	if !strings.Contains(buf.String(), "Vault client not initialised") {
		t.Errorf("expected skip log; got:\n%s", buf.String())
	}
}

func TestRenderTemplates_NilCfgAndNoTemplates_NoOp(t *testing.T) {
	RenderTemplates(nil)
	RenderTemplates(&VaultConfig{})
}

func TestRenderTemplates_PreexistingLooseDest_TightenedTo0600(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"K": "v"}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "t.tmpl", "{{ secretField \"kv/app\" \"K\" }}")
	dest := filepath.Join(dir, "t.out")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest}}})
	info, _ := os.Stat(dest)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("dest mode after render: got %o, want 0600", info.Mode().Perm())
	}
}

// TestRenderTemplates_ReloadRunsOnChange — the reload command runs (sh -c) only
// when the rendered file actually changes, not on an unchanged re-render.
func TestRenderTemplates_ReloadRunsOnChange(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"K": "v1"}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "r.tmpl", "{{ secretField \"kv/app\" \"K\" }}")
	dest := filepath.Join(dir, "r.out")
	marker := filepath.Join(dir, "reloaded")
	spec := []TemplateSpec{{Source: src, Dest: dest, Reload: "touch " + marker}}

	RenderTemplates(&VaultConfig{Templates: spec})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("reload command should run on change (marker missing): %v", err)
	}
	// Unchanged second render → reload must NOT run.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	RenderTemplates(&VaultConfig{Templates: spec})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("reload command must NOT run when content is unchanged")
	}
}

// TestRenderTemplates_ReloadFailure_FailSoft — a failing reload logs a Warning
// but the render still succeeds (dest written).
func TestRenderTemplates_ReloadFailure_FailSoft(t *testing.T) {
	setupFakeVault(t, kvHandler(map[string]interface{}{"K": "v"}))
	dir := mkTempDir(t)
	src := writeTemplateFile(t, dir, "rf.tmpl", "{{ secretField \"kv/app\" \"K\" }}")
	dest := filepath.Join(dir, "rf.out")
	buf := captureLogs(t, LevelWarning)
	RenderTemplates(&VaultConfig{Templates: []TemplateSpec{{Source: src, Dest: dest, Reload: "exit 7"}}})
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("render must succeed even if reload fails: %v", err)
	}
	if !strings.Contains(buf.String(), "reload command for") {
		t.Errorf("expected a reload-failure warning; got:\n%s", buf.String())
	}
}
