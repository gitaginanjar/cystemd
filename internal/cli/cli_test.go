// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	hcli "github.com/hashicorp/cli"

	"github.com/gitaginanjar/cystemd/internal/apitest"
	cycli "github.com/gitaginanjar/cystemd/internal/cli"
	"github.com/gitaginanjar/cystemd/internal/config"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

type apiServer = apitest.Server

func newAPIServer(t *testing.T) *apiServer { return apitest.New(t) }

// isolate gives the test its own contexts file and no CYSTEMD or Nomad
// environment.
func isolate(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	t.Setenv("CYSTEMD_CONFIG", path)
	for _, k := range []string{"CYSTEMD_ADDR", "CYSTEMD_TOKEN", "CYSTEMD_CACERT", "CYSTEMD_SKIP_VERIFY", "NOMAD_ADDR", "NOMAD_TOKEN", "NOMAD_CACERT", "NOMAD_SKIP_VERIFY"} {
		t.Setenv(k, "")
	}
	return path
}

type result struct {
	code             int
	stdout, messages string // command output; Ui output and errors
}

func (r result) all() string { return r.stdout + r.messages }

// run runs a command line with input as standard input and the Ui's input.
func run(t *testing.T, o cycli.Options, input string, args ...string) result {
	t.Helper()
	ui := hcli.NewMockUi()
	// MockUi reads each answer through a new bufio.Reader: feed it a
	// byte at a time so that no answer swallows the next.
	ui.InputReader = iotest.OneByteReader(strings.NewReader(input))
	var out bytes.Buffer
	if o.Name == "" {
		o.Name = "cystemdctl"
	}
	o.Version, o.Stdout, o.Stdin = "test", &out, strings.NewReader(input)
	c := &hcli.CLI{Name: o.Name, Args: args, Commands: cycli.Commands(ui, o), HelpWriter: &out}
	code, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	return result{code: code, stdout: out.String(), messages: ui.OutputWriter.String() + ui.ErrorWriter.String()}
}

func ctl(t *testing.T, input string, args ...string) result {
	t.Helper()
	return run(t, cycli.Options{}, input, args...)
}

func mustOK(t *testing.T, r result, want ...string) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.all())
	}
	for _, w := range want {
		if !strings.Contains(r.all(), w) {
			t.Fatalf("output lacks %q:\n%s", w, r.all())
		}
	}
}

func mustFail(t *testing.T, r result, want string) {
	t.Helper()
	if r.code == 0 || !strings.Contains(r.all(), want) {
		t.Fatalf("want a failure with %q, got exit %d:\n%s", want, r.code, r.all())
	}
}

func login(t *testing.T, s *apiServer) {
	t.Helper()
	mustOK(t, ctl(t, s.Password+"\n", "login", "-password-stdin", "-name", "test", s.URL), "Logged in to "+s.URL+" as admin")
}

func TestLoginContextsAndLogout(t *testing.T) {
	s := newAPIServer(t)
	cfgPath := isolate(t)

	// Not logged in: the error says how to.
	mustFail(t, ctl(t, "", "app", "list", "-address", s.URL), "Log in with 'cystemdctl login <server>'")
	mustFail(t, ctl(t, "wrong\n", "login", "-password-stdin", s.URL), "Error:")

	login(t, s)
	if fi, err := os.Stat(cfgPath); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("contexts file: %v %v", fi, err)
	}
	mustOK(t, ctl(t, "", "context"), "*        test  "+s.URL+"  admin")
	mustOK(t, ctl(t, "", "account", "get-user-info"), "Username:   admin", "role:admin")
	mustOK(t, ctl(t, "", "version"), "cystemdctl: test", "server: "+s.URL+" test")

	// The prompt is used without -password(-stdin).
	mustOK(t, ctl(t, s.Password+"\n", "login", "-name", "second", s.URL), `(context "second")`)
	mustOK(t, ctl(t, "", "context", "test"), `Switched to context "test"`)
	mustOK(t, ctl(t, "", "logout"), "Logged out of test.")
	mustOK(t, ctl(t, "", "context"), "test    "+s.URL+"  (logged out)")
	mustFail(t, ctl(t, "", "app", "list"), "Log in with")
	mustOK(t, ctl(t, "", "app", "list", "-context", "second"), "NAME")
	mustOK(t, ctl(t, "", "context", "-delete", "second"), `Deleted context "second"`)
	mustFail(t, ctl(t, "", "context", "nope"), `No context "nope"`)

	// CYSTEMD_ADDR and CYSTEMD_TOKEN override the context.
	login(t, s)
	b, _ := os.ReadFile(cfgPath)
	var cfg struct {
		Contexts map[string]struct{ Token string }
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CYSTEMD_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("CYSTEMD_ADDR", s.URL)
	mustFail(t, ctl(t, "", "app", "list"), "Log in with")
	t.Setenv("CYSTEMD_TOKEN", cfg.Contexts["test"].Token)
	mustOK(t, ctl(t, "", "app", "list"), "NAME")
}

func TestNomadFallback(t *testing.T) {
	s := newAPIServer(t)
	isolate(t)
	agent := cycli.Options{Name: "cystemd", NomadFallback: true, SessionPrefix: "ctl "}
	// NOMAD_ADDR locates the API; NOMAD_TOKEN is exchanged for a session
	// (this server's Nomad is unreachable, so the exchange fails).
	t.Setenv("NOMAD_ADDR", s.URL)
	t.Setenv("NOMAD_TOKEN", "00000000-0000-0000-0000-000000000000")
	mustFail(t, run(t, agent, "", "app", "list"), "log in with NOMAD_TOKEN")
	t.Setenv("NOMAD_TOKEN", "")
	mustFail(t, run(t, agent, "", "app", "list"), "Log in with 'cystemd ctl login <server>'")
	// Session commands live under ctl.
	mustOK(t, run(t, agent, s.Password+"\n", "ctl", "login", "-password-stdin"), "Logged in to "+s.URL)
	mustOK(t, run(t, agent, "", "app", "list"), "NAME")
	r := run(t, agent, "", "ctl")
	if !strings.Contains(r.all(), "Usage: cystemd ctl <subcommand>") {
		t.Fatalf("ctl help:\n%s", r.all())
	}
	if got := strings.Join(cycli.TopLevel(agent), " "); got != "account app cluster ctl migrate proj repo" {
		t.Errorf("agent top-level commands: %s", got)
	}
}

func TestApplicationCommands(t *testing.T) {
	s := newAPIServer(t)
	isolate(t)
	login(t, s)

	mustOK(t, ctl(t, "", "app", "create", "web",
		"-repo", "https://git.example.com/ops.git", "-path", "apps/web", "-revision", "main",
		"-dest-namespace", "web", "-sync-policy", "automated", "-auto-prune", "-label", "team=a",
		"-var", "image=nginx:1", "-var-file", "prod.vars.hcl", "-consul-kv", "kv.yaml"), "application 'web' created")
	mustOK(t, ctl(t, "", "app", "create", "unit",
		"-repo", "https://git.example.com/ops.git", "-path", "svc/unit", "-systemd-values", "values.yaml",
		"-selector", "unit", "-node-selector", "pool", "-replicas", "0"), "application 'unit' created")
	mustFail(t, ctl(t, "", "app", "create", "bad", "-repo", "https://git.example.com/ops.git", "-self-heal"), "need -sync-policy=automated")
	mustFail(t, ctl(t, "", "app", "create", "web", "-repo", "https://git.example.com/ops.git"), `application "web" already exists`)

	// Flags may follow the name.
	r := ctl(t, "", "app", "get", "web", "-o", "json")
	mustOK(t, r)
	var app v1.Application
	if err := json.Unmarshal([]byte(r.stdout), &app); err != nil {
		t.Fatalf("%v:\n%s", err, r.stdout)
	}
	src := app.Spec.Source
	if src.Nomad == nil || src.Nomad.Vars["image"] != "nginx:1" || src.Nomad.ConsulKV != "kv.yaml" ||
		!app.Spec.SyncPolicy.IsAutomated() || !app.Spec.SyncPolicy.Automated.Prune || app.Metadata.Labels["team"] != "a" {
		t.Fatalf("web: %+v", app.Spec)
	}
	mustOK(t, ctl(t, "", "app", "get", "unit"), "Systemd", "Manual", "/ui/applications/unit")
	mustOK(t, ctl(t, "", "app", "list", "-o", "name"), "unit\nweb\n")
	mustOK(t, ctl(t, "", "app", "list", "-l", "team=a", "-o", "wide"), "https://git.example.com/ops.git  apps/web")

	// set changes only what is given.
	mustOK(t, ctl(t, "", "app", "set", "web", "-sync-policy", "none", "-revision", "v2"), "application 'web' updated")
	got, err := s.Store.GetApplication(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.SyncPolicy.IsAutomated() || got.Spec.Source.TargetRevision != "v2" || got.Spec.Source.Nomad.Vars["image"] != "nginx:1" {
		t.Fatalf("after set: %+v", got.Spec)
	}
	mustFail(t, ctl(t, "", "app", "set", "web"), "Nothing to change")
	mustFail(t, ctl(t, "", "app", "set", "unit", "-var", "x=1"), "cannot be combined with a systemd source")

	// Manifests: projects first and kept as they are on a second run,
	// applications created or (-upsert) replaced.
	manifest := filepath.Join(t.TempDir(), "apps.yaml")
	if err := os.WriteFile(manifest, []byte(`apiVersion: cystemd.io/v1alpha1
kind: AppProject
metadata:
  name: team
spec:
  sourceRepos: ["https://git.example.com/*"]
  destinations: [{name: in-cluster, namespace: team}]
---
apiVersion: cystemd.io/v1alpha1
kind: Application
metadata:
  name: api
spec:
  project: team
  source: {repoURL: "https://git.example.com/ops.git", path: apps/api}
  destination: {namespace: team}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	mustOK(t, ctl(t, "", "app", "create", "-f", manifest), "project 'team' created", "application 'api' created")
	mustOK(t, ctl(t, "", "app", "create", "-f", manifest, "-upsert"), "project 'team' updated", "application 'api' created or updated")
	r = ctl(t, "", "app", "create", "-f", manifest)
	if !strings.Contains(r.all(), "project 'team' already exists: unchanged") {
		t.Fatalf("second run:\n%s", r.all())
	}
	mustOK(t, ctl(t, "", "proj", "list"), "team", "https://git.example.com/*", "in-cluster/team")
	mustOK(t, ctl(t, "", "proj", "get", "team", "-o", "json"), `"name": "team"`)
	mustFail(t, ctl(t, "", "proj", "delete", "team"), `used by application "api"`)

	// delete asks for the name unless -y.
	mustFail(t, ctl(t, "nope\n", "app", "delete", "api", "-cascade=false"), "Not confirmed.")
	mustOK(t, ctl(t, "api\n", "app", "delete", "api", "-cascade=false"), "application 'api' deleted")
	mustOK(t, ctl(t, "", "app", "delete", "-y", "-cascade=false", "unit"), "application 'unit' deleted")
	mustOK(t, ctl(t, "", "proj", "delete", "team"), "project 'team' deleted")
	mustFail(t, ctl(t, "", "app", "get", "unit"), "not found")

	// Usage errors.
	mustFail(t, ctl(t, "", "app", "get"), "This command takes 1 argument(s).")
	mustFail(t, ctl(t, "", "app", "list", "-o", "toml"), `unknown output format "toml"`)
	mustFail(t, ctl(t, "", "app", "sync"), "Usage: cystemdctl app sync")
	mustFail(t, ctl(t, "", "app", "sync", "web", "-resource", "bad"), "expected KIND:NAMESPACE:NAME")
	mustFail(t, ctl(t, "", "app", "rollback", "web", "x"), `history id "x"`)
}

func TestAccountsReposClusters(t *testing.T) {
	s := newAPIServer(t)
	isolate(t)
	login(t, s)
	mustOK(t, ctl(t, "", "account", "list"), "admin  true")
	mustFail(t, ctl(t, s.Password+"\nnew-password-1\nother\n", "account", "update-password"), "do not match")
	mustOK(t, ctl(t, s.Password+"\nnew-password-1\nnew-password-1\n", "account", "update-password"), "Password updated")
	// The other sessions are signed out, this one too: log in again.
	mustFail(t, ctl(t, "", "app", "list"), "Log in with")
	mustOK(t, ctl(t, "new-password-1\n", "login", "-password-stdin", s.URL), "Logged in")

	mustFail(t, ctl(t, "", "repo", "add", "https://127.0.0.1:1/ops.git"), "Error:")
	mustOK(t, ctl(t, "", "repo", "add", "https://127.0.0.1:1/ops.git", "-force", "-name", "ops", "-username", "u", "-password", "p"), "repository 'https://127.0.0.1:1/ops.git' added (Failed)")
	mustOK(t, ctl(t, "", "repo", "list"), "https://127.0.0.1:1/ops.git  ops   stored")
	mustOK(t, ctl(t, "", "repo", "rm", "https://127.0.0.1:1/ops.git"), "removed")
	mustOK(t, ctl(t, "", "cluster", "list"), "in-cluster", "http://127.0.0.1:1")
}

func TestAccountTokens(t *testing.T) {
	yes := true
	s := apitest.NewWith(t, func(c *config.Config) {
		c.Auth.Accounts = []config.AccountConfig{{Name: "ci", Capabilities: []string{"apiKey"}, Enabled: &yes}}
		c.RBAC.Policy = "p, ci, applications, sync, default/*, allow\n"
	})
	isolate(t)
	login(t, s)
	mustOK(t, ctl(t, "", "account", "get", "-account", "ci"), "Name:              ci", "apiKey", "No API tokens.")
	mustFail(t, ctl(t, "", "account", "generate-token", "-account", "ci", "-expires-in", "soon"), "-expires-in")
	mustFail(t, ctl(t, "", "account", "generate-token"), "account admin does not have the apiKey capability")
	r := ctl(t, "", "account", "generate-token", "-account", "ci", "-id", "pipeline", "-expires-in", "90d")
	mustOK(t, r)
	token := strings.TrimSpace(r.stdout)
	if strings.Count(token, ".") != 2 || strings.Contains(r.messages, token) {
		t.Fatalf("token output: %q / %q", r.stdout, r.messages)
	}
	mustOK(t, ctl(t, "", "account", "get", "-account", "ci"), "TOKEN ID  ISSUED AT", "pipeline")
	mustOK(t, ctl(t, "", "account", "can-i", "update", "projects", "default"), "yes")

	// The token works as CYSTEMD_TOKEN.
	t.Setenv("CYSTEMD_ADDR", s.URL)
	t.Setenv("CYSTEMD_TOKEN", token)
	mustOK(t, ctl(t, "", "account", "get-user-info"), "Username:   ci")
	mustOK(t, ctl(t, "", "account", "get", "-o", "json"), `"id": "pipeline"`)
	if r := ctl(t, "", "account", "can-i", "sync", "applications", "default/web"); r.stdout != "yes\n" {
		t.Fatalf("can-i sync default/web: %q", r.stdout)
	}
	if r := ctl(t, "", "account", "can-i", "sync", "applications", "platform/web"); r.code != 0 || r.stdout != "no\n" {
		t.Fatalf("can-i sync platform/web: %d %q", r.code, r.stdout)
	}
	mustFail(t, ctl(t, "", "account", "generate-token"), "permission denied")

	t.Setenv("CYSTEMD_TOKEN", "")
	mustOK(t, ctl(t, "", "account", "delete-token", "-account", "ci", "pipeline"), "Token pipeline of account ci deleted.")
	mustFail(t, ctl(t, "", "account", "delete-token", "-account", "ci", "pipeline"), `has no token "pipeline"`)
	mustOK(t, ctl(t, "", "app", "list", "-token", "", "-address", s.URL))
	t.Setenv("CYSTEMD_TOKEN", token)
	mustFail(t, ctl(t, "", "account", "get-user-info"), "Log in with")
}

func TestAdminSetsAnotherPassword(t *testing.T) {
	yes := true
	s := apitest.NewWith(t, func(c *config.Config) {
		c.Auth.Accounts = []config.AccountConfig{{Name: "alice", Capabilities: []string{"login"}, Enabled: &yes}}
	})
	isolate(t)
	login(t, s)
	mustFail(t, ctl(t, "wrong\nalice-password\nalice-password\n", "account", "update-password", "-account", "alice"), "Error:")
	mustOK(t, ctl(t, s.Password+"\nalice-password\nalice-password\n", "account", "update-password", "-account", "alice"), "Password of account alice updated.")
	// The admin's session goes on; alice logs in.
	mustOK(t, ctl(t, "", "account", "list"), "alice")
	mustOK(t, ctl(t, "alice-password\n", "login", "-password-stdin", "-username", "alice", s.URL), "as alice")
}

func TestMigrate(t *testing.T) {
	s := newAPIServer(t)
	isolate(t)
	out := filepath.Join(t.TempDir(), "out")
	args := []string{"migrate", "-config", "../migrate/testdata/config.yml", "-env", "../migrate/testdata/env",
		"-templates-dir", "../migrate/testdata/templates", "-out", out}
	mustOK(t, ctl(t, "", args...), "Wrote 4 applications, 3 hosts", "Warning: no_values")
	for _, f := range []string{"applications.yaml", "MIGRATION.md", "hosts/host002.hcl", "vault/apply.sh"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Error(err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(out, "vault/apply.sh")); fi == nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("apply.sh mode: %v", fi)
	}
	mustFail(t, ctl(t, "", args...), "is not empty")
	mustFail(t, ctl(t, "", "migrate"), "-config is required")

	login(t, s)
	mustOK(t, ctl(t, "", append(args, "-force", "-apply")...), "application 'payments-api' created")
	app, err := s.Store.GetApplication(context.Background(), "myproject_myapplication-poc")
	if err != nil || app.Spec.Source.Systemd.NodeSelector != "poc" {
		t.Fatalf("migrated application: %+v %v", app, err)
	}
	mustFail(t, ctl(t, "", append(args, "-force", "-apply")...), "already exists")
	mustOK(t, ctl(t, "", append(args, "-force", "-apply", "-upsert")...), "created")
}

func TestProjectSyncWindows(t *testing.T) {
	s := newAPIServer(t)
	isolate(t)
	login(t, s)
	mustFail(t, ctl(t, "", "proj", "windows", "add", "default", "-kind", "deny", "-schedule", "every friday", "-duration", "1h", "-applications", "*"),
		`schedule "every friday" is not a cron expression`)
	mustOK(t, ctl(t, "", "proj", "windows", "add", "default", "-kind", "deny", "-schedule", "* * * * *", "-duration", "2m",
		"-applications", "web, api", "-time-zone", "Europe/Amsterdam", "-description", "freeze"), "Sync window 0 added to project default.")
	mustOK(t, ctl(t, "", "proj", "windows", "add", "default", "-kind", "allow", "-schedule", "0 2 * * *", "-duration", "1h",
		"-namespaces", "batch"), "Sync window 1 added")
	mustOK(t, ctl(t, "", "proj", "windows", "list", "default"),
		"0   Active    deny   * * * * *  2m        Europe/Amsterdam  web,api", "1   Inactive  allow  0 2 * * *  1h        -                 -             batch")
	mustOK(t, ctl(t, "", "proj", "windows", "enable-manual-sync", "default", "0"), "updated")
	mustOK(t, ctl(t, "", "proj", "windows", "list", "-o", "json", "default"), `"manualSync": true`)
	mustFail(t, ctl(t, "", "proj", "windows", "delete", "default", "7"), "has no sync window \"7\"")
	mustOK(t, ctl(t, "", "proj", "windows", "delete", "default", "1"), "deleted")

	// The application's sync window, and a sync it blocks.
	mustOK(t, ctl(t, "", "app", "create", "web", "-repo", "https://127.0.0.1:1/ops.git", "-path", "apps/web"), "created")
	mustOK(t, ctl(t, "", "proj", "windows", "disable-manual-sync", "default", "0"), "updated")
	mustOK(t, ctl(t, "", "app", "get", "web"), "Sync window:    Denied")
	mustFail(t, ctl(t, "", "app", "sync", "web", "-async"), "cannot sync: blocked by sync window")
}

func TestProjectRoles(t *testing.T) {
	s := newAPIServer(t)
	isolate(t)
	login(t, s)
	mustOK(t, ctl(t, "", "app", "create", "web", "-repo", "https://127.0.0.1:1/ops.git", "-path", "apps/web"), "created")
	mustOK(t, ctl(t, "", "app", "create", "db", "-repo", "https://127.0.0.1:1/ops.git", "-path", "apps/db"), "created")

	mustFail(t, ctl(t, "", "proj", "role", "create", "default", "-ci"), `role name "-ci"`)
	mustOK(t, ctl(t, "", "proj", "role", "create", "default", "ci", "-description", "GitLab"), "Role ci created in project default.")
	mustFail(t, ctl(t, "", "proj", "role", "create", "default", "ci"), `already has a role "ci"`)
	mustOK(t, ctl(t, "", "proj", "role", "add-policy", "default", "ci", "-action", "sync", "-object", "*"),
		"Policy added: p, proj:default:ci, applications, sync, default/*, allow")
	mustOK(t, ctl(t, "", "proj", "role", "add-policy", "default", "ci", "-action", "sync", "-object", "default/db", "-permission", "deny"), "default/db, deny")
	mustFail(t, ctl(t, "", "proj", "role", "add-policy", "default", "ci", "-action", "update", "-object", "x", "-resource", "projects"), "-resource is applications or logs")
	mustOK(t, ctl(t, "", "proj", "role", "add-group", "default", "ci", "team-payments"), "Group team-payments has role ci.")
	mustOK(t, ctl(t, "", "proj", "role", "list", "default"), "ROLE  DESCRIPTION  POLICIES  GROUPS         TOKENS", "ci    GitLab       2         team-payments  0")

	r := ctl(t, "", "proj", "role", "create-token", "default", "ci", "-id", "gitlab", "-expires-in", "30d")
	mustOK(t, r, "Token created for proj:default:ci.", "ID:          gitlab", "Token:")
	r = ctl(t, "", "proj", "role", "create-token", "default", "ci", "-token-only")
	mustOK(t, r)
	token := strings.TrimSpace(r.stdout)
	if strings.Count(token, ".") != 2 {
		t.Fatalf("token only: %q", r.stdout)
	}
	mustOK(t, ctl(t, "", "proj", "role", "get", "default", "ci"), "Subject:      proj:default:ci", "p, proj:default:ci, applications, sync, default/*, allow", "gitlab")
	mustOK(t, ctl(t, "", "proj", "role", "get", "default", "ci", "-o", "json"), `"name": "ci"`, `"id": "gitlab"`)
	mustOK(t, ctl(t, "", "proj", "role", "list-tokens", "default", "ci"), "TOKEN ID", "gitlab")

	// The token acts as the role.
	t.Setenv("CYSTEMD_ADDR", s.URL)
	t.Setenv("CYSTEMD_TOKEN", token)
	mustOK(t, ctl(t, "", "account", "get-user-info"), "Username:   proj:default:ci", "Provider:   project")
	if r := ctl(t, "", "account", "can-i", "sync", "applications", "default/web"); r.stdout != "yes\n" {
		t.Fatalf("can-i web: %q", r.all())
	}
	if r := ctl(t, "", "account", "can-i", "sync", "applications", "default/db"); r.stdout != "no\n" {
		t.Fatalf("can-i db: %q", r.all())
	}
	mustFail(t, ctl(t, "", "proj", "role", "create-token", "default", "ci"), "permission denied")
	t.Setenv("CYSTEMD_TOKEN", "")

	mustFail(t, ctl(t, "", "proj", "role", "delete-token", "default", "ci", "nope"), `has no token "nope"`)
	mustOK(t, ctl(t, "", "proj", "role", "delete-token", "default", "ci", "gitlab"), "Token gitlab of role proj:default:ci deleted.")
	mustOK(t, ctl(t, "", "proj", "role", "remove-policy", "default", "ci", "-action", "sync", "-object", "db", "-permission", "deny"), "Policy removed")
	mustFail(t, ctl(t, "", "proj", "role", "remove-policy", "default", "ci", "-action", "sync", "-object", "db", "-permission", "deny"), "has no policy")
	mustOK(t, ctl(t, "", "proj", "role", "remove-group", "default", "ci", "team-payments"), "no longer has role ci")
	mustOK(t, ctl(t, "", "proj", "role", "delete", "default", "ci"), "Role ci deleted from project default.")
	mustFail(t, ctl(t, "", "proj", "role", "get", "default", "ci"), `project default has no role "ci"`)
	t.Setenv("CYSTEMD_TOKEN", token)
	mustFail(t, ctl(t, "", "account", "get-user-info"), "Log in with")
}

func TestAppOfAppsFlags(t *testing.T) {
	s := newAPIServer(t)
	isolate(t)
	login(t, s)
	mustFail(t, ctl(t, "", "app", "create", "platform", "-repo", "https://127.0.0.1:1/ops.git", "-path", "applications",
		"-applications", "-var", "x=1"), "cannot be combined")
	mustOK(t, ctl(t, "", "app", "create", "platform", "-repo", "https://127.0.0.1:1/ops.git", "-path", "applications",
		"-applications", "-applications-project", "default"), "created")
	mustOK(t, ctl(t, "", "app", "get", "platform"), "Source type:    Applications")
	r := ctl(t, "", "app", "get", "platform", "-o", "json")
	mustOK(t, r, `"applications": {`, `"projects": [`)
	mustOK(t, ctl(t, "", "app", "set", "platform", "-applications=false"), "updated")
	mustOK(t, ctl(t, "", "app", "get", "platform"), "Source type:    Nomad")
}
