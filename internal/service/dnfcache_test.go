package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubMakecache replaces the exec seam for one test and records every
// invocation's argument vector. Restores the real command on cleanup.
func stubMakecache(t *testing.T, out []byte, err error) *[][]string {
	t.Helper()
	calls := &[][]string{}
	orig := makecacheCommand
	t.Cleanup(func() { makecacheCommand = orig })
	makecacheCommand = func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, args)
		return out, err
	}
	return calls
}

// resetDNFCacheForTest starts a test from the unconfigured (disabled) default
// and restores the previous settings on cleanup.
func resetDNFCacheForTest(t *testing.T) {
	t.Helper()
	orig := dnfCache.Load()
	t.Cleanup(func() { dnfCache.Store(orig) })
	dnfCache.Store(nil)
}

// ─── ResolveMakecacheRepos ─────────────────────────────────────────────────

// TestResolveMakecacheRepos_DefaultsToDisabled pins the no-hardcode rule: with
// neither env nor config the list is empty — no repo ID is compiled in.
func TestResolveMakecacheRepos_DefaultsToDisabled(t *testing.T) {
	if got := ResolveMakecacheRepos("", nil); len(got) != 0 {
		t.Errorf("no env and no config must disable the refresh (no repo may be hardcoded); got %v", got)
	}
}

func TestResolveMakecacheRepos_EnvWinsOverConfig(t *testing.T) {
	got := ResolveMakecacheRepos("fromenv", []string{"fromconfig"})
	if len(got) != 1 || got[0] != "fromenv" {
		t.Errorf("env must win over config.yml; got %v", got)
	}
}

func TestResolveMakecacheRepos_FallsBackToConfig(t *testing.T) {
	got := ResolveMakecacheRepos("", []string{"repo-a", "repo-b"})
	if len(got) != 2 || got[0] != "repo-a" || got[1] != "repo-b" {
		t.Errorf("config list must be used when env is empty; got %v", got)
	}
}

// TestResolveMakecacheRepos_TrimsAndDropsEmpties covers the shape an operator
// actually types into an env var: spaces after commas, and a trailing comma.
func TestResolveMakecacheRepos_TrimsAndDropsEmpties(t *testing.T) {
	got := ResolveMakecacheRepos("  repo-a , , repo-b ,", nil)
	if len(got) != 2 || got[0] != "repo-a" || got[1] != "repo-b" {
		t.Errorf("entries must be trimmed and empties dropped; got %v", got)
	}
}

// TestResolveMakecacheRepos_WhitespaceOnlyEnvFallsBackToConfig pins that a set
// but blank env var falls through to config instead of disabling the refresh.
func TestResolveMakecacheRepos_WhitespaceOnlyEnvFallsBackToConfig(t *testing.T) {
	got := ResolveMakecacheRepos("   ", []string{"repo-a"})
	if len(got) != 1 || got[0] != "repo-a" {
		t.Errorf("a blank env var must fall through to config, not disable the refresh; got %v", got)
	}
}

// ─── ResolveMakecacheTimeout ───────────────────────────────────────────────

func TestResolveMakecacheTimeout_Precedence(t *testing.T) {
	cases := []struct {
		name, env, cfg string
		want           time.Duration
	}{
		{"env wins", "5s", "30s", 5 * time.Second},
		{"config when env empty", "", "30s", 30 * time.Second},
		{"default when both empty", "", "", DefaultDNFMakecacheTimeout},
		{"invalid falls back", "not-a-duration", "", DefaultDNFMakecacheTimeout},
		// Not a disable switch here (the repo list is): a zero deadline always fails.
		{"zero falls back", "0s", "", DefaultDNFMakecacheTimeout},
		{"negative falls back", "-5s", "", DefaultDNFMakecacheTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ResolveMakecacheTimeout(c.env, c.cfg); got != c.want {
				t.Errorf("ResolveMakecacheTimeout(%q, %q) = %s, want %s", c.env, c.cfg, got, c.want)
			}
		})
	}
}

// ─── refreshRepoMetadata ───────────────────────────────────────────────────

// TestRefreshRepoMetadata_NoopWhenUnconfigured pins the default: an operator
// who has not opted in must never have dnf spawned on their install path.
func TestRefreshRepoMetadata_NoopWhenUnconfigured(t *testing.T) {
	resetDNFCacheForTest(t)
	calls := stubMakecache(t, nil, nil)

	refreshRepoMetadata()

	if len(*calls) != 0 {
		t.Errorf("refresh must not run any command when no repos are configured; got %v", *calls)
	}
}

// TestRefreshRepoMetadata_ScopesToConfiguredRepos pins one scoped makecache per
// configured repo; it fails if simplified to a blanket refresh. See ADR-0015.
func TestRefreshRepoMetadata_ScopesToConfiguredRepos(t *testing.T) {
	resetDNFCacheForTest(t)
	calls := stubMakecache(t, []byte("Metadata cache created."), nil)
	SetDNFCacheSettings([]string{"repo-a", "repo-b"}, time.Minute)

	refreshRepoMetadata()

	// One call per repo, never a comma-joined --enablerepo.
	if len(*calls) != 2 {
		t.Fatalf("expected one makecache call per configured repo (2), got %d: %v", len(*calls), *calls)
	}
	for i, want := range []string{
		"makecache --disablerepo=* --enablerepo=repo-a",
		"makecache --disablerepo=* --enablerepo=repo-b",
	} {
		if got := strings.Join((*calls)[i], " "); got != want {
			t.Errorf("call %d must be scoped to a single repo.\n got: %s\nwant: %s", i, got, want)
		}
	}
}

// TestRefreshRepoMetadata_UnknownRepoDoesNotBlockTheOthers pins that a repo ID
// unknown on this host cannot stop the others, so one config value fits hosts
// that name the mirror differently. See ADR-0015.
func TestRefreshRepoMetadata_UnknownRepoDoesNotBlockTheOthers(t *testing.T) {
	resetDNFCacheForTest(t)
	var attempted []string
	orig := makecacheCommand
	t.Cleanup(func() { makecacheCommand = orig })
	makecacheCommand = func(_ context.Context, args ...string) ([]byte, error) {
		repo := strings.TrimPrefix(args[len(args)-1], "--enablerepo=")
		attempted = append(attempted, repo)
		if repo == "absent-here" {
			return []byte("Error: Unknown repo: 'absent-here'"), errors.New("exit status 1")
		}
		return []byte("Metadata cache created."), nil
	}
	SetDNFCacheSettings([]string{"absent-here", "present-here"}, time.Minute)

	refreshRepoMetadata()

	if len(attempted) != 2 {
		t.Fatalf("both repos must be attempted independently; got %v", attempted)
	}
	if attempted[1] != "present-here" {
		t.Errorf("a repo that does not exist on this host must not stop the one that does; attempted %v", attempted)
	}
}

// TestRefreshRepoMetadata_FailureIsSwallowed pins the fail-soft contract: a
// failed refresh logs below Error and the install proceeds on cached metadata.
func TestRefreshRepoMetadata_FailureIsSwallowed(t *testing.T) {
	resetDNFCacheForTest(t)
	stubMakecache(t, []byte("Errors during downloading metadata"), errors.New("exit status 1"))
	SetDNFCacheSettings([]string{"repo-a"}, time.Minute)

	buf := captureLogs(t, LevelDebug)
	refreshRepoMetadata() // must simply return; a panic or hang fails the test.
	out := buf.String()

	if !strings.Contains(out, "continuing with cached metadata") {
		t.Errorf("a failed refresh must say it is continuing anyway; got:\n%s", out)
	}
	if strings.Contains(out, `"level":"error"`) {
		t.Errorf("a failed refresh is an expected, self-correcting condition and must not log at error level; got:\n%s", out)
	}
}

// TestRefreshRepoMetadata_PassesConfiguredTimeout checks the deadline actually
// reaches the command, so the timeout knob is not decorative.
func TestRefreshRepoMetadata_PassesConfiguredTimeout(t *testing.T) {
	resetDNFCacheForTest(t)
	var gotDeadline time.Duration
	orig := makecacheCommand
	t.Cleanup(func() { makecacheCommand = orig })
	makecacheCommand = func(ctx context.Context, _ ...string) ([]byte, error) {
		if dl, ok := ctx.Deadline(); ok {
			gotDeadline = time.Until(dl)
		}
		return nil, nil
	}
	SetDNFCacheSettings([]string{"repo-a"}, 5*time.Second)

	refreshRepoMetadata()

	if gotDeadline <= 0 || gotDeadline > 5*time.Second {
		t.Errorf("configured timeout must bound the command; remaining deadline was %s", gotDeadline)
	}
}

// TestSetDNFCacheSettings_NonPositiveTimeoutTakesDefault pins that the holder
// itself rejects a zero deadline, even from a caller bypassing the resolver.
func TestSetDNFCacheSettings_NonPositiveTimeoutTakesDefault(t *testing.T) {
	resetDNFCacheForTest(t)
	SetDNFCacheSettings([]string{"repo-a"}, 0)
	if got := currentDNFCacheSettings().Timeout; got != DefaultDNFMakecacheTimeout {
		t.Errorf("a non-positive timeout must take the default; got %s", got)
	}
}
