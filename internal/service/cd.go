package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"gopkg.in/yaml.v3"
)

// DefaultGitInterval is the CD cadence when GIT_INTERVAL is unset, empty,
// malformed or negative.
const DefaultGitInterval = 3 * time.Minute

// cdCloneTimeout caps each clone attempt (the main clone and every per-branch
// values clone).
const cdCloneTimeout = 60 * time.Second

// cdSSHDialTimeout bounds the SSH TCP dial + handshake of a clone, which the
// clone context does not cover (go-git dials on its own background context).
// Keep it below cdCloneTimeout. Applied via timeoutAuth / applyCloneAuth.
// Why: DOCS/CLAUDE.md § Continuous Deployment
const cdSSHDialTimeout = 30 * time.Second

// CDConfig is the continuous-deployment configuration resolved by
// LoadCDConfig. Every field comes from the environment, never config.yml.
// See ADR-0005.
type CDConfig struct {
	GitRepository    string        // GIT_REPOSITORY: SSH URL of the GitOps repo
	GitConfiguration string        // GIT_CONFIGURATION: file path within the repo
	GitInterval      time.Duration // GIT_INTERVAL (default DefaultGitInterval; 0 disables)
	LocalPath        string        // GIT_CONFIGURATION_LOCAL, else DefaultLocalPath

	// DefaultLocalPath is LoadCDConfig's defaultLocalPath, captured even when
	// GIT_CONFIGURATION_LOCAL is set: RunContinuousDeployment reverts LocalPath
	// to it when GIT_CONFIGURATION_LOCAL is cleared at runtime.
	DefaultLocalPath string
}

// cdEnvSeen records, per running CD loop (keyed by *CDConfig identity),
// whether a live non-empty GIT_REPOSITORY / GIT_CONFIGURATION /
// GIT_CONFIGURATION_LOCAL was ever observed, so a later empty read means
// "cleared by the operator". Only RunContinuousDeployment touches it, always
// under syncMu, so it needs no lock of its own.
// Why: DOCS/CLAUDE.md § Continuous Deployment
var cdEnvSeen = map[*CDConfig]struct{ repo, configuration, localPath bool }{}

// LoadCDConfig reads the CD configuration from the environment.
// defaultLocalPath (main.go passes cystemd's resolved config.yml path) is used
// when GIT_CONFIGURATION_LOCAL is unset; GIT_INTERVAL goes through
// resolveGitInterval.
func LoadCDConfig(defaultLocalPath string) *CDConfig {
	cfg := &CDConfig{
		GitRepository:    strings.TrimSpace(os.Getenv("GIT_REPOSITORY")),
		GitConfiguration: strings.TrimSpace(os.Getenv("GIT_CONFIGURATION")),
		LocalPath:        strings.TrimSpace(os.Getenv("GIT_CONFIGURATION_LOCAL")),
		DefaultLocalPath: defaultLocalPath,
	}
	if cfg.LocalPath == "" {
		cfg.LocalPath = defaultLocalPath
	}

	cfg.GitInterval = resolveGitInterval(strings.TrimSpace(os.Getenv("GIT_INTERVAL")))

	Debugf("cd: config loaded: git_repository=%q git_configuration=%q local_path=%q git_interval=%s",
		cfg.GitRepository, cfg.GitConfiguration, cfg.LocalPath, cfg.GitInterval)
	return cfg
}

// resolveGitInterval parses GIT_INTERVAL: empty → DefaultGitInterval;
// malformed or negative → DefaultGitInterval with a Warning; 0 is kept (it
// disables the loop).
func resolveGitInterval(intervalStr string) time.Duration {
	if intervalStr == "" {
		return DefaultGitInterval
	}
	d, err := time.ParseDuration(intervalStr)
	if err != nil {
		Warningf("cd: invalid GIT_INTERVAL %q: %v — using default %s",
			intervalStr, err, DefaultGitInterval)
		return DefaultGitInterval
	}
	if d < 0 {
		Warningf("cd: negative GIT_INTERVAL %s — using default %s",
			d, DefaultGitInterval)
		return DefaultGitInterval
	}
	return d
}

// resolveHostKeyCallback returns the host-key callback for CD clones from
// GIT_SSH_KNOWN_HOSTS. Unset → InsecureIgnoreHostKey plus a Warning (the clone
// still runs). Set → the file is created if missing (ensureKnownHostsFile) and
// verified accept-new (acceptNewHostKeyCallback). Error strings carry no "cd:"
// prefix: callers add their own (it used to appear twice).
func resolveHostKeyCallback() (cryptossh.HostKeyCallback, error) {
	path := strings.TrimSpace(os.Getenv("GIT_SSH_KNOWN_HOSTS"))
	if path == "" {
		Warningf("cd: GIT_SSH_KNOWN_HOSTS is not set — SSH host key verification is DISABLED; " +
			"set it to a known_hosts file path to prevent MITM attacks on the git remote")
		return cryptossh.InsecureIgnoreHostKey(), nil
	}
	if err := ensureKnownHostsFile(path); err != nil {
		return nil, fmt.Errorf("cannot create known_hosts file GIT_SSH_KNOWN_HOSTS=%q: %w", path, err)
	}
	cb, err := acceptNewHostKeyCallback(path)
	if err != nil {
		return nil, fmt.Errorf("cannot load known_hosts from GIT_SSH_KNOWN_HOSTS=%q: %w", path, err)
	}
	// Debug, not Info: reached every CD cycle (+ /sync, /diff) with nothing
	// changed. Pinned by TestResolveHostKeyCallback_SuccessIsNotLoggedAtInfo.
	Debugf("cd: SSH host key verification enabled (known_hosts=%s, accept-new)", path)
	return cb, nil
}

// knownHostsMu makes the accept-new check → append → rebuild atomic across
// every clone in the process: /diff clones run outside syncMu, and the
// callback can fire once per connection attempt.
var knownHostsMu sync.Mutex

// ensureKnownHostsFile creates path (parent dirs 0755, file 0600) when it is
// missing, and tightens an existing file to 0600: a writable known_hosts lets
// another user pin host keys of their choosing.
func ensureKnownHostsFile(path string) error {
	if info, err := os.Stat(path); err == nil {
		// Every resolve, like ensureEnvFileMode: a plain `ssh-keyscan >>`
		// leaves the file 0644 under the usual umask.
		if perm := info.Mode().Perm(); perm != 0o600 {
			if cherr := os.Chmod(path, 0o600); cherr != nil {
				Warningf("cd: cannot tighten known_hosts %q to 0600 (currently %04o): %v — the file may be writable by other users", path, perm, cherr)
			} else {
				Infof("cd: tightened known_hosts %q permissions %04o → 0600", path, perm)
			}
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil // raced with another creator — fine, it exists
		}
		return err
	}
	_ = f.Close()
	Infof("cd: created known_hosts file %s (GIT_SSH_KNOWN_HOSTS pointed at a missing file)", path)
	return nil
}

// acceptNewHostKeyCallback implements OpenSSH StrictHostKeyChecking=accept-new
// against the known_hosts file at path: a known host must present its recorded
// key (a changed key is rejected, never overwritten); an unknown host's key is
// appended (trust-on-first-use, logged at Info), the checker rebuilt, and the
// connection accepted. Failing to record the key rejects the connection.
func acceptNewHostKeyCallback(path string) (cryptossh.HostKeyCallback, error) {
	check, err := knownhosts.New(path)
	if err != nil {
		return nil, err
	}
	return func(hostname string, remote net.Addr, key cryptossh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()

		verr := check(hostname, remote, key)
		if verr == nil {
			return nil
		}
		var kerr *knownhosts.KeyError
		if !errors.As(verr, &kerr) || len(kerr.Want) != 0 {
			// Mismatch (host known with a different key) or any non-KeyError
			// (revoked marker, parse problem): reject.
			return verr
		}

		// Unknown host → trust-on-first-use: record, rebuild, accept.
		line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
		f, ferr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if ferr != nil {
			return fmt.Errorf("cannot record host key for %s in %s: %w", hostname, path, ferr)
		}
		_, werr := fmt.Fprintln(f, line)
		cerr := f.Close()
		if werr != nil {
			return fmt.Errorf("cannot record host key for %s in %s: %w", hostname, path, werr)
		}
		if cerr != nil {
			return fmt.Errorf("cannot record host key for %s in %s: %w", hostname, path, cerr)
		}
		// Rebuild so a same-cycle reconnect sees the new key. A failure is only
		// logged: the key is on disk, the worst case is a duplicate line.
		if fresh, rerr := knownhosts.New(path); rerr == nil {
			check = fresh
		} else {
			Warningf("cd: cannot re-load known_hosts %q after recording %s: %v — a same-cycle reconnect may record a duplicate line", path, hostname, rerr)
		}
		Infof("cd: recorded new SSH host key for %s in %s (accept-new / trust-on-first-use)", hostname, path)
		return nil
	}, nil
}

// loadSSHKey resolves GIT_SSH_KEY_PRIVATE to key bytes. A value containing a
// PEM marker, a newline or a literal `\n` is raw PEM (`\n` escapes are
// unescaped); anything else is a key-file path.
//
// Secret: the value must never reach a log line or HTTP response. Callers
// wrap this error into a Warning or the /sync body, and os.ReadFile's
// *fs.PathError embeds the value, hence the generic error on that branch.
func loadSSHKey(value string) ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("GIT_SSH_KEY_PRIVATE is empty")
	}

	looksRaw := strings.Contains(value, "-----BEGIN") ||
		strings.Contains(value, "\n") ||
		strings.Contains(value, `\n`)

	if looksRaw {
		// .env cannot hold literal newlines, so a one-line key uses `\n`
		// escapes: unescape them unless real newlines are already present.
		if !strings.Contains(value, "\n") {
			value = strings.ReplaceAll(value, `\n`, "\n")
		}
		return []byte(value), nil
	}

	data, err := os.ReadFile(value)
	if err != nil {
		return nil, fmt.Errorf("GIT_SSH_KEY_PRIVATE is neither valid PEM content nor a readable file path")
	}
	return data, nil
}

// timeoutAuth wraps *gitssh.PublicKeys so its ClientConfig carries Timeout,
// bounding the SSH dial. Never swap it for gitssh.NewClient with an
// ssh.ClientConfig{Timeout: …}: go-git's overrideConfig would wipe User, Auth
// and HostKeyCallback.
// Why: DOCS/CLAUDE.md § Continuous Deployment
type timeoutAuth struct {
	*gitssh.PublicKeys
	timeout time.Duration
}

// ClientConfig returns the embedded PublicKeys' client config with the dial
// timeout set; User, Auth and HostKeyCallback are left intact.
func (a *timeoutAuth) ClientConfig() (*cryptossh.ClientConfig, error) {
	cfg, err := a.PublicKeys.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = a.timeout
	return cfg, nil
}

// applyCloneAuth sets opts.Auth to auth bounded by cdSSHDialTimeout; every CD,
// /sync and /diff clone goes through it. A nil auth (file:// or https://
// remotes, as in tests) leaves opts untouched.
func applyCloneAuth(opts *git.CloneOptions, auth *gitssh.PublicKeys) {
	if auth == nil {
		return
	}
	opts.Auth = &timeoutAuth{PublicKeys: auth, timeout: cdSSHDialTimeout}
}

// resolveCloneSSHAuth builds clone auth from GIT_SSH_KEY_PRIVATE (loadSSHKey)
// with host-key verification from GIT_SSH_KNOWN_HOSTS; shared by CD, /sync and
// /diff. Returns (nil, nil) when the key is unset (clone without auth, as for
// file:// remotes) and (nil, err) when the key or known_hosts cannot be
// loaded, in which case the caller logs and skips its work.
func resolveCloneSSHAuth() (*gitssh.PublicKeys, error) {
	keyValue := os.Getenv("GIT_SSH_KEY_PRIVATE")
	if keyValue == "" {
		Debugf("cd: GIT_SSH_KEY_PRIVATE not set — clone will use no auth")
		return nil, nil
	}
	keyBytes, err := loadSSHKey(keyValue)
	if err != nil {
		return nil, fmt.Errorf("cannot load SSH key from GIT_SSH_KEY_PRIVATE: %w", err)
	}
	auth, err := gitssh.NewPublicKeys("git", keyBytes, "")
	if err != nil {
		return nil, fmt.Errorf("invalid SSH key from GIT_SSH_KEY_PRIVATE: %w", err)
	}
	cb, err := resolveHostKeyCallback()
	if err != nil {
		return nil, err
	}
	auth.HostKeyCallback = cb
	Debugf("cd: SSH auth resolved (using GIT_SSH_KEY_PRIVATE)")
	return auth, nil
}

// expandSelector replaces every ${selector} in keyExpr with selector (also
// used to log the resolved key).
func expandSelector(keyExpr, selector string) string {
	return strings.ReplaceAll(keyExpr, "${selector}", selector)
}

// writeFileAtomic replaces destFile with content via destFile.tmp + rename,
// keeping destFile's mode (0644 on first write), and logs
// `cd: wrote N bytes to <destFile>[ (from <fromSuffix>)]` at Info. A stale
// .tmp is removed first and the new one opened O_CREATE|O_EXCL, so a leftover
// file or a planted symlink fails the write instead of being written through.
// Returns false on any failure (logged, .tmp removed, destFile unchanged);
// true lets the caller trigger a restart.
// Why: DOCS/CLAUDE.md § Continuous Deployment
func writeFileAtomic(destFile string, content []byte, fromSuffix string) bool {
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(destFile); statErr == nil {
		mode = info.Mode().Perm()
	}
	tmpFile := destFile + ".tmp"
	_ = os.Remove(tmpFile)
	f, err := os.OpenFile(tmpFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		Warningf("cd: cannot write %s: %v", tmpFile, err)
		_ = os.Remove(tmpFile)
		return false
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		Warningf("cd: cannot write %s: %v", tmpFile, err)
		_ = os.Remove(tmpFile)
		return false
	}
	if err := f.Close(); err != nil {
		Warningf("cd: cannot write %s: %v", tmpFile, err)
		_ = os.Remove(tmpFile)
		return false
	}
	if err := os.Rename(tmpFile, destFile); err != nil {
		Warningf("cd: cannot rename %s → %s: %v", tmpFile, destFile, err)
		_ = os.Remove(tmpFile)
		return false
	}
	if fromSuffix == "" {
		Infof("cd: wrote %d bytes to %s", len(content), destFile)
	} else {
		Infof("cd: wrote %d bytes to %s (from %s)", len(content), destFile, fromSuffix)
	}
	return true
}

// extractYAMLKey resolves keyExpr (a yq-style dot-path; ${selector} expanded)
// in data and returns the sub-document as YAML (2-space indent, sorted keys).
// A plain segment indexes a mapping; "[]" takes a sequence's first element or
// a single-key map's only value (re-parsed when that value is a YAML string:
// the Helm config-blob case) and passes a multi-key map through. A multi-line
// string leaf is returned verbatim. Errors on a missing key, a wrong node type
// or unparseable YAML.
// Why: DOCS/CLAUDE.md § Continuous Deployment
func extractYAMLKey(data []byte, keyExpr, selector string) ([]byte, error) {
	keyExpr = expandSelector(keyExpr, selector)
	keyExpr = strings.TrimPrefix(keyExpr, ".")

	var root interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	current := root
	if keyExpr != "" {
		for _, seg := range strings.Split(keyExpr, ".") {
			if seg == "" {
				continue
			}
			switch seg {
			case "[]":
				switch v := current.(type) {
				case []interface{}:
					if len(v) == 0 {
						return nil, fmt.Errorf("[] on empty sequence")
					}
					current = v[0]
				case map[string]interface{}:
					if len(v) == 1 {
						for _, only := range v {
							current = only
						}
						// Helm `{ config.yml: "<yaml-as-string>" }`: unwrap a
						// string that parses as a map or sequence.
						if s, ok := current.(string); ok {
							var inner interface{}
							if err := yaml.Unmarshal([]byte(s), &inner); err == nil {
								switch inner.(type) {
								case map[string]interface{}, []interface{}:
									current = inner
								}
							}
						}
					}
					// Multi-key map: pass through.
				default:
					return nil, fmt.Errorf("expected sequence or mapping at [] but got %T", current)
				}
			default:
				m, ok := current.(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("expected mapping at %q but got %T", seg, current)
				}
				val, exists := m[seg]
				if !exists {
					return nil, fmt.Errorf("key %q not found in mapping", seg)
				}
				current = val
			}
		}
	}

	// A multi-line string leaf (e.g. an Alloy config) is returned verbatim: the
	// encoder would wrap it in a block scalar the target cannot parse.
	if s, ok := current.(string); ok && strings.Contains(s, "\n") {
		return []byte(s), nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(current); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("yaml marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yaml encoder close: %w", err)
	}
	return buf.Bytes(), nil
}

// RunContinuousDeployment runs one CD cycle under syncMu: clone
// cfg.GitRepository into a temp dir (removed on return), copy
// cfg.GitConfiguration to cfg.LocalPath when its content changed, then, with a
// non-nil appCfg, deploy every managed service's git_values
// (deployManagedServices) and run the self-update check. Fail-soft: every
// failure logs and returns; the HTTP API is never affected. Metrics:
// recordCDCycle once past the skip checks, recordCDClone for the main clone.
func RunContinuousDeployment(cfg *CDConfig, appCfg *Config) {
	if cfg == nil {
		Debugf("cd: cycle skipped — no continuous_deployment config")
		return
	}

	// Serialises the cycle with the secrets-refresh tick and /sync (same files,
	// dnf, systemctl). syncMu is non-reentrant: nothing under it may call back
	// into RunContinuousDeployment.
	syncMu.Lock()
	defer syncMu.Unlock()

	// Re-read the three GIT_* path vars every cycle so an .env edit applies
	// without a restart; the shallow copy is safe while every CDConfig field is
	// a scalar. GitInterval is deliberately not re-read: the ticker is fixed at
	// start. An empty read clears a field only once a live value was seen
	// (cdEnvSeen): GitRepository/GitConfiguration to "" (disabled), LocalPath
	// to DefaultLocalPath, since an empty save path would abort every cycle.
	seen := cdEnvSeen[cfg]
	effective := *cfg
	if v := strings.TrimSpace(os.Getenv("GIT_REPOSITORY")); v != "" {
		effective.GitRepository = v
		seen.repo = true
	} else if seen.repo {
		effective.GitRepository = ""
	}
	if v := strings.TrimSpace(os.Getenv("GIT_CONFIGURATION")); v != "" {
		effective.GitConfiguration = v
		seen.configuration = true
	} else if seen.configuration {
		effective.GitConfiguration = ""
	}
	if v := strings.TrimSpace(os.Getenv("GIT_CONFIGURATION_LOCAL")); v != "" {
		effective.LocalPath = v
		seen.localPath = true
	} else if seen.localPath {
		effective.LocalPath = cfg.DefaultLocalPath
	}
	cdEnvSeen[cfg] = seen
	cfg = &effective

	if cfg.GitRepository == "" {
		Debugf("cd: cycle skipped — GIT_REPOSITORY is empty")
		return
	}
	if cfg.GitConfiguration == "" {
		Debugf("cd: cycle skipped — GIT_CONFIGURATION is empty")
		return
	}
	if cfg.LocalPath == "" {
		Warningf("cd: local save path is empty; cannot persist fetched config")
		return
	}

	Debugf("cd: cycle started for repo=%s file=%s save=%s",
		cfg.GitRepository, cfg.GitConfiguration, cfg.LocalPath)

	// Past the skip checks this is a genuine attempt: the defer records it
	// exactly once on every return path. cycleOK = the main clone+read+write
	// succeeded; per-service outcomes have their own metric families.
	cycleOK := false
	defer func() { recordCDCycle(cycleOK) }()

	auth, err := resolveCloneSSHAuth()
	if err != nil {
		Warningf("cd: %v — skipping cycle", err)
		return
	}

	tmpDir, err := os.MkdirTemp("", "cystemd_cd_*")
	if err != nil {
		Warningf("cd: cannot create temp dir: %v — skipping cycle", err)
		return
	}
	defer os.RemoveAll(tmpDir)

	cloneOpts := &git.CloneOptions{
		URL:          cfg.GitRepository,
		Depth:        1,
		SingleBranch: true,
	}
	applyCloneAuth(cloneOpts, auth)

	ctx, cancel := context.WithTimeout(context.Background(), cdCloneTimeout)
	defer cancel()

	Debugf("cd: cloning %s into %s", cfg.GitRepository, tmpDir)
	if _, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts); err != nil {
		Errorf("cd: clone of %s failed: %v — continuing", cfg.GitRepository, err)
		recordCDClone(false)
		return
	}
	Debugf("cd: clone succeeded")
	recordCDClone(true)

	fileInRepo := filepath.Join(tmpDir, cfg.GitConfiguration)
	content, err := os.ReadFile(fileInRepo)
	if err != nil {
		Warningf("cd: cannot read %q from repo: %v — continuing", cfg.GitConfiguration, err)
		return
	}

	// Rewrite only on change (stable mtime, quiet journal). Either way fall
	// through: the values files live on their own branches.
	if existing, readErr := os.ReadFile(cfg.LocalPath); readErr != nil || !bytes.Equal(existing, content) {
		if !writeFileAtomic(cfg.LocalPath, content, fmt.Sprintf("%s:%s", cfg.GitRepository, cfg.GitConfiguration)) {
			return
		}
	} else {
		Debugf("cd: %s unchanged; not rewritten", cfg.LocalPath)
	}
	cycleOK = true

	if appCfg != nil {
		deployManagedServices(cfg.GitRepository, auth, appCfg.managedEntries(), false)

		// At the cycle tail, so a failed self-update dnf retries next tick.
		RunSelfVersionInstall(appCfg)
	}
}

// deployManagedServices deploys every managed entry's git_values, cloning
// each distinct branch of gitRepo exactly once per call. force overrides the
// per-entry autosync gate (/sync passes true, the CD cycle false). Fail-soft:
// a failed clone skips only its branch, a failed read or extract only that
// service.
func deployManagedServices(gitRepo string, auth *gitssh.PublicKeys, entries []ServiceEntry, force bool) {
	for _, g := range groupEntriesByBranch(entries) {
		deployBranchGroup(gitRepo, g.branch, auth, g.entries, force)
	}
}

// deployBranchGroup clones gitRepo@branch once and runs deployServiceFromTree
// for each entry; a clone failure logs once and skips them all. Only the clone
// scaffolding (cloneBranchTreeOnce) is shared with /diff: the writing
// deployServiceFromTree and the read-only diffServiceFromTree stay separate.
func deployBranchGroup(gitRepo, branch string, auth *gitssh.PublicKeys, entries []ServiceEntry, force bool) {
	cloneBranchTreeOnce(gitRepo, branch, auth, len(entries), "cystemd_vals_*", "cd", recordCDClone,
		func(treeDir string) {
			for _, entry := range entries {
				deployServiceFromTree(treeDir, gitRepo, branch, entry, force)
			}
		})
}

// branchGroup is one git_branch and its entries, from groupEntriesByBranch.
type branchGroup struct {
	branch  string
	entries []ServiceEntry
}

// groupEntriesByBranch groups the entries that set both Service and GitValues
// by effective git_branch (serviceGitBranch, default "master"), in
// first-occurrence order so clone and log order stay deterministic. Shared by
// the CD and /diff paths.
func groupEntriesByBranch(entries []ServiceEntry) []branchGroup {
	var groups []branchGroup
	pos := make(map[string]int)
	for _, e := range entries {
		if e.Service == "" || e.GitValues == "" {
			continue // nothing to deploy/diff for this entry
		}
		b := serviceGitBranch(e)
		if i, ok := pos[b]; ok {
			groups[i].entries = append(groups[i].entries, e)
		} else {
			pos[b] = len(groups)
			groups = append(groups, branchGroup{branch: b, entries: []ServiceEntry{e}})
		}
	}
	return groups
}

// cloneBranchTreeOnce clones gitRepo@branch once into a fresh temp dir (always
// removed before return) and calls withTree only after a successful clone;
// what happens to the tree is entirely the caller's (no dry-run mode here).
// tmpPrefix and logPrefix keep each caller's temp-dir name and log prefix.
// onCloneResult, when non-nil, gets the clone outcome: CD passes
// recordCDClone, /diff passes nil (deliberately uninstrumented).
func cloneBranchTreeOnce(gitRepo, branch string, auth *gitssh.PublicKeys, entryCount int, tmpPrefix, logPrefix string, onCloneResult func(ok bool), withTree func(treeDir string)) {
	tmpDir, err := os.MkdirTemp("", tmpPrefix)
	if err != nil {
		Warningf("%s: cannot create temp dir for values clone: %v — skipping %d service(s) on %s@%s",
			logPrefix, err, entryCount, gitRepo, branch)
		return
	}
	defer os.RemoveAll(tmpDir)

	cloneOpts := &git.CloneOptions{
		URL:           gitRepo,
		Depth:         1,
		SingleBranch:  true,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
	}
	applyCloneAuth(cloneOpts, auth)

	ctx, cancel := context.WithTimeout(context.Background(), cdCloneTimeout)
	defer cancel()

	if _, err := git.PlainCloneContext(ctx, tmpDir, false, cloneOpts); err != nil {
		Errorf("%s: values clone of %s@%s failed: %v — skipping %d managed service(s) on this branch",
			logPrefix, gitRepo, branch, err, entryCount)
		if onCloneResult != nil {
			onCloneResult(false)
		}
		return
	}
	Debugf("%s: cloned %s@%s once for %d managed service(s)", logPrefix, gitRepo, branch, entryCount)
	if onCloneResult != nil {
		onCloneResult(true)
	}

	withTree(tmpDir)
}

// deployServiceFromTree applies one managed service's git_values from an
// already-cloned tree: the git_values_config_key sub-document to
// ${workdir}/${service}/${service_config} (default target.config.yml), else
// the raw file to values.yaml; then the version install, the config-change
// restart and the resource quota. force treats the autosync gate as open
// (/sync). Fail-soft: every error logs and returns without blocking the
// branch's other services. See ADR-0003.
func deployServiceFromTree(treeDir, gitRepo, branch string, entry ServiceEntry, force bool) {
	svcName := entry.Service
	// autosync gates writes, installs and restarts, never the read and compare.
	autosync := serviceAutoSync(entry) || force

	destDir := serviceDir(entry)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		Warningf("cd: cannot create service dir %q: %v — skipping", destDir, err)
		return
	}

	rawContent, err := os.ReadFile(filepath.Join(treeDir, entry.GitValues))
	if err != nil {
		Warningf("cd: cannot read %q from %s@%s: %v — skipping", entry.GitValues, gitRepo, branch, err)
		return
	}

	// Deferred so the quota still applies when config extraction or the write
	// returns early, and runs last on the happy path (set-property is live and
	// survives the restart). Must stay after rawContent is read.
	if entry.GitValuesCPUKey != "" || entry.GitValuesMemoryKey != "" {
		defer runServiceQuotaApply(svcName, &entry, rawContent, destDir, autosync)
	}

	var writeContent []byte
	var destFile string
	if entry.GitValuesConfigKey != "" {
		// ${selector} expands to SelectorKey when set; pool membership uses
		// Selector alone.
		sub := effectiveSelector(entry)
		resolvedKey := expandSelector(entry.GitValuesConfigKey, sub)
		extracted, err := extractYAMLKey(rawContent, entry.GitValuesConfigKey, sub)
		if err != nil {
			Warningf("cd: cannot extract key %q (resolved %q) from %q for %s: %v — skipping",
				entry.GitValuesConfigKey, resolvedKey, entry.GitValues, svcName, err)
			return
		}
		writeContent = extracted
		destFile = filepath.Join(destDir, serviceServiceConfig(entry))
		Debugf("cd: extracted key %q (resolved %q) → %s",
			entry.GitValuesConfigKey, resolvedKey, destFile)
	} else {
		writeContent = rawContent
		destFile = filepath.Join(destDir, "values.yaml")
	}

	// The in-sync path must NOT return: the version check below still runs.
	configChanged := false
	existing, readErr := os.ReadFile(destFile)
	// Recorded for /diff and /lastdiff whether in sync or not, whatever autosync.
	recordConfigDiff(svcName, existing, writeContent)
	switch {
	case readErr == nil && bytes.Equal(existing, writeContent):
		Debugf("cd: %s in sync", destFile)
	case !autosync:
		Infof("cd: %s out-of-date (autosync=false); not rewriting", destFile)
	default:
		if !writeFileAtomic(destFile, writeContent, fmt.Sprintf("%s@%s:%s", gitRepo, branch, entry.GitValues)) {
			return
		}
		configChanged = true
	}

	// ── version install ────────────────────────────────────────────────────
	// true = a restart was already attempted, so the config-change restart
	// below is skipped (one restart covers both).
	versionRestarted := false
	if entry.GitValuesVersionKey != "" {
		versionRestarted = runServiceVersionInstall(svcName, &entry, rawContent, destDir, autosync)
	}

	// ── config-change restart ──────────────────────────────────────────────
	// Only when ${service_config} changed (raw values.yaml is not what the
	// service reads) and the version step attempted no restart; no-resurrect
	// applies, so a stopped unit picks the file up at its next start.
	if configChanged && !versionRestarted && entry.GitValuesConfigKey != "" {
		restartIfRunningForUpdate("cd", svcName, serviceServiceConfig(entry)+" change")
	}
}

// extractVersionString is extractScalarString for git_values_version_key,
// e.g. ".${selector}.global.image.tag" → "abc123def456…".
func extractVersionString(data []byte, keyExpr, selector string) (string, error) {
	return extractScalarString(data, keyExpr, selector)
}

// ─── tracking-file helpers ─────────────────────────────────────────────────

// Tracking files hold the last value cystemd applied, for reporting only:
// never an input to an install or apply decision.
const (
	installedVersionFilename = ".installed_version"
	appliedQuotaFilename     = ".applied_quota"
)

// readTrackingFile returns the trimmed contents of destDir/name, or "" when the
// file is absent or unreadable.
func readTrackingFile(destDir, name string) string {
	data, err := os.ReadFile(filepath.Join(destDir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeTrackingFile writes value to destDir/name via writeFileAtomic, so a
// reader or a crash never sees a torn file. A failure logs a Warning naming
// label and is never propagated: the side-effect it records (dnf install,
// set-property) already succeeded.
func writeTrackingFile(destDir, name, value, label string) {
	path := filepath.Join(destDir, name)
	if !writeFileAtomic(path, []byte(value+"\n"), "") {
		Warningf("cd: cannot write %s %q — continuing", label, path)
	}
}

// rpmVersionsOf is the rpm-query seam; tests swap it to exercise the rpm-truth
// decision on hosts without an rpm database.
var rpmVersionsOf = rpmInstalledVersions

// currentServiceVersion reports what the rpm database says is installed for
// svcName. known=false: rpm could not be consulted; the caller must not decide
// and must never fall back to .installed_version (retry next cycle).
// atWanted: some installed version equals wanted, duplicates included, so no
// reinstall. current: every installed version joined with "," (reporting
// only), "" when not installed. See ADR-0011.
func currentServiceVersion(svcName, wanted string) (current string, atWanted, known bool) {
	versions, ok := rpmVersionsOf(svcName)
	if !ok {
		return "", false, false
	}
	for _, v := range versions {
		if v == wanted {
			return wanted, true, true
		}
	}
	// Not installed: the empty slice joins to "", the "no current version".
	return strings.Join(versions, ","), false, true
}

// readInstalledVersion returns .installed_version's content, or "". Never the
// source of truth: see currentServiceVersion.
func readInstalledVersion(destDir string) string {
	return readTrackingFile(destDir, installedVersionFilename)
}

// writeInstalledVersion persists version to the version-tracking file.
func writeInstalledVersion(destDir, version string) {
	writeTrackingFile(destDir, installedVersionFilename, version, "version file")
}

// runServiceVersionInstall reads the version at entry.GitValuesVersionKey and,
// when rpm says it is not installed and autosync (the entry's gate or /sync's
// force) is set, installs ${service}-${version}, records .installed_version,
// daemon-reloads and try-restarts the unit only if it is running
// (no-resurrect); drift is logged at Info either way. Returns true only when a
// restart was attempted, so the caller skips its config-change restart.
// Fail-soft: every failure (extraction, rpm inconclusive, dnf) logs and
// returns false, retried next cycle. See ADR-0011.
func runServiceVersionInstall(svcName string, entry *ServiceEntry, rawContent []byte, destDir string, autosync bool) bool {
	sub := effectiveSelector(*entry)
	resolvedKey := expandSelector(entry.GitValuesVersionKey, sub)

	version, err := extractVersionString(rawContent, entry.GitValuesVersionKey, sub)
	if err != nil {
		Warningf("cd: cannot extract version key %q (resolved %q) from %q for %s: %v — skipping install",
			entry.GitValuesVersionKey, resolvedKey, entry.GitValues, svcName, err)
		return false
	}

	current, atWanted, known := currentServiceVersion(svcName, version)
	if !known {
		// rpm inconclusive: decline rather than trust .installed_version.
		Warningf("cd: %s version check skipped — rpm database could not be consulted; retrying next cycle", svcName)
		return false
	}
	if atWanted {
		Debugf("cd: %s already at version %s (rpm database); skipping install", svcName, version)
		return false
	}
	// Package absent while the tracking file claims the wanted version: the
	// file only picks the message (something removed the package outside
	// cystemd), never the decision.
	staleTracking := current == "" && readInstalledVersion(destDir) == version
	// Recorded for /diff and /lastdiff whatever autosync says.
	changed := true // reaching here means rpm says the wanted version is not installed
	recordVersionDiff(svcName, current, version, changed)

	if !autosync {
		if staleTracking {
			Infof("cd: %s package not installed but %s claims version %s (autosync=false); not reinstalling",
				svcName, installedVersionFilename, version)
		} else {
			Infof("cd: %s version out-of-date (current=%q wanted=%q, autosync=false); not installing",
				svcName, current, version)
		}
		return false
	}

	if staleTracking {
		Infof("cd: %s %s claims version %s but the package is not installed — reinstalling",
			svcName, installedVersionFilename, version)
	} else if current == "" {
		Infof("cd: %s initial version %s detected; installing RPM", svcName, version)
	} else {
		Infof("cd: %s version change detected: %q → %q; installing RPM", svcName, current, version)
	}

	out, err := InstallServiceVersion(svcName, version)
	if err != nil {
		Warningf("cd: install %s version %s failed: %v — service unchanged",
			svcName, version, err)
		if out != "" {
			Debugf("cd: dnf output:\n%s", out)
		}
		recordCDInstall(false)
		return false
	}
	if out != "" {
		Debugf("cd: dnf install output:\n%s", out)
	}
	recordCDInstall(true)

	// Recorded before the restart, so the breadcrumb holds even if it fails.
	writeInstalledVersion(destDir, version)
	Infof("cd: %s installed version %s", svcName, version)

	// Reload even when no restart follows: the next operator start must use
	// the new unit file. Fail-soft.
	if err := ReloadSystemdDaemon(); err != nil {
		Warningf("cd: daemon-reload after installing %s failed: %v — continuing", svcName, err)
	}

	return restartIfRunningForUpdate("cd", svcName, fmt.Sprintf("version install (%s)", version))
}

// restartIfRunningForUpdate applies no-resurrect to every automated update
// (version install, ${service_config} or .env change): it try-restarts svcName
// only when it is running and leaves a stopped, failed or never-started unit
// alone. try-restart re-checks at job time; the state read serves the logs and
// the return value. Returns true when a restart was attempted (even a failed
// one). logPrefix is the caller's journal prefix ("cd", "vault"); what names
// the update. See ADR-0004.
func restartIfRunningForUpdate(logPrefix, svcName, what string) bool {
	state := ServiceActiveState(svcName)
	if !unitIsRunning(state) {
		Infof("%s: %s is not running (ActiveState=%q); not restarting after %s — a stopped service is never started by an automated update",
			logPrefix, svcName, state, what)
		return false
	}
	Infof("%s: restarting %s after %s", logPrefix, svcName, what)
	if _, err := TryRestartService(svcName); err != nil {
		Warningf("%s: restart %s after %s failed: %v", logPrefix, svcName, what, err)
		recordCDRestart(false)
		return true
	}
	Infof("%s: %s restarted after %s", logPrefix, svcName, what)
	recordCDRestart(true)
	return true
}

// ─── resource-quota helpers ────────────────────────────────────────────────

// maxQuotaScalar rejects CPU-millicore / memory-byte values that would overflow
// int64 (below math.MaxInt64 ≈ 9.22e18, with margin for float rounding). It is
// checked before any int64(float) conversion, whose out-of-range result is
// platform-dependent.
const maxQuotaScalar = 9.0e18

// extractScalarString resolves keyExpr (yq-style dot-path, ${selector}
// expanded) to a scalar leaf and returns its raw text, trimmed; used for the
// version and quota keys. It reads node.Value, never a decode + re-encode, so
// an unquoted "1.10" stays "1.10" (a decode yields 1.1: the wrong
// ${service}-1.1 package). Errors on a missing key, bad YAML, or a non-scalar
// or empty leaf.
func extractScalarString(data []byte, keyExpr, selector string) (string, error) {
	resolved := expandSelector(keyExpr, selector)
	path := strings.TrimPrefix(resolved, ".")

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("yaml unmarshal: %w", err)
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return "", fmt.Errorf("key %q: empty yaml document", resolved)
		}
		node = node.Content[0]
	}

	if path != "" {
		for _, seg := range strings.Split(path, ".") {
			if seg == "" {
				continue
			}
			next, err := scalarNavSegment(node, seg)
			if err != nil {
				return "", fmt.Errorf("key %q: %w", resolved, err)
			}
			node = next
		}
	}

	// The leaf may itself be an alias, and a zero-segment path never passes
	// through scalarNavSegment, so resolve here too.
	node = resolveYAMLAlias(node)
	if node == nil {
		return "", fmt.Errorf("key %q: unresolvable YAML alias", resolved)
	}
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("key %q did not resolve to a scalar", resolved)
	}
	v := strings.TrimSpace(node.Value)
	if v == "" {
		return "", fmt.Errorf("key %q resolved to empty value", resolved)
	}
	return v, nil
}

// scalarNavSegment advances one dot-path segment toward a scalar leaf, with
// extractYAMLKey's "[]" semantics minus the Helm string re-parse. Anchors and
// merge keys are resolved by hand (resolveYAMLAlias, mappingValue), never via
// an interface{} decode, which would re-type "1.10" as 1.1.
func scalarNavSegment(node *yaml.Node, seg string) (*yaml.Node, error) {
	node = resolveYAMLAlias(node)
	if node == nil {
		return nil, fmt.Errorf("unresolvable YAML alias at %q", seg)
	}
	if seg == "[]" {
		switch node.Kind {
		case yaml.SequenceNode:
			if len(node.Content) == 0 {
				return nil, fmt.Errorf("[] on empty sequence")
			}
			return node.Content[0], nil
		case yaml.MappingNode:
			if len(node.Content) == 2 { // single key/value pair → its value
				return node.Content[1], nil
			}
			return node, nil // multi-key map → pass through
		default:
			return nil, fmt.Errorf("expected sequence or mapping at [] but got node kind %d", node.Kind)
		}
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping at %q but got node kind %d", seg, node.Kind)
	}
	if v, ok := mappingValue(node, seg, 0); ok {
		return v, nil
	}
	return nil, fmt.Errorf("key %q not found in mapping", seg)
}

// mergeKeyTag is yaml.v3's tag on a `<<` merge key; matching the tag, not the
// text, leaves a quoted "<<" an ordinary key.
const mergeKeyTag = "!!merge"

// maxYAMLAliasDepth bounds alias chains and merge recursion, so a hand-crafted
// document pulled from git cannot hang a CD cycle. Exceeding it is an error,
// never "absent": a missing version key would quietly skip the install.
const maxYAMLAliasDepth = 32

// resolveYAMLAlias follows an alias chain to its anchored node, or returns nil
// when the chain is broken or deeper than maxYAMLAliasDepth; a non-alias node
// comes back unchanged. It only returns existing nodes, which keeps scalar
// text verbatim.
func resolveYAMLAlias(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode; depth++ {
		if depth >= maxYAMLAliasDepth {
			return nil
		}
		node = node.Alias
	}
	return node
}

// mappingValue looks up seg in a mapping node with YAML merge-key precedence,
// as yaml.v3's decode applies it: explicit keys first, then `<<` sources in
// document order (an earlier entry of `<<: [*a, *b]` wins), recursing into
// merged mappings. It must keep agreeing with extractYAMLKey, which reads the
// same values.yaml.
func mappingValue(node *yaml.Node, seg string, depth int) (*yaml.Node, bool) {
	if node == nil || node.Kind != yaml.MappingNode || depth >= maxYAMLAliasDepth {
		return nil, false
	}
	// Explicit keys first — they outrank every merge source.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Tag != mergeKeyTag && node.Content[i].Value == seg {
			return node.Content[i+1], true
		}
	}
	// Then merged mappings, in order.
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Tag != mergeKeyTag {
			continue
		}
		for _, src := range mergeSources(node.Content[i+1]) {
			if v, ok := mappingValue(resolveYAMLAlias(src), seg, depth+1); ok {
				return v, true
			}
		}
	}
	return nil, false
}

// mergeSources lists the mappings a `<<` value merges, in order: one for an
// alias, each element for a sequence (the caller alias-resolves them).
func mergeSources(v *yaml.Node) []*yaml.Node {
	r := resolveYAMLAlias(v)
	if r == nil {
		return nil
	}
	if r.Kind == yaml.SequenceNode {
		return r.Content
	}
	return []*yaml.Node{r}
}

// k8sCPUToCPUQuota converts a Kubernetes CPU quantity to a systemd CPUQuota
// percentage (100% = one core):
//
//	"200m" → "20%"    "2" → "200%"    "0.5" → "50%"    "1500m" → "150%"
//	"250m" → "25%"    "1m" → "0.1%"
//
// It rounds to whole millicores and emits at most one decimal, which systemd
// stores exactly: a second decimal would re-apply the cap every cycle.
// Fractional percentages need systemd >= 240. Rejects non-positive,
// non-finite and overflowing input. See ADR-0016.
func k8sCPUToCPUQuota(cpu string) (string, error) {
	s := strings.TrimSpace(cpu)
	if s == "" {
		return "", fmt.Errorf("empty cpu value")
	}
	var milliF float64
	if rest, ok := strings.CutSuffix(s, "m"); ok {
		f, err := strconv.ParseFloat(strings.TrimSpace(rest), 64)
		if err != nil {
			return "", fmt.Errorf("invalid cpu millicores %q: %w", s, err)
		}
		milliF = f
	} else {
		cores, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return "", fmt.Errorf("invalid cpu cores %q: %w", s, err)
		}
		milliF = cores * 1000
	}
	if math.IsNaN(milliF) || math.IsInf(milliF, 0) {
		return "", fmt.Errorf("cpu %q is not a finite number", s)
	}
	milliF = math.Round(milliF)
	if milliF <= 0 {
		return "", fmt.Errorf("cpu %q resolves to a non-positive quota", s)
	}
	if milliF >= maxQuotaScalar {
		return "", fmt.Errorf("cpu %q exceeds the maximum supported quota", s)
	}
	milli := int64(milliF)
	whole, tenths := milli/10, milli%10
	if tenths == 0 {
		return fmt.Sprintf("%d%%", whole), nil
	}
	return fmt.Sprintf("%d.%d%%", whole, tenths), nil
}

// k8sMemoryToBytes converts a Kubernetes memory quantity to a MemoryMax byte
// count: Ki…Ei are powers of 1024, k/K…E powers of 1000, no suffix is bytes.
//
//	"1Gi" → "1073741824"   "512Mi" → "536870912"   "512M" → "512000000"
//	"1073741824" → "1073741824"
//
// The result is rounded; non-positive, non-finite and overflowing input is
// rejected before the int64 conversion.
func k8sMemoryToBytes(mem string) (string, error) {
	s := strings.TrimSpace(mem)
	if s == "" {
		return "", fmt.Errorf("empty memory value")
	}
	// Longest suffix first so "Mi" matches before "M".
	suffixes := []struct {
		suf  string
		mult float64
	}{
		{"Ei", 1 << 60}, {"Pi", 1 << 50}, {"Ti", 1 << 40},
		{"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10},
		{"E", 1e18}, {"P", 1e15}, {"T", 1e12}, {"G", 1e9}, {"M", 1e6}, {"k", 1e3}, {"K", 1e3},
	}
	mant := s
	mult := 1.0
	for _, u := range suffixes {
		if rest, ok := strings.CutSuffix(s, u.suf); ok {
			mant = rest
			mult = u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(mant), 64)
	if err != nil {
		return "", fmt.Errorf("invalid memory %q: %w", s, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("memory %q is not a finite number", s)
	}
	scaled := f * mult
	if scaled <= 0 {
		return "", fmt.Errorf("memory %q resolves to a non-positive byte count", s)
	}
	if scaled >= maxQuotaScalar {
		return "", fmt.Errorf("memory %q exceeds the maximum supported byte count", s)
	}
	return strconv.FormatInt(int64(math.Round(scaled)), 10), nil
}

// readAppliedQuota returns .applied_quota's content, or "". Reporting only:
// the live unit decides (currentServiceQuota).
func readAppliedQuota(destDir string) string {
	return readTrackingFile(destDir, appliedQuotaFilename)
}

// writeAppliedQuota persists quota to the quota-tracking file.
func writeAppliedQuota(destDir, quota string) {
	writeTrackingFile(destDir, appliedQuotaFilename, quota, "quota file")
}

// runServiceQuotaApply converts entry's cpu/memory keys to CPUQuota/MemoryMax
// and, when they differ from the live unit and autosync is set, applies them
// with `systemctl set-property` (persistent drop-in, live, no restart), then
// records .applied_quota. Each property resolves independently; a failed one
// is skipped with a Warning. Unreadable live state skips the cycle, and a
// set-property failure retries next cycle. Cumulative: a removed or
// unresolvable key never clears a live cap. See ADR-0016.
func runServiceQuotaApply(svcName string, entry *ServiceEntry, rawContent []byte, destDir string, autosync bool) {
	sub := effectiveSelector(*entry)

	// One row per managed property, resolved independently; a new property
	// (e.g. TasksMax) is a new row.
	managed := []struct {
		keyExpr string
		label   string
		prop    string
		convert func(string) (string, error)
	}{
		{entry.GitValuesCPUKey, "cpu", "CPUQuota", k8sCPUToCPUQuota},
		{entry.GitValuesMemoryKey, "memory", "MemoryMax", k8sMemoryToBytes},
	}

	var props []string
	for _, m := range managed {
		if m.keyExpr == "" {
			continue
		}
		resolvedKey := expandSelector(m.keyExpr, sub)
		raw, err := extractScalarString(rawContent, m.keyExpr, sub)
		if err != nil {
			Warningf("cd: cannot extract %s key %q (resolved %q) from %q for %s: %v — skipping %s quota",
				m.label, m.keyExpr, resolvedKey, entry.GitValues, svcName, err, m.label)
			continue
		}
		converted, err := m.convert(raw)
		if err != nil {
			Warningf("cd: invalid %s limit %q (from %q) for %s: %v — skipping %s quota",
				m.label, raw, resolvedKey, svcName, err, m.label)
			continue
		}
		props = append(props, m.prop+"="+converted)
	}

	if len(props) == 0 {
		return
	}

	desired := strings.Join(props, " ")

	// The live unit decides, never .applied_quota.
	current, atWanted, known := currentServiceQuota(svcName, props)
	if !known {
		Debugf("cd: %s quota check skipped — the live unit state could not be read; retrying next cycle", svcName)
		return
	}
	if atWanted {
		Debugf("cd: %s resource quota already applied (%s)", svcName, desired)
		return
	}

	// The file agrees but the live unit does not: the cap was cleared out of
	// band. Say so; nothing else would.
	if readAppliedQuota(destDir) == desired {
		Infof("cd: %s %s claims quota %s but the live unit reports %q — the cap was cleared out of band",
			svcName, appliedQuotaFilename, desired, current)
	}

	if !autosync {
		Infof("cd: %s resource quota out-of-date (current=%q wanted=%q, autosync=false); not applying",
			svcName, current, desired)
		return
	}

	if quotaAllUnset(current) {
		Infof("cd: %s applying resource quota: %s", svcName, desired)
	} else {
		Infof("cd: %s resource quota change detected: %q → %q; applying", svcName, current, desired)
	}

	out, err := SetServiceResourceQuota(svcName, props)
	if err != nil {
		Warningf("cd: set resource quota for %s failed: %v — quota unchanged", svcName, err)
		if out != "" {
			Debugf("cd: set-property output:\n%s", out)
		}
		return
	}
	if out != "" {
		Debugf("cd: set-property output:\n%s", out)
	}

	// Recorded only after a successful set-property (reporting only).
	writeAppliedQuota(destDir, desired)
	Infof("cd: %s resource quota applied (%s) — set-property is live, no restart needed", svcName, desired)
}

// StartContinuousDeployment runs RunContinuousDeployment in a goroutine, once
// immediately and then every cfg.GitInterval until ctx is done; it never
// blocks the caller. A nil cfg, an empty GitRepository/GitConfiguration/
// LocalPath or a non-positive interval makes it a no-op (logged at Info).
// appCfg may be nil (no per-service deployment).
func StartContinuousDeployment(ctx context.Context, cfg *CDConfig, appCfg *Config) {
	if cfg == nil {
		Infof("cd: continuous deployment disabled — no config")
		return
	}
	if cfg.GitRepository == "" {
		Infof("cd: continuous deployment disabled — GIT_REPOSITORY is empty")
		return
	}
	if cfg.GitConfiguration == "" {
		Infof("cd: continuous deployment disabled — GIT_CONFIGURATION is empty")
		return
	}
	if cfg.LocalPath == "" {
		Infof("cd: continuous deployment disabled — local save path is empty")
		return
	}
	if cfg.GitInterval <= 0 {
		Infof("cd: continuous deployment disabled — GIT_INTERVAL=%s", cfg.GitInterval)
		return
	}

	Infof("cd: continuous deployment loop started (interval=%s, repo=%s, file=%s, save=%s)",
		cfg.GitInterval, cfg.GitRepository, cfg.GitConfiguration, cfg.LocalPath)

	go func() {
		// Both calls go through safeCycle: a panic skips one cycle and the
		// next tick retries.
		safeCycle("cd", func() { RunContinuousDeployment(cfg, appCfg) })

		ticker := time.NewTicker(cfg.GitInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				Infof("cd: continuous deployment loop stopping (context cancelled)")
				return
			case <-ticker.C:
				safeCycle("cd", func() { RunContinuousDeployment(cfg, appCfg) })
			}
		}
	}()
}

// RunSelfVersionInstall installs cystemd-<cfg.Version> and restarts cystemd
// when cfg.Version (read under cfg.mu) matches neither the running Version()
// nor Commit(); matching Commit() is what lets untagged per-commit RPMs
// converge. No-op for a nil cfg, an empty cfg.Version or a dev/unknown build;
// otherwise removeDuplicateRPMPackages always runs first. Fail-soft: a failure
// logs a Warning and retries on the next call. The restart is abrupt (no
// SIGTERM handler): never call this from an HTTP handler.
// Why: DOCS/CLAUDE.md § Self-update
func RunSelfVersionInstall(cfg *Config) {
	if cfg == nil {
		Debugf("cd: cystemd self-install skipped — no config")
		return
	}
	cfg.mu.RLock()
	wanted := strings.TrimSpace(cfg.Version)
	cfg.mu.RUnlock()

	if wanted == "" {
		Debugf("cd: cystemd self-install skipped — config.version not set")
		return
	}
	current := Version()
	if isDevVersion(current) {
		Debugf("cd: cystemd self-install skipped — running build has no injected version (Version()=%q); refusing to auto-install over a dev binary", current)
		return
	}

	// Every call, so hosts converge after an interrupted self-update; a no-op
	// with a single cystemd RPM installed.
	removeDuplicateRPMPackages()

	runningCommit := Commit()
	if current == wanted || runningCommit == wanted {
		Debugf("cd: cystemd already at version %s (commit %s); skipping self-install", current, runningCommit)
		return
	}

	Infof("cd: cystemd version change detected: %q → %q; installing RPM", current, wanted)
	out, err := installSelfVersion(wanted)
	if err != nil {
		Warningf("cd: install cystemd version %s failed: %v — staying on %s", wanted, err, current)
		if out != "" {
			Debugf("cd: dnf output:\n%s", out)
		}
		// Must be counted: a failed self-update has no other symptom (the old
		// binary keeps running and reporting healthy).
		recordCDInstall(false)
		return
	}
	if out != "" {
		Debugf("cd: dnf install output:\n%s", out)
	}
	recordCDInstall(true)
	Infof("cd: cystemd installed version %s; restarting cystemd to load the new binary", wanted)
	if _, err := RestartService("cystemd"); err != nil {
		Warningf("cd: restart cystemd after self-install failed: %v — binary updated but old process still running until next operator restart", err)
		return
	}
	// Normally unreachable: systemd kills this process during RestartService.
	Infof("cd: cystemd restart issued; running binary will be replaced")
}
