// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

const helmValues = `
myapp:
  global:
    image:
      tag: "1.10"
  services:
    myapp:
      deployment:
        resources:
          limits:
            cpu: 200m
            memory: 1Gi
      config:
        config.yml: |
          server:
            port: 8080
`

func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func systemdApp(src *v1alpha1.SystemdSource) *v1alpha1.Application {
	return &v1alpha1.Application{
		Metadata: v1alpha1.ObjectMeta{Name: "myproject-myapp"},
		Spec: v1alpha1.ApplicationSpec{
			Project:     "default",
			Source:      v1alpha1.ApplicationSource{RepoURL: "git@h:o/r.git", Systemd: src},
			Destination: v1alpha1.ApplicationDestination{Namespace: "apps"},
		},
	}
}

func legacySource() *v1alpha1.SystemdSource {
	return &v1alpha1.SystemdSource{
		Selector:     "myapp",
		ConfigKey:    ".${selector}.services.${selector}.config.[]",
		VersionKey:   ".${selector}.global.image.tag",
		CPUKey:       ".${selector}.services.${selector}.deployment.resources.limits.cpu",
		MemoryKey:    ".${selector}.services.${selector}.deployment.resources.limits.memory",
		Unit:         "myproject_myapp",
		NodeSelector: "myapp",
		Secrets:      []v1alpha1.SystemdSecret{{VaultPath: "kubernetes/myproject/myapp"}},
	}
}

func TestSystemdRendererConsulDelivery(t *testing.T) {
	dir := writeFiles(t, map[string]string{"values.yaml": helmValues})
	r := &Renderer{Opts: Options{ConsulEnabled: true, DefaultVaultRole: "cystemd-workloads"}}
	m, err := r.Render(context.Background(), systemdApp(legacySource()), dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.SourceType != v1alpha1.SourceTypeSystemd || len(m.Jobs) != 1 {
		t.Fatalf("manifests = %+v", m)
	}
	job := m.Jobs[0]
	if *job.ID != "myproject-myapp" || *job.Type != "service" || *job.Namespace != "apps" {
		t.Fatalf("job id=%s type=%s ns=%s", *job.ID, *job.Type, *job.Namespace)
	}
	if job.Meta[v1alpha1.AppMetaKey] != "myproject-myapp" {
		t.Fatalf("tracking meta missing: %v", job.Meta)
	}
	tg := job.TaskGroups[0]
	if *tg.Count != 1 {
		t.Fatalf("count = %d", *tg.Count)
	}
	var selector bool
	for _, c := range tg.Constraints {
		if c.LTarget == "${meta.cystemd_selector_myapp}" && c.RTarget == "true" {
			selector = true
		}
	}
	if !selector {
		t.Fatalf("selector constraint missing: %+v", tg.Constraints)
	}
	task := tg.Tasks[0]
	if task.Driver != "systemd" || task.Config["unit"] != "myproject_myapp.service" {
		t.Fatalf("task = %s %v", task.Driver, task.Config)
	}
	pkg := task.Config["package"].([]map[string]any)[0]
	// "1.10" must not become 1.1.
	if pkg["name"] != "myproject_myapp" || pkg["version"] != "1.10" {
		t.Fatalf("package = %v", pkg)
	}
	if task.Config["cpu_quota"] != "200m" || task.Config["memory_max"] != "1Gi" {
		t.Fatalf("limits = %v %v", task.Config["cpu_quota"], task.Config["memory_max"])
	}

	// Config delivered through a managed Consul key.
	if len(m.KV) != 1 || m.KV[0].Key != "cystemd/values/myproject-myapp/target.config.yml" {
		t.Fatalf("kv = %+v", m.KV)
	}
	if string(m.KV[0].Value) != "server:\n  port: 8080\n" {
		t.Fatalf("config = %q", m.KV[0].Value)
	}
	if got := *task.Templates[0].EmbeddedTmpl; got != `{{ key "cystemd/values/myproject-myapp/target.config.yml" }}` {
		t.Fatalf("config template = %s", got)
	}

	// Secrets: Nomad-native Vault, KV v2 data path, private env file synced
	// to the v4 location and used as EnvironmentFile.
	if task.Vault == nil || task.Vault.Role != "cystemd-workloads" {
		t.Fatalf("vault = %+v", task.Vault)
	}
	secret := *task.Templates[1].EmbeddedTmpl
	if !strings.Contains(secret, `with secret "kubernetes/data/myproject/myapp"`) || !strings.Contains(secret, ".Data.data") {
		t.Fatalf("secret template = %s", secret)
	}
	if *task.Templates[1].Perms != "0600" || *task.Templates[1].DestPath != "secrets/.env" {
		t.Fatalf("secret template perms/dest = %s %s", *task.Templates[1].Perms, *task.Templates[1].DestPath)
	}
	files := task.Config["file"].([]map[string]any)
	if len(files) != 2 || files[0]["destination"] != "/opt/myproject_myapp/target.config.yml" || files[1]["destination"] != "/opt/myproject_myapp/.env" || files[1]["mode"] != "0600" {
		t.Fatalf("files = %v", files)
	}
	if env := task.Config["environment_files"].([]string); len(env) != 1 || env[0] != "secrets/.env" {
		t.Fatalf("environment_files = %v", env)
	}
	if len(m.Images) != 1 || m.Images[0] != "pkg:myproject_myapp@1.10" {
		t.Fatalf("images = %v", m.Images)
	}
}

func TestSystemdRendererInlineDeliveryIsInert(t *testing.T) {
	dir := writeFiles(t, map[string]string{"values.yaml": "greeting: '{{ not a template }}'\n"})
	src := &v1alpha1.SystemdSource{Unit: "hello"}
	m, err := (&Renderer{}).Render(context.Background(), systemdApp(src), dir)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := m.Jobs[0].TaskGroups[0].Tasks[0].Templates[0]
	if *tmpl.LeftDelim != inertLeftDelim || !strings.Contains(*tmpl.EmbeddedTmpl, "{{ not a template }}") {
		t.Fatalf("inline template = %+v", tmpl)
	}
	if len(m.KV) != 0 {
		t.Fatalf("inline delivery wrote kv: %+v", m.KV)
	}
}

func TestSystemdRendererReplicasAndService(t *testing.T) {
	dir := writeFiles(t, map[string]string{"values.yaml": "a: 1\n"})
	zero, three := 0, 3

	m, err := (&Renderer{}).Render(context.Background(), systemdApp(&v1alpha1.SystemdSource{Unit: "x", Replicas: &zero}), dir)
	if err != nil || *m.Jobs[0].Type != "system" {
		t.Fatalf("replicas 0 must be a system job: %v %v", err, *m.Jobs[0].Type)
	}

	src := &v1alpha1.SystemdSource{Unit: "x", Replicas: &three,
		Service: &v1alpha1.ServiceSpec{Port: 8080, Check: &v1alpha1.CheckSpec{Type: "http", Path: "/healthz"}}}
	m, err = (&Renderer{Opts: Options{ConsulEnabled: true}}).Render(context.Background(), systemdApp(src), dir)
	if err != nil {
		t.Fatal(err)
	}
	tg := m.Jobs[0].TaskGroups[0]
	if *tg.Count != 3 || tg.Constraints[0].Operand != "distinct_hosts" {
		t.Fatalf("count=%d constraints=%+v", *tg.Count, tg.Constraints)
	}
	if svc := tg.Services[0]; svc.Provider != "consul" || svc.Checks[0].Path != "/healthz" || *tg.Update.HealthCheck != "checks" {
		t.Fatalf("service = %+v", svc)
	}
	if tg.Networks[0].ReservedPorts[0].Value != 8080 {
		t.Fatalf("port = %+v", tg.Networks[0])
	}
}

func TestSystemdRendererVaultAgentDelivery(t *testing.T) {
	dir := writeFiles(t, map[string]string{"values.yaml": "a: 1\n"})
	src := &v1alpha1.SystemdSource{Unit: "x", SecretsDelivery: "vault-agent",
		Secrets: []v1alpha1.SystemdSecret{{VaultPath: "secret/x", KVVersion: 1}}}
	if _, err := (&Renderer{}).Render(context.Background(), systemdApp(src), dir); err == nil {
		t.Fatal("vault-agent delivery without a Vault address accepted")
	}
	r := &Renderer{Opts: Options{VaultAddress: "https://vault:8200", DefaultVaultRole: "workloads"}}
	m, err := r.Render(context.Background(), systemdApp(src), dir)
	if err != nil {
		t.Fatal(err)
	}
	tasks := m.Jobs[0].TaskGroups[0].Tasks
	if len(tasks) != 3 || tasks[0].Name != "vault-agent-init" || tasks[1].Name != "vault-agent" {
		t.Fatalf("tasks = %d", len(tasks))
	}
	if tasks[0].Lifecycle.Sidecar || !tasks[1].Lifecycle.Sidecar {
		t.Fatal("init must not be a sidecar; the agent must be")
	}
	agentCfg := *tasks[0].Templates[0].EmbeddedTmpl
	for _, want := range []string{"exit_after_auth = true", `method "jwt"`, `role                     = "workloads"`, `with secret "secret/x"`, ".Data }}"} {
		if !strings.Contains(agentCfg, want) {
			t.Errorf("agent config lacks %q:\n%s", want, agentCfg)
		}
	}
	if !tasks[0].Identities[0].File || tasks[0].Identities[0].Name != "vault_default" {
		t.Fatalf("identity = %+v", tasks[0].Identities[0])
	}
	main := tasks[2]
	if main.Vault != nil {
		t.Fatal("vault-agent delivery must not use Nomad's Vault integration")
	}
	files := main.Config["file"].([]map[string]any)
	if files[1]["source"] != "alloc/data/vault/.env" || files[1]["on_change"] != "restart" {
		t.Fatalf("files = %v", files)
	}
}

func TestSystemdRendererErrors(t *testing.T) {
	dir := writeFiles(t, map[string]string{"values.yaml": helmValues + "bad: lots\n"})
	for name, src := range map[string]*v1alpha1.SystemdSource{
		"missing values file": {ValuesFile: "nope.yaml"},
		"escaping values":     {ValuesFile: "../../etc/passwd"},
		"bad config key":      {ConfigKey: ".nope.[]"},
		"bad version key":     {VersionKey: ".myapp.global"},
		"bad cpu":             {CPUKey: ".bad"},
		"bad memory":          {MemoryKey: ".bad"},
		"service config path": {ServiceConfig: "../x"},
	} {
		if _, err := (&Renderer{}).Render(context.Background(), systemdApp(src), dir); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNomadRendererJSONAndKV(t *testing.T) {
	job := map[string]any{"Job": map[string]any{"ID": "web", "Name": "web", "Type": "service",
		"TaskGroups": []any{map[string]any{"Name": "web", "Tasks": []any{map[string]any{"Name": "web", "Driver": "docker", "Config": map[string]any{"image": "nginx:1.27"}}}}}}}
	b, _ := json.Marshal(job)
	dir := writeFiles(t, map[string]string{
		"web.nomad.json": string(b),
		"ignored.txt":    "x",
		"kv.yaml":        "- key: web/config.yml\n  file: conf/web.yml\n- key: web/flag\n  value: 'on'\n",
		"conf/web.yml":   "listen: 80\n",
	})
	app := &v1alpha1.Application{
		Metadata: v1alpha1.ObjectMeta{Name: "web"},
		Spec: v1alpha1.ApplicationSpec{Project: "default", Source: v1alpha1.ApplicationSource{
			RepoURL: "x", Nomad: &v1alpha1.NomadSource{ConsulKV: "kv.yaml"},
		}, Destination: v1alpha1.ApplicationDestination{Namespace: "team-a", Region: "eu"}},
	}
	m, err := (&Renderer{}).Render(context.Background(), app, dir)
	if err != nil {
		t.Fatal(err)
	}
	j := m.Jobs[0]
	if *j.Namespace != "team-a" || *j.Region != "eu" || j.Meta[v1alpha1.AppMetaKey] != "web" {
		t.Fatalf("destination/tracking not applied: ns=%s region=%s meta=%v", *j.Namespace, *j.Region, j.Meta)
	}
	if len(m.KV) != 2 || m.KV[0].Key != "web/config.yml" || string(m.KV[0].Value) != "listen: 80\n" || string(m.KV[1].Value) != "on" {
		t.Fatalf("kv = %+v", m.KV)
	}
	if len(m.Images) != 1 || m.Images[0] != "nginx:1.27" {
		t.Fatalf("images = %v", m.Images)
	}
}

func TestNomadRendererHCLThroughParser(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"api.nomad.hcl": `job "api" {}`,
		"vars/base.hcl": "image = \"api:1\"\ncount = 2\n",
		"vars/prod.hcl": "count = 5\n",
	})
	var gotVars string
	parse := func(_ context.Context, hcl, vars string) (*api.Job, error) {
		gotVars = vars
		return &api.Job{ID: ptr("api"), Name: ptr("api")}, nil
	}
	app := &v1alpha1.Application{
		Metadata: v1alpha1.ObjectMeta{Name: "api"},
		Spec: v1alpha1.ApplicationSpec{Project: "default", Source: v1alpha1.ApplicationSource{RepoURL: "x",
			Nomad: &v1alpha1.NomadSource{VarFiles: []string{"vars/base.hcl", "vars/prod.hcl"}, Vars: map[string]string{"image": "api:${evil}"}},
		}},
	}
	m, err := (&Renderer{Parse: parse}).Render(context.Background(), app, dir)
	if err != nil {
		t.Fatal(err)
	}
	// Later files override earlier ones; explicit vars override files and
	// are inert strings (no interpolation).
	want := "image = \"api:$${evil}\"\ncount = 5\n"
	if gotVars != want {
		t.Fatalf("variables:\n%s\nwant:\n%s", gotVars, want)
	}
	if m.Sources["api"].Source != `job "api" {}` || m.Sources["api"].Format != "hcl2" {
		t.Fatalf("submission = %+v", m.Sources["api"])
	}
}

func TestNomadRendererRejects(t *testing.T) {
	ctx := context.Background()
	app := func(src *v1alpha1.NomadSource) *v1alpha1.Application {
		return &v1alpha1.Application{Metadata: v1alpha1.ObjectMeta{Name: "x"},
			Spec: v1alpha1.ApplicationSpec{Source: v1alpha1.ApplicationSource{RepoURL: "x", Nomad: src}}}
	}
	empty := writeFiles(t, map[string]string{"README.md": "x"})
	if _, err := (&Renderer{}).Render(ctx, app(nil), empty); err == nil {
		t.Error("no jobspec accepted")
	}
	dup := writeFiles(t, map[string]string{
		"a.nomad.json": `{"ID":"same","Name":"same"}`,
		"b.nomad.json": `{"ID":"same","Name":"same"}`,
	})
	if _, err := (&Renderer{}).Render(ctx, app(nil), dup); err == nil {
		t.Error("duplicate job accepted")
	}
	reserved := writeFiles(t, map[string]string{"a.nomad.json": `{"ID":"a"}`, "kv.yaml": "- key: cystemd/applications/x\n  value: pwned\n"})
	if _, err := (&Renderer{}).Render(ctx, app(&v1alpha1.NomadSource{ConsulKV: "kv.yaml"}), reserved); err == nil {
		t.Error("key under the reserved prefix accepted")
	}
}
