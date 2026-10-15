// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/plugins/drivers"
)

// runtimeUnitDir is where runtime drop-ins live: they vanish at reboot,
// exactly when Nomad re-creates them by starting the task again.
var runtimeUnitDir = "/run/systemd/system"

// ownerDir records which allocation currently owns a unit on this node.
var ownerDir = "/run/cystemd/units"

// taskEnvFileName is the task environment file, written into the task's
// secrets directory (0600). The environment can carry tokens, so it never
// goes into a world-readable drop-in.
const taskEnvFileName = ".cystemd-task.env"

// dropInPath is the drop-in file the driver owns for unit.
func dropInPath(unit string) string {
	return filepath.Join(runtimeUnitDir, unit+".d", "90-cystemd.conf")
}

// renderDropIn renders the [Service] drop-in for a unit-mode task: the task
// environment and declared environment files, resource limits and allowed
// extra directives. Output is deterministic (sorted), so an unchanged task
// yields a byte-identical file.
func renderDropIn(cfg *drivers.TaskConfig, tc *TaskConfig, taskEnvFile string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Managed by the CYSTEMD systemd task driver — do not edit.\n")
	fmt.Fprintf(&b, "# Nomad job %q, group %q, task %q, allocation %s.\n", cfg.JobID, cfg.TaskGroupName, cfg.Name, cfg.AllocID)
	b.WriteString("[Service]\n")
	fmt.Fprintf(&b, "EnvironmentFile=%s\n", taskEnvFile)
	for _, e := range tc.EnvironmentFiles {
		// "-": a not-yet-rendered optional file must not block the start.
		fmt.Fprintf(&b, "EnvironmentFile=-%s\n", filepath.Join(cfg.TaskDir().Dir, filepath.Clean(e)))
	}
	if tc.CPUQuota != "" {
		q, err := cpuQuotaPerSecUSec(tc.CPUQuota)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "CPUQuota=%s\n", cpuQuotaPercent(q))
	}
	if tc.MemoryMax != "" {
		m, err := memoryBytes(tc.MemoryMax)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "MemoryMax=%d\n", m)
	} else if lr := linuxResources(cfg); lr != nil && lr.MemoryLimitBytes > 0 {
		// No explicit limit: enforce the Nomad memory reservation (memory_max
		// when the job sets it, else memory), like every Nomad driver.
		fmt.Fprintf(&b, "MemoryMax=%d\n", lr.MemoryLimitBytes)
	}
	if lr := linuxResources(cfg); lr != nil && lr.CPUShares > 0 {
		fmt.Fprintf(&b, "CPUWeight=%d\n", cpuWeight(lr.CPUShares))
	}
	props := tc.flatProperties()
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%s\n", k, props[k])
	}
	return b.String(), nil
}

func linuxResources(cfg *drivers.TaskConfig) *drivers.LinuxResources {
	if cfg.Resources == nil {
		return nil
	}
	return cfg.Resources.LinuxResources
}

// cpuWeight maps Nomad CPU shares (MHz-based, cgroup v1 cpu.shares) onto the
// cgroup v2 CPUWeight range [1, 10000], as runc does.
func cpuWeight(shares int64) uint64 {
	if shares <= 2 {
		return 1
	}
	if shares >= 262144 {
		return 10000
	}
	return uint64(1 + ((shares-2)*9999)/262142)
}

// renderEnvFile renders the task environment as a systemd EnvironmentFile:
// sorted KEY="VALUE" lines. Values are double-quoted with \\, ", $ and `
// backslash-escaped (systemd's double-quote rules). A value with a newline
// cannot be expressed on one line and is skipped; its key is returned.
func renderEnvFile(env map[string]string) (string, []string) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if validEnvKey(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	var skipped []string
	for _, k := range keys {
		v := env[k]
		if strings.ContainsAny(v, "\n\r") {
			skipped = append(skipped, k)
			continue
		}
		fmt.Fprintf(&b, "%s=\"%s\"\n", k, envEscaper.Replace(v))
	}
	return b.String(), skipped
}

var envEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

func validEnvKey(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// writeOwned writes content to path (0600 for the env file, 0644 for the
// drop-in) atomically and reports whether the bytes changed.
func writeOwned(path, content string, mode os.FileMode) (bool, error) {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return false, nil
	}
	f := FileConfig{Destination: path, Mode: strconv.FormatUint(uint64(mode), 8)}
	if err := writeFileAtomic(f, []byte(content)); err != nil {
		return false, err
	}
	return true, nil
}

// claimUnit records allocID as the owner of unit. A unit is a node-wide
// singleton: two allocations driving it would stop each other's process.
// The claim succeeds when the unit is unowned, already ours, or owned by an
// allocation this driver no longer runs (stale) — isLive answers the last.
func claimUnit(unit, allocID string, isLive func(allocID string) bool) error {
	if err := os.MkdirAll(ownerDir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(ownerDir, unit)
	if b, err := os.ReadFile(p); err == nil {
		owner := strings.TrimSpace(string(b))
		if owner != "" && owner != allocID && isLive(owner) {
			return fmt.Errorf("unit %s is already managed by allocation %s on this node", unit, owner)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return os.WriteFile(p, []byte(allocID+"\n"), 0o644)
}

// releaseUnit removes the owner record when allocID still holds it.
func releaseUnit(unit, allocID string) {
	p := filepath.Join(ownerDir, unit)
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) == allocID {
		_ = os.Remove(p)
	}
}
