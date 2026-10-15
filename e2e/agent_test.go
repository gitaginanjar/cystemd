// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gitaginanjar/cystemd/internal/testutil"
	"github.com/gitaginanjar/cystemd/pkg/client"
)

// agentBinary is the cystemd agent (make agent), or CYSTEMD_TEST_AGENT.
func agentBinary(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("CYSTEMD_TEST_AGENT")
	if bin == "" {
		bin = filepath.Join(repoRoot, "bin", "cystemd")
	}
	abs, err := filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Skipf("no cystemd agent binary (make agent, or set CYSTEMD_TEST_AGENT): %v", err)
	}
	return abs
}

// TestEndToEndAgent runs the cystemd agent, the Nomad fork, as a dev
// agent: the Nomad API, the CYSTEMD API and UI on the same address, the
// embedded control plane deploying an application, and the agent binary's
// own command line.
func TestEndToEndAgent(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end test")
	}
	bin := agentBinary(t)
	isolate(t)
	consul := testutil.StartConsul(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "admin-password"), "e2e-admin-password\n")
	write(t, filepath.Join(dir, "control-plane.hcl"), fmt.Sprintf(`
log_level = "debug"
auth {
  admin_password_file = %q
}
controller {
  reconcile_interval = "3s"
}
`, filepath.Join(dir, "admin-password")))
	agent := testutil.StartNomad(t, testutil.NomadOptions{Binary: bin, ConsulAddr: consul.Addr, ExtraConfig: fmt.Sprintf(`
cystemd {
  control_plane {
    enabled     = true
    config_file = %q
  }
}
`, filepath.Join(dir, "control-plane.hcl"))})
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("agent log:\n%s", tail(agent.Log(), 150))
		}
	})

	// One address: Nomad's API, CYSTEMD's API, and the UI under Nomad's
	// content security policy.
	var ver client.BuildInfo
	testutil.Eventually(t, time.Minute, func() (bool, string) {
		code, body := httpGet(t, agent.URL+"/api/version", "")
		return code == 200 && json.Unmarshal([]byte(body), &ver) == nil && ver.Mode == "agent", body
	})
	if code, body := httpGet(t, agent.URL+"/v1/agent/self", ""); code != 200 || !strings.Contains(body, `"Version"`) {
		t.Fatalf("Nomad API: %d", code)
	}
	resp, err := httpClient().Get(agent.URL + "/ui/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("UI: %d, CSP %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "CYSTEMD") || !strings.Contains(string(out), "Nomad v2.0.7") {
		t.Fatalf("cystemd version: %v\n%s", err, out)
	}

	// The command line finds the API at NOMAD_ADDR.
	t.Setenv("NOMAD_ADDR", agent.URL)
	mustOK(t, runCLI(t, agentOptions(), "e2e-admin-password\n", "ctl", "login", "-password-stdin"), "Logged in to "+agent.URL)

	repo := testutil.NewGitRepo(t)
	rev := repo.Commit(t, "hello", map[string]string{"apps/hello/hello.nomad.hcl": tickJob("hello", 1)})
	mustOK(t, runCLI(t, agentOptions(), "", "app", "create", "hello", "-repo", repo.URL(), "-path", "apps/hello",
		"-revision", "master", "-sync-policy", "automated", "-self-heal"), "created")
	mustOK(t, runCLI(t, agentOptions(), "", "app", "wait", "hello", "-timeout", "3m"), "hello: Synced, Healthy")
	mustOK(t, runCLI(t, agentOptions(), "", "app", "get", "hello"), "Synced to master ("+rev[:7]+")")

	// Self-heal against the Nomad API on the same agent.
	if out, err := exec.Command(bin, "job", "stop", "-address", agent.URL, "-detach", "hello").CombinedOutput(); err != nil {
		t.Fatalf("job stop: %v\n%s", err, out)
	}
	testutil.Eventually(t, 2*time.Minute, func() (bool, string) {
		_, body := httpGet(t, agent.URL+"/v1/job/hello", "")
		var job struct {
			Stop    bool
			Status  string
			Version int
		}
		_ = json.Unmarshal([]byte(body), &job)
		return !job.Stop && job.Status == "running" && job.Version >= 2, fmt.Sprintf("%+v", job)
	})
	mustOK(t, runCLI(t, agentOptions(), "", "app", "wait", "hello", "-timeout", "3m"), "hello: Synced, Healthy")

	// The real binary's commands, with the session saved by ctl login.
	cmd := exec.Command(bin, "app", "list", "-o", "name")
	cmd.Env = append(os.Environ(), "NOMAD_ADDR="+agent.URL)
	out, err = cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "hello" {
		t.Fatalf("cystemd app list: %v\n%s", err, out)
	}
	mustOK(t, runCLI(t, agentOptions(), "", "app", "delete", "-y", "hello", "-wait"), "deleted")
	testutil.Eventually(t, time.Minute, func() (bool, string) {
		code, _ := httpGet(t, agent.URL+"/v1/job/hello", "")
		return code == 404, fmt.Sprint(code)
	})
}
