package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"gopkg.in/yaml.v3"
)

// ─── JSON shapes ────────────────────────────────────────────────────────────

// EnvDiffJSON lists the .env variable NAMES that differ between the on-disk
// file and a fresh Vault read — NEVER a value (the /env redaction contract).
// Fields stay in JSON-tag order.
type EnvDiffJSON struct {
	Added   []string `json:"added"`
	Changed []string `json:"changed"`
	Removed []string `json:"removed"`
}

// VersionDiffJSON reports the installed vs. upstream-declared image/version
// tag for one managed service.
type VersionDiffJSON struct {
	Changed bool   `json:"changed"`
	Current string `json:"current"`
	Wanted  string `json:"wanted"`
}

// ServiceDiffJSON is the per-service /diff and /lastdiff payload: Config holds
// one line per differing YAML property (computeConfigDiff); ComputedAt
// (RFC3339) is when a facet was last recorded. Fields stay in JSON-tag order.
type ServiceDiffJSON struct {
	ComputedAt string          `json:"computed_at"`
	Config     []string        `json:"config"`
	Env        EnvDiffJSON     `json:"env"`
	Service    string          `json:"service"`
	Version    VersionDiffJSON `json:"version"`
}

// DiffResponseJSON is the top-level payload for /diff and /lastdiff.
type DiffResponseJSON struct {
	GeneratedAt string            `json:"generated_at"`
	Services    []ServiceDiffJSON `json:"services"`
}

// ─── diff concurrency limit ────────────────────────────────────────────────────

// diffConcurrencyLimit caps concurrent /diff computations (live clones + Vault
// reads); handleDiff try-acquires diffConcurrencySem and returns busy-500 when
// full, never queuing. Never use syncMu here: /diff must not block on CD or /sync.
// Why: DOCS/CLAUDE.md § `/diff` and `/lastdiff` — drift visibility
const diffConcurrencyLimit = 2

var diffConcurrencySem = make(chan struct{}, diffConcurrencyLimit)

// ─── last-diff cache ────────────────────────────────────────────────────────

// lastDiffCache holds the latest diff per managed service, keyed by Service
// and written only through the record*Diff facet recorders (CD, the secrets
// fetch, /diff). Guarded by lastDiffMu.
var (
	lastDiffMu    sync.RWMutex
	lastDiffCache = make(map[string]ServiceDiffJSON)
)

// recordConfigDiff computes and caches svc's config facet. deployServiceFromTree
// calls it at its drift check on every CD cycle, regardless of autosync (always
// read/compare; only writes are gated — ADR-0003).
func recordConfigDiff(svc string, oldBytes, newBytes []byte) {
	lines := computeConfigDiff(oldBytes, newBytes)
	lastDiffMu.Lock()
	defer lastDiffMu.Unlock()
	e := lastDiffCache[svc]
	e.Service = svc
	e.Config = lines
	e.ComputedAt = time.Now().Format(time.RFC3339)
	lastDiffCache[svc] = e
}

// recordVersionDiff caches svc's version facet. Callers derive changed from
// the rpm database (currentServiceVersion, ADR-0011), before any autosync gate:
// diffServiceFromTree on every compare, runServiceVersionInstall only when the
// wanted version is not installed.
func recordVersionDiff(svc, current, wanted string, changed bool) {
	lastDiffMu.Lock()
	defer lastDiffMu.Unlock()
	e := lastDiffCache[svc]
	e.Service = svc
	e.Version = VersionDiffJSON{Changed: changed, Current: current, Wanted: wanted}
	e.ComputedAt = time.Now().Format(time.RFC3339)
	lastDiffCache[svc] = e
}

// recordEnvDiff computes and caches svc's env facet (names only). Callers pass
// the on-disk .env as oldPairs BEFORE any rewrite and the fresh Vault map as
// newPairs.
func recordEnvDiff(svc string, oldPairs, newPairs map[string]string) {
	d := computeEnvDiff(oldPairs, newPairs)
	lastDiffMu.Lock()
	defer lastDiffMu.Unlock()
	e := lastDiffCache[svc]
	e.Service = svc
	e.Env = d
	e.ComputedAt = time.Now().Format(time.RFC3339)
	lastDiffCache[svc] = e
}

// getLastDiffs returns the cached diff of each service, in order. Missing
// entries AND the unrecorded facets of partial entries (e.g. no env facet on a
// Vault-degraded host) get empty slices, so the JSON shows [] and never null.
func getLastDiffs(services []string) []ServiceDiffJSON {
	lastDiffMu.RLock()
	defer lastDiffMu.RUnlock()
	out := make([]ServiceDiffJSON, 0, len(services))
	for _, svc := range services {
		e, ok := lastDiffCache[svc]
		if !ok {
			e.Service = svc
		}
		if e.Config == nil {
			e.Config = []string{}
		}
		if e.Env.Added == nil {
			e.Env.Added = []string{}
		}
		if e.Env.Changed == nil {
			e.Env.Changed = []string{}
		}
		if e.Env.Removed == nil {
			e.Env.Removed = []string{}
		}
		out = append(out, e)
	}
	return out
}

// resetLastDiffCacheForTest clears the package-global cache so no test sees
// another's diffs.
func resetLastDiffCacheForTest() {
	lastDiffMu.Lock()
	defer lastDiffMu.Unlock()
	lastDiffCache = make(map[string]ServiceDiffJSON)
}

// ─── pure diff computation ──────────────────────────────────────────────────

// computeConfigDiff compares oldBytes (nil/empty when the file does not
// exist yet — everything is reported as added) against newBytes and returns
// one line per differing YAML property, sorted by dotted key path:
//
//	"+ path.to.key: value"       present only in newBytes
//	"- path.to.key: value"       present only in oldBytes
//	"~ path.to.key: old -> new"  present in both, value differs
//
// Falls back to diffRawLines (whole lines, +/- only) when either side is not a
// YAML mapping or sequence — e.g. a verbatim non-YAML (Alloy/River) config.
func computeConfigDiff(oldBytes, newBytes []byte) []string {
	if bytes.Equal(oldBytes, newBytes) {
		return []string{}
	}
	oldProps, oldErr := flattenYAMLProperties(oldBytes)
	newProps, newErr := flattenYAMLProperties(newBytes)
	if oldErr != nil || newErr != nil {
		return diffRawLines(string(oldBytes), string(newBytes))
	}
	return diffProperties(oldProps, newProps)
}

// flattenYAMLProperties flattens YAML into "dotted.path" -> scalar-string
// pairs (blank input → empty map). Errors when data is not YAML or its root is
// not a mapping or sequence; the caller then falls back to a line diff.
func flattenYAMLProperties(data []byte) (map[string]string, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]string{}, nil
	}
	var root interface{}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	switch root.(type) {
	case map[string]interface{}, []interface{}:
		out := make(map[string]string)
		flattenYAMLNode("", root, out)
		return out, nil
	default:
		return nil, fmt.Errorf("not a structured YAML document (map or sequence)")
	}
}

// flattenYAMLNode writes one prefix -> value entry per leaf into out. Empty
// maps/sequences become "{}"/"[]" so an emptied section still shows as a change.
func flattenYAMLNode(prefix string, node interface{}, out map[string]string) {
	switch v := node.(type) {
	case map[string]interface{}:
		if len(v) == 0 {
			out[prefix] = "{}"
			return
		}
		for k, val := range v {
			key := k
			if prefix != "" {
				key = prefix + "." + k
			}
			flattenYAMLNode(key, val, out)
		}
	case []interface{}:
		if len(v) == 0 {
			out[prefix] = "[]"
			return
		}
		for i, val := range v {
			flattenYAMLNode(fmt.Sprintf("%s[%d]", prefix, i), val, out)
		}
	case nil:
		out[prefix] = "null"
	default:
		out[prefix] = fmt.Sprintf("%v", v)
	}
}

// diffProperties merges two flattened-YAML property maps into a sorted,
// prefixed diff (see computeConfigDiff for the line convention).
func diffProperties(oldProps, newProps map[string]string) []string {
	keySet := make(map[string]struct{}, len(oldProps)+len(newProps))
	for k := range oldProps {
		keySet[k] = struct{}{}
	}
	for k := range newProps {
		keySet[k] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := []string{}
	for _, k := range keys {
		oldVal, oldOK := oldProps[k]
		newVal, newOK := newProps[k]
		switch {
		case oldOK && newOK && oldVal != newVal:
			out = append(out, fmt.Sprintf("~ %s: %s -> %s", k, oldVal, newVal))
		case !oldOK && newOK:
			out = append(out, fmt.Sprintf("+ %s: %s", k, newVal))
		case oldOK && !newOK:
			out = append(out, fmt.Sprintf("- %s: %s", k, oldVal))
		}
	}
	return out
}

// diffRawLines is the non-YAML fallback: a set-based diff of non-blank lines,
// added/removed only (opaque text has no per-property "changed").
func diffRawLines(oldText, newText string) []string {
	oldLines := splitNonEmptyLines(oldText)
	newLines := splitNonEmptyLines(newText)
	oldSet := make(map[string]bool, len(oldLines))
	for _, l := range oldLines {
		oldSet[l] = true
	}
	newSet := make(map[string]bool, len(newLines))
	for _, l := range newLines {
		newSet[l] = true
	}

	out := []string{}
	for _, l := range oldLines {
		if !newSet[l] {
			out = append(out, "- "+l)
		}
	}
	for _, l := range newLines {
		if !oldSet[l] {
			out = append(out, "+ "+l)
		}
	}
	return out
}

// splitNonEmptyLines splits s on newlines, trims a trailing \r, and drops
// blank/whitespace-only lines.
func splitNonEmptyLines(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimRight(l, "\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	return out
}

// computeEnvDiff returns the sorted variable NAMES added, changed or removed
// between the on-disk .env pairs and a fresh Vault map. Values only feed the
// compare and never appear in the result (the /env redaction contract).
func computeEnvDiff(oldPairs, newPairs map[string]string) EnvDiffJSON {
	added := []string{}
	changed := []string{}
	removed := []string{}
	for k, newVal := range newPairs {
		if oldVal, ok := oldPairs[k]; !ok {
			added = append(added, k)
		} else if oldVal != newVal {
			changed = append(changed, k)
		}
	}
	for k := range oldPairs {
		if _, ok := newPairs[k]; !ok {
			removed = append(removed, k)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return EnvDiffJSON{Added: added, Changed: changed, Removed: removed}
}

// ─── live, read-only diff (the /diff engine) ───────────────────────────────

// ComputeCurrentDiffs is the /diff engine: it diffs every managed service
// against git values and Vault, records the results into lastDiffCache and
// returns them. It writes, installs and restarts nothing (own temp clones), so
// it takes no syncMu. Errors only when this host manages no service.
func ComputeCurrentDiffs(appCfg *Config) ([]ServiceDiffJSON, error) {
	managed := appCfg.managedEntries()
	if len(managed) == 0 {
		Warningf("diff: no managed services resolved for this host — nothing to diff")
		return nil, fmt.Errorf("no managed service is configured for this host")
	}

	gitRepo := strings.TrimSpace(os.Getenv("GIT_REPOSITORY"))
	switch {
	case gitRepo == "":
		Warningf("diff: GIT_REPOSITORY is empty — skipping config/version diff")
	default:
		auth, err := resolveCloneSSHAuth()
		if err != nil {
			Warningf("diff: %v — skipping config/version diff", err)
		} else {
			diffManagedServiceValues(gitRepo, auth, managed)
		}
	}

	if state := GetVaultClient(); state == nil || state.Client == nil {
		Warningf("diff: Vault client not available (degraded or not configured) — skipping env diff")
	} else {
		for _, entry := range managed {
			diffServiceEnv(entry)
		}
	}

	return getLastDiffs(serviceNames(managed)), nil
}

// diffManagedServiceValues clones each distinct git_branch of gitRepo once
// (groupEntriesByBranch, as CD does) and records every entry's config and
// version diff. Read-only.
func diffManagedServiceValues(gitRepo string, auth *gitssh.PublicKeys, entries []ServiceEntry) {
	for _, g := range groupEntriesByBranch(entries) {
		diffBranchGroup(gitRepo, g.branch, auth, g.entries)
	}
}

// diffBranchGroup clones gitRepo@branch once into a temp dir (removed before
// returning) and diffs every entry on it; a clone failure logs once and skips
// the branch. nil onCloneResult: cystemd_cd_clone_total counts CD clones only.
// Only the clone scaffolding is shared with CD — never call deployServiceFromTree.
func diffBranchGroup(gitRepo, branch string, auth *gitssh.PublicKeys, entries []ServiceEntry) {
	cloneBranchTreeOnce(gitRepo, branch, auth, len(entries), "cystemd_diff_*", "diff", nil,
		func(treeDir string) {
			for _, entry := range entries {
				diffServiceFromTree(treeDir, entry)
			}
		})
}

// diffServiceFromTree records one service's config and version diff from an
// already-cloned tree, mirroring deployServiceFromTree's extraction; it never
// writes a file.
func diffServiceFromTree(treeDir string, entry ServiceEntry) {
	svcName := entry.Service
	destDir := serviceDir(entry)

	rawContent, err := os.ReadFile(filepath.Join(treeDir, entry.GitValues))
	if err != nil {
		Warningf("diff: cannot read %q for %s: %v — skipping config/version diff", entry.GitValues, svcName, err)
		return
	}

	var newContent []byte
	var destFile string
	if entry.GitValuesConfigKey != "" {
		sub := effectiveSelector(entry)
		extracted, err := extractYAMLKey(rawContent, entry.GitValuesConfigKey, sub)
		if err != nil {
			Warningf("diff: cannot extract key %q from %q for %s: %v — skipping config diff",
				entry.GitValuesConfigKey, entry.GitValues, svcName, err)
		} else {
			newContent = extracted
			destFile = filepath.Join(destDir, serviceServiceConfig(entry))
		}
	} else {
		newContent = rawContent
		destFile = filepath.Join(destDir, "values.yaml")
	}
	if destFile != "" {
		existing, _ := os.ReadFile(destFile) // missing file → nil, "everything added"
		recordConfigDiff(svcName, existing, newContent)
	}

	if entry.GitValuesVersionKey != "" {
		sub := effectiveSelector(entry)
		wanted, err := extractVersionString(rawContent, entry.GitValuesVersionKey, sub)
		if err != nil {
			Warningf("diff: cannot extract version key %q for %s: %v — skipping version diff",
				entry.GitValuesVersionKey, svcName, err)
		} else {
			// rpm is the truth (ADR-0011); atWanted covers both "not installed"
			// and "another version". Unknown → record nothing: a fabricated
			// "no drift" is worse than a gap.
			current, atWanted, known := currentServiceVersion(svcName, wanted)
			if !known {
				Warningf("diff: %s version diff skipped — rpm database could not be consulted", svcName)
			} else {
				recordVersionDiff(svcName, current, wanted, !atWanted)
			}
		}
	}
}

// diffServiceEnv records one service's .env key diff against a fresh Vault
// read; it never writes, and the fetched values only feed the compare. Safe
// without syncMu: fetchServiceSecrets replaces .env by same-directory rename,
// so this read sees the whole old file or the whole new one, never a torn one.
func diffServiceEnv(entry ServiceEntry) {
	svcName := entry.Service
	if svcName == "" || entry.Secrets == "" {
		return
	}
	state := GetVaultClient()
	if state == nil || state.Client == nil {
		return
	}

	destDir := serviceDir(entry)
	destFile := filepath.Join(destDir, ".env")
	oldPairs, ferr := ParseEnvFile(destFile)
	if ferr != nil {
		oldPairs = map[string]string{}
	}

	newSecrets, err := readSecretsV2ThenV1(state.Client, entry.Secrets)
	if err != nil {
		Warningf("diff: failed to read service secrets for %q at %q: %v — skipping env diff",
			svcName, entry.Secrets, err)
		return
	}

	recordEnvDiff(svcName, oldPairs, newSecrets)
}
