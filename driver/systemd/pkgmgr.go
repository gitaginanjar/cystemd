// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// validPackageName / validPackageVersion keep job input out of option
// parsing: a name or version can never start with "-" or carry whitespace.
var (
	validPackageName    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)
	validPackageVersion = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+~:^-]*$`)
)

// rpmQueryTimeout bounds read-only package database queries: a contended
// database must cost one inconclusive check, never a hung task start.
const rpmQueryTimeout = 10 * time.Second

// commandRunner runs a command and returns its stdout and stderr. It is the
// exec seam of the package managers (tests substitute a fake).
type commandRunner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// packageManager installs exact package versions. Implementations treat the
// package database as the only source of truth for what is installed.
type packageManager interface {
	Name() string

	// Installed returns every installed version of pkg. known=false means
	// the database could not be consulted: the caller must not decide (an
	// inconclusive answer read as "missing" would reinstall a healthy unit).
	Installed(ctx context.Context, pkg string) (versions []string, known bool)

	// Install installs pkg at version and returns the tool's output.
	Install(ctx context.Context, pkg, version string) (string, error)

	// Refresh refreshes the metadata of the given repositories. Fail-soft:
	// errors are logged and never block the install.
	Refresh(ctx context.Context, repos []string, timeout time.Duration)
}

// detectPackageManager picks dnf, yum or apt from what is on PATH; nil when
// none is (packages are then unsupported on this node).
func detectPackageManager(name string, run commandRunner, logger hclog.Logger) packageManager {
	if name == "" {
		for _, candidate := range []string{"dnf", "yum", "apt-get"} {
			if _, err := exec.LookPath(candidate); err == nil {
				name = strings.TrimSuffix(candidate, "-get")
				break
			}
		}
	}
	switch name {
	case "dnf", "yum":
		return &rpmManager{tool: name, run: run, logger: logger}
	case "apt":
		return &aptManager{run: run, logger: logger}
	default:
		return nil
	}
}

// installMu serialises installs on the node: dnf/apt take a global lock
// anyway, and failing fast on it would turn concurrent task starts into
// spurious task failures.
var installMu sync.Mutex

// rpmManager is dnf or yum over the rpm database.
type rpmManager struct {
	tool   string
	run    commandRunner
	logger hclog.Logger
}

func (m *rpmManager) Name() string { return m.tool }

// Installed queries `rpm -q`. rpm exits 1 both for "not installed" (stderr
// empty) and for a database failure ("error: …" on stderr), so only a
// SILENT exit 1 means absent.
func (m *rpmManager) Installed(ctx context.Context, pkg string) ([]string, bool) {
	ctx, cancel := context.WithTimeout(ctx, rpmQueryTimeout)
	defer cancel()
	stdout, stderr, err := m.run(ctx, "rpm", "-q", "--queryformat", "%{VERSION}\n", "--", pkg)
	if ctx.Err() != nil {
		m.logger.Warn("rpm query timed out; treating as inconclusive", "package", pkg)
		return nil, false
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(bytes.TrimSpace(stderr)) == 0 {
			return nil, true
		}
		if errText := strings.TrimSpace(string(stderr)); errText != "" {
			m.logger.Warn("rpm query failed; treating as inconclusive", "package", pkg, "error", errText)
		} else {
			m.logger.Warn("rpm query failed; treating as inconclusive", "package", pkg, "error", err)
		}
		return nil, false
	}
	versions := splitLines(stdout)
	if len(versions) == 0 {
		return nil, false
	}
	return versions, true
}

func (m *rpmManager) Install(ctx context.Context, pkg, version string) (string, error) {
	stdout, stderr, err := m.run(ctx, m.tool, "-y", "install", "--", pkg+"-"+version)
	out := strings.TrimSpace(string(stdout) + "\n" + string(stderr))
	if err != nil {
		return out, fmt.Errorf("%s install %s-%s: %w", m.tool, pkg, version, err)
	}
	return out, nil
}

// Refresh runs one scoped makecache per repo, never a blanket refresh: dnf
// rejects an unknown repo ID and then refreshes nothing, and an unreachable
// upstream must not fail the install behind it.
func (m *rpmManager) Refresh(ctx context.Context, repos []string, timeout time.Duration) {
	for _, repo := range repos {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		start := time.Now()
		_, stderr, err := m.run(rctx, m.tool, "makecache", "--disablerepo=*", "--enablerepo="+repo)
		cancel()
		if err != nil {
			m.logger.Debug("makecache failed; continuing with cached metadata",
				"repo", repo, "duration", time.Since(start), "error", err, "stderr", strings.TrimSpace(string(stderr)))
			continue
		}
		m.logger.Debug("makecache completed", "repo", repo, "duration", time.Since(start))
	}
}

// aptManager is apt-get over the dpkg database.
type aptManager struct {
	run    commandRunner
	logger hclog.Logger
}

func (m *aptManager) Name() string { return "apt" }

// Installed queries dpkg-query; only an installed ("install ok installed")
// package counts. dpkg-query exits 1 for an unknown package.
func (m *aptManager) Installed(ctx context.Context, pkg string) ([]string, bool) {
	ctx, cancel := context.WithTimeout(ctx, rpmQueryTimeout)
	defer cancel()
	stdout, stderr, err := m.run(ctx, "dpkg-query", "-W", "-f=${Status}\t${Version}\n", "--", pkg)
	if ctx.Err() != nil {
		return nil, false
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, true
		}
		m.logger.Warn("dpkg-query failed; treating as inconclusive", "package", pkg, "error", err,
			"stderr", strings.TrimSpace(string(stderr)))
		return nil, false
	}
	var versions []string
	for _, line := range splitLines(stdout) {
		status, version, ok := strings.Cut(line, "\t")
		if ok && status == "install ok installed" && version != "" {
			versions = append(versions, version)
		}
	}
	return versions, true
}

func (m *aptManager) Install(ctx context.Context, pkg, version string) (string, error) {
	stdout, stderr, err := m.run(ctx, "apt-get", "install", "-y", "--allow-downgrades", "--", pkg+"="+version)
	out := strings.TrimSpace(string(stdout) + "\n" + string(stderr))
	if err != nil {
		return out, fmt.Errorf("apt-get install %s=%s: %w", pkg, version, err)
	}
	return out, nil
}

// Refresh runs `apt-get update` scoped to each repo's sources file when the
// repo names one (repos are sources.list.d file names), else a full update.
func (m *aptManager) Refresh(ctx context.Context, repos []string, timeout time.Duration) {
	for _, repo := range repos {
		rctx, cancel := context.WithTimeout(ctx, timeout)
		_, stderr, err := m.run(rctx, "apt-get", "update",
			"-o", "Dir::Etc::sourcelist=sources.list.d/"+repo,
			"-o", "Dir::Etc::sourceparts=-", "-o", "APT::Get::List-Cleanup=0")
		cancel()
		if err != nil {
			m.logger.Debug("apt-get update failed; continuing with cached metadata",
				"repo", repo, "error", err, "stderr", strings.TrimSpace(string(stderr)))
		}
	}
}

// ensurePackage installs pkg at version unless the package database already
// holds it (duplicates included). It reports whether an install ran. An
// inconclusive database query fails the start (and Nomad's restart policy
// retries) rather than reinstalling on a guess.
func ensurePackage(ctx context.Context, pm packageManager, cfg *Config, pkg, version string, logger hclog.Logger, emit func(string)) (bool, error) {
	if pm == nil {
		return false, errors.New("no supported package manager (dnf, yum, apt) on this node")
	}
	versions, known := pm.Installed(ctx, pkg)
	if !known {
		return false, fmt.Errorf("cannot determine the installed version of %s: package database inconclusive", pkg)
	}
	for _, v := range versions {
		if v == version {
			logger.Debug("package already at wanted version", "package", pkg, "version", version)
			return false, nil
		}
	}

	installMu.Lock()
	defer installMu.Unlock()

	if len(versions) == 0 {
		emit(fmt.Sprintf("Installing package %s %s", pkg, version))
	} else {
		emit(fmt.Sprintf("Upgrading package %s %s → %s", pkg, strings.Join(versions, ","), version))
	}
	ictx, cancel := context.WithTimeout(ctx, cfg.installTimeout())
	defer cancel()
	if len(cfg.MakecacheRepos) > 0 {
		pm.Refresh(ictx, cfg.MakecacheRepos, cfg.makecacheTimeout())
	}
	out, err := pm.Install(ictx, pkg, version)
	if err != nil {
		logger.Warn("package install failed", "package", pkg, "version", version, "output", out)
		return false, err
	}
	logger.Info("package installed", "package", pkg, "version", version, "manager", pm.Name())
	emit(fmt.Sprintf("Installed package %s %s", pkg, version))
	return true, nil
}

func splitLines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if v := strings.TrimSpace(line); v != "" {
			out = append(out, v)
		}
	}
	return out
}
