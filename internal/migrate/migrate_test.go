// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"text/template"

	"github.com/hashicorp/hcl/v2/hclparse"
	vaultapi "github.com/hashicorp/vault/api"

	"github.com/gitaginanjar/cystemd/internal/manifest"
	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/internal/testutil"
	"github.com/gitaginanjar/cystemd/internal/vaultagent"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

var update = flag.Bool("update", false, "rewrite testdata/golden")

func convertTestdata(t *testing.T) (*Result, []string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/config.yml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, warnings, err := ParseConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile("testdata/env")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Convert(cfg, ParseEnv(env), Options{TemplatesDir: "testdata/templates"})
	if err != nil {
		t.Fatal(err)
	}
	return res, warnings
}

// TestGolden compares the whole output with testdata/golden (go test
// -update rewrites it): it is also the reference of what migrate writes.
func TestGolden(t *testing.T) {
	res, _ := convertTestdata(t)
	golden := filepath.Join("testdata", "golden")
	if *update {
		if err := os.RemoveAll(golden); err != nil {
			t.Fatal(err)
		}
		for name, b := range res.Files {
			p := filepath.Join(golden, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	var want []string
	err := filepath.Walk(golden, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(golden, p)
		want = append(want, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for name := range res.Files {
		got = append(got, name)
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("files:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, name := range got {
		b, err := os.ReadFile(filepath.Join(golden, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, res.Files[name]) {
			t.Errorf("%s differs from the golden file (go test ./internal/migrate -run TestGolden -update):\n%s", name, res.Files[name])
		}
	}
}

func TestConvert(t *testing.T) {
	res, warnings := convertTestdata(t)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "unknown_key") {
		t.Errorf("config warnings = %q", warnings)
	}

	apps := map[string]*v1.Application{}
	for _, a := range res.Applications {
		apps[a.Metadata.Name] = a
	}
	var names []string
	for n := range apps {
		names = append(names, n)
	}
	sort.Strings(names)
	if got := strings.Join(names, " "); got != "myproject_myapplication-myapplication myproject_myapplication-poc myproject_myapplication2 payments-api" {
		t.Fatalf("applications: %s", got)
	}

	a := apps["myproject_myapplication-myapplication"]
	src := a.Spec.Source
	sd := src.Systemd
	switch {
	case src.RepoURL != "git@git.example.com:platform/deploy.git", src.TargetRevision != "master",
		src.Path != "products/myproject/environments/dev/myproject_myapplication", sd.ValuesFile != "values.yaml":
		t.Errorf("source: %+v", src)
	case sd.Unit != "myproject_myapplication", sd.Selector != "myapplication", sd.NodeSelector != "myapplication",
		sd.Replicas == nil || *sd.Replicas != 0, sd.ServiceConfig != "config.yml", sd.Workdir != "":
		t.Errorf("systemd: %+v", sd)
	case len(sd.Secrets) != 1 || sd.Secrets[0].VaultPath != "kubernetes/myproject/myapplication/myproject-myapplication-dev" ||
		sd.VaultRole != "cystemd-app-myproject_myapplication-myapplication":
		t.Errorf("secrets: %+v role %q", sd.Secrets, sd.VaultRole)
	case !a.Spec.SyncPolicy.IsAutomated() || !a.Spec.SyncPolicy.Automated.SelfHeal || a.Spec.SyncPolicy.Automated.Prune:
		t.Errorf("sync policy: %+v", a.Spec.SyncPolicy)
	}

	// selectorkey is the ${selector}; the pool still selects the nodes.
	a2 := apps["myproject_myapplication2"].Spec.Source.Systemd
	if a2.Selector != "myapplication" || a2.NodeSelector != "myapplication2" || a2.Workdir != "/srv" || a2.Unit != "" {
		t.Errorf("myproject_myapplication2: %+v", a2)
	}
	if apps["myproject_myapplication2"].Spec.SyncPolicy != nil {
		t.Error(`autosync "no" must be manual`)
	}
	if p := apps["payments-api"]; p.Spec.Source.Systemd.Unit != "Payments@API" || !p.Spec.SyncPolicy.IsAutomated() {
		t.Errorf("payments-api: unit %q policy %+v", p.Spec.Source.Systemd.Unit, p.Spec.SyncPolicy)
	}
	if p := apps["myproject_myapplication-poc"].Spec.Source.Systemd; p.Secrets[0].VaultPath != "kubernetes/myproject/poc" {
		t.Errorf("poc secret path %q", p.Secrets[0].VaultPath)
	}

	hosts := map[string]Host{}
	for _, h := range res.Hosts {
		hosts[h.Name] = h
	}
	if h := hosts["host002"]; strings.Join(h.Selectors, ",") != "myapplication,myapplication2" ||
		strings.Join(h.Apps, ",") != "myproject_myapplication-myapplication,myproject_myapplication2,payments-api" {
		t.Errorf("host002: %+v", h)
	}
	if len(hosts) != 3 {
		t.Errorf("hosts: %+v", res.Hosts)
	}

	all := strings.Join(res.Warnings, "\n")
	for _, w := range []string{
		"no_values: no git_values",
		"continuous_deployment[5] has no service",
		"host HOST001: myproject_myapplication-myapplication and myproject_myapplication-poc both manage myproject_myapplication.service",
		"template testdata/templates/missing.tmpl",
		`field "api-key" is read with index`,
		"line 2 calls secret with a non-literal path",
		"copy the Git SSH key /opt/cystemd/.ssh/id_ed25519",
		"v4 skipped Vault TLS verification",
	} {
		if !strings.Contains(all, w) {
			t.Errorf("no warning %q in:\n%s", w, all)
		}
	}

	// Credentials are never copied.
	for name, b := range res.Files {
		for _, secret := range []string{"not-a-real-secret-id", "00000000-1111-2222-3333-444444444444"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s contains a credential", name)
			}
		}
	}

	// The manifest decodes strictly, and the HCL files parse.
	objs, err := manifest.Decode(bytes.NewReader(res.Files["applications.yaml"]))
	if err != nil || len(objs) != 4 {
		t.Fatalf("applications.yaml: %d objects, %v", len(objs), err)
	}
	p := hclparse.NewParser()
	for name, b := range res.Files {
		if strings.HasSuffix(name, ".hcl") && !strings.HasPrefix(name, "vault/policies/") {
			if _, diags := p.ParseHCL(b, name); diags.HasErrors() {
				t.Errorf("%s: %v", name, diags)
			}
		}
	}
	if !strings.Contains(string(res.Files["control-plane.hcl"]), `reconcile_interval = "30s"`) ||
		!strings.Contains(string(res.Files["control-plane.hcl"]), `legacy_public_key_file = "/opt/cystemd/jwt.pub"`) {
		t.Errorf("control-plane.hcl:\n%s", res.Files["control-plane.hcl"])
	}
}

// TestRender renders a converted Application with the v4 values fixture:
// the migrated spec produces the job v4's behaviour corresponds to.
func TestRender(t *testing.T) {
	res, _ := convertTestdata(t)
	var app *v1.Application
	for _, a := range res.Applications {
		if a.Metadata.Name == "myproject_myapplication-myapplication" {
			app = a
		}
	}
	dir := t.TempDir()
	values, err := os.ReadFile("testdata/values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), values, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &render.Renderer{Opts: render.Options{VaultAddress: "https://vault.service.consul:8200"}}
	m, err := r.Render(context.Background(), app, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Jobs) != 1 {
		t.Fatalf("jobs: %d", len(m.Jobs))
	}
	job := m.Jobs[0]
	if *job.ID != app.Metadata.Name || *job.Type != "system" {
		t.Errorf("job %s type %s", *job.ID, *job.Type)
	}
	found := false
	for _, c := range job.TaskGroups[0].Constraints {
		found = found || c.LTarget == "${meta."+render.SelectorMetaKey("myapplication")+"}"
	}
	if !found {
		t.Errorf("no node selector constraint: %+v", job.TaskGroups[0].Constraints)
	}
	task := job.TaskGroups[0].Tasks[0]
	cfg := task.Config
	pkg, _ := cfg["package"].([]map[string]any)
	if cfg["unit"] != "myproject_myapplication.service" || len(pkg) != 1 ||
		pkg[0]["version"] != "abc123def456abc123def456abc123def456abc123" || cfg["cpu_quota"] != "200m" || cfg["memory_max"] != "1Gi" {
		t.Errorf("task config: %+v", cfg)
	}
	if task.Vault == nil || task.Vault.Role != "cystemd-app-myproject_myapplication-myapplication" {
		t.Errorf("vault: %+v", task.Vault)
	}
	var tmpl string
	for _, tp := range task.Templates {
		if tp.EmbeddedTmpl != nil {
			tmpl += *tp.EmbeddedTmpl
		}
	}
	if !strings.Contains(tmpl, `secret "kubernetes/data/myproject/myapplication/myproject-myapplication-dev"`) {
		t.Errorf("templates do not read the v4 secret:\n%s", tmpl)
	}
}

func TestConvertTemplate(t *testing.T) {
	for _, tc := range []struct {
		name, src, want string
		notes           int
	}{{
		name: "fields",
		src:  `a={{ secretField "kv/app" "user" }} b={{ secretField "kv/app" "api-key" }} c={{ (secret "kv/app").x }}`,
		want: `a={{ $cystemd_s1 := secret "kv/data/app" }}{{ if $cystemd_s1 }}{{ $cystemd_s1.Data.data.user }}{{ end }}` +
			` b={{ $cystemd_s2 := secret "kv/data/app" }}{{ if $cystemd_s2 }}{{ (index $cystemd_s2.Data.data "api-key") }}{{ end }}` +
			` c={{ $cystemd_s3 := secret "kv/data/app" }}{{ if $cystemd_s3 }}{{ $cystemd_s3.Data.data.x }}{{ end }}`,
		notes: 1,
	}, {
		name: "blocks, text and comments",
		src:  `{{ with secret "kv/app" }}{{ .y }}{{ else }}none{{ end }} secret "kv/plain" {{/* secret "kv/c" */}}`,
		want: `{{ $cystemd_s1 := secret "kv/data/app" }}{{ if $cystemd_s1 }}{{ with $cystemd_s1.Data.data }}{{ .y }}{{ else }}none{{ end }}{{ end }} secret "kv/plain" {{/* secret "kv/c" */}}`,
	}, {
		name: "trim markers",
		src:  "x {{- secretField \"kv/a\" \"f\" -}} y\n{{- range $k, $v := secret \"kv/env\" -}}\n{{ $k }}\n{{- end -}}\nz",
		want: "x {{- $cystemd_s1 := secret \"kv/data/a\" }}{{ if $cystemd_s1 }}{{ $cystemd_s1.Data.data.f }}{{ end -}} y\n" +
			"{{- $cystemd_s2 := secret \"kv/data/env\" }}{{ if $cystemd_s2 }}{{ range $k, $v := $cystemd_s2.Data.data -}}\n{{ $k }}\n{{- end }}{{ end -}}\nz",
	}, {
		name: "two secrets",
		src:  `{{ printf "%s:%s" (secretField "a/x" "u") (secretField "b/y" "p") }}`,
		want: `{{ $cystemd_s1 := secret "a/data/x" }}{{ $cystemd_s2 := secret "b/data/y" }}{{ if and $cystemd_s1 $cystemd_s2 }}{{ printf "%s:%s" ($cystemd_s1.Data.data.u) ($cystemd_s2.Data.data.p) }}{{ end }}`,
	}, {
		name:  "unguardable",
		src:   `{{ $x := (secret "a/b").c }}{{ if true }}{{ else if (secret "a/b").d }}{{ end }}{{ secret $p }}`,
		want:  `{{ $x := (secret "a/data/b").Data.data.c }}{{ if true }}{{ else if (secret "a/data/b").Data.data.d }}{{ end }}{{ secret $p }}`,
		notes: 3,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got, notes := ConvertTemplate(tc.src, 2)
			if got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
			if len(notes) != tc.notes {
				t.Errorf("notes: %q", notes)
			}
		})
	}
	got, _ := ConvertTemplate(`{{ secretField "kv/app" "user" }}`, 1)
	if want := `{{ $cystemd_s1 := secret "kv/app" }}{{ if $cystemd_s1 }}{{ $cystemd_s1.Data.user }}{{ end }}`; got != want {
		t.Errorf("kv v1: %s", got)
	}
}

// fakeSecret is what consul-template's secret function returns.
type fakeSecret struct {
	Data map[string]any
}

// TestConvertedTemplateExecutes runs the converted fixture the way Vault
// Agent does with error_on_missing_key: a first pass where every secret
// is nil, then one with the data; neither may fail, and a missing field
// must.
func TestConvertedTemplateExecutes(t *testing.T) {
	src, err := os.ReadFile("testdata/templates/app.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	converted, _ := ConvertTemplate(string(src), 2)
	secrets := map[string]map[string]any{
		"secret/data/myproject/app": {"db_user": "app", "db_password": "pw", "api-key": "k"},
		"secret/data/myproject/tls": {"crt": "CERT"},
		"secret/data/myproject/env": {"A": "1"},
	}
	run := func(lookup func(string) any) (string, error) {
		tmpl, err := template.New("").Option("missingkey=error").Funcs(template.FuncMap{
			"secret": func(p string) (any, error) { return lookup(p), nil },
		}).Parse(converted)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		err = tmpl.Execute(&b, nil)
		return b.String(), err
	}
	if _, err := run(func(string) any { return nil }); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	got, err := run(func(p string) any {
		if d, ok := secrets[p]; ok {
			return &fakeSecret{Data: map[string]any{"data": d}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "db_password = pw\napi_key = k\ncert = CERT\nA=1\n") {
		t.Errorf("rendered:\n%s", got)
	}
	delete(secrets["secret/data/myproject/app"], "db_password")
	if _, err := run(func(p string) any { return &fakeSecret{Data: map[string]any{"data": secrets[p]}} }); err == nil {
		t.Error("a missing field rendered")
	}
}

func TestParseEnvAndSpecs(t *testing.T) {
	env := ParseEnv([]byte("# c\nA=1\nexport B='two'\nC=\"x\\ny\"\nbad\n=nokey\nD = spaced \n"))
	if env["A"] != "1" || env["B"] != "two" || env["C"] != "x\ny" || env["D"] != "spaced" || len(env) != 4 {
		t.Errorf("env: %q", env)
	}
	specs := ParseTemplateSpecs("a:/b:cmd --x=1:2; c:/d\n\n:nosrc;bad;e:")
	if len(specs) != 2 || specs[0] != (TemplateSpec{"a", "/b", "cmd --x=1:2"}) || specs[1] != (TemplateSpec{"c", "/d", ""}) {
		t.Errorf("specs: %+v", specs)
	}
}

func TestAppName(t *testing.T) {
	for in, want := range map[string]string{
		"myproject_myapplication": "myproject_myapplication",
		"Payments@API":            "payments-api",
		"nginx@80.service":        "nginx-80.service",
		"__x__":                   "x",
		"@@@":                     "app",
	} {
		if got := AppName(in); got != want {
			t.Errorf("AppName(%q) = %q, want %q", in, got, want)
		}
		if err := v1.ValidateName(AppName(in)); err != nil {
			t.Error(err)
		}
	}
}

func TestConvertErrors(t *testing.T) {
	cfg := &Legacy{ContinuousDeployment: []Entry{{Service: "a", Selector: "p", GitValues: "v.yaml"}}}
	if _, err := Convert(cfg, nil, Options{}); err == nil || !strings.Contains(err.Error(), "GIT_REPOSITORY") {
		t.Errorf("no repository: %v", err)
	}
	if _, err := Convert(cfg, nil, Options{RepoURL: "x", KVVersion: 3}); err == nil {
		t.Error("kv version 3 accepted")
	}
	res, err := Convert(cfg, nil, Options{RepoURL: "https://git/x.git", KVVersion: 1, SecretsDelivery: "vault-agent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applications[0].Spec.Source.Path != "" || res.Applications[0].Spec.Source.Systemd.ValuesFile != "v.yaml" {
		t.Errorf("root values file: %+v", res.Applications[0].Spec.Source)
	}
	if _, ok := res.Files["vault-agent/vault-agent.hcl"]; ok {
		t.Error("vault agent without VAULT_TEMPLATES")
	}
	empty, warnings, err := ParseConfig(nil)
	if err != nil || warnings != nil || len(empty.ContinuousDeployment) != 0 {
		t.Errorf("empty config: %v %v", warnings, err)
	}
	if _, _, err := ParseConfig([]byte("continuous_deployment: [{replica: many}]")); err == nil {
		t.Error("type error accepted")
	}
}

// TestRealVaultAgentRendersConvertedTemplate renders a converted v4 template with
// a real Vault Agent: what v4 read, Vault Agent reads the same way.
func TestRealVaultAgentRendersConvertedTemplate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	srv, root := testutil.StartVault(t)
	vc, err := vaultapi.NewClient(&vaultapi.Config{Address: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	vc.SetToken(root)
	ctx := context.Background()
	for path, data := range map[string]map[string]any{
		"myproject/app": {"db_user": "app", "db_password": `p"w$ {x}`, "api-key": "k-1"},
		"myproject/tls": {"crt": "CERT"},
		"myproject/env": {"A": "1", "B": "2"},
	} {
		if _, err := vc.KVv2("secret").Put(ctx, path, data); err != nil {
			t.Fatal(err)
		}
	}
	src, err := os.ReadFile("testdata/templates/app.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	converted, _ := ConvertTemplate(string(src), 2)
	dir := t.TempDir()
	tmpl, out, tok := filepath.Join(dir, "app.ctmpl"), filepath.Join(dir, "app.conf"), filepath.Join(dir, "token")
	if err := os.WriteFile(tmpl, []byte(converted), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tok, []byte(root), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := vaultagent.Config{
		Vault:     vaultagent.Vault{Address: srv.URL},
		AutoAuth:  vaultagent.AutoAuth{Method: vaultagent.MethodTokenFile, TokenFilePath: tok},
		Templates: []vaultagent.Template{{Source: tmpl, Destination: out, ErrorOnMissingKey: true}},
	}
	hcl, err := cfg.HCL()
	if err != nil {
		t.Fatal(err)
	}
	agent := testutil.StartVaultAgent(t, hcl, func() bool {
		_, err := os.Stat(out)
		return err == nil
	})
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("%v\n%s", err, agent.Log())
	}
	want := `# Rendered by CYSTEMD v4 from Vault.
db_user = app
db_password = p"w$ {x}
api_key = k-1
cert = CERT
A=1
B=2
note = plain text that mentions secret "here" stays as it is
`
	if string(got) != want {
		t.Errorf("rendered:\n%s\nwant:\n%s", got, want)
	}
}
