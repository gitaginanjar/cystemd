// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestParseFullConfig(t *testing.T) {
	src := `
bind_addr = "127.0.0.1:9090"
url       = "https://cystemd.example.com"

nomad {
  address = "https://nomad.service.consul:4646"
  token_file = "/run/secrets/nomad"
}

cluster "eu-prod" {
  address = "https://nomad.eu:4646"
  region  = "eu"
}

consul {
  address   = "127.0.0.1:8501"
  kv_prefix = "platform/cystemd"
}

vault {
  agent_address = "http://127.0.0.1:8100"
}

controller {
  reconcile_interval = "1m"
}

rbac {
  default_role = ""
  policy = <<EOT
p, role:deployer, applications, sync, */*, allow
g, alice, role:deployer
EOT
}

repository {
  url                  = "git@bitbucket.org:org/gitops.git"
  ssh_private_key_file = "/etc/cystemd/gitops_ed25519"
}
`
	c, err := Parse([]byte(src), "server.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if c.BindAddr != "127.0.0.1:9090" || len(c.Clusters) != 1 || c.Clusters[0].Name != "eu-prod" {
		t.Fatalf("config = %+v", c)
	}
	if !c.ConsulEnabled() || c.Consul.KVPrefix != "platform/cystemd" {
		t.Fatalf("consul = %+v", c.Consul)
	}
	if !c.VaultEnabled() || c.Vault.KVMount != "secret" || c.Vault.WorkloadRole != "cystemd-workloads" {
		t.Fatalf("vault defaults = %+v", c.Vault)
	}
	if !strings.Contains(c.RBAC.Policy, "role:deployer") || c.RBAC.DefaultRole != "role:readonly" {
		t.Fatalf("rbac = %+v", c.RBAC)
	}
	if len(c.Repositories) != 1 || Duration(c.Controller.ReconcileInterval, 0).Minutes() != 1 {
		t.Fatalf("repositories/controller = %+v %+v", c.Repositories, c.Controller)
	}
}

func TestParseRejects(t *testing.T) {
	for name, src := range map[string]string{
		"unknown block":     `bogus {}`,
		"bad duration":      "controller {\n reconcile_interval = \"often\"\n}",
		"duplicate cluster": "cluster \"a\" {\n address = \"x\"\n}\ncluster \"a\" {\n address = \"y\"\n}",
		"reserved cluster":  "cluster \"in-cluster\" {\n address = \"x\"\n}",
		"half tls":          `tls_cert_file = "/x"`,
		"bad account name":  "auth {\n account \"ci/x\" {}\n}",
		"duplicate account": "auth {\n account \"ci\" {}\n account \"ci\" {}\n}",
		"bad capability":    "auth {\n account \"ci\" {\n capabilities = [\"apikey\"]\n }\n}",
		"disabled admin":    "auth {\n account \"admin\" {\n enabled = false\n }\n}",
	} {
		if _, err := Parse([]byte(src), "x.hcl"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestAccounts(t *testing.T) {
	c, err := Parse([]byte(`
auth {
  account "alice" {}
  account "ci" {
    capabilities = ["apiKey"]
  }
  account "old.bot@example" {
    capabilities = ["login", "apiKey"]
    enabled      = false
  }
  account "admin" {
    capabilities = ["login", "apiKey"]
  }
}
`), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"alice": "login true", "ci": "apiKey true", "old.bot@example": "login,apiKey false", "admin": "login,apiKey true"}
	if len(c.Auth.Accounts) != len(want) {
		t.Fatalf("accounts = %+v", c.Auth.Accounts)
	}
	for _, a := range c.Auth.Accounts {
		if got := strings.Join(a.Capabilities, ",") + " " + fmt.Sprint(*a.Enabled); got != want[a.Name] {
			t.Errorf("%s: %s", a.Name, got)
		}
	}
}

func TestDefaultsAndEnv(t *testing.T) {
	t.Setenv("NOMAD_ADDR", "http://nomad:4646")
	t.Setenv("CONSUL_HTTP_ADDR", "")
	t.Setenv("VAULT_AGENT_ADDR", "")
	t.Setenv("VAULT_ADDR", "")
	c := Default()
	if c.Nomad.Address != "http://nomad:4646" || c.ConsulEnabled() || c.VaultEnabled() {
		t.Fatalf("defaults = %+v consul=%v vault=%v", c.Nomad, c.ConsulEnabled(), c.VaultEnabled())
	}
	t.Setenv("CONSUL_HTTP_ADDR", "consul:8500")
	t.Setenv("VAULT_AGENT_ADDR", "http://127.0.0.1:8100")
	c = Default()
	if !c.ConsulEnabled() || c.Consul.Address != "consul:8500" || !c.VaultEnabled() || c.Vault.AgentAddress != "http://127.0.0.1:8100" {
		t.Fatalf("env = %+v %+v", c.Consul, c.Vault)
	}
}

func TestParseEmbedded(t *testing.T) {
	t.Setenv("NOMAD_ADDR", "")
	t.Setenv("CONSUL_HTTP_ADDR", "")
	t.Setenv("VAULT_AGENT_ADDR", "")
	t.Setenv("VAULT_ADDR", "")
	d := &EmbeddedDefaults{
		DataDir: "/var/lib/nomad/cystemd", NomadAddress: "https://127.0.0.1:4646", NomadRegion: "eu",
		NomadCACert: "/etc/nomad/ca.pem", NomadClientCert: "/etc/nomad/cli.pem", NomadClientKey: "/etc/nomad/cli-key.pem",
		ConsulAddress: "127.0.0.1:8501", ConsulToken: "agent-token",
	}
	c, err := ParseEmbedded(nil, "", d)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != d.DataDir || c.Nomad.Address != d.NomadAddress || c.Nomad.Region != "eu" ||
		c.Nomad.CACert != d.NomadCACert || c.Nomad.ClientKey != d.NomadClientKey || c.ConsulEnabled() {
		t.Fatalf("embedded defaults: %+v nomad=%+v", c, c.Nomad)
	}
	// The file wins; Consul enabled without an address uses the agent's.
	src := []byte(`
data_dir = "/srv/cystemd"
nomad { address = "http://nomad.service.consul:4646" }
consul { kv_prefix = "cd" }
`)
	c, err = ParseEmbedded(src, "cp.hcl", d)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/srv/cystemd" || c.Nomad.Address != "http://nomad.service.consul:4646" || c.Nomad.CACert != "" ||
		!c.ConsulEnabled() || c.Consul.Address != "127.0.0.1:8501" || c.Consul.Token != "agent-token" || c.Consul.KVPrefix != "cd" {
		t.Fatalf("file over embedded: %+v nomad=%+v consul=%+v", c, c.Nomad, c.Consul)
	}
	if _, err := ParseEmbedded([]byte(`nomad { client_cert = "/x.pem" }`), "cp.hcl", nil); err == nil {
		t.Fatal("client_cert without client_key accepted")
	}
}

// The shipped example must stay valid.
func TestDeployExampleParses(t *testing.T) {
	c, err := Load("../../deploy/agent/control-plane.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if !c.ConsulEnabled() || !c.VaultEnabled() || c.Vault.AgentAddress != "http://127.0.0.1:8100" ||
		len(c.Repositories) != 1 || !strings.Contains(c.RBAC.Policy, "role:deployer") {
		t.Fatalf("example: %+v", c)
	}
}
