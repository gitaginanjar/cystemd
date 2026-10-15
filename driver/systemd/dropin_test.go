// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"strings"
	"testing"

	"github.com/hashicorp/nomad/plugins/drivers"
)

func TestRenderDropIn(t *testing.T) {
	cfg := &drivers.TaskConfig{
		JobID: "myapp", TaskGroupName: "myapp", Name: "myapp", AllocID: "a1b2c3d4-0000",
		AllocDir: "/var/lib/nomad/alloc/a1b2c3d4-0000",
		Resources: &drivers.Resources{LinuxResources: &drivers.LinuxResources{
			MemoryLimitBytes: 256 << 20, CPUShares: 500,
		}},
	}
	tc := &TaskConfig{
		Unit:             "myapp",
		EnforceResources: true,
		EnvironmentFiles: []string{"secrets/.env", "alloc/data/.env"},
		CPUQuota:         "200m",
		Properties:       []map[string]string{{"Restart": "always", "LimitNOFILE": "65536"}},
	}
	got, err := renderDropIn(cfg, tc, "/x/secrets/.cystemd-task.env")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Service]\n",
		"EnvironmentFile=/x/secrets/.cystemd-task.env\n",
		"EnvironmentFile=-/var/lib/nomad/alloc/a1b2c3d4-0000/myapp/secrets/.env\n",
		"EnvironmentFile=-/var/lib/nomad/alloc/a1b2c3d4-0000/alloc/data/.env\n",
		"CPUQuota=20%\n",
		"MemoryMax=268435456\n", // no memory_max: the Nomad reservation is enforced
		"CPUWeight=",
		"LimitNOFILE=65536\nRestart=always\n", // sorted
	} {
		if !strings.Contains(got, want) {
			t.Errorf("drop-in lacks %q:\n%s", want, got)
		}
	}

	// An explicit memory_max wins over the reservation.
	tc.MemoryMax = "1Gi"
	got, _ = renderDropIn(cfg, tc, "/env")
	if !strings.Contains(got, "MemoryMax=1073741824\n") || strings.Contains(got, "MemoryMax=268435456") {
		t.Errorf("explicit memory_max not applied:\n%s", got)
	}

	// Unit mode without enforce_resources never caps an adopted daemon.
	tc.MemoryMax, tc.EnforceResources = "", false
	if got, _ := renderDropIn(cfg, tc, "/env"); strings.Contains(got, "MemoryMax") {
		t.Errorf("reservation enforced without enforce_resources:\n%s", got)
	}
	tc.MemoryMax = "1Gi"

	// Deterministic: same input, same bytes (no spurious daemon-reload).
	again, _ := renderDropIn(cfg, tc, "/env")
	if again != got {
		t.Error("renderDropIn is not deterministic")
	}
}

func TestRenderEnvFile(t *testing.T) {
	got, skipped := renderEnvFile(map[string]string{
		"B":           `quote " back\slash $HOME` + "`x`",
		"A":           "plain",
		"MULTI":       "line1\nline2",
		"1BAD":        "x",
		"bad-key":     "x",
		"NOMAD_TOKEN": "s3cr3t",
	})
	want := "A=\"plain\"\n" +
		"B=\"quote \\\" back\\\\slash \\$HOME\\`x\\`\"\n" +
		"NOMAD_TOKEN=\"s3cr3t\"\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if len(skipped) != 1 || skipped[0] != "MULTI" {
		t.Fatalf("skipped = %v", skipped)
	}
}

func TestCPUWeight(t *testing.T) {
	for shares, want := range map[int64]uint64{1: 1, 2: 1, 1024: 39, 262144: 10000, 500000: 10000} {
		if got := cpuWeight(shares); got != want {
			t.Errorf("cpuWeight(%d) = %d, want %d", shares, got, want)
		}
	}
}

func TestClaimUnit(t *testing.T) {
	ownerDir = t.TempDir()
	live := map[string]bool{"alloc-a": true}
	isLive := func(id string) bool { return live[id] }

	if err := claimUnit("myapp.service", "alloc-a", isLive); err != nil {
		t.Fatal(err)
	}
	// Same allocation re-claims (task restart).
	if err := claimUnit("myapp.service", "alloc-a", isLive); err != nil {
		t.Fatal(err)
	}
	// Another allocation is refused while the owner runs.
	if err := claimUnit("myapp.service", "alloc-b", isLive); err == nil {
		t.Fatal("second allocation claimed a live unit")
	}
	// Stale owner (no longer running here) is taken over.
	live["alloc-a"] = false
	if err := claimUnit("myapp.service", "alloc-b", isLive); err != nil {
		t.Fatal(err)
	}
	// Release only by the owner.
	releaseUnit("myapp.service", "alloc-a")
	if err := claimUnit("myapp.service", "alloc-c", func(id string) bool { return id == "alloc-b" }); err == nil {
		t.Fatal("non-owner release removed the claim")
	}
	releaseUnit("myapp.service", "alloc-b")
	if err := claimUnit("myapp.service", "alloc-c", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
}
