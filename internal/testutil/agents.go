// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package testutil starts real Consul, Nomad and Vault dev agents for
// integration tests. Each helper skips the test when the binary is not on
// PATH (or the CYSTEMD_TEST_*_ADDR variable points at a running agent).
package testutil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// FreePorts returns n distinct free TCP ports.
func FreePorts(t testing.TB, n int) []int {
	t.Helper()
	var ports []int
	var ls []net.Listener
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		ports = append(ports, l.Addr().(*net.TCPAddr).Port)
	}
	for _, l := range ls {
		l.Close()
	}
	return ports
}

// Agent is a running dev agent.
type Agent struct {
	Addr string // host:port of the HTTP API
	URL  string // http://host:port
	// Token is a management token when the agent runs with ACLs.
	Token string
	cmd   *exec.Cmd
	log   *syncBuffer
}

// Log returns the agent's output (for failure diagnostics).
func (a *Agent) Log() string { return a.log.String() }

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

// agentEnv is the environment of agent subprocesses: AWS_CA_BUNDLE breaks
// Nomad's AWS fingerprinter, so it is removed.
func agentEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "AWS_CA_BUNDLE=") {
			continue
		}
		env = append(env, kv)
	}
	return env
}

func start(t testing.TB, name string, args []string, ready func(addr string) bool, addr string) *Agent {
	t.Helper()
	bin, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s binary not found on PATH: %v", name, err)
	}
	a := &Agent{Addr: addr, URL: "http://" + addr, log: &syncBuffer{}}
	a.cmd = exec.Command(bin, args...)
	a.cmd.Env = agentEnv()
	a.cmd.Stdout, a.cmd.Stderr = a.log, a.log
	if err := a.cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	exited := make(chan struct{})
	go func() { _ = a.cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = a.cmd.Process.Signal(os.Interrupt)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = a.cmd.Process.Kill()
			<-exited
		}
	})
	deadline := time.Now().Add(60 * time.Second)
	for !ready(addr) {
		select {
		case <-exited:
			t.Fatalf("%s exited before becoming ready:\n%s", name, a.Log())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not become ready:\n%s", name, a.Log())
		}
		time.Sleep(250 * time.Millisecond)
	}
	return a
}

func httpOK(url string) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ConsulOptions configures StartConsulWith.
type ConsulOptions struct {
	// ACL enables ACLs with default deny; Agent.Token is the management
	// token (also the agent's own token).
	ACL bool
}

// StartConsul starts `consul agent -dev` on free ports.
func StartConsul(t testing.TB) *Agent { return StartConsulWith(t, ConsulOptions{}) }

// StartConsulWith starts `consul agent -dev` on free ports with options.
func StartConsulWith(t testing.TB, o ConsulOptions) *Agent {
	t.Helper()
	if addr := os.Getenv("CYSTEMD_TEST_CONSUL_ADDR"); addr != "" && !o.ACL {
		return &Agent{Addr: addr, URL: "http://" + addr, log: &syncBuffer{}}
	}
	p := FreePorts(t, 3)
	addr := fmt.Sprintf("127.0.0.1:%d", p[0])
	args := []string{"agent", "-dev", "-bind=127.0.0.1", "-client=127.0.0.1",
		fmt.Sprintf("-http-port=%d", p[0]), "-grpc-port=-1", "-dns-port=-1",
		fmt.Sprintf("-server-port=%d", p[1]), fmt.Sprintf("-serf-lan-port=%d", p[2]), "-serf-wan-port=-1"}
	token := ""
	if o.ACL {
		token = "cystemd-test-consul-root"
		cfg := filepath.Join(t.TempDir(), "acl.hcl")
		hcl := fmt.Sprintf("acl {\n  enabled = true\n  default_policy = \"deny\"\n  tokens {\n    initial_management = %q\n    agent = %q\n  }\n}\n", token, token)
		if err := os.WriteFile(cfg, []byte(hcl), 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, "-config-file="+cfg)
	}
	a := start(t, "consul", args, func(addr string) bool {
		resp, err := http.Get("http://" + addr + "/v1/status/leader")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode == 200 && len(strings.Trim(b.String(), `"`)) > 2
	}, addr)
	a.Token = token
	return a
}

// NomadOptions configures StartNomad.
type NomadOptions struct {
	// ConsulAddr wires Nomad to Consul (service registration, templates).
	ConsulAddr string
	// Binary overrides the nomad binary (e.g. the cystemd fork).
	Binary string
	// ConsulToken is Nomad's Consul token.
	ConsulToken string
	// ACL enables ACLs and bootstraps them: Agent.Token is the management
	// token.
	ACL bool
	// ExtraConfig is appended to the agent configuration (HCL).
	ExtraConfig string
}

// StartNomad starts a Nomad dev agent (server + client, raw_exec enabled)
// and waits until the client node is ready.
func StartNomad(t testing.TB, o NomadOptions) *Agent {
	t.Helper()
	if addr := os.Getenv("CYSTEMD_TEST_NOMAD_ADDR"); addr != "" && !o.ACL && o.Binary == "" {
		return &Agent{Addr: addr, URL: "http://" + addr, log: &syncBuffer{}}
	}
	p := FreePorts(t, 3)
	dir := t.TempDir()
	consul := `consul { enabled = false }`
	if o.ConsulAddr != "" {
		consul = fmt.Sprintf("consul {\n  address = %q\n  token   = %q\n}", o.ConsulAddr, o.ConsulToken)
	}
	if o.ACL {
		consul += "\nacl {\n  enabled = true\n}"
	}
	cfg := fmt.Sprintf(`
bind_addr = "127.0.0.1"
data_dir  = %q
ports {
  http = %d
  rpc  = %d
  serf = %d
}
%s
plugin "raw_exec" {
  config { enabled = true }
}
%s
`, filepath.Join(dir, "data"), p[0], p[1], p[2], consul, o.ExtraConfig)
	cfgPath := filepath.Join(dir, "agent.hcl")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	name := "nomad"
	if o.Binary != "" {
		name = o.Binary
	}
	addr := fmt.Sprintf("127.0.0.1:%d", p[0])
	token := ""
	a := start(t, name, []string{"agent", "-dev", "-config=" + cfgPath}, func(addr string) bool {
		if o.ACL && token == "" {
			token = bootstrapNomadACL(addr)
			if token == "" {
				return false
			}
		}
		req, _ := http.NewRequest(http.MethodGet, "http://"+addr+"/v1/nodes", nil)
		req.Header.Set("X-Nomad-Token", token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode == 200 && strings.Contains(b.String(), `"Status":"ready"`)
	}, addr)
	a.Token = token
	return a
}

// bootstrapNomadACL bootstraps a Nomad agent's ACLs and returns the
// management token ("" until the server can).
func bootstrapNomadACL(addr string) string {
	resp, err := http.Post("http://"+addr+"/v1/acl/bootstrap", "application/json", nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var out struct{ SecretID string }
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		return ""
	}
	return out.SecretID
}

// StartVault starts `vault server -dev` with a known root token.
func StartVault(t testing.TB) (*Agent, string) {
	t.Helper()
	token := "cystemd-test-root"
	if addr := os.Getenv("CYSTEMD_TEST_VAULT_ADDR"); addr != "" {
		return &Agent{Addr: addr, URL: "http://" + addr, log: &syncBuffer{}}, os.Getenv("CYSTEMD_TEST_VAULT_TOKEN")
	}
	p := FreePorts(t, 1)
	addr := fmt.Sprintf("127.0.0.1:%d", p[0])
	args := []string{"server", "-dev", "-dev-root-token-id=" + token, "-dev-listen-address=" + addr}
	a := start(t, "vault", args, func(addr string) bool { return httpOK("http://" + addr + "/v1/sys/health") }, addr)
	return a, token
}

// Eventually polls cond until it returns true or the timeout elapses.
func Eventually(t testing.TB, timeout time.Duration, cond func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		ok, msg := cond()
		if ok {
			return
		}
		last = msg
		if time.Now().After(deadline) {
			t.Fatalf("condition not met after %s: %s", timeout, last)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// StartVaultAgent runs `vault agent` with config (HCL) and waits until
// ready reports true (for example, the api_proxy listener answering).
func StartVaultAgent(t testing.TB, config []byte, ready func() bool) *Agent {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault-agent.hcl")
	if err := os.WriteFile(path, config, 0o600); err != nil {
		t.Fatal(err)
	}
	return start(t, "vault", []string{"agent", "-config=" + path}, func(string) bool { return ready() }, "")
}
