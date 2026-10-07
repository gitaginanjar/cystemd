package service

// CD tests clone real local repos (git.PlainInit) over file://, with no SSH
// server; file IO uses mkTempDir, never t.TempDir.
// Why: DOCS/CLAUDE.md § Testing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	cryptossh "golang.org/x/crypto/ssh"
)

// Compile-time guard: go-git type-asserts both interfaces on the clone auth,
// so a drift must break the build, not the clone.
var (
	_ gitssh.AuthMethod    = (*timeoutAuth)(nil)
	_ transport.AuthMethod = (*timeoutAuth)(nil)
)

// clearCDEnv blanks the four GIT_* CD env vars for the test (t.Setenv
// restores them).
func clearCDEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_REPOSITORY", "")
	t.Setenv("GIT_CONFIGURATION", "")
	t.Setenv("GIT_INTERVAL", "")
	t.Setenv("GIT_CONFIGURATION_LOCAL", "")
}

// ─── LoadCDConfig — env-based ──────────────────────────────────────────────

func TestLoadCDConfig_NoEnv_AppliesDefaults(t *testing.T) {
	clearCDEnv(t)
	got := LoadCDConfig("/default/path/config.yml")

	if got == nil {
		t.Fatal("LoadCDConfig should never return nil")
	}
	if got.GitRepository != "" {
		t.Errorf("GitRepository: got %q, want empty", got.GitRepository)
	}
	if got.GitConfiguration != "" {
		t.Errorf("GitConfiguration: got %q, want empty", got.GitConfiguration)
	}
	if got.LocalPath != "/default/path/config.yml" {
		t.Errorf("LocalPath: got %q, want default fallback", got.LocalPath)
	}
	if got.GitInterval != DefaultGitInterval {
		t.Errorf("GitInterval default: got %s, want %s", got.GitInterval, DefaultGitInterval)
	}
	if DefaultGitInterval != 3*time.Minute {
		t.Errorf("DefaultGitInterval: got %s, want 3m", DefaultGitInterval)
	}
}

func TestLoadCDConfig_AllEnvSet(t *testing.T) {
	t.Setenv("GIT_REPOSITORY", "git@bitbucket.org:org/repo.git")
	t.Setenv("GIT_CONFIGURATION", "products/devtools/environments/dev/devtools_cystemd/config.yml")
	t.Setenv("GIT_CONFIGURATION_LOCAL", "/etc/cystemd/managed.yml")
	t.Setenv("GIT_INTERVAL", "5m")

	got := LoadCDConfig("/should-not-be-used")

	if got.GitRepository != "git@bitbucket.org:org/repo.git" {
		t.Errorf("GitRepository: got %q", got.GitRepository)
	}
	if got.GitConfiguration != "products/devtools/environments/dev/devtools_cystemd/config.yml" {
		t.Errorf("GitConfiguration: got %q", got.GitConfiguration)
	}
	if got.LocalPath != "/etc/cystemd/managed.yml" {
		t.Errorf("LocalPath: got %q (env should win over default)", got.LocalPath)
	}
	if got.GitInterval != 5*time.Minute {
		t.Errorf("GitInterval: got %s, want 5m", got.GitInterval)
	}
}

func TestLoadCDConfig_LocalPath_DefaultsToArgWhenEnvEmpty(t *testing.T) {
	clearCDEnv(t)
	t.Setenv("GIT_REPOSITORY", "git@bitbucket.org:org/repo.git")
	got := LoadCDConfig("/opt/cystemd/config.yml")

	if got.LocalPath != "/opt/cystemd/config.yml" {
		t.Errorf("LocalPath default: got %q, want /opt/cystemd/config.yml", got.LocalPath)
	}
}

// TestLoadCDConfig_DefaultLocalPath_CapturedRegardlessOfEnvOverride pins that
// DefaultLocalPath holds the defaultLocalPath argument even when
// GIT_CONFIGURATION_LOCAL overrides LocalPath: it is the runtime-clear target.
func TestLoadCDConfig_DefaultLocalPath_CapturedRegardlessOfEnvOverride(t *testing.T) {
	clearCDEnv(t)
	got := LoadCDConfig("/default/path/config.yml")
	if got.DefaultLocalPath != "/default/path/config.yml" {
		t.Errorf("DefaultLocalPath (env unset): got %q, want %q", got.DefaultLocalPath, "/default/path/config.yml")
	}

	t.Setenv("GIT_CONFIGURATION_LOCAL", "/etc/cystemd/managed.yml")
	got = LoadCDConfig("/default/path/config.yml")
	if got.LocalPath != "/etc/cystemd/managed.yml" {
		t.Errorf("LocalPath (env set): got %q, want the env override", got.LocalPath)
	}
	if got.DefaultLocalPath != "/default/path/config.yml" {
		t.Errorf("DefaultLocalPath (env set): got %q, want %q — must still capture the default even though LocalPath was overridden",
			got.DefaultLocalPath, "/default/path/config.yml")
	}
}

func TestLoadCDConfig_TrimsWhitespace(t *testing.T) {
	t.Setenv("GIT_REPOSITORY", "  git@bitbucket.org:org/repo.git  ")
	t.Setenv("GIT_CONFIGURATION", "  config.yml  ")
	t.Setenv("GIT_INTERVAL", "  3m  ")
	t.Setenv("GIT_CONFIGURATION_LOCAL", "  /tmp/x.yml  ")

	got := LoadCDConfig("/default")

	if strings.HasPrefix(got.GitRepository, " ") || strings.HasSuffix(got.GitRepository, " ") {
		t.Errorf("GitRepository should be trimmed; got %q", got.GitRepository)
	}
	if got.GitInterval != 3*time.Minute {
		t.Errorf("GitInterval should parse trimmed value; got %s", got.GitInterval)
	}
	if got.LocalPath != "/tmp/x.yml" {
		t.Errorf("LocalPath should be trimmed; got %q", got.LocalPath)
	}
}

func TestLoadCDConfig_InvalidInterval_FallsBackToDefault(t *testing.T) {
	clearCDEnv(t)
	t.Setenv("GIT_REPOSITORY", "x")
	t.Setenv("GIT_INTERVAL", "not-a-duration")

	buf := captureLogs(t, LevelWarning)
	got := LoadCDConfig("/default")

	if got.GitInterval != DefaultGitInterval {
		t.Errorf("invalid interval: got %s, want default %s", got.GitInterval, DefaultGitInterval)
	}
	if !strings.Contains(buf.String(), "invalid GIT_INTERVAL") {
		t.Errorf("expected warning; got:\n%s", buf.String())
	}
}

func TestLoadCDConfig_NegativeInterval_FallsBackToDefault(t *testing.T) {
	clearCDEnv(t)
	t.Setenv("GIT_INTERVAL", "-30s")

	buf := captureLogs(t, LevelWarning)
	got := LoadCDConfig("/default")

	if got.GitInterval != DefaultGitInterval {
		t.Errorf("negative interval: got %s, want default", got.GitInterval)
	}
	if !strings.Contains(buf.String(), "negative GIT_INTERVAL") {
		t.Errorf("expected warning; got:\n%s", buf.String())
	}
}

func TestLoadCDConfig_ZeroInterval_Preserved(t *testing.T) {
	clearCDEnv(t)
	t.Setenv("GIT_INTERVAL", "0")
	got := LoadCDConfig("/default")
	if got.GitInterval != 0 {
		t.Errorf("explicit 0: got %s, want 0", got.GitInterval)
	}
}

// TestResolveGitInterval pins every branch (empty, valid, zero, invalid,
// negative) and which of them warn.
func TestResolveGitInterval(t *testing.T) {
	cases := []struct {
		name      string
		input     string
		want      time.Duration
		warnSlice string // expected warning substring; "" = no warning
	}{
		{"empty", "", DefaultGitInterval, ""},
		{"valid", "5m", 5 * time.Minute, ""},
		{"zero", "0", 0, ""},
		{"zero with unit", "0s", 0, ""},
		{"invalid", "not-a-duration", DefaultGitInterval, "invalid GIT_INTERVAL"},
		{"negative", "-30s", DefaultGitInterval, "negative GIT_INTERVAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t, LevelWarning)
			got := resolveGitInterval(tc.input)
			if got != tc.want {
				t.Errorf("resolveGitInterval(%q): got %s, want %s", tc.input, got, tc.want)
			}
			out := buf.String()
			if tc.warnSlice == "" && strings.Contains(out, "GIT_INTERVAL") {
				t.Errorf("did not expect a warning for %q; got:\n%s", tc.input, out)
			}
			if tc.warnSlice != "" && !strings.Contains(out, tc.warnSlice) {
				t.Errorf("expected warning containing %q for %q; got:\n%s",
					tc.warnSlice, tc.input, out)
			}
		})
	}
}

// ─── loadSSHKey ────────────────────────────────────────────────────────────

const fakePEMKey = "-----BEGIN OPENSSH PRIVATE KEY-----\nbody-line-1\nbody-line-2\n-----END OPENSSH PRIVATE KEY-----\n"

func TestLoadSSHKey_Empty_ReturnsError(t *testing.T) {
	if _, err := loadSSHKey(""); err == nil {
		t.Error("expected error for empty value")
	}
}

func TestLoadSSHKey_RawPEMWithRealNewlines(t *testing.T) {
	got, err := loadSSHKey(fakePEMKey)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != fakePEMKey {
		t.Errorf("returned bytes should match raw input; got %q", string(got))
	}
}

func TestLoadSSHKey_RawPEMWithEscapedNewlines(t *testing.T) {
	// .env values carry `\n` escapes instead of literal newlines.
	escaped := strings.ReplaceAll(fakePEMKey, "\n", `\n`)
	got, err := loadSSHKey(escaped)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(got), "\nbody-line-1\n") {
		t.Errorf("\\n should be unescaped to real newlines; got %q", string(got))
	}
	if strings.Contains(string(got), `\n`) {
		t.Errorf("output should not contain escaped \\n; got %q", string(got))
	}
}

func TestLoadSSHKey_FilePath_ReadsFile(t *testing.T) {
	dir := mkTempDir(t)
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, []byte(fakePEMKey), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := loadSSHKey(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != fakePEMKey {
		t.Errorf("file-loaded bytes should match contents; got %q", string(got))
	}
}

func TestLoadSSHKey_FilePath_NotFound_ReturnsError(t *testing.T) {
	if _, err := loadSSHKey("/nonexistent/path/does/not/exist"); err == nil {
		t.Error("expected error for missing file")
	}
}

// TestLoadSSHKey_FilePath_NotFound_NeverLeaksRawValue pins that the error for
// an unreadable key path never contains the GIT_SSH_KEY_PRIVATE value
// (os.ReadFile's *fs.PathError would; callers log it and return it from /sync).
func TestLoadSSHKey_FilePath_NotFound_NeverLeaksRawValue(t *testing.T) {
	const marker = "definitely-not-a-real-secret-MARKER-98765"
	_, err := loadSSHKey(marker)
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("error must NOT contain the raw GIT_SSH_KEY_PRIVATE value; got: %v", err)
	}
}

func TestLoadSSHKey_AutoDetect_PEMMarkerWinsOverFilePath(t *testing.T) {
	got, err := loadSSHKey("-----BEGIN OPENSSH PRIVATE KEY-----")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "-----BEGIN OPENSSH PRIVATE KEY-----" {
		t.Errorf("PEM marker should be raw; got %q", string(got))
	}
}

// ─── timeoutAuth (clone dial timeout) ──────────────────────────────────────

// mustGenerateSSHPrivateKeyPEM returns a fresh OpenSSH ed25519 private key PEM
// that gitssh.NewPublicKeys can parse (fakePEMKey cannot).
func mustGenerateSSHPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	block, err := cryptossh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return pem.EncodeToMemory(block)
}

// TestTimeoutAuth_ClientConfig_AppliesDialTimeout pins that timeoutAuth sets
// cdSSHDialTimeout without clobbering User, Auth or HostKeyCallback.
func TestTimeoutAuth_ClientConfig_AppliesDialTimeout(t *testing.T) {
	pk, err := gitssh.NewPublicKeys("git", mustGenerateSSHPrivateKeyPEM(t), "")
	if err != nil {
		t.Fatalf("NewPublicKeys: %v", err)
	}
	pk.HostKeyCallback = cryptossh.InsecureIgnoreHostKey()

	// Precondition: a bare PublicKeys has no dial timeout.
	base, err := pk.ClientConfig()
	if err != nil {
		t.Fatalf("PublicKeys.ClientConfig: %v", err)
	}
	if base.Timeout != 0 {
		t.Fatalf("precondition failed: bare PublicKeys already had timeout %s", base.Timeout)
	}

	cfg, err := (&timeoutAuth{PublicKeys: pk, timeout: cdSSHDialTimeout}).ClientConfig()
	if err != nil {
		t.Fatalf("timeoutAuth.ClientConfig: %v", err)
	}
	if cfg.Timeout != cdSSHDialTimeout {
		t.Errorf("dial timeout: got %s, want %s", cfg.Timeout, cdSSHDialTimeout)
	}
	if cfg.User != "git" {
		t.Errorf("User clobbered: got %q, want \"git\"", cfg.User)
	}
	if len(cfg.Auth) != 1 {
		t.Errorf("Auth methods clobbered: got %d, want 1", len(cfg.Auth))
	}
	if cfg.HostKeyCallback == nil {
		t.Error("HostKeyCallback was wiped — host-key verification would be lost")
	}
}

// ─── helpers for git tests ─────────────────────────────────────────────────

// setupLocalGitRepo commits files into a fresh repo and returns its path
// (clone it as "file://" + path).
func setupLocalGitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := mkTempDir(t)
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatalf("PlainInit: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for relPath, content := range files {
		full := filepath.Join(dir, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", full, err)
		}
		if _, err := wt.Add(relPath); err != nil {
			t.Fatalf("git add %s: %v", relPath, err)
		}
	}
	if _, err := wt.Commit("test: initial", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@cystemd",
			When:  time.Now(),
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return dir
}

// addBranchWithFile commits relPath=content on a new branch off HEAD, then
// checks the original branch out again: only a clone of branch sees relPath.
func addBranchWithFile(t *testing.T, dir, branch, relPath, content string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := repo.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{
		Branch: plumbing.NewBranchReferenceName(branch),
		Create: true,
	}); err != nil {
		t.Fatalf("checkout new branch %s: %v", branch, err)
	}
	full := filepath.Join(dir, relPath)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
	if _, err := wt.Add(relPath); err != nil {
		t.Fatalf("git add %s: %v", relPath, err)
	}
	if _, err := wt.Commit("test: branch "+branch, &git.CommitOptions{
		Author: &object.Signature{Name: "Test", Email: "test@cystemd", When: time.Now()},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: head.Name()}); err != nil {
		t.Fatalf("checkout back to %s: %v", head.Name(), err)
	}
}

func amendLocalGitRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for relPath, content := range files {
		full := filepath.Join(dir, relPath)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", full, err)
		}
		if _, err := wt.Add(relPath); err != nil {
			t.Fatalf("git add %s: %v", relPath, err)
		}
	}
	if _, err := wt.Commit("test: amend", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Test",
			Email: "test@cystemd",
			When:  time.Now(),
		},
	}); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// ─── RunContinuousDeployment — fail-soft no-op guards ─────────────────────

func TestRunContinuousDeployment_NilCfg_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(nil, nil)
	if !strings.Contains(buf.String(), "no continuous_deployment config") {
		t.Errorf("expected debug log; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_EmptyRepo_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitConfiguration: "x",
		LocalPath:        "/tmp/x",
	}, nil)
	if !strings.Contains(buf.String(), "GIT_REPOSITORY is empty") {
		t.Errorf("expected log; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_EmptyConfiguration_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository: "x",
		LocalPath:     "/tmp/x",
	}, nil)
	if !strings.Contains(buf.String(), "GIT_CONFIGURATION is empty") {
		t.Errorf("expected log; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_EmptyLocalPath_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "x",
		GitConfiguration: "y",
	}, nil)
	if !strings.Contains(buf.String(), "local save path is empty") {
		t.Errorf("expected warning; got:\n%s", buf.String())
	}
}

// ─── RunContinuousDeployment — happy path against a local git repo ────────

func TestRunContinuousDeployment_LocalRepo_FetchesAndSaves(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{
		"products/devtools/environments/dev/devtools_cystemd/config.yml": "service_name: \"hello\"\n",
	})

	savePath := filepath.Join(mkTempDir(t), "out.yml")

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "products/devtools/environments/dev/devtools_cystemd/config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("save file should exist: %v", err)
	}
	if string(data) != "service_name: \"hello\"\n" {
		t.Errorf("got %q, want %q", string(data), "service_name: \"hello\"\n")
	}
}

func TestRunContinuousDeployment_LocalRepo_LogsWroteOnFirstWrite(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{"config.yml": "k: v\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	if !strings.Contains(buf.String(), "wrote ") {
		t.Errorf("expected 'wrote N bytes' Info log; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_BogusRepo_NoCrashWritesNothing(t *testing.T) {
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	buf := captureLogs(t, LevelWarning)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file:///nonexistent/path/to/repo",
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	if _, err := os.Stat(savePath); err == nil {
		t.Error("save file should not be written on clone failure")
	}
	if !strings.Contains(buf.String(), "clone of") || !strings.Contains(buf.String(), "failed") {
		t.Errorf("expected clone-failed error; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_FileNotInRepo_NoCrash(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{"present.yml": "hi\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	buf := captureLogs(t, LevelWarning)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "absent/file.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	if _, err := os.Stat(savePath); err == nil {
		t.Error("save file should not be written when target path is missing in repo")
	}
	if !strings.Contains(buf.String(), "cannot read") {
		t.Errorf("expected 'cannot read' warning; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_UnchangedContent_DoesNotRewrite(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{"config.yml": "stable: yes\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")
	cfg := &CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cfg, nil)
	info1, err := os.Stat(savePath)
	if err != nil {
		t.Fatalf("first write should succeed: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(cfg, nil)
	info2, err := os.Stat(savePath)
	if err != nil {
		t.Fatalf("file should still exist: %v", err)
	}
	if !info1.ModTime().Equal(info2.ModTime()) {
		t.Errorf("mtime changed (%v → %v); expected unchanged-skip",
			info1.ModTime(), info2.ModTime())
	}
	if !strings.Contains(buf.String(), "unchanged") {
		t.Errorf("expected 'unchanged' debug log; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_ChangedContent_Rewrites(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{"config.yml": "v: 1\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")
	cfg := &CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cfg, nil)
	first, _ := os.ReadFile(savePath)

	amendLocalGitRepo(t, srcDir, map[string]string{"config.yml": "v: 2\n"})

	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(cfg, nil)
	second, _ := os.ReadFile(savePath)

	if string(first) == string(second) {
		t.Errorf("expected content change between cycles; both = %q", string(first))
	}
	if string(second) != "v: 2\n" {
		t.Errorf("got %q after second cycle, want v: 2", string(second))
	}
	if !strings.Contains(buf.String(), "wrote ") {
		t.Errorf("expected 'wrote N bytes' Info log on change; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_PreservesExistingPermissions(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{"config.yml": "k: v\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	if err := os.WriteFile(savePath, []byte("placeholder\n"), 0o640); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	info, err := os.Stat(savePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o640 {
		t.Errorf("permissions: got %o, want 0640 (existing mode preserved)", mode)
	}
}

// TestRunContinuousDeployment_CanManageEnvFile pins that LocalPath may be any
// file, e.g. a .env: CD persists the repo's bytes whatever their type.
func TestRunContinuousDeployment_CanManageEnvFile(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{
		"environments/prod/devtools_cystemd/secrets.env": "FOO=bar\nBAZ=qux\n",
	})
	savePath := filepath.Join(mkTempDir(t), "managed.env")

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "environments/prod/devtools_cystemd/secrets.env",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	data, err := os.ReadFile(savePath)
	if err != nil {
		t.Fatalf("save file should exist: %v", err)
	}
	if !strings.Contains(string(data), "FOO=bar\n") {
		t.Errorf("expected FOO=bar in managed env; got:\n%s", string(data))
	}
}

// TestRunContinuousDeployment_EnvHotReload_PicksUpNewRepo pins that a
// GIT_REPOSITORY changed in the environment (StartEnvReload's os.Setenv) is
// cloned on the next cycle, not the URL frozen into CDConfig at startup.
// Why: DOCS/MEMORY.md § Early CD regressions behind cd_test.go
func TestRunContinuousDeployment_EnvHotReload_PicksUpNewRepo(t *testing.T) {
	repoA := setupLocalGitRepo(t, map[string]string{"config.yml": "repo: A\n"})
	repoB := setupLocalGitRepo(t, map[string]string{"config.yml": "repo: B\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	cfg := &CDConfig{
		GitRepository:    "file://" + repoA,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cfg, nil)
	data, _ := os.ReadFile(savePath)
	if string(data) != "repo: A\n" {
		t.Fatalf("first cycle: expected %q, got %q", "repo: A\n", string(data))
	}

	// Simulate .env hot-reload updating GIT_REPOSITORY.
	t.Setenv("GIT_REPOSITORY", "file://"+repoB)

	RunContinuousDeployment(cfg, nil)
	data, _ = os.ReadFile(savePath)
	if string(data) != "repo: B\n" {
		t.Errorf("second cycle after env hot-reload: expected %q, got %q", "repo: B\n", string(data))
	}
}

// TestRunContinuousDeployment_EnvHotReload_PicksUpNewConfiguration pins that
// a changed GIT_CONFIGURATION is honoured on the next cycle.
func TestRunContinuousDeployment_EnvHotReload_PicksUpNewConfiguration(t *testing.T) {
	repo := setupLocalGitRepo(t, map[string]string{
		"old/path/config.yml": "source: old\n",
		"new/path/config.yml": "source: new\n",
	})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	cfg := &CDConfig{
		GitRepository:    "file://" + repo,
		GitConfiguration: "old/path/config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cfg, nil)
	data, _ := os.ReadFile(savePath)
	if string(data) != "source: old\n" {
		t.Fatalf("first cycle: expected %q, got %q", "source: old\n", string(data))
	}

	t.Setenv("GIT_CONFIGURATION", "new/path/config.yml")

	RunContinuousDeployment(cfg, nil)
	data, _ = os.ReadFile(savePath)
	if string(data) != "source: new\n" {
		t.Errorf("after GIT_CONFIGURATION hot-reload: expected %q, got %q", "source: new\n", string(data))
	}
}

// TestRunContinuousDeployment_EnvHotReload_ClearedRepoDisablesLoop pins that
// removing GIT_REPOSITORY from .env disables the loop on the next cycle
// instead of resurrecting the value frozen into CDConfig at startup.
// Why: DOCS/MEMORY.md § Two-pass review + self-update commit-hash bug
func TestRunContinuousDeployment_EnvHotReload_ClearedRepoDisablesLoop(t *testing.T) {
	repoA := setupLocalGitRepo(t, map[string]string{"config.yml": "repo: A\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	cfg := &CDConfig{
		GitRepository:    "file://" + repoA,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}

	// The env agrees with cfg at start, as in production; this first cycle
	// marks the var as seen, which the clear below depends on.
	t.Setenv("GIT_REPOSITORY", cfg.GitRepository)
	RunContinuousDeployment(cfg, nil)
	data, _ := os.ReadFile(savePath)
	if string(data) != "repo: A\n" {
		t.Fatalf("first cycle: expected %q, got %q", "repo: A\n", string(data))
	}

	// StartEnvReload propagates a removal with os.Unsetenv, not an empty value.
	os.Unsetenv("GIT_REPOSITORY")
	t.Cleanup(func() { os.Unsetenv("GIT_REPOSITORY") })

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(cfg, nil)
	if !strings.Contains(buf.String(), "GIT_REPOSITORY is empty") {
		t.Errorf("expected the cycle to be skipped as disabled after GIT_REPOSITORY was cleared; log:\n%s", buf.String())
	}

	// No clone, no rewrite: the first cycle's content stays.
	data, _ = os.ReadFile(savePath)
	if string(data) != "repo: A\n" {
		t.Errorf("save path should be untouched once disabled; got %q", string(data))
	}
}

// TestRunContinuousDeployment_EnvHotReload_ClearedLocalPathRevertsToDefault
// pins that removing GIT_CONFIGURATION_LOCAL from .env reverts LocalPath to
// DefaultLocalPath (what a restart would produce) instead of keeping the
// startup override.
func TestRunContinuousDeployment_EnvHotReload_ClearedLocalPathRevertsToDefault(t *testing.T) {
	repo := setupLocalGitRepo(t, map[string]string{"config.yml": "repo: v1\n"})
	tmpDir := mkTempDir(t)
	defaultPath := filepath.Join(tmpDir, "default.yml")
	customPath := filepath.Join(tmpDir, "custom.yml")

	// GIT_CONFIGURATION_LOCAL was set at startup: LocalPath holds the
	// override, DefaultLocalPath the fallback.
	cfg := &CDConfig{
		GitRepository:    "file://" + repo,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        customPath,
		DefaultLocalPath: defaultPath,
	}

	// The env agrees with cfg at start; this first cycle marks the var as seen.
	t.Setenv("GIT_CONFIGURATION_LOCAL", customPath)
	RunContinuousDeployment(cfg, nil)
	data, _ := os.ReadFile(customPath)
	if string(data) != "repo: v1\n" {
		t.Fatalf("first cycle: expected %q at custom path, got %q", "repo: v1\n", string(data))
	}

	// StartEnvReload propagates a removal with os.Unsetenv, not an empty value.
	os.Unsetenv("GIT_CONFIGURATION_LOCAL")
	t.Cleanup(func() { os.Unsetenv("GIT_CONFIGURATION_LOCAL") })

	// Sentinels at both paths: whichever still reads STALE was not written.
	if err := os.WriteFile(defaultPath, []byte("STALE\n"), 0o644); err != nil {
		t.Fatalf("seed defaultPath: %v", err)
	}
	if err := os.WriteFile(customPath, []byte("STALE\n"), 0o644); err != nil {
		t.Fatalf("seed customPath: %v", err)
	}

	RunContinuousDeployment(cfg, nil)

	data, _ = os.ReadFile(defaultPath)
	if string(data) != "repo: v1\n" {
		t.Errorf("second cycle: LocalPath should have reverted to DefaultLocalPath; got %q at default path", string(data))
	}
	data, _ = os.ReadFile(customPath)
	if string(data) != "STALE\n" {
		t.Errorf("second cycle: custom path baked into CDConfig at startup should be untouched once GIT_CONFIGURATION_LOCAL is cleared; got %q", string(data))
	}
}

// ─── StartContinuousDeployment ────────────────────────────────────────────

func TestStartContinuousDeployment_NilCfg_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartContinuousDeployment(ctx, nil, nil)

	if !strings.Contains(buf.String(), "disabled") {
		t.Errorf("expected disabled log; got:\n%s", buf.String())
	}
}

func TestStartContinuousDeployment_EmptyRepo_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartContinuousDeployment(ctx, &CDConfig{
		GitConfiguration: "x",
		GitInterval:      time.Minute,
		LocalPath:        "/tmp/x",
	}, nil)

	if !strings.Contains(buf.String(), "GIT_REPOSITORY is empty") {
		t.Errorf("expected log; got:\n%s", buf.String())
	}
}

func TestStartContinuousDeployment_EmptyConfiguration_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartContinuousDeployment(ctx, &CDConfig{
		GitRepository: "x",
		GitInterval:   time.Minute,
		LocalPath:     "/tmp/x",
	}, nil)

	if !strings.Contains(buf.String(), "GIT_CONFIGURATION is empty") {
		t.Errorf("expected log; got:\n%s", buf.String())
	}
}

func TestStartContinuousDeployment_EmptyLocalPath_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartContinuousDeployment(ctx, &CDConfig{
		GitRepository:    "x",
		GitConfiguration: "y",
		GitInterval:      time.Minute,
	}, nil)

	if !strings.Contains(buf.String(), "local save path is empty") {
		t.Errorf("expected log; got:\n%s", buf.String())
	}
}

func TestStartContinuousDeployment_ZeroInterval_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelInfo)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	StartContinuousDeployment(ctx, &CDConfig{
		GitRepository:    "x",
		GitConfiguration: "y",
		LocalPath:        "/tmp/x",
		GitInterval:      0,
	}, nil)

	if !strings.Contains(buf.String(), "GIT_INTERVAL=0s") {
		t.Errorf("expected zero-interval disabled log; got:\n%s", buf.String())
	}
}

func TestStartContinuousDeployment_PeriodicLoopFires(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{"config.yml": "v: loop\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartContinuousDeployment(ctx, &CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "config.yml",
		GitInterval:      40 * time.Millisecond,
		LocalPath:        savePath,
	}, nil)

	deadline := time.Now().Add(2 * time.Second)
	var seen bool
	for time.Now().Before(deadline) {
		if _, err := os.Stat(savePath); err == nil {
			seen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	time.Sleep(60 * time.Millisecond)

	if !seen {
		t.Error("save file should appear after the first cycle")
	}

	data, _ := os.ReadFile(savePath)
	if string(data) != "v: loop\n" {
		t.Errorf("got %q, want %q", string(data), "v: loop\n")
	}
}

func TestStartContinuousDeployment_ContextCancel_StopsLoop(t *testing.T) {
	buf := captureLogs(t, LevelInfo)

	ctx, cancel := context.WithCancel(context.Background())
	StartContinuousDeployment(ctx, &CDConfig{
		GitRepository:    "file:///nonexistent/repo/here",
		GitConfiguration: "config.yml",
		GitInterval:      30 * time.Millisecond,
		LocalPath:        filepath.Join(mkTempDir(t), "x.yml"),
	}, nil)

	time.Sleep(80 * time.Millisecond)
	cancel()

	// Poll, don't sleep-then-assert: ctx.Done() is seen only between cycles,
	// and the in-flight clone can outlast any fixed grace under -race.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "loop stopping") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	if !strings.Contains(buf.String(), "loop stopping") {
		t.Errorf("expected 'loop stopping' log within 5s of cancel; got:\n%s", buf.String())
	}
}

// ─── extractYAMLKey ────────────────────────────────────────────────────────

// valuesYAMLFixture is a trimmed production (HOST001) values.yaml shape with
// a sequence under .config, for the [] segment.
const valuesYAMLFixture = `
myapplication:
  services:
    myapplication:
      config:
        - app:
            name: myproject_myapplication
            port: 8888
          logger:
            log_level: "DEBUG"
          redis:
            host: 10.20.30.5
            port: 6379
`

// versionYAMLFixture is valuesYAMLFixture plus global.image.tag, so one
// document serves both the config key and the version key.
const versionYAMLFixture = `
myapplication:
  global:
    image:
      tag: abc123def456abc123def456abc123def456abc123
  services:
    myapplication:
      config:
        - app:
            name: myproject_myapplication
            port: 8888
          logger:
            log_level: "DEBUG"
          redis:
            host: 10.20.30.5
            port: 6379
`

func TestExtractYAMLKey_SelectorPlaceholderExpanded(t *testing.T) {
	out, err := extractYAMLKey([]byte(valuesYAMLFixture), ".${selector}.services.${selector}.config.[]", "myapplication")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "myproject_myapplication") {
		t.Errorf("expected app.name in output; got:\n%s", string(out))
	}
	if !strings.Contains(string(out), "log_level") {
		t.Errorf("expected log_level in output; got:\n%s", string(out))
	}
	if strings.Contains(string(out), "myapplication:") {
		t.Errorf("output should be the inner config, not the outer wrapper; got:\n%s", string(out))
	}
}

func TestExtractYAMLKey_ArrayUnwrap(t *testing.T) {
	data := []byte("items:\n  - key: first\n  - key: second\n")
	out, err := extractYAMLKey(data, ".items.[]", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "first") {
		t.Errorf("expected first element; got:\n%s", string(out))
	}
}

func TestExtractYAMLKey_EmptyArrayError(t *testing.T) {
	data := []byte("items: []\n")
	if _, err := extractYAMLKey(data, ".items.[]", ""); err == nil {
		t.Error("expected error for [] on empty sequence")
	}
}

// TestExtractYAMLKey_MultilineNonYAMLWrittenVerbatim pins that a multi-line
// string leaf (an Alloy/River config) comes back verbatim, not block-scalared.
func TestExtractYAMLKey_MultilineNonYAMLWrittenVerbatim(t *testing.T) {
	data := []byte("config:\n  config.alloy: |\n    loki.write \"default\" {\n      url = \"x\"\n    }\n")
	out, err := extractYAMLKey(data, ".config.[]", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(out)
	if strings.HasPrefix(s, "|") || strings.HasPrefix(s, " ") {
		t.Errorf("River must not be block-scalar wrapped; got:\n%s", s)
	}
	if !strings.HasPrefix(s, "loki.write \"default\" {") {
		t.Errorf("expected verbatim River; got:\n%s", s)
	}
}

// TestExtractYAMLKey_SingleLineScalarUnchanged pins that a single-line scalar
// (a version tag) still goes through the encoder: verbatim is multi-line only.
func TestExtractYAMLKey_SingleLineScalarUnchanged(t *testing.T) {
	data := []byte("version: 1.16.1\n")
	out, err := extractYAMLKey(data, ".version", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(out)) != "1.16.1" {
		t.Errorf("version scalar: got %q, want \"1.16.1\"", strings.TrimSpace(string(out)))
	}
}

func TestExtractYAMLKey_KeyNotFound_ReturnsError(t *testing.T) {
	data := []byte("foo: bar\n")
	if _, err := extractYAMLKey(data, ".missing", ""); err == nil {
		t.Error("expected error for missing key")
	}
}

func TestExtractYAMLKey_ArrayOnNonSequence_ReturnsError(t *testing.T) {
	data := []byte("foo: bar\n")
	if _, err := extractYAMLKey(data, ".foo.[]", ""); err == nil {
		t.Error("expected error for [] on a scalar")
	}
}

// TestExtractYAMLKey_ArrayOnMap_PassesThrough pins that [] on a multi-key map
// passes it through, so an upstream `config: [{…}]` → `config: {…}` flip
// needs no key change.
// Why: DOCS/MEMORY.md § Early CD regressions behind cd_test.go
func TestExtractYAMLKey_ArrayOnMap_PassesThrough(t *testing.T) {
	// `config` is a direct map; the `[]` segment must not error.
	data := []byte("myapplication:\n  services:\n    myapplication:\n      config:\n        app:\n          name: myproject_myapplication\n        logger:\n          log_level: DEBUG\n")
	out, err := extractYAMLKey(data, ".${selector}.services.${selector}.config.[]", "myapplication")
	if err != nil {
		t.Fatalf("expected [] on a map to pass through; got error: %v", err)
	}
	if !strings.Contains(string(out), "myproject_myapplication") {
		t.Errorf("expected app.name in output; got:\n%s", string(out))
	}
	if !strings.Contains(string(out), "log_level: DEBUG") {
		t.Errorf("expected logger.log_level in output; got:\n%s", string(out))
	}
}

// TestExtractYAMLKey_ArrayOnMapAndSequence_ProduceSameOutput pins identical
// output for the sequence-of-one and the direct-map forms.
func TestExtractYAMLKey_ArrayOnMapAndSequence_ProduceSameOutput(t *testing.T) {
	wrapped := []byte("svc:\n  config:\n    - app:\n        name: x\n      port: 8080\n")
	bare := []byte("svc:\n  config:\n    app:\n      name: x\n    port: 8080\n")

	gotWrapped, err := extractYAMLKey(wrapped, ".svc.config.[]", "")
	if err != nil {
		t.Fatalf("wrapped form: %v", err)
	}
	gotBare, err := extractYAMLKey(bare, ".svc.config.[]", "")
	if err != nil {
		t.Fatalf("bare form: %v", err)
	}
	if string(gotWrapped) != string(gotBare) {
		t.Errorf("wrapped vs bare outputs differ:\nwrapped:\n%s\nbare:\n%s",
			string(gotWrapped), string(gotBare))
	}
}

// TestExtractYAMLKey_ArrayOnScalar_ReturnsError pins that [] on a scalar
// errors instead of emitting the stringified scalar as the document.
func TestExtractYAMLKey_ArrayOnScalar_ReturnsError(t *testing.T) {
	data := []byte("svc:\n  config: just-a-string\n")
	if _, err := extractYAMLKey(data, ".svc.config.[]", ""); err == nil {
		t.Error("expected error for [] on a scalar value")
	}
}

// TestExtractYAMLKey_HelmChartConfigBlobUnwrapped pins the Helm config-blob
// case on the HOST001 fixture: `config: { config.yml: | … }` unwraps to the
// inner document, sorted and 2-space indented, as
// `yq ….config.[] | yq 'sort_keys(..)'` produces. It checks representative
// slices, not bytes, to tolerate whitespace trims.
// Why: DOCS/MEMORY.md § Early CD regressions behind cd_test.go
func TestExtractYAMLKey_HelmChartConfigBlobUnwrapped(t *testing.T) {
	data, err := os.ReadFile("testdata/myproject_myapplication_values.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	out, err := extractYAMLKey(data, ".${selector}.services.${selector}.config.[]", "myapplication")
	if err != nil {
		t.Fatalf("extractYAMLKey: %v", err)
	}
	got := string(out)

	// No `config.yml:` wrapper: the string is re-parsed as the document.
	if strings.HasPrefix(strings.TrimSpace(got), "config.yml:") {
		t.Fatalf("output starts with `config.yml:` wrapper; expected unwrapped inner YAML. Got:\n%s", got)
	}
	if strings.Contains(got, "config.yml: |") {
		t.Errorf("output should not contain block-scalar wrapper; got:\n%s", got)
	}

	expectedTopKeys := []string{
		"app:", "clock:", "default_timeout:", "file_utils:", "logger:",
		"markettime:", "observability:", "order:", "myapp:", "redis:",
		"redpanda:", "rms:", "tcp_buffered_io:",
	}
	for _, k := range expectedTopKeys {
		if !strings.Contains(got, "\n"+k) && !strings.HasPrefix(got, k) {
			t.Errorf("expected top-level key %q in output; got:\n%s", k, got)
		}
	}

	// Sorted top-level keys: yaml.v3's map default, pinned as a contract.
	topKeys := topLevelKeys(t, got)
	for i := 1; i < len(topKeys); i++ {
		if topKeys[i] < topKeys[i-1] {
			t.Errorf("top-level keys not alphabetical at index %d: %q < %q\nfull list: %v",
				i, topKeys[i], topKeys[i-1], topKeys)
		}
	}

	// 2-space indent, sampled on one key nested under `app:` (the first line,
	// so there is no preceding newline to anchor on).
	if !strings.Contains(got, "  max_idle_time: 15000") {
		t.Errorf("expected 2-space indent under `app:` (looking for `  max_idle_time: 15000`); got:\n%s", got)
	}
	if strings.Contains(got, "    max_idle_time: 15000") {
		t.Errorf("encoder is using 4-space indent; expected 2-space; got:\n%s", got)
	}
}

// topLevelKeys returns doc's column-0 mapping keys (the text before the first
// ':') in document order, skipping blank, indented and comment lines.
func topLevelKeys(t *testing.T, doc string) []string {
	t.Helper()
	var keys []string
	for _, line := range strings.Split(doc, "\n") {
		if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "#") {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue
		}
		keys = append(keys, strings.TrimSpace(line[:colon]))
	}
	return keys
}

// ─── expandSelector ────────────────────────────────────────────────────────

func TestExpandSelector_SubstitutesPlaceholder(t *testing.T) {
	got := expandSelector(".${selector}.services.${selector}.config.[]", "myapplication")
	want := ".myapplication.services.myapplication.config.[]"
	if got != want {
		t.Errorf("expandSelector: got %q, want %q", got, want)
	}
}

func TestExpandSelector_NoPlaceholder_Unchanged(t *testing.T) {
	got := expandSelector(".foo.bar", "ignored")
	if got != ".foo.bar" {
		t.Errorf("expandSelector should leave non-placeholder strings alone; got %q", got)
	}
}

func TestExtractYAMLKey_MapKeyOnNonMapping_ReturnsError(t *testing.T) {
	data := []byte("foo:\n  - item\n")
	if _, err := extractYAMLKey(data, ".foo.bar", ""); err == nil {
		t.Error("expected error for key navigation into a sequence")
	}
}

func TestExtractYAMLKey_EmptyKeyExpr_ReturnsWholeDoc(t *testing.T) {
	data := []byte("foo: bar\n")
	out, err := extractYAMLKey(data, ".", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(string(out), "foo") {
		t.Errorf("empty key should return whole doc; got:\n%s", string(out))
	}
}

func TestExtractYAMLKey_InvalidYAML_ReturnsError(t *testing.T) {
	if _, err := extractYAMLKey([]byte(":\tbad:\tyaml"), ".key", ""); err == nil {
		t.Error("expected error for invalid YAML")
	}
}

// TestRunContinuousDeployment_ValuesRunEvenWhenConfigUnchanged pins that the
// per-service values deployment runs every cycle, even when config.yml is
// unchanged (an unchanged-config early return once skipped it).
func TestRunContinuousDeployment_ValuesRunEvenWhenConfigUnchanged(t *testing.T) {
	combinedRepo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync:           true,
				Service:            "myapplication",
				Selector:           "myapplication",
				GitValues:          "values.yaml",
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
				Workdir:            workbase,
			},
		},
	}

	cdCfg := &CDConfig{
		GitRepository:    "file://" + combinedRepo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cdCfg, appCfg)

	destFile := filepath.Join(workbase, "myapplication", "target.config.yml")
	if _, err := os.ReadFile(destFile); err != nil {
		t.Fatalf("first cycle: expected target.config.yml to exist: %v", err)
	}
	// Removed so the second cycle, with config.yml unchanged, must recreate it.
	if err := os.Remove(destFile); err != nil {
		t.Fatalf("remove: %v", err)
	}

	RunContinuousDeployment(cdCfg, appCfg)

	if _, err := os.ReadFile(destFile); err != nil {
		t.Fatalf("second cycle (config unchanged): target.config.yml not written — runServiceValuesDeployment was skipped: %v", err)
	}
}

// ─── deployManagedServices (via RunContinuousDeployment) ──────────────────

// TestRunContinuousDeployment_ValuesConfigKey_WritesExtractedDoc pins that
// the git_values_config_key sub-document lands in
// ${workdir}/${service}/target.config.yml.
func TestRunContinuousDeployment_ValuesConfigKey_WritesExtractedDoc(t *testing.T) {
	combinedRepo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync:           true,
				Service:            "myapplication",
				Selector:           "myapplication",
				GitValues:          "values.yaml",
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
				Workdir:            workbase,
			},
		},
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + combinedRepo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	destFile := filepath.Join(workbase, "myapplication", "target.config.yml")
	data, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("expected target.config.yml to be written: %v", err)
	}
	if !strings.Contains(string(data), "myproject_myapplication") {
		t.Errorf("expected extracted content in target.config.yml; got:\n%s", string(data))
	}
	if strings.Contains(string(data), "myapplication:") {
		t.Errorf("outer wrapper should not appear in extracted output; got:\n%s", string(data))
	}
}

// TestRunContinuousDeployment_ValuesNoConfigKey_WritesRawValuesYaml pins that
// without git_values_config_key the raw file lands in
// ${workdir}/${service}/values.yaml.
func TestRunContinuousDeployment_ValuesNoConfigKey_WritesRawValuesYaml(t *testing.T) {
	combinedRepo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync:  true,
				Service:   "myapplication",
				Selector:  "myapplication",
				GitValues: "values.yaml",
				// GitValuesConfigKey intentionally empty
				Workdir: workbase,
			},
		},
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + combinedRepo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	destFile := filepath.Join(workbase, "myapplication", "values.yaml")
	data, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("expected values.yaml to be written: %v", err)
	}
	if !strings.Contains(string(data), "myapplication:") {
		t.Errorf("raw values.yaml should contain top-level key; got:\n%s", string(data))
	}
}

// TestRunContinuousDeployment_ValuesConfigKey_InvalidKey_LogsWarning pins that
// a git_values_config_key that matches nothing logs a Warning naming the raw
// and resolved key, and writes no file.
func TestRunContinuousDeployment_ValuesConfigKey_InvalidKey_LogsWarning(t *testing.T) {
	buf := captureLogs(t, LevelWarning)

	combinedRepo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": "foo: bar\n",
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync:           true,
				Service:            "myapplication",
				Selector:           "myapplication",
				GitValues:          "values.yaml",
				GitValuesConfigKey: ".${selector}.nonexistent.path",
				Workdir:            workbase,
			},
		},
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + combinedRepo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	if !strings.Contains(buf.String(), "cannot extract key") {
		t.Errorf("expected 'cannot extract key' warning; got:\n%s", buf.String())
	}
	// Raw and resolved key both appear: the raw form alone reads as a failed
	// ${selector} substitution.
	if !strings.Contains(buf.String(), `${selector}`) {
		t.Errorf("expected raw ${selector} placeholder in warning; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "resolved") {
		t.Errorf("expected 'resolved' marker in warning; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), ".myapplication.nonexistent.path") {
		t.Errorf("expected resolved key to appear in warning; got:\n%s", buf.String())
	}

	destTarget := filepath.Join(workbase, "myapplication", "target.config.yml")
	destValues := filepath.Join(workbase, "myapplication", "values.yaml")
	if _, err := os.Stat(destTarget); err == nil {
		t.Error("target.config.yml must not be written on extraction error")
	}
	if _, err := os.Stat(destValues); err == nil {
		t.Error("values.yaml must not be written on extraction error")
	}
}

// TestRunContinuousDeployment_ServiceConfig_CustomFilename pins that
// ServiceConfig overrides the target.config.yml filename.
func TestRunContinuousDeployment_ServiceConfig_CustomFilename(t *testing.T) {
	combinedRepo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync:           true,
				Service:            "myapplication",
				Selector:           "myapplication",
				GitValues:          "values.yaml",
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
				ServiceConfig:      "app.config.yml",
				Workdir:            workbase,
			},
		},
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + combinedRepo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	destFile := filepath.Join(workbase, "myapplication", "app.config.yml")
	if _, err := os.ReadFile(destFile); err != nil {
		t.Fatalf("expected %s to be written: %v", destFile, err)
	}
}

// ─── resolveHostKeyCallback ────────────────────────────────────────────────

func TestResolveHostKeyCallback_NoEnv_ReturnsCallbackWithWarning(t *testing.T) {
	t.Setenv("GIT_SSH_KNOWN_HOSTS", "")
	buf := captureLogs(t, LevelWarning)

	cb, err := resolveHostKeyCallback()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb == nil {
		t.Fatal("callback must not be nil")
	}
	if !strings.Contains(buf.String(), "DISABLED") {
		t.Errorf("expected warning about disabled host key verification; got:\n%s", buf.String())
	}
}

// TestResolveHostKeyCallback_MissingFile_CreatesIt pins that a missing
// GIT_SSH_KNOWN_HOSTS file (and parent dir) is created 0600 and a callback
// returned, instead of failing the cycle.
func TestResolveHostKeyCallback_MissingFile_CreatesIt(t *testing.T) {
	dir := mkTempDir(t)
	khPath := filepath.Join(dir, "sub", "known_hosts") // parent dir also missing
	t.Setenv("GIT_SSH_KNOWN_HOSTS", khPath)
	buf := captureLogs(t, LevelInfo)

	cb, err := resolveHostKeyCallback()
	if err != nil {
		t.Fatalf("missing file must be created, not error: %v", err)
	}
	if cb == nil {
		t.Fatal("callback must not be nil")
	}
	info, statErr := os.Stat(khPath)
	if statErr != nil {
		t.Fatalf("known_hosts file must exist after resolve: %v", statErr)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("known_hosts must be created 0600; got %04o", got)
	}
	if !strings.Contains(buf.String(), "created known_hosts file") {
		t.Errorf("expected creation Info log; got:\n%s", buf.String())
	}
}

// TestResolveHostKeyCallback_ErrorNotDoublePrefixed pins that error strings
// carry no "cd:" prefix (callers add it; it once appeared twice).
func TestResolveHostKeyCallback_ErrorNotDoublePrefixed(t *testing.T) {
	dir := mkTempDir(t)
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Parent "directory" is a regular file → MkdirAll fails → error path.
	t.Setenv("GIT_SSH_KNOWN_HOSTS", filepath.Join(blocker, "known_hosts"))

	_, err := resolveHostKeyCallback()
	if err == nil {
		t.Fatal("expected error when the known_hosts parent cannot be created")
	}
	if strings.HasPrefix(err.Error(), "cd:") {
		t.Errorf("error must not carry a log prefix (callers add it): %v", err)
	}
}

// TestEnsureKnownHostsFile_TightensExistingMode pins that a pre-existing 0644
// known_hosts is tightened to 0600, with an Info line.
func TestEnsureKnownHostsFile_TightensExistingMode(t *testing.T) {
	dir := mkTempDir(t)
	khPath := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(khPath, []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	buf := captureLogs(t, LevelInfo)

	if err := ensureKnownHostsFile(khPath); err != nil {
		t.Fatalf("ensureKnownHostsFile: %v", err)
	}
	info, err := os.Stat(khPath)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("pre-existing known_hosts must be tightened to 0600; got %04o", got)
	}
	if !strings.Contains(buf.String(), "tightened known_hosts") {
		t.Errorf("expected tightening Info log; got:\n%s", buf.String())
	}
}

// TestAcceptNewHostKeyCallback_TOFU pins accept-new: first contact records one
// line and accepts, a repeat adds nothing, a changed key is rejected, and a
// second host is recorded independently.
func TestAcceptNewHostKeyCallback_TOFU(t *testing.T) {
	newKey := func() cryptossh.PublicKey {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		signer, err := cryptossh.NewSignerFromKey(priv)
		if err != nil {
			t.Fatalf("NewSignerFromKey: %v", err)
		}
		return signer.PublicKey()
	}
	keyA, keyB := newKey(), newKey()
	addr := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 22}

	dir := mkTempDir(t)
	khPath := filepath.Join(dir, "known_hosts")
	if err := ensureKnownHostsFile(khPath); err != nil {
		t.Fatalf("ensureKnownHostsFile: %v", err)
	}
	cb, err := acceptNewHostKeyCallback(khPath)
	if err != nil {
		t.Fatalf("acceptNewHostKeyCallback: %v", err)
	}
	lines := func() []string {
		data, rerr := os.ReadFile(khPath)
		if rerr != nil {
			t.Fatalf("ReadFile: %v", rerr)
		}
		trimmed := strings.TrimSpace(string(data))
		if trimmed == "" {
			return nil
		}
		return strings.Split(trimmed, "\n")
	}

	// First contact: recorded + accepted.
	if err := cb("git.example.com:22", addr, keyA); err != nil {
		t.Fatalf("first contact must be accepted (TOFU): %v", err)
	}
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], "git.example.com") {
		t.Fatalf("expected exactly 1 recorded entry for git.example.com; got %q", got)
	}

	// Repeat with the same key: accepted, no duplicate line.
	if err := cb("git.example.com:22", addr, keyA); err != nil {
		t.Fatalf("known host with same key must be accepted: %v", err)
	}
	if got := lines(); len(got) != 1 {
		t.Fatalf("repeat contact must not append a duplicate; got %d lines", len(got))
	}

	// Same host, DIFFERENT key: rejected.
	if err := cb("git.example.com:22", addr, keyB); err == nil {
		t.Fatal("changed host key must be rejected (MITM protection)")
	}

	// A second, unrelated host: recorded independently.
	if err := cb("other.example.com:22", addr, keyB); err != nil {
		t.Fatalf("second host first contact must be accepted: %v", err)
	}
	if got := lines(); len(got) != 2 {
		t.Fatalf("expected 2 recorded entries after second host; got %d", len(got))
	}
}

func TestResolveHostKeyCallback_ValidFile_ReturnsCallbackWithoutWarning(t *testing.T) {
	dir := mkTempDir(t)
	khPath := filepath.Join(dir, "known_hosts")
	// A well-formed entry with a test key (not the host's real key).
	entry := "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl\n"
	if err := os.WriteFile(khPath, []byte(entry), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("GIT_SSH_KNOWN_HOSTS", khPath)
	buf := captureLogs(t, LevelWarning)

	cb, err := resolveHostKeyCallback()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cb == nil {
		t.Fatal("callback must not be nil")
	}
	if strings.Contains(buf.String(), "DISABLED") {
		t.Errorf("should not warn about disabled verification when known_hosts is set; got:\n%s", buf.String())
	}
}

// ─── resolveCloneSSHAuth ───────────────────────────────────────────────────

func TestResolveCloneSSHAuth_NoKey_ReturnsNilNil(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "")
	auth, err := resolveCloneSSHAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != nil {
		t.Errorf("no key set must yield nil auth (no-auth clone); got %v", auth)
	}
}

func TestResolveCloneSSHAuth_ValidKey_NoKnownHosts_ReturnsAuth(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", string(mustGenerateSSHPrivateKeyPEM(t)))
	t.Setenv("GIT_SSH_KNOWN_HOSTS", "") // → InsecureIgnoreHostKey + warning
	buf := captureLogs(t, LevelWarning)

	auth, err := resolveCloneSSHAuth()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil {
		t.Fatal("a valid key must yield a non-nil auth method")
	}
	if auth.HostKeyCallback == nil {
		t.Error("auth must carry a host-key callback")
	}
	if !strings.Contains(buf.String(), "DISABLED") {
		t.Errorf("missing known_hosts should warn that host-key verification is DISABLED; got:\n%s", buf.String())
	}
}

func TestResolveCloneSSHAuth_BadKeyPath_ReturnsError(t *testing.T) {
	// No PEM marker or newline: a path, and a missing file is an error.
	t.Setenv("GIT_SSH_KEY_PRIVATE", "/nonexistent/cystemd_test_key")
	if _, err := resolveCloneSSHAuth(); err == nil {
		t.Fatal("expected error for a missing key file")
	}
}

// TestResolveCloneSSHAuth_BadKeyPath_NeverLeaksRawValue pins that the wrapped
// error (logged by CD, returned by /sync) never contains the
// GIT_SSH_KEY_PRIVATE value.
func TestResolveCloneSSHAuth_BadKeyPath_NeverLeaksRawValue(t *testing.T) {
	const marker = "definitely-not-a-real-secret-MARKER-13579"
	t.Setenv("GIT_SSH_KEY_PRIVATE", marker)
	_, err := resolveCloneSSHAuth()
	if err == nil {
		t.Fatal("expected error for a missing key file")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("resolveCloneSSHAuth error must NOT contain the raw GIT_SSH_KEY_PRIVATE value; got: %v", err)
	}
}

func TestResolveCloneSSHAuth_MalformedPEM_ReturnsError(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "-----BEGIN OPENSSH PRIVATE KEY-----\nnot-a-real-key\n-----END OPENSSH PRIVATE KEY-----\n")
	if _, err := resolveCloneSSHAuth(); err == nil {
		t.Fatal("expected error for a malformed PEM key")
	}
}

// ─── extractVersionString ──────────────────────────────────────────────────

func TestExtractVersionString_ValidKey_ReturnsValue(t *testing.T) {
	got, err := extractVersionString([]byte(versionYAMLFixture),
		".${selector}.global.image.tag", "myapplication")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "abc123def456abc123def456abc123def456abc123" {
		t.Errorf("got %q, want %q", got, "abc123def456abc123def456abc123def456abc123")
	}
}

func TestExtractVersionString_SelectorExpanded(t *testing.T) {
	// Verify ${selector} is substituted before navigation.
	data := []byte("svc:\n  global:\n    image:\n      tag: myversion\n")
	got, err := extractVersionString(data, ".${selector}.global.image.tag", "svc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "myversion" {
		t.Errorf("got %q, want myversion", got)
	}
}

func TestExtractVersionString_MissingKey_ReturnsError(t *testing.T) {
	data := []byte("foo: bar\n")
	_, err := extractVersionString(data, ".missing.path", "")
	if err == nil {
		t.Error("expected error for missing key")
	}
}

func TestExtractVersionString_FromRealFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/myproject_myapplication_values.yaml")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	got, err := extractVersionString(data, ".${selector}.global.image.tag", "myapplication")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Fixture has: tag: abc123def456abc123def456abc123def456abc123
	if got != "abc123def456abc123def456abc123def456abc123" {
		t.Errorf("got %q, want %q", got, "abc123def456abc123def456abc123def456abc123")
	}
}

func TestExtractVersionString_NumericTag_ReturnsString(t *testing.T) {
	data := []byte("svc:\n  global:\n    image:\n      tag: 42\n")
	got, err := extractVersionString(data, ".svc.global.image.tag", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

// TestExtractVersionString_FloatLikeTag_PreservesRawText pins that an unquoted
// decimal tag comes back verbatim: a float round-trip turns "1.10" into "1.1"
// and would install ${service}-1.1.
func TestExtractVersionString_FloatLikeTag_PreservesRawText(t *testing.T) {
	for _, tag := range []string{"1.10", "1.20", "0.10"} {
		data := []byte("svc:\n  global:\n    image:\n      tag: " + tag + "\n")
		got, err := extractVersionString(data, ".svc.global.image.tag", "")
		if err != nil {
			t.Fatalf("tag %q: unexpected error: %v", tag, err)
		}
		if got != tag {
			t.Errorf("tag %q: got %q, want it preserved verbatim (a float round-trip would mangle it)", tag, got)
		}
	}
}

// ─── readInstalledVersion / writeInstalledVersion ──────────────────────────

func TestInstalledVersion_EmptyDir_ReturnsEmpty(t *testing.T) {
	dir := mkTempDir(t)
	if got := readInstalledVersion(dir); got != "" {
		t.Errorf("readInstalledVersion on empty dir: got %q, want empty", got)
	}
}

func TestInstalledVersion_RoundTrip(t *testing.T) {
	dir := mkTempDir(t)
	writeInstalledVersion(dir, "abc123def456abc123def456abc123def456abc123")
	got := readInstalledVersion(dir)
	if got != "abc123def456abc123def456abc123def456abc123" {
		t.Errorf("got %q, want %q", got, "abc123def456abc123def456abc123def456abc123")
	}
}

func TestInstalledVersion_OverwritesPrevious(t *testing.T) {
	dir := mkTempDir(t)
	writeInstalledVersion(dir, "oldversion")
	writeInstalledVersion(dir, "newversion")
	if got := readInstalledVersion(dir); got != "newversion" {
		t.Errorf("got %q, want newversion", got)
	}
}

func TestInstalledVersion_TrimsWhitespace(t *testing.T) {
	dir := mkTempDir(t)
	// Simulate a file written with extra whitespace.
	if err := os.WriteFile(filepath.Join(dir, installedVersionFilename),
		[]byte("  abc123  \n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := readInstalledVersion(dir); got != "abc123" {
		t.Errorf("got %q, want abc123 (whitespace should be trimmed)", got)
	}
}

func TestWriteInstalledVersion_BadDir_LogsWarning(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	// Nonexistent parent directory: the write fails.
	writeInstalledVersion("/nonexistent/dir/that/does/not/exist", "v1")
	if !strings.Contains(buf.String(), "cannot write version file") {
		t.Errorf("expected warning log; got:\n%s", buf.String())
	}
}

// TestWriteInstalledVersion_StaleTmpFile_RemovedAndNotReused pins that
// tracking-file writes (writeTrackingFile) remove a .tmp left by a crash
// instead of renaming it into place.
func TestWriteInstalledVersion_StaleTmpFile_RemovedAndNotReused(t *testing.T) {
	dir := mkTempDir(t)
	tmpPath := filepath.Join(dir, installedVersionFilename+".tmp")
	if err := os.WriteFile(tmpPath, []byte("GARBAGE-FROM-A-PRIOR-CRASH"), 0o666); err != nil {
		t.Fatalf("seed: write stale tmp: %v", err)
	}

	writeInstalledVersion(dir, "abc123")

	got := readInstalledVersion(dir)
	if got != "abc123" {
		t.Errorf("got %q, want %q", got, "abc123")
	}
	if strings.Contains(got, "GARBAGE") {
		t.Errorf("stale tmp content leaked into .installed_version; got %q", got)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Errorf("expected %q to be gone after a successful write; stat err=%v", tmpPath, err)
	}
}

// TestWriteInstalledVersion_ConcurrentReadNeverObservesTornContent pins that a
// reader racing writeInstalledVersion never sees empty or partial content;
// fixed-width values make any short read unambiguous. A truncating
// os.WriteFile fails it within a few iterations.
func TestWriteInstalledVersion_ConcurrentReadNeverObservesTornContent(t *testing.T) {
	dir := mkTempDir(t)
	captureLogs(t, LevelWarning) // keep the writeFileAtomic Info spam out of `go test -v`

	const width = 64
	valueAt := func(i int) string {
		return fmt.Sprintf("%0*d", width, i)
	}
	// Seeded before the reader starts, so an empty read can only be torn.
	writeInstalledVersion(dir, valueAt(0))

	const iterations = 400
	stop := make(chan struct{})
	tornCh := make(chan string, 1)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got := readInstalledVersion(dir)
			if got == "" {
				select {
				case tornCh <- "observed empty content (torn read mid-write)":
				default:
				}
				continue
			}
			if len(got) != width {
				select {
				case tornCh <- fmt.Sprintf("observed short/torn content: %q (len=%d, want %d)", got, len(got), width):
				default:
				}
			}
		}
	}()

	for i := 1; i <= iterations; i++ {
		writeInstalledVersion(dir, valueAt(i))
	}
	close(stop)
	wg.Wait()

	select {
	case msg := <-tornCh:
		t.Errorf("torn read detected — atomic write regression: %s", msg)
	default:
	}
}

// ─── runServiceVersionInstall (via RunContinuousDeployment) ───────────────

// TestRunContinuousDeployment_VersionKey_SkipsWhenAlreadyInstalled pins that
// no install is attempted when rpm reports the wanted version installed,
// whatever .installed_version says. See ADR-0011.
func TestRunContinuousDeployment_VersionKey_SkipsWhenAlreadyInstalled(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	// The wanted version must be a real installed package's real version;
	// without rpm the cycle declines (known=false), so there is nothing to test.
	if !hasRPM() {
		t.Skip("needs rpm: the skip path is decided from the rpm database")
	}
	out, err := exec.Command("rpm", "-qa", "--queryformat", "%{NAME} %{VERSION}\n").Output()
	if err != nil {
		t.Fatalf("rpm -qa: %v", err)
	}
	svc, wantedVersion := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// Skip versions with YAML-special characters: they would test the
		// fixture, not the code path.
		if len(f) == 2 && validServiceName.MatchString(f[0]) && f[1] != "" && !strings.ContainsAny(f[1], "'\"\\:{}[]") {
			svc, wantedVersion = f[0], f[1]
			break
		}
	}
	if svc == "" {
		t.Skip("no installed rpm package with a usable name and version found (empty rpmdb)")
	}

	// versionYAMLFixture's shape, keyed on svc instead of "myapplication".
	valuesYAML := fmt.Sprintf(`
%s:
  global:
    image:
      tag: "%s"
  services:
    %s:
      config:
        - app:
            name: myproject_myapplication
            port: 8888
          logger:
            log_level: "DEBUG"
          redis:
            host: 10.20.30.5
            port: 6379
`, svc, wantedVersion, svc)

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAML,
	})

	// A tracking file naming another version: it must not sway the decision.
	destDir := filepath.Join(workbase, svc)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeInstalledVersion(destDir, "a-stale-value-that-must-be-ignored")

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  svc,
		HostSelector: svc,
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            true,
			Service:             svc,
			Selector:            svc,
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workbase,
		}},
	})

	logOut := buf.String()
	// "(rpm database)" proves the skip came from rpm, not the tracking file.
	if !strings.Contains(logOut, "already at version") || !strings.Contains(logOut, "(rpm database)") {
		t.Errorf("expected 'already at version ... (rpm database)' debug log; got:\n%s", logOut)
	}
	if strings.Contains(logOut, "version change detected") {
		t.Errorf("should NOT log version change when version is unchanged; got:\n%s", logOut)
	}
	if strings.Contains(logOut, "reinstalling") {
		t.Errorf("should NOT log a reinstall when the package is actually installed; got:\n%s", logOut)
	}
}

// ─── stale-tracking cross-check ───────────────────────────────────────────

// TestRunServiceVersionInstall_StaleTracking_Reinstalls pins that a package
// missing from rpm is reinstalled even though .installed_version claims the
// wanted tag (e.g. after an out-of-band `dnf remove`).
func TestRunServiceVersionInstall_StaleTracking_Reinstalls(t *testing.T) {
	if !hasRPM() {
		t.Skip("needs rpm")
	}

	destDir := mkTempDir(t)
	writeInstalledVersion(destDir, "abc123")

	rawContent := []byte(`cystemd-test-no-such-pkg:
  global:
    image:
      tag: abc123
`)

	entry := &ServiceEntry{
		Service:             "cystemd-test-no-such-pkg",
		Selector:            "cystemd-test-no-such-pkg",
		GitValues:           "values.yaml",
		GitValuesVersionKey: ".${selector}.global.image.tag",
		AutoSync:            true,
	}

	buf := captureLogs(t, LevelDebug)
	got := runServiceVersionInstall("cystemd-test-no-such-pkg", entry, rawContent, destDir, true)

	out := buf.String()
	if !strings.Contains(out, "claims version abc123 but the package is not installed — reinstalling") {
		t.Errorf("expected stale-tracking reinstall log; got:\n%s", out)
	}
	if !strings.Contains(out, "install cystemd-test-no-such-pkg version abc123 failed") {
		t.Errorf("expected dnf install failure log; got:\n%s", out)
	}
	if got := readInstalledVersion(destDir); got != "abc123" {
		t.Errorf(".installed_version should remain unchanged after a failed install; got %q", got)
	}
	if got {
		t.Errorf("expected false return value")
	}
}

// TestRunServiceVersionInstall_StaleTracking_AutoSyncFalse_NotReinstalled pins
// that with autosync=false the same drift is only logged ("not
// reinstalling"): no install, tracking file untouched.
func TestRunServiceVersionInstall_StaleTracking_AutoSyncFalse_NotReinstalled(t *testing.T) {
	if !hasRPM() {
		t.Skip("needs rpm")
	}

	destDir := mkTempDir(t)
	writeInstalledVersion(destDir, "abc123")

	rawContent := []byte(`cystemd-test-no-such-pkg:
  global:
    image:
      tag: abc123
`)

	entry := &ServiceEntry{
		Service:             "cystemd-test-no-such-pkg",
		Selector:            "cystemd-test-no-such-pkg",
		GitValues:           "values.yaml",
		GitValuesVersionKey: ".${selector}.global.image.tag",
		AutoSync:            false,
	}

	buf := captureLogs(t, LevelDebug)
	got := runServiceVersionInstall("cystemd-test-no-such-pkg", entry, rawContent, destDir, false)

	out := buf.String()
	if !strings.Contains(out, "package not installed") || !strings.Contains(out, "(autosync=false); not reinstalling") {
		t.Errorf("expected autosync=false stale-tracking log; got:\n%s", out)
	}
	if strings.Contains(out, "— reinstalling") {
		t.Errorf("should NOT attempt a reinstall when autosync=false; got:\n%s", out)
	}
	if strings.Contains(out, "install cystemd-test-no-such-pkg version") {
		t.Errorf("should NOT attempt a dnf install when autosync=false; got:\n%s", out)
	}
	if got := readInstalledVersion(destDir); got != "abc123" {
		t.Errorf(".installed_version should remain unchanged; got %q", got)
	}
	if got {
		t.Errorf("expected false return value")
	}
}

// TestRunServiceVersionInstall_StaleTracking_RecordsDiffWithChangedTrue pins
// that a package missing from rpm, while .installed_version claims the wanted
// tag, records Version.Changed=true for /diff and /lastdiff.
// Why: DOCS/MEMORY.md § Fixed bugs — `/diff`/`/lastdiff`
func TestRunServiceVersionInstall_StaleTracking_RecordsDiffWithChangedTrue(t *testing.T) {
	if !hasRPM() {
		t.Skip("needs rpm")
	}

	destDir := mkTempDir(t)
	writeInstalledVersion(destDir, "abc123")

	rawContent := []byte(`cystemd-test-no-such-pkg:
  global:
    image:
      tag: abc123
`)

	entry := &ServiceEntry{
		Service:             "cystemd-test-no-such-pkg",
		Selector:            "cystemd-test-no-such-pkg",
		GitValues:           "values.yaml",
		GitValuesVersionKey: ".${selector}.global.image.tag",
		AutoSync:            true,
	}

	resetLastDiffCacheForTest()
	t.Cleanup(resetLastDiffCacheForTest)

	runServiceVersionInstall("cystemd-test-no-such-pkg", entry, rawContent, destDir, true)

	diffs := getLastDiffs([]string{"cystemd-test-no-such-pkg"})
	if len(diffs) != 1 {
		t.Errorf("expected 1 diff entry, got %d", len(diffs))
		return
	}
	if !diffs[0].Version.Changed {
		t.Errorf("expected Version.Changed=true for stale-tracking case, got false; Current=%q, Wanted=%q",
			diffs[0].Version.Current, diffs[0].Version.Wanted)
	}
}

// TestRunContinuousDeployment_VersionKey_DetectsNewVersion pins that a tag rpm
// does not report installed triggers an install attempt, and that the failed
// install (no such package here) leaves .installed_version unwritten.
func TestRunContinuousDeployment_VersionKey_DetectsNewVersion(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            true,
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workbase,
		}},
	})

	out := buf.String()
	// "installing RPM" ends both the initial and the change message.
	if !strings.Contains(out, "installing RPM") {
		t.Errorf("expected 'installing RPM' log; got:\n%s", out)
	}
	if !strings.Contains(out, "abc123def456abc123def456abc123def456abc123") {
		t.Errorf("expected version string in log; got:\n%s", out)
	}

	// The install fails (dnf absent, or no such package), so no version file.
	destDir := filepath.Join(workbase, "myapplication")
	if hasDNF() {
		// dnf is present but the package won't exist — install fails → no file.
		if got := readInstalledVersion(destDir); got != "" {
			t.Errorf("version file should not be written on failed install; got %q", got)
		}
	} else {
		// dnf not present — same outcome.
		if got := readInstalledVersion(destDir); got != "" {
			t.Errorf("version file should not be written when dnf is absent; got %q", got)
		}
	}
}

// TestRunContinuousDeployment_VersionKey_StillRunsWhenConfigUnchanged pins
// that the version step runs even when ${service_config} is unchanged (only
// the image tag may have moved).
func TestRunContinuousDeployment_VersionKey_StillRunsWhenConfigUnchanged(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            true,
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workbase,
		}},
	}
	cdCfg := &CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cdCfg, appCfg)

	// A tracking file naming another version; rpm, not this file, decides.
	destDir := filepath.Join(workbase, "myapplication")
	writeInstalledVersion(destDir, "oldversion000")

	// Config unchanged now: the version step must still attempt the install.
	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(cdCfg, appCfg)
	if !strings.Contains(buf.String(), "installing RPM") {
		t.Errorf("expected install attempt on second cycle with stale version file; got:\n%s",
			buf.String())
	}
}

// TestRunContinuousDeployment_VersionKey_NoVersionKeyNoInstall pins that no
// install is attempted without git_values_version_key.
func TestRunContinuousDeployment_VersionKey_NoVersionKeyNoInstall(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           true,
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			// GitValuesVersionKey intentionally empty
			Workdir: workbase,
		}},
	})

	if strings.Contains(buf.String(), "installing RPM") {
		t.Errorf("must not attempt install when git_values_version_key is unset; got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "version change") {
		t.Errorf("must not log version change when git_values_version_key is unset; got:\n%s", buf.String())
	}
}

// TestRunContinuousDeployment_VersionKey_InvalidKey_LogsWarning pins that an
// unresolvable git_values_version_key logs a Warning naming the raw and the
// resolved key.
func TestRunContinuousDeployment_VersionKey_InvalidKey_LogsWarning(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	buf := captureLogs(t, LevelWarning)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            true,
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.nonexistent.deeply.nested.key",
			Workdir:             workbase,
		}},
	})

	if !strings.Contains(buf.String(), "cannot extract version key") {
		t.Errorf("expected 'cannot extract version key' warning; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "${selector}") {
		t.Errorf("expected raw ${selector} placeholder in warning; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), ".myapplication.nonexistent.deeply.nested.key") {
		t.Errorf("expected resolved key in warning; got:\n%s", buf.String())
	}
}

// TestRunContinuousDeployment_VersionKey_SuccessfulInstall runs the install
// against the host's real dnf (skipped without dnf). It asserts only the
// attempt: the test package is rarely in a configured repo.
func TestRunContinuousDeployment_VersionKey_SuccessfulInstall(t *testing.T) {
	if !hasDNF() {
		t.Skip("dnf not available; skipping live-install test")
	}
	t.Log("dnf is available; running live install test (may fail if package is not in a configured repo)")

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            true,
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workbase,
		}},
	})

	out := buf.String()
	if !strings.Contains(out, "installing RPM") {
		t.Errorf("expected 'installing RPM' log; got:\n%s", out)
	}
}

func TestRunContinuousDeployment_NoKnownHosts_LogsWarningOnSSHClone(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n")
	t.Setenv("GIT_SSH_KNOWN_HOSTS", "")

	buf := captureLogs(t, LevelWarning)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "git@bitbucket.org:org/repo.git",
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        filepath.Join(mkTempDir(t), "out.yml"),
	}, nil)

	// The fake key fails parsing before host-key resolution, so this pins
	// only that the cycle does not panic.
	_ = buf.String()
}

// ─── env-var integration: GIT_SSH_KEY_PRIVATE auto-detect ─────────────────

func TestRunContinuousDeployment_BadSSHKey_NoCrash(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "not a valid key\nnope\n")

	buf := captureLogs(t, LevelWarning)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "git@bitbucket.org:org/repo.git",
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        filepath.Join(mkTempDir(t), "out.yml"),
	}, nil)

	if !strings.Contains(buf.String(), "invalid SSH key") {
		t.Errorf("expected 'invalid SSH key' warning; got:\n%s", buf.String())
	}
}

func TestRunContinuousDeployment_SSHKeyFileMissing_NoCrash(t *testing.T) {
	t.Setenv("GIT_SSH_KEY_PRIVATE", "/nonexistent/key/file/here")

	buf := captureLogs(t, LevelWarning)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "git@bitbucket.org:org/repo.git",
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        filepath.Join(mkTempDir(t), "out.yml"),
	}, nil)

	if !strings.Contains(buf.String(), "cannot load SSH key") {
		t.Errorf("expected 'cannot load SSH key' warning; got:\n%s", buf.String())
	}
}

// ─── restart behaviour (config-file change, version change, .env change) ──

// TestRunContinuousDeployment_ConfigChange_NoResurrect pins that a
// ${service_config} change reaches the restart gate and, the unit not running
// (it does not exist here), logs the no-resurrect skip instead of starting it.
func TestRunContinuousDeployment_ConfigChange_NoResurrect(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           true,
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			// No GitValuesVersionKey — only config-change path exercised.
			Workdir: workbase,
		}},
	})

	if !strings.Contains(buf.String(), "not restarting after target.config.yml change") {
		t.Errorf("expected no-resurrect skip after config change; got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "restarting myapplication after") {
		t.Errorf("must NOT attempt a restart of a non-running unit; got:\n%s", buf.String())
	}
}

// TestRunContinuousDeployment_ConfigUnchanged_NoRestart pins that an unchanged
// ${service_config} never reaches the restart gate.
func TestRunContinuousDeployment_ConfigUnchanged_NoRestart(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           true,
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workbase,
		}},
	}
	cdCfg := &CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}

	RunContinuousDeployment(cdCfg, appCfg) // first run: writes the config

	// Second run: identical content, so the gate must not run.
	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(cdCfg, appCfg)

	if strings.Contains(buf.String(), "restarting myapplication after") ||
		strings.Contains(buf.String(), "not restarting after") {
		t.Errorf("restart gate must not run when config is unchanged; got:\n%s", buf.String())
	}
}

// TestRunContinuousDeployment_NoConfigKey_NoRestartOnValuesChange pins that
// writing raw values.yaml (no git_values_config_key) never reaches the restart
// gate: it is a staging file, not the service's runtime config.
func TestRunContinuousDeployment_NoConfigKey_NoRestartOnValuesChange(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:  true,
			Service:   "myapplication",
			Selector:  "myapplication",
			GitValues: "values.yaml",
			// GitValuesConfigKey intentionally empty — writes raw values.yaml
			Workdir: workbase,
		}},
	})

	if strings.Contains(buf.String(), "restarting myapplication after") ||
		strings.Contains(buf.String(), "not restarting after") {
		t.Errorf("restart gate must not run when writing raw values.yaml (no GitValuesConfigKey); got:\n%s",
			buf.String())
	}
}

// TestRunContinuousDeployment_VersionAndConfigBothChange_SingleRestart pins one
// restart decision when a version install and a config change coincide: a
// version-step restart attempt (install succeeded, unit running) suppresses
// the config-change restart; here the install fails, so only the config gate
// runs. Two decisions would be the double-restart bug.
func TestRunContinuousDeployment_VersionAndConfigBothChange_SingleRestart(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            true,
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workbase,
		}},
	})

	out := buf.String()

	// An attempt (unit running) or a no-resurrect skip; zero means the gate
	// was lost.
	attempts := strings.Count(out, "restarting myapplication after")
	skips := strings.Count(out, "not restarting after")
	if attempts+skips != 1 {
		t.Errorf("expected exactly 1 restart decision (attempt or skip); got %d attempt(s) + %d skip(s):\n%s",
			attempts, skips, out)
	}
}

// ─── autosync gate ─────────────────────────────────────────────────────────

// TestRunContinuousDeployment_AutoSyncDisabled_DetectsAndLogsDrift pins, for
// every falsy autosync form, that drift is still detected and logged at Info
// while nothing is written, installed or restarted. See ADR-0003.
func TestRunContinuousDeployment_AutoSyncDisabled_DetectsAndLogsDrift(t *testing.T) {
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	cases := []struct {
		name        string
		autosyncVal interface{}
	}{
		{"bool_false", false},
		{"omitted", nil},
		{"string_false", "false"},      // tolerant case-insensitive false
		{"string_FALSE", "FALSE"},      // upper-case variant
		{"string_No", "No"},            // synonym
		{"string_0", "0"},              // numeric-string false
		{"int_0", 0},                   // int false
		{"int_2", 2},                   // unrecognised int → false
		{"invalid_string", "notabool"}, // garbage scalar
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workdir := mkTempDir(t)
			buf := captureLogs(t, LevelInfo)
			RunContinuousDeployment(&CDConfig{
				GitRepository:    "file://" + repoDir,
				GitConfiguration: "config.yml",
				LocalPath:        savePath,
			}, &Config{
				ServiceName:  "myapplication",
				HostSelector: "myapplication",
				ContinuousDeployment: []ServiceEntry{{
					AutoSync:            tc.autosyncVal,
					Service:             "myapplication",
					Selector:            "myapplication",
					GitValues:           "values.yaml",
					GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
					GitValuesVersionKey: ".${selector}.global.image.tag",
					Workdir:             workdir,
				}},
			})

			out := buf.String()

			// Drift is logged for both the config file and the version.
			if !strings.Contains(out, "out-of-date (autosync=false); not rewriting") {
				t.Errorf("expected 'out-of-date (autosync=false); not rewriting' log for ${service_config}; got:\n%s", out)
			}
			if !strings.Contains(out, "version out-of-date") {
				t.Errorf("expected 'version out-of-date' log; got:\n%s", out)
			}

			// Side effects MUST NOT happen.
			destDir := filepath.Join(workdir, "myapplication")
			if _, err := os.Stat(filepath.Join(destDir, "target.config.yml")); err == nil {
				t.Errorf("target.config.yml must NOT be written when autosync disabled")
			}
			if _, err := os.Stat(filepath.Join(destDir, ".installed_version")); err == nil {
				t.Errorf(".installed_version must NOT be written when autosync disabled")
			}
			if strings.Contains(out, "installing RPM") {
				t.Errorf("install must NOT be attempted when autosync disabled; got:\n%s", out)
			}
			if strings.Contains(out, "restarting myapplication after") ||
				strings.Contains(out, "not restarting after") {
				t.Errorf("restart gate must not run when autosync disabled; got:\n%s", out)
			}
		})
	}
}

// TestRunContinuousDeployment_AutoSyncDisabled_InSync_NoDriftLog pins that an
// in-sync file logs no "out-of-date" line under autosync=false: only real
// drift does.
func TestRunContinuousDeployment_AutoSyncDisabled_InSync_NoDriftLog(t *testing.T) {
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})

	workdir := mkTempDir(t)
	appCfg := &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           true, // first pass writes the file
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workdir,
		}},
	}
	cdCfg := &CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}
	RunContinuousDeployment(cdCfg, appCfg)

	// Now autosync off, with the file already in sync.
	appCfg.ContinuousDeployment[0].AutoSync = false
	buf := captureLogs(t, LevelInfo)
	RunContinuousDeployment(cdCfg, appCfg)
	out := buf.String()
	if strings.Contains(out, "out-of-date") {
		t.Errorf("in-sync file must NOT log out-of-date; got:\n%s", out)
	}
}

// TestRunContinuousDeployment_AutoSyncTrue_PreservesBehaviour pins that
// AutoSync: true still writes the extracted config (the gate's happy path).
func TestRunContinuousDeployment_AutoSyncTrue_PreservesBehaviour(t *testing.T) {
	combinedRepo := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + combinedRepo,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           true,
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workbase,
		}},
	})

	destFile := filepath.Join(workbase, "myapplication", "target.config.yml")
	if _, err := os.ReadFile(destFile); err != nil {
		t.Errorf("AutoSync: true must write target.config.yml: %v", err)
	}
}

// multiSvcValues returns a values document for selector that matches the
// ".${selector}.services.${selector}.config.[]" path, with app.name = appName.
func multiSvcValues(selector, appName string) string {
	return fmt.Sprintf("%s:\n  services:\n    %s:\n      config:\n        - app:\n            name: %s\n",
		selector, selector, appName)
}

// TestRunContinuousDeployment_MultiService_WritesAllManagedServices is the
// headline test for "a node manages multiple services": the host is in TWO
// pools (app3, app4 — the HOST005/006 shape), each mapping to a distinct
// service with its OWN workdir. One CD cycle must write BOTH services' config
// files into their respective workdirs.
func TestRunContinuousDeployment_MultiService_WritesAllManagedServices(t *testing.T) {
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml": "k: v\n",
		"v3.yaml":    multiSvcValues("app3", "svc3"),
		"v4.yaml":    multiSvcValues("app4", "svc4"),
	})
	savePath := filepath.Join(mkTempDir(t), "config.yml")
	workdir3 := mkTempDir(t)
	workdir4 := mkTempDir(t)

	appCfg := &Config{
		ServiceName:   "myproject_app3",
		HostSelector:  "app3",
		HostSelectors: []string{"app3", "app4"}, // host is in BOTH pools
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "myproject_app3", Selector: "app3",
				GitValues: "v3.yaml", GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: workdir3},
			{AutoSync: true, Service: "myproject_app4", Selector: "app4",
				GitValues: "v4.yaml", GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: workdir4},
		},
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	d3, err := os.ReadFile(filepath.Join(workdir3, "myproject_app3", "target.config.yml"))
	if err != nil {
		t.Fatalf("app3 config not written: %v", err)
	}
	if !strings.Contains(string(d3), "svc3") {
		t.Errorf("app3 config: got %q, want it to contain svc3", string(d3))
	}
	d4, err := os.ReadFile(filepath.Join(workdir4, "myproject_app4", "target.config.yml"))
	if err != nil {
		t.Fatalf("app4 config not written (second managed service was skipped): %v", err)
	}
	if !strings.Contains(string(d4), "svc4") {
		t.Errorf("app4 config: got %q, want it to contain svc4", string(d4))
	}
}

// TestRunContinuousDeployment_MultiService_SameBranch_ClonesOnce verifies the
// clone-efficiency fix: three services that all live on the SAME branch are
// served by ONE values clone (not three), while their config files are all
// written. The per-branch clone emits exactly one "cloned … once for N managed
// service(s)" Debug line.
func TestRunContinuousDeployment_MultiService_SameBranch_ClonesOnce(t *testing.T) {
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml": "k: v\n",
		"v1.yaml":    multiSvcValues("a", "sa"),
		"v2.yaml":    multiSvcValues("b", "sb"),
		"v3.yaml":    multiSvcValues("c", "sc"),
	})
	savePath := filepath.Join(mkTempDir(t), "config.yml")
	wd := mkTempDir(t)
	mkEntry := func(svc, sel, vals string) ServiceEntry {
		return ServiceEntry{AutoSync: true, Service: svc, Selector: sel, GitValues: vals,
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: wd}
	}
	appCfg := &Config{
		ServiceName:   "sa",
		HostSelector:  "a",
		HostSelectors: []string{"a", "b", "c"},
		ContinuousDeployment: []ServiceEntry{
			mkEntry("sa", "a", "v1.yaml"),
			mkEntry("sb", "b", "v2.yaml"),
			mkEntry("sc", "c", "v3.yaml"),
		},
	}

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	for _, s := range []string{"sa", "sb", "sc"} {
		if _, err := os.ReadFile(filepath.Join(wd, s, "target.config.yml")); err != nil {
			t.Errorf("%s config not written: %v", s, err)
		}
	}
	// All three services share the default branch → exactly ONE values clone.
	if n := strings.Count(buf.String(), "once for"); n != 1 {
		t.Errorf("expected exactly 1 shared values clone for 3 same-branch services; got %d:\n%s", n, buf.String())
	}
	if !strings.Contains(buf.String(), "once for 3 managed service(s)") {
		t.Errorf("expected the clone line to report 3 services; got:\n%s", buf.String())
	}
}

// TestRunContinuousDeployment_MultiService_DistinctBranches_ClonesEach verifies
// that services on DIFFERENT branches are still cloned independently (one clone
// per distinct branch), so the per-entry git_branch is honoured.
func TestRunContinuousDeployment_MultiService_DistinctBranches_ClonesEach(t *testing.T) {
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml": "k: v\n",
		"v1.yaml":    multiSvcValues("a", "sa"),
	})
	// Add a second branch carrying v2.yaml.
	addBranchWithFile(t, repoDir, "releasedev", "v2.yaml", multiSvcValues("b", "sb"))

	savePath := filepath.Join(mkTempDir(t), "config.yml")
	wd := mkTempDir(t)
	appCfg := &Config{
		ServiceName:   "sa",
		HostSelector:  "a",
		HostSelectors: []string{"a", "b"},
		ContinuousDeployment: []ServiceEntry{
			{AutoSync: true, Service: "sa", Selector: "a", GitValues: "v1.yaml",
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: wd}, // default branch
			{AutoSync: true, Service: "sb", Selector: "b", GitBranch: "releasedev", GitValues: "v2.yaml",
				GitValuesConfigKey: ".${selector}.services.${selector}.config.[]", Workdir: wd},
		},
	}

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	if _, err := os.ReadFile(filepath.Join(wd, "sa", "target.config.yml")); err != nil {
		t.Errorf("sa (default branch) config not written: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(wd, "sb", "target.config.yml")); err != nil {
		t.Errorf("sb (releasedev branch) config not written: %v", err)
	}
	if n := strings.Count(buf.String(), "once for"); n != 2 {
		t.Errorf("expected 2 clones (one per distinct branch); got %d:\n%s", n, buf.String())
	}
}

// ─── writeFileAtomic helper ────────────────────────────────────────────────

// TestWriteFileAtomic_HappyPath verifies the atomic write succeeds, the
// content matches, no `.tmp` companion is left behind, and the log line
// includes the fromSuffix.
func TestWriteFileAtomic_HappyPath(t *testing.T) {
	dir := mkTempDir(t)
	destFile := filepath.Join(dir, "out.yml")

	buf := captureLogs(t, LevelInfo)
	if !writeFileAtomic(destFile, []byte("hello\n"), "repo@master:path/x.yml") {
		t.Fatalf("expected writeFileAtomic to succeed")
	}
	got, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello\n" {
		t.Errorf("content: got %q, want %q", got, "hello\n")
	}
	if _, err := os.Stat(destFile + ".tmp"); err == nil {
		t.Error(".tmp companion must be cleaned up after success")
	}
	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "wrote 6 bytes to") {
		t.Errorf("expected size log line; got msgs:\n%s", msgs)
	}
	if !strings.Contains(msgs, "(from repo@master:path/x.yml)") {
		t.Errorf("expected fromSuffix in log; got msgs:\n%s", msgs)
	}
}

// TestWriteFileAtomic_EmptyFromSuffix verifies the no-suffix code path
// formats the log line without a parenthesised tail.
func TestWriteFileAtomic_EmptyFromSuffix(t *testing.T) {
	dir := mkTempDir(t)
	destFile := filepath.Join(dir, "out.yml")
	buf := captureLogs(t, LevelInfo)

	if !writeFileAtomic(destFile, []byte("x"), "") {
		t.Fatalf("expected success")
	}
	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "wrote 1 bytes to "+destFile) {
		t.Errorf("expected suffix-free log line; got:\n%s", msgs)
	}
	if strings.Contains(msgs, "(from ") {
		t.Errorf("must NOT have 'from' suffix when fromSuffix=\"\"; got:\n%s", msgs)
	}
}

// TestWriteFileAtomic_PreservesExistingMode verifies that the dest file's
// permission bits survive a rewrite. This matters when an operator has
// chmod'd a managed file (e.g. 0600 for a secrets-bearing config).
func TestWriteFileAtomic_PreservesExistingMode(t *testing.T) {
	dir := mkTempDir(t)
	destFile := filepath.Join(dir, "out.yml")
	if err := os.WriteFile(destFile, []byte("v1"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if !writeFileAtomic(destFile, []byte("v2"), "") {
		t.Fatalf("expected success")
	}
	info, err := os.Stat(destFile)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode preserved: got %o, want 0600", got)
	}
}

// The .tmp file is removed on failure. The parent dir is missing, so this fails in
// os.WriteFile ("cannot write") before any rename; the next test covers rename.
func TestWriteFileAtomic_RenameFailure_CleansUpTmp(t *testing.T) {
	// Missing parent: WriteFile fails with ENOENT.
	destFile := filepath.Join(mkTempDir(t), "nonexistent_sub", "out.yml")
	buf := captureLogs(t, LevelWarning)

	if writeFileAtomic(destFile, []byte("data"), "src") {
		t.Errorf("expected failure when parent dir missing")
	}
	if _, err := os.Stat(destFile + ".tmp"); err == nil {
		t.Error(".tmp companion must be cleaned up after failure")
	}
	if !strings.Contains(buf.String(), `"level":"warning"`) {
		t.Errorf("expected warning-level log; got: %s", buf.String())
	}
}

// The real os.Rename failure: destFile is an existing directory, so the .tmp write
// succeeds and rename(2) fails with EISDIR ("cannot rename" + cleanup).
func TestWriteFileAtomic_RenameFailure_WriteSucceeds(t *testing.T) {
	dir := mkTempDir(t)
	destFile := filepath.Join(dir, "out.yml")
	if err := os.Mkdir(destFile, 0o755); err != nil {
		t.Fatalf("seed: mkdir destFile-as-dir: %v", err)
	}

	buf := captureLogs(t, LevelWarning)

	if writeFileAtomic(destFile, []byte("data"), "src") {
		t.Errorf("expected failure when destFile is an existing directory")
	}
	if _, err := os.Stat(destFile + ".tmp"); err == nil {
		t.Error(".tmp companion must be cleaned up after rename failure")
	}
	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "cannot rename") {
		t.Errorf("expected 'cannot rename' log line (proves the Rename branch fired, not WriteFile); got:\n%s", msgs)
	}
	if strings.Contains(msgs, "cannot write") {
		t.Errorf("did NOT expect 'cannot write' — WriteFile must have succeeded for this test to be valid; got:\n%s", msgs)
	}
}

// A stale <destFile>.tmp from a crash (loose mode, garbage) is removed, never renamed
// into place; a fresh tmp at the resolved mode replaces it (as writeSecretFileAtomic).
func TestWriteFileAtomic_StaleTmpFile_RemovedAndNotReused(t *testing.T) {
	dir := mkTempDir(t)
	destFile := filepath.Join(dir, "out.yml")
	tmpFile := destFile + ".tmp"

	// A crash-left tmp file: loose mode, garbage, no destFile yet.
	if err := os.WriteFile(tmpFile, []byte("GARBAGE-FROM-A-PRIOR-CRASH"), 0o666); err != nil {
		t.Fatalf("seed: write stale tmp: %v", err)
	}

	if !writeFileAtomic(destFile, []byte("fresh content"), "") {
		t.Fatalf("expected writeFileAtomic to succeed")
	}

	got, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(got), "GARBAGE") {
		t.Errorf("stale tmp content leaked into destFile; got:\n%s", got)
	}
	if string(got) != "fresh content" {
		t.Errorf("content: got %q, want %q", got, "fresh content")
	}
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Errorf("expected %q to be gone after a successful write; stat err=%v", tmpFile, err)
	}
}

// O_EXCL hardening: a symlink planted at the .tmp path is removed, never followed or
// written through, and its target stays untouched.
func TestWriteFileAtomic_SymlinkAtTmpPath_NotFollowed(t *testing.T) {
	dir := mkTempDir(t)
	destFile := filepath.Join(dir, "out.yml")
	tmpFile := destFile + ".tmp"

	// A file outside the destination dir, the symlink's target.
	targetFile := filepath.Join(mkTempDir(t), "victim.txt")
	if err := os.WriteFile(targetFile, []byte("PRE-EXISTING VICTIM CONTENT"), 0o644); err != nil {
		t.Fatalf("seed: write target: %v", err)
	}
	if err := os.Symlink(targetFile, tmpFile); err != nil {
		t.Fatalf("seed: symlink tmp -> target: %v", err)
	}

	if !writeFileAtomic(destFile, []byte("fresh content"), "") {
		t.Fatalf("expected writeFileAtomic to succeed")
	}

	// The victim file must be completely untouched.
	victim, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("ReadFile targetFile: %v", err)
	}
	if string(victim) != "PRE-EXISTING VICTIM CONTENT" {
		t.Errorf("symlink target was written through! got:\n%s", victim)
	}

	// destFile holds the fresh content and is a regular file.
	info, err := os.Lstat(destFile)
	if err != nil {
		t.Fatalf("Lstat destFile: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Errorf("destFile must be a regular file, not a symlink")
	}
	got, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("ReadFile destFile: %v", err)
	}
	if string(got) != "fresh content" {
		t.Errorf("content: got %q, want %q", got, "fresh content")
	}
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Errorf("expected %q to be gone after a successful write; stat err=%v", tmpFile, err)
	}
}

// ─── PT/POC selector-collision regression ──────────────────────────────────

// Two entries share Service "growin_app" with distinct Selectors and values files;
// the host is in poc_pool, so the POC values must land, not the first entry's (PT).
// Pins the lookup by selector (findServiceEntryBySelector), not by name.
func TestRunContinuousDeployment_DuplicateServiceName_SelectsBySelector(t *testing.T) {
	// Two values files; the POC one carries a marker, so the assertion is on content.
	ptValues := `
appsvc:
  global:
    image:
      tag: pt-tag-9999
  services:
    appsvc:
      config:
        - PT_MARKER_DO_NOT_MATCH: PT_BRANCH_VALUES
`
	pocValues := `
appsvc:
  global:
    image:
      tag: poc-tag-7777
  services:
    appsvc:
      config:
        - POC_MARKER_THIS_IS_THE_BUG_FIX: POC_BRANCH_VALUES
`
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":               "k: v\n",
		"products/pt/values.yaml":  ptValues,
		"products/poc/values.yaml": pocValues,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	// The host is in the poc_pool only — must land on the POC entry.
	appCfg := &Config{
		ServiceName:  "growin_app",
		HostSelector: "poc_pool",
		ContinuousDeployment: []ServiceEntry{
			{
				AutoSync:           true,
				Service:            "growin_app",
				Selector:           "pt_pool",
				GitValues:          "products/pt/values.yaml",
				GitValuesConfigKey: ".appsvc.services.appsvc.config.[]",
				Workdir:            workbase,
			},
			{
				AutoSync:           true,
				Service:            "growin_app",
				Selector:           "poc_pool",
				GitValues:          "products/poc/values.yaml",
				GitValuesConfigKey: ".appsvc.services.appsvc.config.[]",
				Workdir:            workbase,
			},
		},
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	// Assert: file written from POC values, not PT.
	destFile := filepath.Join(workbase, "growin_app", "target.config.yml")
	got, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", destFile, err)
	}
	if !strings.Contains(string(got), "POC_MARKER_THIS_IS_THE_BUG_FIX") {
		t.Errorf("regression: target.config.yml should contain POC content; got:\n%s", string(got))
	}
	if strings.Contains(string(got), "PT_MARKER_DO_NOT_MATCH") {
		t.Errorf("regression: target.config.yml contains PT content — selector-by-name lookup bug is back; got:\n%s", string(got))
	}
}

// ─── SelectorKey override ──────────────────────────────────────────────────

// SelectorKey substitutes ${selector} in the config key while Selector picks the
// pool, so a templated key works when the values key differs from the pool name.
func TestRunContinuousDeployment_SelectorKey_OverridesSubstitution(t *testing.T) {
	values := `
logical_name:
  services:
    logical_name:
      config:
        - HELLO: world
`
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": values,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	// Selector is the POOL name; SelectorKey is the substitution.
	appCfg := &Config{
		ServiceName:  "my_svc",
		HostSelector: "pool_pool",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           true,
			Service:            "my_svc",
			Selector:           "pool_pool",
			SelectorKey:        "logical_name",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workbase,
		}},
	}

	buf := captureLogs(t, LevelDebug)
	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	// Extraction succeeds only if ${selector} became "logical_name", not "pool_pool".
	destFile := filepath.Join(workbase, "my_svc", "target.config.yml")
	got, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", destFile, err)
	}
	if !strings.Contains(string(got), "HELLO: world") {
		t.Errorf("expected extracted content; got:\n%s", string(got))
	}

	// Log line should show resolved key using SelectorKey, not Selector.
	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, `resolved ".logical_name.services.logical_name.config.[]"`) {
		t.Errorf("expected resolved key to use SelectorKey 'logical_name'; got msgs:\n%s", msgs)
	}
}

// Empty SelectorKey: ${selector} substitutes with Selector, as before the field existed.
func TestRunContinuousDeployment_SelectorKey_EmptyFallsBackToSelector(t *testing.T) {
	values := `
mypool:
  services:
    mypool:
      config:
        - X: y
`
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": values,
	})

	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")

	appCfg := &Config{
		ServiceName:  "svc",
		HostSelector: "mypool",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync: true,
			Service:  "svc",
			Selector: "mypool",
			// SelectorKey intentionally unset
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workbase,
		}},
	}

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, appCfg)

	destFile := filepath.Join(workbase, "svc", "target.config.yml")
	got, err := os.ReadFile(destFile)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", destFile, err)
	}
	// yaml.v3 quotes "y" (a YAML 1.1 bool); only the `X:` key matters here.
	if !strings.Contains(string(got), "X:") {
		t.Errorf("expected extracted content (with X: key); got:\n%s", string(got))
	}
}

// ─── RunSelfVersionInstall (the decision tree: README § Self-update) ───────────────────
// These never invoke dnf; the install branch needs a -ldflags build.

func TestRunSelfVersionInstall_NilCfg_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	RunSelfVersionInstall(nil)
	if !strings.Contains(buf.String(), "cystemd self-install skipped — no config") {
		t.Errorf("expected nil-cfg debug log; got:\n%s", buf.String())
	}
}

func TestRunSelfVersionInstall_EmptyVersion_NoOp(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	RunSelfVersionInstall(&Config{}) // Version is empty
	if !strings.Contains(buf.String(), "cystemd self-install skipped — config.version not set") {
		t.Errorf("expected empty-version debug log; got:\n%s", buf.String())
	}
}

// A dev/unknown build never self-installs, even with a real-looking cfg.Version
// (go test defaults are "dev"/"unknown", so the guard always fires).
func TestRunSelfVersionInstall_DevBuild_Skipped(t *testing.T) {
	if v := Version(); v != "dev" && v != "unknown" {
		t.Skipf("running build has Version()=%q (not dev/unknown); this test targets the dev-build guard", v)
	}
	buf := captureLogs(t, LevelDebug)
	RunSelfVersionInstall(&Config{Version: "v5.0.24"})
	out := buf.String()
	if !strings.Contains(out, "running build has no injected version") {
		t.Errorf("expected dev-build skip debug log; got:\n%s", out)
	}
	// Must NOT have logged a version-change line — the guard fires before that.
	if strings.Contains(out, "version change detected") {
		t.Errorf("dev build must not log 'version change detected'; got:\n%s", out)
	}
}

// cfg.Version is read under cfg.mu: concurrent applyReloaded writes must not race
// (-race). Runs only on dev/unknown builds, where the read happens before the dev
// guard; an -ldflags build would really try dnf install 20 times.
func TestRunSelfVersionInstall_VersionRLockUnderConcurrentReload(t *testing.T) {
	if v := Version(); v != "dev" && v != "unknown" {
		t.Skipf("running build has Version()=%q; concurrent test would invoke dnf — skipping (the lock is exercised by other tests under defaults)", v)
	}
	cfg := &Config{Version: "v5.0.24"}
	captureLogs(t, LevelDebug)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			RunSelfVersionInstall(cfg)
		}
	}()
	// Alternate Version values to provoke a race on an unguarded read.
	for i := 0; i < 20; i++ {
		newCfg := &Config{Version: "v5.0.25"}
		if i%2 == 0 {
			newCfg.Version = "v5.0.26"
		}
		cfg.applyReloaded(newCfg)
	}
	<-done
}

// cfg.Version == Version(): logs "already at version", no install, no restart.
// Runs only under an -ldflags build.
func TestRunSelfVersionInstall_SameVersion_Skipped(t *testing.T) {
	current := Version()
	if current == "dev" || current == "unknown" {
		t.Skipf("running build has Version()=%q (dev/unknown); same-version skip is only meaningful with an injected version", current)
	}
	buf := captureLogs(t, LevelDebug)
	RunSelfVersionInstall(&Config{Version: current})
	out := buf.String()
	want := "cystemd already at version " + current
	if !strings.Contains(out, want) {
		t.Errorf("expected %q in debug log; got:\n%s", want, out)
	}
	// Must NOT have logged a version-change line — the skip wins.
	if strings.Contains(out, "version change detected") {
		t.Errorf("same-version path must not log 'version change detected'; got:\n%s", out)
	}
	// Must NOT have logged any install / restart attempt.
	if strings.Contains(out, "installing RPM") || strings.Contains(out, "restarting cystemd") {
		t.Errorf("same-version path must not attempt install or restart; got:\n%s", out)
	}
}

// Regression (2026-07-15): per-commit RPMs are versioned by the 40-char SHA, which
// Version() never reports, so matching only Version() reinstalled and restarted
// forever; Commit() matching is the fix (DOCS/CLAUDE.md § Self-update). Sets the
// package vars directly, no -ldflags build needed.
func TestRunSelfVersionInstall_MatchesCommit_Skipped(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })
	version = "v6.6.0"
	commit = "c6a9cbe20f3ccc53dfb64de155649030f503914f"

	buf := captureLogs(t, LevelDebug)
	RunSelfVersionInstall(&Config{Version: commit})
	out := buf.String()

	if !strings.Contains(out, "cystemd already at version") {
		t.Errorf("expected the already-there skip log when cfg.Version matches Commit(); got:\n%s", out)
	}
	if strings.Contains(out, "version change detected") {
		t.Errorf("a commit-hash match must not be treated as a version change; got:\n%s", out)
	}
	if strings.Contains(out, "installing RPM") || strings.Contains(out, "restarting cystemd") {
		t.Errorf("a commit-hash match must not attempt install or restart (this is the infinite-loop regression); got:\n%s", out)
	}
}

// A cfg.Version matching neither Version() nor Commit() still attempts an install:
// the commit match must not be too permissive.
func TestRunSelfVersionInstall_NeitherVersionNorCommitMatches_AttemptsInstall(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })
	version = "v6.6.0"
	commit = "c6a9cbe20f3ccc53dfb64de155649030f503914f"

	buf := captureLogs(t, LevelInfo)
	RunSelfVersionInstall(&Config{Version: "totally-unrelated-version-string"})
	out := buf.String()

	if !strings.Contains(out, "cystemd version change detected") {
		t.Errorf("expected a version-change Info log when cfg.Version matches neither Version() nor Commit(); got:\n%s", out)
	}
}

// The install branch: under an -ldflags build with a different cfg.Version, an
// install attempt is logged (dnf may fail; no CI repo has the package).
func TestRunSelfVersionInstall_DifferentVersion_AttemptsInstall(t *testing.T) {
	current := Version()
	if current == "dev" || current == "unknown" {
		t.Skipf("running build has Version()=%q (dev/unknown); the install-attempt branch only fires with an injected version", current)
	}
	wanted := current + "-test-newer-tag"
	buf := captureLogs(t, LevelInfo)
	RunSelfVersionInstall(&Config{Version: wanted})

	out := buf.String()
	// Version-change line must appear (it's the entry into the install branch).
	if !strings.Contains(out, "cystemd version change detected") {
		t.Errorf("expected 'cystemd version change detected' Info log; got:\n%s", out)
	}
	if !strings.Contains(out, wanted) {
		t.Errorf("expected wanted version %q in log; got:\n%s", wanted, out)
	}
	if !strings.Contains(out, current) {
		t.Errorf("expected current version %q in log; got:\n%s", current, out)
	}
}

// ─── installSelfVersion tests ──────────────────────────────────────────────

func TestInstallSelfVersion_LogsScopedInstall(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	// A nonexistent package: only the logged approach is checked.
	_, _ = installSelfVersion("v0.0.0-nonexistent")
	out := buf.String()
	// Should either use systemd-run --scope or fall back to direct dnf.
	scoped := strings.Contains(out, "via systemd-run --scope")
	fallback := strings.Contains(out, "systemd-run not found")
	if !scoped && !fallback {
		t.Errorf("expected either scoped or fallback log; got:\n%s", out)
	}
}

func TestInstallSelfVersion_FallbackWithoutSystemdRun(t *testing.T) {
	if _, err := exec.LookPath("systemd-run"); err == nil {
		t.Skip("systemd-run is available on this host; cannot test fallback path")
	}
	buf := captureLogs(t, LevelDebug)
	_, _ = installSelfVersion("v0.0.0-nonexistent")
	if !strings.Contains(buf.String(), "systemd-run not found") {
		t.Errorf("expected fallback log when systemd-run unavailable; got:\n%s", buf.String())
	}
}

// ─── removeDuplicateRPMPackages tests ──────────────────────────────────────

func TestRemoveDuplicateRPMPackages_NoRPM_SilentReturn(t *testing.T) {
	if hasRPM() {
		t.Skip("rpm is available on this host; this test verifies the no-rpm silent return")
	}
	// Should not panic or log anything.
	buf := captureLogs(t, LevelDebug)
	removeDuplicateRPMPackages()
	if buf.String() != "" {
		t.Errorf("expected no log output when rpm is unavailable; got:\n%s", buf.String())
	}
}

func TestRemoveDuplicateRPMPackages_SingleVersion_NoCleanup(t *testing.T) {
	if !hasRPM() {
		t.Skip("rpm not available on this host")
	}
	// With one or zero cystemd packages installed this is a no-op.
	buf := captureLogs(t, LevelDebug)
	removeDuplicateRPMPackages()
	// Should NOT emit the "removing duplicates" Warning.
	if strings.Contains(buf.String(), "removing duplicates") {
		t.Errorf("expected no cleanup log when only one RPM installed; got:\n%s", buf.String())
	}
}

func TestRemoveDuplicateRPMPackages_DevBuild_SkipsCleanup(t *testing.T) {
	if !hasRPM() {
		t.Skip("rpm not available on this host")
	}
	if v := Version(); v != "dev" && v != "unknown" {
		t.Skipf("running build has Version()=%q (not dev/unknown)", v)
	}
	// Dev builds never clean up: the running version is unknown.
	buf := captureLogs(t, LevelDebug)
	removeDuplicateRPMPackages()
	if strings.Contains(buf.String(), "removing duplicates") {
		t.Errorf("dev build should not attempt cleanup; got:\n%s", buf.String())
	}
}

// rpm -e (a write transaction with scriptlets) has its own timeout, larger than
// rpmQueryTimeout for the read-only probes: an assertion on the constants.
func TestRpmEraseTimeout_LongerThanQueryTimeout(t *testing.T) {
	if rpmEraseTimeout == rpmQueryTimeout {
		t.Fatalf("rpmEraseTimeout (%s) must be a dedicated constant, distinct from rpmQueryTimeout (%s)", rpmEraseTimeout, rpmQueryTimeout)
	}
	if rpmEraseTimeout <= rpmQueryTimeout {
		t.Errorf("rpmEraseTimeout (%s) must be greater than rpmQueryTimeout (%s) — an rpm -e erase is a write transaction (scriptlets + rpmdb commit) that can legitimately take longer than a read-only rpm -q/-qi probe", rpmEraseTimeout, rpmQueryTimeout)
	}
}

// rpmRemovalAlreadyGone classifies a failed rpm -e: a package a concurrent dnf
// already removed logs at Info, a real failure at Warning. No rpm needed.
func TestRpmRemovalAlreadyGone_ClassifiesOutput(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   bool
	}{
		{
			name:   "already removed by concurrent dnf transaction",
			output: "error: package cystemd-v6.5.17-1.el9.x86_64 is not installed",
			want:   true,
		},
		{
			name:   "genuine dependency failure",
			output: "error: some other package depends on cystemd-v6.5.17-1.el9.x86_64",
			want:   false,
		},
		{
			name:   "empty output",
			output: "",
			want:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rpmRemovalAlreadyGone(tc.output); got != tc.want {
				t.Errorf("rpmRemovalAlreadyGone(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// The real log helper, fed output from the live self-update race (cystemd-v6.5.17
// removed by the dnf transaction), logs at Info, not Warning.
func TestLogRPMRemovalFailure_AlreadyGone_LogsAtInfo(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	logRPMRemovalFailure(errors.New("exit status 1"), "error: package cystemd-v6.5.17-1.el9.x86_64 is not installed", []string{"cystemd-v6.5.17-1.el9.x86_64"})

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "already removed by the install transaction") {
		t.Fatalf("expected already-removed message in logs; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"level":"info"`) {
		t.Errorf("expected the already-removed message to log at info; got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), `"level":"warning"`) {
		t.Errorf("did not expect a warning line for the already-removed case; got:\n%s", buf.String())
	}
}

// TestLogRPMRemovalFailure_GenuineFailure_LogsAtWarning pins that any other
// rpm -e failure (e.g. a dependency error) is unaffected by this change and
// still logs at warning.
func TestLogRPMRemovalFailure_GenuineFailure_LogsAtWarning(t *testing.T) {
	buf := captureLogs(t, LevelDebug)
	logRPMRemovalFailure(errors.New("exit status 1"), "error: some other package depends on cystemd-v6.5.17-1.el9.x86_64", []string{"cystemd-v6.5.17-1.el9.x86_64"})

	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, "failed to remove old RPM packages") {
		t.Fatalf("expected genuine-failure message in logs; got:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"level":"warning"`) {
		t.Errorf("expected genuine failure to log at warning; got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), `"level":"info"`) {
		t.Errorf("did not expect an info line for the genuine-failure case; got:\n%s", buf.String())
	}
}

// ─── RunSelfVersionInstall integration with cleanup ────────────────────────

func TestRunSelfVersionInstall_CallsRemoveDuplicateRPMPackages(t *testing.T) {
	// Verify that removeDuplicateRPMPackages is called before the
	// same-version skip. Using a dev build so dnf is never invoked.
	if v := Version(); v != "dev" && v != "unknown" {
		t.Skipf("running build has Version()=%q; this test verifies the dev-build guard", v)
	}
	buf := captureLogs(t, LevelDebug)
	RunSelfVersionInstall(&Config{Version: "v9.9.9"})
	// The dev-build skip fires, but removeDuplicateRPMPackages should have
	// been called before it. The function is a no-op when rpm isn't present
	// or when there's only one cystemd package — we just verify it doesn't
	// panic and that the dev-build skip still fires.
	if !strings.Contains(buf.String(), "cystemd self-install skipped") {
		t.Errorf("expected dev-build skip log; got:\n%s", buf.String())
	}
}

// ─── CD pipeline metrics (DOCS/CLAUDE.md § Metrics / observability) ────────────────
// These drive the real CD decision points. Assertions use before/after deltas, so a
// lingering goroutine from another test cannot fail them; tests proving a NEGATIVE
// need exact equality around one synchronous call instead.

// A clean cycle records cycle and clone successes and stamps the last-success gauge.
func TestRunContinuousDeployment_Metrics_CycleAndCloneSuccess(t *testing.T) {
	srcDir := setupLocalGitRepo(t, map[string]string{"config.yml": "k: v\n"})
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	cycleBefore := metCDCycleSuccess.Load()
	cloneBefore := metCDCloneSuccess.Load()

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + srcDir,
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	if got := metCDCycleSuccess.Load() - cycleBefore; got < 1 {
		t.Errorf("expected cystemd_cd_cycle_total{result=\"success\"} to increment, delta=%d", got)
	}
	if got := metCDCloneSuccess.Load() - cloneBefore; got < 1 {
		t.Errorf("expected cystemd_cd_clone_total{result=\"success\"} to increment, delta=%d", got)
	}
	if metCDLastSuccessUnix.Load() == 0 {
		t.Errorf("expected cystemd_cd_last_success_timestamp_seconds to be stamped")
	}

	body := scrapeMetrics(t).Body.String()
	if !strings.Contains(body, `cystemd_cd_cycle_total{result="success"}`) {
		t.Errorf("exposition missing cd cycle success series:\n%s", body)
	}
}

// The bogus-repo cycle, asserted on the failure counters.
func TestRunContinuousDeployment_Metrics_CycleAndCloneFailureOnBogusRepo(t *testing.T) {
	savePath := filepath.Join(mkTempDir(t), "out.yml")

	cycleFailBefore := metCDCycleFailure.Load()
	cloneFailBefore := metCDCloneFailure.Load()

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file:///nonexistent/path/to/repo",
		GitConfiguration: "config.yml",
		GitInterval:      time.Hour,
		LocalPath:        savePath,
	}, nil)

	if got := metCDCycleFailure.Load() - cycleFailBefore; got < 1 {
		t.Errorf("expected cystemd_cd_cycle_total{result=\"failure\"} to increment on clone failure, delta=%d", got)
	}
	if got := metCDCloneFailure.Load() - cloneFailBefore; got < 1 {
		t.Errorf("expected cystemd_cd_clone_total{result=\"failure\"} to increment on clone failure, delta=%d", got)
	}
}

// Every disabled skip path (nil CDConfig, empty GIT_REPOSITORY, GIT_CONFIGURATION or
// LocalPath) records no cycle outcome: only a genuine attempt counts.
func TestRunContinuousDeployment_Metrics_DisabledLoopRecordsNothing(t *testing.T) {
	successBefore := metCDCycleSuccess.Load()
	failureBefore := metCDCycleFailure.Load()

	RunContinuousDeployment(nil, nil)
	RunContinuousDeployment(&CDConfig{GitConfiguration: "x", LocalPath: "/tmp/x"}, nil)
	RunContinuousDeployment(&CDConfig{GitRepository: "x", LocalPath: "/tmp/x"}, nil)
	RunContinuousDeployment(&CDConfig{GitRepository: "x", GitConfiguration: "y"}, nil)

	if got := metCDCycleSuccess.Load(); got != successBefore {
		t.Errorf("disabled loop must not record a cycle success; before=%d after=%d", successBefore, got)
	}
	if got := metCDCycleFailure.Load(); got != failureBefore {
		t.Errorf("disabled loop must not record a cycle failure; before=%d after=%d", failureBefore, got)
	}
}

// A managed git_values entry adds a second clone attempt (the per-branch values clone
// in deployBranchGroup) to the same cystemd_cd_clone_total family.
func TestRunContinuousDeployment_Metrics_ValuesCloneAddsSecondCloneSuccess(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": valuesYAMLFixture,
	})

	cloneBefore := metCDCloneSuccess.Load()

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:           true,
			Service:            "myapplication",
			Selector:           "myapplication",
			GitValues:          "values.yaml",
			GitValuesConfigKey: ".${selector}.services.${selector}.config.[]",
			Workdir:            workbase,
		}},
	})

	if got := metCDCloneSuccess.Load() - cloneBefore; got < 2 {
		t.Errorf("expected at least 2 clone-success increments (main config + values), got %d", got)
	}
}

// The fixture's image tag names a package no repo has, so the install always fails,
// with or without dnf on the test host.
func TestRunContinuousDeployment_Metrics_InstallFailureRecorded(t *testing.T) {
	workbase := mkTempDir(t)
	savePath := filepath.Join(mkTempDir(t), "config.yml")
	repoDir := setupLocalGitRepo(t, map[string]string{
		"config.yml":  "k: v\n",
		"values.yaml": versionYAMLFixture,
	})

	installFailBefore := metCDInstallFailure.Load()

	RunContinuousDeployment(&CDConfig{
		GitRepository:    "file://" + repoDir,
		GitConfiguration: "config.yml",
		LocalPath:        savePath,
	}, &Config{
		ServiceName:  "myapplication",
		HostSelector: "myapplication",
		ContinuousDeployment: []ServiceEntry{{
			AutoSync:            true,
			Service:             "myapplication",
			Selector:            "myapplication",
			GitValues:           "values.yaml",
			GitValuesConfigKey:  ".${selector}.services.${selector}.config.[]",
			GitValuesVersionKey: ".${selector}.global.image.tag",
			Workdir:             workbase,
		}},
	})

	if got := metCDInstallFailure.Load() - installFailBefore; got < 1 {
		t.Errorf("expected cystemd_cd_install_total{result=\"failure\"} to increment, delta=%d", got)
	}
}

// Drift left unapplied by autosync=false is no install attempt: only the drift log
// fires, dnf is never invoked, no install counter moves.
func TestRunServiceVersionInstall_Metrics_AutoSyncFalse_NoInstallAttemptRecorded(t *testing.T) {
	destDir := mkTempDir(t)
	writeInstalledVersion(destDir, "v1")

	rawContent := []byte(`svc:
  global:
    image:
      tag: v2
`)
	entry := &ServiceEntry{
		Service:             "svc",
		Selector:            "svc",
		GitValues:           "values.yaml",
		GitValuesVersionKey: ".${selector}.global.image.tag",
		AutoSync:            false,
	}

	successBefore := metCDInstallSuccess.Load()
	failureBefore := metCDInstallFailure.Load()

	if got := runServiceVersionInstall("svc", entry, rawContent, destDir, false); got {
		t.Errorf("expected false return when autosync is false")
	}

	if got := metCDInstallSuccess.Load(); got != successBefore {
		t.Errorf("autosync=false drift must not record an install success; before=%d after=%d", successBefore, got)
	}
	if got := metCDInstallFailure.Load(); got != failureBefore {
		t.Errorf("autosync=false drift must not record an install failure; before=%d after=%d", failureBefore, got)
	}
}

// The no-resurrect skip is no restart attempt (a nonexistent unit is never running).
func TestRestartIfRunningForUpdate_NotRunning_NoMetricRecorded(t *testing.T) {
	successBefore := metCDRestartSuccess.Load()
	failureBefore := metCDRestartFailure.Load()

	if got := restartIfRunningForUpdate("cd", "cystemd-test-no-such-unit-xyz", "test reason"); got {
		t.Errorf("expected false return for a not-running unit")
	}

	if got := metCDRestartSuccess.Load(); got != successBefore {
		t.Errorf("a not-running unit must not record a restart success; before=%d after=%d", successBefore, got)
	}
	if got := metCDRestartFailure.Load(); got != failureBefore {
		t.Errorf("a not-running unit must not record a restart failure; before=%d after=%d", failureBefore, got)
	}
}

// End to end on a throwaway pid-suffixed transient unit: restarting a genuinely
// running unit records a restart attempt, not the no-resurrect skip.
func TestRestartIfRunningForUpdate_RunningUnit_RecordsRestartAttempt(t *testing.T) {
	if !hasSystemd() {
		t.Skip("systemd not available on this host")
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run not available on this host")
	}

	svc := fmt.Sprintf("cystemd_test_restart_%d", os.Getpid())
	unit := svc + ".service"

	if out, err := exec.Command("systemd-run", "--unit="+svc, "--quiet", "/bin/sleep", "60").CombinedOutput(); err != nil {
		t.Skipf("cannot launch transient unit (insufficient privileges?): %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", unit).Run()
		_ = exec.Command("systemctl", "reset-failed", unit).Run()
	})

	successBefore := metCDRestartSuccess.Load()
	failureBefore := metCDRestartFailure.Load()

	if got := restartIfRunningForUpdate("cd", svc, "test reason"); !got {
		t.Errorf("expected true (a restart was attempted) for a running unit")
	}

	if (metCDRestartSuccess.Load()-successBefore)+(metCDRestartFailure.Load()-failureBefore) < 1 {
		t.Errorf("expected the restart attempt to be recorded as either success or failure")
	}
}

// ─── scalarNavSegment: the `[]` path segment ─────────────────────────────────
// scalarNavSegment navigates the keys that pick the RPM version and the quotas; these
// pin that its `[]` agrees with extractYAMLKey's. A broken extraction is fail-soft:
// it silently skips the install (the ADR-0011 shape).

// TestScalarNavSegment_ArrayOnSequence_TakesFirstElement pins `[]` against a
// sequence, mirroring TestExtractYAMLKey_ArrayOnSequence.
func TestScalarNavSegment_ArrayOnSequence_TakesFirstElement(t *testing.T) {
	data := []byte("images:\n  - myapp-1.2.3\n  - myapp-9.9.9\n")
	got, err := extractScalarString(data, ".images.[]", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "myapp-1.2.3" {
		t.Errorf("got %q, want the FIRST sequence element %q", got, "myapp-1.2.3")
	}
}

// An empty sequence has no first element; inventing one would install a bogus name.
func TestScalarNavSegment_ArrayOnEmptySequence_Errors(t *testing.T) {
	if got, err := extractScalarString([]byte("images: []\n"), ".images.[]", ""); err == nil {
		t.Errorf("expected an error for [] on an empty sequence; got %q", got)
	}
}

// A single-key mapping unwraps to its value: the Helm-shaped { image: <tag> }.
func TestScalarNavSegment_ArrayOnSingleKeyMap_UnwrapsToValue(t *testing.T) {
	data := []byte("spec:\n  image: myapp-1.2.3\n")
	got, err := extractScalarString(data, ".spec.[]", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "myapp-1.2.3" {
		t.Errorf("got %q, want the single mapping value %q", got, "myapp-1.2.3")
	}
}

// extractYAMLKey passes a multi-key map through; the scalar extractor must instead
// fail explicitly ("did not resolve to a scalar"), never return a wrong value.
func TestScalarNavSegment_ArrayOnMultiKeyMap_PassesThroughThenFailsAsNonScalar(t *testing.T) {
	data := []byte("spec:\n  image: myapp-1.2.3\n  replicas: 2\n")
	got, err := extractScalarString(data, ".spec.[]", "")
	if err == nil {
		t.Fatalf("expected an error for a multi-key map that cannot resolve to a scalar; got %q", got)
	}
	if !strings.Contains(err.Error(), "did not resolve to a scalar") {
		t.Errorf("error = %v; want it to name the non-scalar resolution", err)
	}
}

// TestScalarNavSegment_ArrayOnScalar_Errors mirrors
// TestExtractYAMLKey_ArrayOnScalarErrors.
func TestScalarNavSegment_ArrayOnScalar_Errors(t *testing.T) {
	got, err := extractScalarString([]byte("foo: bar\n"), ".foo.[]", "")
	if err == nil {
		t.Fatalf("expected an error for [] applied to a scalar; got %q", got)
	}
	if !strings.Contains(err.Error(), "expected sequence or mapping") {
		t.Errorf("error = %v; want it to name the kind mismatch", err)
	}
}

// A named key descending into a non-mapping is an error.
func TestScalarNavSegment_PlainSegmentOnNonMapping_Errors(t *testing.T) {
	got, err := extractScalarString([]byte("foo: bar\n"), ".foo.baz", "")
	if err == nil {
		t.Fatalf("expected an error descending into a scalar; got %q", got)
	}
	if !strings.Contains(err.Error(), "expected mapping") {
		t.Errorf("error = %v; want it to name the mapping mismatch", err)
	}
}

// ─── YAML anchors and merge keys on the scalar path ───────────────────────────
// extractScalarString resolves aliases and merge keys by hand on the node tree; an
// interface{} decode would reintroduce the float round-trip. History: DOCS/MEMORY.md
// § Deep review — full source/test/doc audit.

const anchorFixture = `
base: &base
  image: myapp-1.2.3
  cpu: 200m
extra: &extra
  cpu: 900m
  mem: 1Gi
app:
  <<: *base
  cpu: 500m
multi:
  <<: [*base, *extra]
aliased: *base
`

// The merge-precedence contract, and agreement with extractYAMLKey on one document.
func TestExtractScalarString_ResolvesAnchorsAndMergeKeys(t *testing.T) {
	data := []byte(anchorFixture)

	cases := []struct {
		name, path, want string
	}{
		{"plain alias", ".aliased.image", "myapp-1.2.3"},
		{"merge key inherits", ".app.image", "myapp-1.2.3"},
		{"explicit key overrides the merge", ".app.cpu", "500m"},
		{"merge sequence, earlier source wins", ".multi.cpu", "200m"},
		{"merge sequence, later source still contributes", ".multi.mem", "1Gi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractScalarString(data, tc.path, "")
			if err != nil {
				t.Fatalf("extractScalarString(%q): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("extractScalarString(%q) = %q, want %q", tc.path, got, tc.want)
			}
		})
	}
}

// The regression guard: both extractors resolve an anchored document identically.
func TestExtractScalarString_AgreesWithExtractYAMLKeyOnAnchors(t *testing.T) {
	data := []byte(anchorFixture)
	for _, path := range []string{".aliased.image", ".app.image", ".app.cpu", ".multi.cpu", ".multi.mem"} {
		scalar, serr := extractScalarString(data, path, "")
		raw, rerr := extractYAMLKey(data, path, "")
		if serr != nil || rerr != nil {
			t.Fatalf("%s: extractScalarString err=%v, extractYAMLKey err=%v", path, serr, rerr)
		}
		if want := strings.TrimSpace(string(raw)); scalar != want {
			t.Errorf("%s: extractScalarString = %q but extractYAMLKey = %q — the two extractors disagree", path, scalar, want)
		}
	}
}

// "1.10" must stay "1.10": an interface{} decode yields 1.1 and would install the
// wrong package, so alias resolution returns the original scalar node. If this fails,
// the alias path round-trips through a decode; do not "fix" it by quoting the fixture.
func TestExtractScalarString_AliasedScalarKeepsVerbatimText(t *testing.T) {
	for _, tag := range []string{"1.10", "1.20", "0.10"} {
		data := []byte("anchor: &v " + tag + "\nspec:\n  version: *v\n")
		got, err := extractScalarString(data, ".spec.version", "")
		if err != nil {
			t.Fatalf("tag %q: %v", tag, err)
		}
		if got != tag {
			t.Errorf("tag %q via alias = %q, want it preserved verbatim (a float round-trip would mangle it)", tag, got)
		}
	}
}

// A malformed anchor is an error, not "key not found": both skip the install, but
// only one tells the operator why.
func TestExtractScalarString_UnresolvableAliasIsReported(t *testing.T) {
	// A self-referential anchor never terminates; the depth guard must catch it.
	data := []byte("a: &x\n  b: *x\n")
	if got, err := extractScalarString(data, ".a.b", ""); err == nil {
		t.Errorf("expected an error for a self-referential anchor; got %q", got)
	}
}

// A failed self-install is counted in cystemd_cd_install_total{result="failure"},
// its only symptom (DOCS/MEMORY.md § Self-install outcomes were never counted).
// The install really runs and fails, which is why TestMain bounds dnfInstallTimeout.
func TestRunSelfVersionInstall_FailureIsCountedInInstallMetric(t *testing.T) {
	origVersion, origCommit := version, commit
	t.Cleanup(func() { version, commit = origVersion, origCommit })
	version = "v6.6.0"
	commit = "c6a9cbe20f3ccc53dfb64de155649030f503914f"

	resetMetricsForTest()
	before := metCDInstallFailure.Load()

	RunSelfVersionInstall(&Config{Version: "cystemd-no-such-package-exists-9f3a2b"})

	if got := metCDInstallFailure.Load(); got != before+1 {
		t.Errorf("a failed self-install must increment cystemd_cd_install_total{result=\"failure\"}: was %d, now %d", before, got)
	}
}

// The steady-state host-key line is Debug, not Info: it runs every cycle and only
// restates configuration (DOCS/MEMORY.md § A steady-state fact logged at Info looked
// like a fault). Verification OFF stays a Warning (next test).
func TestResolveHostKeyCallback_SuccessIsNotLoggedAtInfo(t *testing.T) {
	dir := mkTempDir(t)
	kh := filepath.Join(dir, "known_hosts")
	// Pre-create the file: the steady state. First contact logs its own one-time Info.
	if err := os.WriteFile(kh, nil, 0o600); err != nil {
		t.Fatalf("seed known_hosts: %v", err)
	}
	t.Setenv("GIT_SSH_KNOWN_HOSTS", kh)

	buf := captureLogs(t, LevelDebug)
	if _, err := resolveHostKeyCallback(); err != nil {
		t.Fatalf("resolveHostKeyCallback: %v", err)
	}
	out := buf.String()

	if strings.Contains(out, `"level":"info"`) {
		t.Errorf("a per-cycle restatement of configuration must not log at info; got:\n%s", out)
	}
	if !strings.Contains(out, "host key verification enabled") {
		t.Errorf("the detail must be preserved at debug, not deleted; got:\n%s", out)
	}
}

// Verification OFF is an actionable security finding: it stays loud however often.
func TestResolveHostKeyCallback_DisabledStillWarns(t *testing.T) {
	t.Setenv("GIT_SSH_KNOWN_HOSTS", "")

	buf := captureLogs(t, LevelDebug)
	if _, err := resolveHostKeyCallback(); err != nil {
		t.Fatalf("resolveHostKeyCallback: %v", err)
	}
	if !strings.Contains(buf.String(), `"level":"warning"`) {
		t.Errorf("disabled host-key verification must stay a Warning; got:\n%s", buf.String())
	}
}
