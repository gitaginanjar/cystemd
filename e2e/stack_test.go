// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	hcli "github.com/hashicorp/cli"
	consulapi "github.com/hashicorp/consul/api"
	nomadapi "github.com/hashicorp/nomad/api"
	vaultapi "github.com/hashicorp/vault/api"

	cycli "github.com/gitaginanjar/cystemd/internal/cli"
	"github.com/gitaginanjar/cystemd/internal/config"
	"github.com/gitaginanjar/cystemd/internal/controlplane"
	"github.com/gitaginanjar/cystemd/internal/server"
	"github.com/gitaginanjar/cystemd/internal/testutil"
	"github.com/gitaginanjar/cystemd/internal/vaultagent"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

// repoRoot is the repository's root (deploy/ assets).
const repoRoot = ".."

// stackOptions select what the stack runs with.
type stackOptions struct {
	// acl runs Consul with default-deny ACLs and Nomad with ACLs; the
	// control plane gets tokens for the policies in deploy/.
	acl bool
	// vault adds Vault, configured by deploy/vault/setup.sh: Nomad gives
	// workloads identities for its JWT auth, and the control plane reaches
	// it through a Vault Agent (AppRole cystemd-server-host, API proxy).
	vault bool
}

// stack is Consul and Nomad dev agents and a control plane on them, in
// this process, with a Git repository for the applications.
type stack struct {
	t        *testing.T
	ctx      context.Context
	consul   *testutil.Agent
	nomad    *testutil.Agent
	nomadAPI *nomadapi.Client  // management
	kv       *consulapi.KV     // management
	repo     *testutil.GitRepo // file://
	vault    *testutil.Agent
	vaultAPI *vaultapi.Client // root
	url      string           // the control plane
	password string           // admin's
	api      *client.Client   // logged in as admin
	logs     *syncBuffer
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func newStack(t *testing.T, o stackOptions) *stack {
	t.Helper()
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	isolate(t)
	s := &stack{t: t, logs: &syncBuffer{}}
	s.consul = testutil.StartConsulWith(t, testutil.ConsulOptions{ACL: o.acl})
	nomadExtra := ""
	if o.vault {
		var root string
		s.vault, root = testutil.StartVault(t)
		vcfg := vaultapi.DefaultConfig()
		vcfg.Address = s.vault.URL
		var err error
		if s.vaultAPI, err = vaultapi.NewClient(vcfg); err != nil {
			t.Fatal(err)
		}
		s.vaultAPI.SetToken(root)
		// Tasks log in to Vault with their workload identity.
		nomadExtra = fmt.Sprintf(`
vault {
  enabled               = true
  address               = %q
  jwt_auth_backend_path = "jwt-nomad"
  default_identity {
    aud = ["vault.io"]
    ttl = "1h"
  }
}
`, s.vault.URL)
	}
	s.nomad = testutil.StartNomad(t, testutil.NomadOptions{ConsulAddr: s.consul.Addr, ConsulToken: s.consul.Token, ACL: o.acl, ExtraConfig: nomadExtra})

	ccfg := consulapi.DefaultConfig()
	ccfg.Address, ccfg.Token = s.consul.Addr, s.consul.Token
	cc, err := consulapi.NewClient(ccfg)
	if err != nil {
		t.Fatal(err)
	}
	s.kv = cc.KV()
	ncfg := nomadapi.DefaultConfig()
	ncfg.Address, ncfg.SecretID = s.nomad.URL, s.nomad.Token
	if s.nomadAPI, err = nomadapi.NewClient(ncfg); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	nomadToken, consulToken := "", ""
	if o.acl {
		nomadToken = s.nomadToken(filepath.Join(repoRoot, "deploy/nomad/acl/cystemd-control-plane.hcl"))
		consulToken = s.consulToken(cc, filepath.Join(repoRoot, "deploy/consul/policies/cystemd-control-plane.hcl"))
	}
	write(t, filepath.Join(dir, "nomad.token"), nomadToken)
	write(t, filepath.Join(dir, "consul.token"), consulToken)
	vaultBlock := ""
	if o.vault {
		vaultBlock = fmt.Sprintf(`
vault {
  enabled         = true
  agent_address   = %q
  kv_mount        = "secret"
  repo_creds_path = "cystemd/repositories"
}
`, s.setUpVault(dir))
	}
	cfgFile := filepath.Join(dir, "control-plane.hcl")
	write(t, cfgFile, fmt.Sprintf(`
data_dir  = %q
log_level = "debug"
%s
nomad {
  address    = %q
  token_file = %q
}

consul {
  enabled    = true
  address    = %q
  token_file = %q
  kv_prefix  = "cystemd"
}

controller {
  reconcile_interval = "3s"
  self_heal_timeout  = "1s"
}
`, filepath.Join(dir, "data"), vaultBlock, s.nomad.URL, filepath.Join(dir, "nomad.token"), s.consul.Addr, filepath.Join(dir, "consul.token")))
	cfg, err := config.Load(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	s.ctx = ctx
	logger := slog.New(slog.NewTextHandler(s.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cp, err := controlplane.New(ctx, cfg, controlplane.Options{Logger: logger, Build: server.BuildInfo{Version: "e2e", Mode: "standalone"}})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	srv := httptest.NewServer(cp.Server.Handler())
	done := make(chan struct{})
	go func() { cp.Run(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		srv.Close()
		<-done
		cp.Close()
		if t.Failed() {
			t.Logf("control plane log:\n%s", tail(s.logs.String(), 200))
		}
	})
	s.url = srv.URL
	pw, err := os.ReadFile(filepath.Join(dir, "data", "initial-admin-password"))
	if err != nil {
		t.Fatal(err)
	}
	s.password = strings.TrimSpace(string(pw))
	if s.api, err = client.New(s.url, client.Options{}); err != nil {
		t.Fatal(err)
	}
	if s.api.Token, err = s.api.Login(ctx, "admin", s.password); err != nil {
		t.Fatal(err)
	}
	s.repo = testutil.NewGitRepo(t)
	return s
}

// setUpVault runs deploy/vault/setup.sh and starts the control plane's
// Vault Agent with the AppRole it creates; it returns the agent's API
// proxy address.
func (s *stack) setUpVault(dir string) string {
	t := s.t
	cmd := exec.Command("bash", filepath.Join(repoRoot, "deploy", "vault", "setup.sh"))
	cmd.Env = append(os.Environ(), "VAULT_ADDR="+s.vault.URL, "VAULT_TOKEN="+s.vaultAPI.Token(), "NOMAD_ADDR="+s.nomad.URL)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("deploy/vault/setup.sh: %v\n%s", err, out)
	}
	rid, err := s.vaultAPI.Logical().Read("auth/approle/role/cystemd-server-host/role-id")
	if err != nil {
		t.Fatal(err)
	}
	sid, err := s.vaultAPI.Logical().Write("auth/approle/role/cystemd-server-host/secret-id", nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "role-id"), rid.Data["role_id"].(string))
	write(t, filepath.Join(dir, "secret-id"), sid.Data["secret_id"].(string))
	proxy := fmt.Sprintf("127.0.0.1:%d", testutil.FreePorts(t, 1)[0])
	cfg := &vaultagent.Config{
		Vault: vaultagent.Vault{Address: s.vault.URL},
		AutoAuth: vaultagent.AutoAuth{Method: vaultagent.MethodAppRole, MountPath: "auth/approle",
			RoleIDFile: filepath.Join(dir, "role-id"), SecretIDFile: filepath.Join(dir, "secret-id")},
		APIProxy: &vaultagent.APIProxy{Address: proxy},
	}
	hcl, err := cfg.HCL()
	if err != nil {
		t.Fatal(err)
	}
	testutil.StartVaultAgent(t, hcl, func() bool {
		// Ready once logged in: a request without a token is proxied
		// with the agent's.
		resp, err := http.Get("http://" + proxy + "/v1/auth/token/lookup-self")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	return "http://" + proxy
}

// nomadToken creates a Nomad ACL policy from a file and returns a client
// token for it.
func (s *stack) nomadToken(policyFile string) string {
	rules, err := os.ReadFile(policyFile)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.nomadAPI.ACLPolicies().Upsert(&nomadapi.ACLPolicy{Name: "cystemd-control-plane", Rules: string(rules)}, nil); err != nil {
		s.t.Fatalf("nomad policy %s: %v", policyFile, err)
	}
	tok, _, err := s.nomadAPI.ACLTokens().Create(&nomadapi.ACLToken{Name: "cystemd-control-plane", Type: "client", Policies: []string{"cystemd-control-plane"}}, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	return tok.SecretID
}

// consulToken does the same in Consul (deploy/consul/setup.sh).
func (s *stack) consulToken(cc *consulapi.Client, policyFile string) string {
	rules, err := os.ReadFile(policyFile)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, _, err := cc.ACL().PolicyCreate(&consulapi.ACLPolicy{Name: "cystemd-control-plane", Rules: string(rules)}, nil); err != nil {
		s.t.Fatalf("consul policy %s: %v", policyFile, err)
	}
	tok, _, err := cc.ACL().TokenCreate(&consulapi.ACLToken{Description: "CYSTEMD control plane",
		Policies: []*consulapi.ACLTokenPolicyLink{{Name: "cystemd-control-plane"}}}, nil)
	if err != nil {
		s.t.Fatal(err)
	}
	return tok.SecretID
}

// app reads an Application.
func (s *stack) app(name string) *v1.Application {
	s.t.Helper()
	a, err := s.api.GetApplication(s.ctx, name, "")
	if err != nil {
		s.t.Fatal(err)
	}
	return a
}

// waitApp polls an Application until cond holds.
func (s *stack) waitApp(name string, timeout time.Duration, cond func(*v1.Application) bool) *v1.Application {
	s.t.Helper()
	var last *v1.Application
	testutil.Eventually(s.t, timeout, func() (bool, string) {
		a, err := s.api.GetApplication(s.ctx, name, "")
		if err != nil {
			return false, err.Error()
		}
		last = a
		return cond(a), fmt.Sprintf("sync=%s health=%s (%s) op=%+v conditions=%+v",
			a.Status.Sync.Status, a.Status.Health.Status, a.Status.Health.Message, a.Status.OperationState, a.Status.Conditions)
	})
	return last
}

// job reads a Nomad job (nil when there is none).
func (s *stack) job(id string) *nomadapi.Job {
	j, _, err := s.nomadAPI.Jobs().Info(id, nil)
	if err != nil {
		return nil
	}
	return j
}

// key reads a Consul key ("" and false when there is none).
func (s *stack) key(k string) (string, bool) {
	p, _, err := s.kv.Get(k, nil)
	if err != nil || p == nil {
		return "", false
	}
	return string(p.Value), true
}

// tickJob is a job printing a counter to stdout.
func tickJob(id string, count int) string {
	return fmt.Sprintf(`job %q {
  datacenters = ["*"]
  group "app" {
    count = %d
    task "tick" {
      driver = "raw_exec"
      config {
        command = "/bin/sh"
        args    = ["-c", "i=0; while true; do echo tick $i; i=$((i+1)); sleep 1; done"]
      }
      resources {
        cpu    = 20
        memory = 16
      }
    }
  }
}
`, id, count)
}

// ─── the command line, in this process ──────────────────────────────────

// isolate gives the test its own CLI contexts and no CYSTEMD, Nomad or
// Consul environment.
func isolate(t *testing.T) {
	t.Setenv("CYSTEMD_CONFIG", filepath.Join(t.TempDir(), "cli.json"))
	for _, k := range []string{"CYSTEMD_ADDR", "CYSTEMD_TOKEN", "CYSTEMD_CACERT", "CYSTEMD_SKIP_VERIFY",
		"NOMAD_ADDR", "NOMAD_TOKEN", "NOMAD_CACERT", "NOMAD_SKIP_VERIFY", "NOMAD_REGION", "NOMAD_NAMESPACE",
		"CONSUL_HTTP_ADDR", "CONSUL_HTTP_TOKEN", "VAULT_ADDR", "VAULT_AGENT_ADDR", "VAULT_TOKEN"} {
		unsetenv(t, k)
	}
}

// unsetenv removes a variable for the test: an empty value is not the same
// as none to the agents (Vault Agent prefers a present VAULT_ADDR, even
// empty, over its configuration).
func unsetenv(t *testing.T, key string) {
	t.Setenv(key, "") // restores the original value afterwards
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}

type result struct {
	code int
	out  string // output and messages
}

// cli runs a CYSTEMD command line (cystemdctl's) with input as the
// answers to prompts.
func cli(t *testing.T, input string, args ...string) result {
	t.Helper()
	return runCLI(t, cycli.Options{Name: "cystemdctl"}, input, args...)
}

// agentOptions are the cystemd agent's command line options.
func agentOptions() cycli.Options {
	return cycli.Options{Name: "cystemd", NomadFallback: true, DefaultAddress: "http://127.0.0.1:4646", SessionPrefix: "ctl "}
}

func runCLI(t *testing.T, o cycli.Options, input string, args ...string) result {
	t.Helper()
	ui := hcli.NewMockUi()
	ui.InputReader = iotest.OneByteReader(strings.NewReader(input))
	var out bytes.Buffer
	o.Version, o.Stdout, o.Stdin = "e2e", &out, strings.NewReader(input)
	c := &hcli.CLI{Name: o.Name, Args: args, Commands: cycli.Commands(ui, o), HelpWriter: &out}
	code, err := c.Run()
	if err != nil {
		t.Fatal(err)
	}
	return result{code: code, out: out.String() + ui.OutputWriter.String() + ui.ErrorWriter.String()}
}

func mustOK(t *testing.T, r result, want ...string) string {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d:\n%s", r.code, r.out)
	}
	for _, w := range want {
		if !strings.Contains(r.out, w) {
			t.Fatalf("output lacks %q:\n%s", w, r.out)
		}
	}
	return r.out
}

func mustExit(t *testing.T, r result, code int, want ...string) string {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d:\n%s", r.code, code, r.out)
	}
	for _, w := range want {
		if !strings.Contains(r.out, w) {
			t.Fatalf("output lacks %q:\n%s", w, r.out)
		}
	}
	return r.out
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func tail(s string, lines int) string {
	l := strings.Split(s, "\n")
	if len(l) > lines {
		l = l[len(l)-lines:]
	}
	return strings.Join(l, "\n")
}

// httpGet fetches a URL with an optional bearer token.
func httpGet(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

// httpClient does not follow redirects.
func httpClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
