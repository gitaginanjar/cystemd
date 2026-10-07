package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_ValidFull(t *testing.T) {
	content := `
listen_address: ":8080"
log_level: info
auth:
  enabled: true
  public_key_file: "/tmp/key.pub"
node_pools:
  - selector: mypool
    nodes:
      - HOST001
      - HOST002
continuous_deployment:
  - service: myproject_myapp
    selector: mypool
    replica: 1
  - service: myproject_otherapp
    selector: otherapp
    replica: 2
`
	cfg := mustWriteTempConfig(t, content)

	if cfg.ListenAddress != ":8080" {
		t.Errorf("ListenAddress: got %q, want %q", cfg.ListenAddress, ":8080")
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel: got %q, want %q (explicit override)", cfg.LogLevel, "info")
	}
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled: got false, want true")
	}
	if cfg.Auth.PublicKeyFile != "/tmp/key.pub" {
		t.Errorf("Auth.PublicKeyFile: got %q, want %q", cfg.Auth.PublicKeyFile, "/tmp/key.pub")
	}
	if len(cfg.NodePools) != 1 {
		t.Fatalf("NodePools length: got %d, want 1", len(cfg.NodePools))
	}
	if cfg.NodePools[0].Selector != "mypool" {
		t.Errorf("NodePools[0].Selector: got %q, want %q", cfg.NodePools[0].Selector, "mypool")
	}
	if len(cfg.NodePools[0].Nodes) != 2 {
		t.Errorf("NodePools[0].Nodes length: got %d, want 2", len(cfg.NodePools[0].Nodes))
	}
	// AllowedServices = all services from continuous_deployment, regardless of hostname match
	if len(cfg.AllowedServices) != 2 {
		t.Errorf("AllowedServices length: got %d, want 2", len(cfg.AllowedServices))
	}
	// ServiceName may be empty if the test host is not HOST001/HOST002
}

func TestLoadConfig_DefaultLogLevelIsDebug(t *testing.T) {
	cfg := mustWriteTempConfig(t, `log_level: ""`)

	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel default: got %q, want %q", cfg.LogLevel, "debug")
	}
}

func TestLoadConfig_DefaultListenAddress(t *testing.T) {
	cfg := mustWriteTempConfig(t, `log_level: debug`)

	if cfg.ListenAddress != ":50080" {
		t.Errorf("ListenAddress default: got %q, want %q", cfg.ListenAddress, ":50080")
	}
}

func TestLoadConfig_EmptyFile(t *testing.T) {
	cfg := mustWriteTempConfig(t, "")

	if cfg.ListenAddress != ":50080" {
		t.Errorf("ListenAddress default on empty file: got %q, want %q", cfg.ListenAddress, ":50080")
	}
	if cfg.ServiceName != "" {
		t.Errorf("ServiceName on empty file: got %q, want empty", cfg.ServiceName)
	}
	if cfg.Auth.Enabled {
		t.Error("Auth.Enabled on empty file: got true, want false")
	}
}

func TestLoadConfig_AuthDisabledByDefault(t *testing.T) {
	cfg := mustWriteTempConfig(t, `log_level: debug`)

	if cfg.Auth.Enabled {
		t.Error("Auth.Enabled should default to false")
	}
}

func TestLoadConfig_FileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/path/no_such_config.yml")
	if err == nil {
		t.Error("expected error for missing config file, got nil")
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	f := mustWriteTemp(t, ":::not: valid: yaml:::")
	defer os.Remove(f)

	_, err := LoadConfig(f)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

func TestLoadConfig_PartialConfig(t *testing.T) {
	content := `
log_level: info
auth:
  enabled: false
`
	cfg := mustWriteTempConfig(t, content)

	// No node_pools → no hostname match → ServiceName empty
	if cfg.ServiceName != "" {
		t.Errorf("ServiceName: got %q, want empty (no node_pools)", cfg.ServiceName)
	}
	if cfg.ListenAddress != ":50080" {
		t.Errorf("ListenAddress: got %q, want default :50080", cfg.ListenAddress)
	}
	if len(cfg.AllowedServices) != 0 {
		t.Errorf("AllowedServices: expected empty (no continuous_deployment), got %v", cfg.AllowedServices)
	}
}

func TestLoadConfig_VaultRefreshInterval(t *testing.T) {
	content := `
vault:
  refresh_interval: 30s
`
	cfg := mustWriteTempConfig(t, content)
	if cfg.Vault.RefreshInterval != "30s" {
		t.Errorf("Vault.RefreshInterval: got %q, want %q", cfg.Vault.RefreshInterval, "30s")
	}
}

func TestLoadConfig_VaultSection_OmittedYieldsEmptyString(t *testing.T) {
	cfg := mustWriteTempConfig(t, `log_level: debug`)
	if cfg.Vault.RefreshInterval != "" {
		t.Errorf("Vault.RefreshInterval should default to empty string when omitted; got %q",
			cfg.Vault.RefreshInterval)
	}
}

// TestLoadConfig_ResolvesServiceFromNodePools verifies end-to-end identity
// resolution using the real hostname.
func TestLoadConfig_ResolvesServiceFromNodePools(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Skip("cannot resolve hostname:", err)
	}
	cfg := mustWriteTempConfig(t,
		"node_pools:\n  - selector: mypool\n    nodes:\n      - "+hostname+
			"\ncontinuous_deployment:\n  - service: myapp_svc\n    selector: mypool\n")

	if cfg.ServiceName != "myapp_svc" {
		t.Errorf("ServiceName: got %q, want %q", cfg.ServiceName, "myapp_svc")
	}
	if len(cfg.AllowedServices) != 1 || cfg.AllowedServices[0] != "myapp_svc" {
		t.Errorf("AllowedServices: got %v, want [myapp_svc]", cfg.AllowedServices)
	}
}

// TestLoadConfig_AllowedServicesFromAllCDEntries pins that AllowedServices lists
// every continuous_deployment service, even when the host is in no node_pool.
func TestLoadConfig_AllowedServicesFromAllCDEntries(t *testing.T) {
	content := `
continuous_deployment:
  - service: svc_a
    selector: pool_a
  - service: svc_b
    selector: pool_b
`
	cfg := mustWriteTempConfig(t, content)
	if len(cfg.AllowedServices) != 2 {
		t.Errorf("AllowedServices length: got %d, want 2", len(cfg.AllowedServices))
	}
}

// ─── resolveFromHostname unit tests ────────────────────────────────────────

// resolvePrimary mirrors LoadConfig's identity resolution: the primary (first)
// managed service, the host's pool selectors and the whitelist.
func resolvePrimary(host string, pools []NodePool, entries []ServiceEntry) (primary string, selectors, allowed []string) {
	selectors, allowed = resolveFromHostname(host, pools, entries)
	managed := managedEntries(entries, selectors)
	if len(managed) > 0 {
		primary = managed[0].Service
	}
	return primary, selectors, allowed
}

func TestResolveFromHostname_HostnameFound(t *testing.T) {
	pools := []NodePool{
		{Selector: "mypool", Nodes: []string{"HOST001", "HOST002"}},
	}
	entries := []ServiceEntry{
		{Service: "myproject_myapp", Selector: "mypool"},
		{Service: "myproject_otherapp", Selector: "otherapp"},
	}
	name, selectors, allowed := resolvePrimary("HOST001", pools, entries)
	if name != "myproject_myapp" {
		t.Errorf("primary service: got %q, want %q", name, "myproject_myapp")
	}
	if len(selectors) != 1 || selectors[0] != "mypool" {
		t.Errorf("hostSelectors: got %v, want [mypool]", selectors)
	}
	if len(allowed) != 2 {
		t.Errorf("AllowedServices length: got %d, want 2", len(allowed))
	}
}

func TestResolveFromHostname_HostnameNotFound(t *testing.T) {
	pools := []NodePool{
		{Selector: "mypool", Nodes: []string{"HOST001"}},
	}
	entries := []ServiceEntry{
		{Service: "myproject_myapp", Selector: "mypool"},
	}
	name, selectors, allowed := resolvePrimary("UNKNOWN", pools, entries)
	if name != "" {
		t.Errorf("primary service: got %q, want empty", name)
	}
	if len(selectors) != 0 {
		t.Errorf("hostSelectors: got %v, want empty", selectors)
	}
	if len(allowed) != 1 || allowed[0] != "myproject_myapp" {
		t.Errorf("AllowedServices: got %v, want [myproject_myapp]", allowed)
	}
}

func TestResolveFromHostname_SelectorNotMatchedInCD(t *testing.T) {
	pools := []NodePool{
		{Selector: "orphan", Nodes: []string{"HOST001"}},
	}
	entries := []ServiceEntry{
		{Service: "myproject_myapp", Selector: "mypool"},
	}
	name, selectors, allowed := resolvePrimary("HOST001", pools, entries)
	if name != "" {
		t.Errorf("primary service: got %q, want empty (selector has no CD entry)", name)
	}
	// The host is in the pool (selector reported), but no entry uses it.
	if len(selectors) != 1 || selectors[0] != "orphan" {
		t.Errorf("hostSelectors: got %v, want [orphan]", selectors)
	}
	if len(allowed) != 1 {
		t.Errorf("AllowedServices: got %v, want 1 entry", allowed)
	}
}

func TestResolveFromHostname_EmptyInput(t *testing.T) {
	name, selectors, allowed := resolvePrimary("HOST001", nil, nil)
	if name != "" {
		t.Errorf("primary service: got %q, want empty", name)
	}
	if len(selectors) != 0 {
		t.Errorf("hostSelectors: got %v, want empty", selectors)
	}
	if len(allowed) != 0 {
		t.Errorf("AllowedServices: got %v, want empty", allowed)
	}
}

func TestResolveFromHostname_CaseInsensitive(t *testing.T) {
	pools := []NodePool{
		{Selector: "mypool", Nodes: []string{"HOST005"}},
	}
	entries := []ServiceEntry{
		{Service: "myproject_myapplication", Selector: "mypool"},
	}
	// lowercase hostname should still match the uppercase pool entry
	name, _, _ := resolvePrimary("host005", pools, entries)
	if name != "myproject_myapplication" {
		t.Errorf("primary service: got %q, want %q", name, "myproject_myapplication")
	}
}

func TestResolveFromHostname_AllowedServicesFromAllEntries(t *testing.T) {
	pools := []NodePool{
		{Selector: "pool_a", Nodes: []string{"HOST001"}},
	}
	entries := []ServiceEntry{
		{Service: "svc_a", Selector: "pool_a"},
		{Service: "svc_b", Selector: "pool_b"},
		{Service: "svc_c", Selector: "pool_c"},
	}
	_, _, allowed := resolvePrimary("HOST001", pools, entries)
	if len(allowed) != 3 {
		t.Errorf("AllowedServices length: got %d, want 3 (all CD entries)", len(allowed))
	}
}

func TestResolveFromHostname_MultipleNodesInPool(t *testing.T) {
	pools := []NodePool{
		{Selector: "myapplication2", Nodes: []string{"HOST003", "HOST004"}},
	}
	entries := []ServiceEntry{
		{Service: "myproject_myapplication2", Selector: "myapplication2"},
	}
	for _, host := range []string{"HOST003", "HOST004"} {
		name, _, _ := resolvePrimary(host, pools, entries)
		if name != "myproject_myapplication2" {
			t.Errorf("host %s: primary service: got %q, want %q", host, name, "myproject_myapplication2")
		}
	}
}

// TestResolveFromHostname_MultiplePoolsManagesAll pins the union semantics: a
// host in two pools manages both services (the example config's HOST005/006).
func TestResolveFromHostname_MultiplePoolsManagesAll(t *testing.T) {
	pools := []NodePool{
		{Selector: "myapplication3", Nodes: []string{"HOST006", "HOST005"}},
		{Selector: "myapplication4", Nodes: []string{"HOST005", "HOST006"}},
	}
	entries := []ServiceEntry{
		{Service: "myproject_myapplication3", Selector: "myapplication3"},
		{Service: "myproject_myapplication4", Selector: "myapplication4"},
	}
	selectors, _ := resolveFromHostname("HOST006", pools, entries)
	if len(selectors) != 2 {
		t.Fatalf("hostSelectors: got %v, want both pools", selectors)
	}
	managed := managedEntries(entries, selectors)
	got := []string{}
	for _, e := range managed {
		got = append(got, e.Service)
	}
	if len(got) != 2 {
		t.Fatalf("managed services: got %v, want both myapplication3 and myapplication4", got)
	}
	// Entry declaration order is preserved.
	if got[0] != "myproject_myapplication3" || got[1] != "myproject_myapplication4" {
		t.Errorf("managed services: got %v, want [myapplication3, myapplication4]", got)
	}
}

func TestResolveFromHostname_MultiplePoolsNoWarning(t *testing.T) {
	pools := []NodePool{
		{Selector: "pool_a", Nodes: []string{"HOST001"}},
		{Selector: "pool_b", Nodes: []string{"HOST001"}},
	}
	entries := []ServiceEntry{
		{Service: "svc_a", Selector: "pool_a"},
		{Service: "svc_b", Selector: "pool_b"},
	}
	buf := captureLogs(t, LevelWarning)
	selectors, _ := resolveFromHostname("HOST001", pools, entries)
	if len(selectors) != 2 {
		t.Errorf("hostSelectors: got %v, want both pools", selectors)
	}
	// Multi-pool membership means multi-service, not an ambiguity.
	if strings.Contains(buf.String(), "multiple node_pools") {
		t.Errorf("multi-pool must NOT warn under the union model; got:\n%s", buf.String())
	}
}

// ─── managedEntries unit tests ─────────────────────────────────────────────

func TestManagedEntries_UnionAcrossPools(t *testing.T) {
	entries := []ServiceEntry{
		{Service: "svc_a", Selector: "pool_a"},
		{Service: "svc_b", Selector: "pool_b"},
		{Service: "svc_c", Selector: "pool_c"},
	}
	managed := managedEntries(entries, []string{"pool_a", "pool_c"})
	if len(managed) != 2 || managed[0].Service != "svc_a" || managed[1].Service != "svc_c" {
		t.Errorf("managed: got %v, want [svc_a, svc_c]", managed)
	}
}

func TestManagedEntries_MultipleEntriesSameSelector(t *testing.T) {
	// A single pool with two distinct services → both managed.
	entries := []ServiceEntry{
		{Service: "svc_a", Selector: "shared"},
		{Service: "svc_b", Selector: "shared"},
	}
	managed := managedEntries(entries, []string{"shared"})
	if len(managed) != 2 {
		t.Errorf("managed: got %d entries, want 2 (both services in the shared pool)", len(managed))
	}
}

func TestManagedEntries_DedupesBySameService(t *testing.T) {
	// Two matched entries declare the SAME Service; ${workdir}/${service} is the
	// shared state key, so only the first is kept.
	entries := []ServiceEntry{
		{Service: "dup", Selector: "pool_a", GitValues: "a.yaml"},
		{Service: "dup", Selector: "pool_b", GitValues: "b.yaml"},
	}
	managed := managedEntries(entries, []string{"pool_a", "pool_b"})
	if len(managed) != 1 {
		t.Fatalf("managed: got %d, want 1 (deduped by service)", len(managed))
	}
	if managed[0].GitValues != "a.yaml" {
		t.Errorf("dedup must keep the FIRST entry; got GitValues=%q", managed[0].GitValues)
	}
}

func TestManagedEntries_NoSelectors_ReturnsNil(t *testing.T) {
	entries := []ServiceEntry{{Service: "svc_a", Selector: "pool_a"}}
	if got := managedEntries(entries, nil); got != nil {
		t.Errorf("managedEntries with no selectors: got %v, want nil", got)
	}
}

// ─── serviceCollisions unit tests ──────────────────────────────────────────

func TestServiceCollisions_DetectsDuplicateAcrossMatchedPools(t *testing.T) {
	entries := []ServiceEntry{
		{Service: "dup", Selector: "pool_a"},
		{Service: "dup", Selector: "pool_b"},
		{Service: "solo", Selector: "pool_a"},
	}
	got := serviceCollisions(entries, []string{"pool_a", "pool_b"})
	if len(got) != 1 || got[0] != "dup" {
		t.Errorf("serviceCollisions: got %v, want [dup]", got)
	}
}

func TestServiceCollisions_NoneWhenDistinctServices(t *testing.T) {
	entries := []ServiceEntry{
		{Service: "svc_a", Selector: "pool_a"},
		{Service: "svc_b", Selector: "pool_b"},
	}
	if got := serviceCollisions(entries, []string{"pool_a", "pool_b"}); len(got) != 0 {
		t.Errorf("serviceCollisions: got %v, want none", got)
	}
}

func TestServiceCollisions_IgnoresUnmatchedPools(t *testing.T) {
	// Two "dup" entries, but only one pool is matched by the host → no collision.
	entries := []ServiceEntry{
		{Service: "dup", Selector: "pool_a"},
		{Service: "dup", Selector: "pool_b"},
	}
	if got := serviceCollisions(entries, []string{"pool_a"}); len(got) != 0 {
		t.Errorf("serviceCollisions: got %v, want none (only one pool matched)", got)
	}
}

func TestServiceCollisions_NoSelectors_ReturnsNil(t *testing.T) {
	entries := []ServiceEntry{{Service: "dup", Selector: "pool_a"}, {Service: "dup", Selector: "pool_a"}}
	if got := serviceCollisions(entries, nil); got != nil {
		t.Errorf("serviceCollisions with no selectors: got %v, want nil", got)
	}
}

// ─── serviceWorkdir / serviceServiceConfig ────────────────────────────────

func TestServiceWorkdir_DefaultsToOpt(t *testing.T) {
	e := ServiceEntry{Service: "svc"}
	if got := serviceWorkdir(e); got != "/opt" {
		t.Errorf("got %q, want /opt", got)
	}
}

func TestServiceWorkdir_ExplicitValue(t *testing.T) {
	e := ServiceEntry{Service: "svc", Workdir: "/var"}
	if got := serviceWorkdir(e); got != "/var" {
		t.Errorf("got %q, want /var", got)
	}
}

func TestServiceServiceConfig_DefaultsToTargetConfigYml(t *testing.T) {
	if got := serviceServiceConfig(ServiceEntry{}); got != "target.config.yml" {
		t.Errorf("default: got %q, want %q", got, "target.config.yml")
	}
}

func TestServiceServiceConfig_ExplicitValue(t *testing.T) {
	if got := serviceServiceConfig(ServiceEntry{ServiceConfig: "app.yml"}); got != "app.yml" {
		t.Errorf("explicit: got %q, want %q", got, "app.yml")
	}
}

// ─── GitValuesVersionKey loading ───────────────────────────────────────────

func TestLoadConfig_GitValuesVersionKey_Parsed(t *testing.T) {
	content := `
continuous_deployment:
  - service: myproject_myapplication
    selector: myapplication
    git_values: products/myproject/environments/dev/myproject_myapplication/values.yaml
    git_values_config_key: .${selector}.services.${selector}.config.[]
    git_values_version_key: .${selector}.global.image.tag
    service_config: config.yml
`
	cfg := mustWriteTempConfig(t, content)

	if len(cfg.AllowedServices) != 1 || cfg.AllowedServices[0] != "myproject_myapplication" {
		t.Fatalf("AllowedServices: got %v, want [myproject_myapplication]", cfg.AllowedServices)
	}
	entry := cfg.ContinuousDeployment[0]
	if entry.GitValuesVersionKey != ".${selector}.global.image.tag" {
		t.Errorf("GitValuesVersionKey: got %q, want %q",
			entry.GitValuesVersionKey, ".${selector}.global.image.tag")
	}
	if entry.GitValuesConfigKey != ".${selector}.services.${selector}.config.[]" {
		t.Errorf("GitValuesConfigKey: got %q, want %q",
			entry.GitValuesConfigKey, ".${selector}.services.${selector}.config.[]")
	}
}

func TestLoadConfig_GitValuesVersionKey_OmittedIsEmpty(t *testing.T) {
	content := `
continuous_deployment:
  - service: myproject_myapp
    selector: mypool
    git_values: some/path/values.yaml
`
	cfg := mustWriteTempConfig(t, content)

	if entry := cfg.ContinuousDeployment[0]; entry.GitValuesVersionKey != "" {
		t.Errorf("GitValuesVersionKey: got %q, want empty when not set", entry.GitValuesVersionKey)
	}
}

func TestLoadConfig_GitValuesVersionKey_MultipleServices(t *testing.T) {
	// Each service entry can independently configure its own version key.
	content := `
continuous_deployment:
  - service: svc_a
    selector: pool_a
    git_values_version_key: .svc_a.global.image.tag
  - service: svc_b
    selector: pool_b
    git_values_version_key: .svc_b.release.version
  - service: svc_c
    selector: pool_c
    # no version key
`
	cfg := mustWriteTempConfig(t, content)

	if len(cfg.ContinuousDeployment) != 3 {
		t.Fatalf("ContinuousDeployment length: got %d, want 3", len(cfg.ContinuousDeployment))
	}
	if cfg.ContinuousDeployment[0].GitValuesVersionKey != ".svc_a.global.image.tag" {
		t.Errorf("svc_a GitValuesVersionKey: got %q", cfg.ContinuousDeployment[0].GitValuesVersionKey)
	}
	if cfg.ContinuousDeployment[1].GitValuesVersionKey != ".svc_b.release.version" {
		t.Errorf("svc_b GitValuesVersionKey: got %q", cfg.ContinuousDeployment[1].GitValuesVersionKey)
	}
	if cfg.ContinuousDeployment[2].GitValuesVersionKey != "" {
		t.Errorf("svc_c GitValuesVersionKey: got %q, want empty", cfg.ContinuousDeployment[2].GitValuesVersionKey)
	}
}

func TestLoadConfig_GitValuesVersionKey_HotReloadPreserved(t *testing.T) {
	// Verify applyReloaded propagates ContinuousDeployment (which contains
	// GitValuesVersionKey) to the live config.
	cfg := &Config{
		ContinuousDeployment: []ServiceEntry{
			{Service: "svc_a", GitValuesVersionKey: ""},
		},
	}
	newCfg := &Config{
		ContinuousDeployment: []ServiceEntry{
			{Service: "svc_a", GitValuesVersionKey: ".svc_a.global.image.tag"},
		},
	}
	cfg.applyReloaded(newCfg)

	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	if cfg.ContinuousDeployment[0].GitValuesVersionKey != ".svc_a.global.image.tag" {
		t.Errorf("GitValuesVersionKey after applyReloaded: got %q, want %q",
			cfg.ContinuousDeployment[0].GitValuesVersionKey, ".svc_a.global.image.tag")
	}
}

func TestServiceGitBranch_DefaultsMaster(t *testing.T) {
	e := ServiceEntry{Service: "svc"}
	if got := serviceGitBranch(e); got != "master" {
		t.Errorf("got %q, want master", got)
	}
}

func TestServiceGitBranch_ExplicitValue(t *testing.T) {
	e := ServiceEntry{Service: "svc", GitBranch: "releasedev"}
	if got := serviceGitBranch(e); got != "releasedev" {
		t.Errorf("got %q, want releasedev", got)
	}
}

// TestManagedEntries_SelectorDrivesEntryPick pins that, of two entries sharing a
// Service, a host manages the one whose Selector matches its pool, not the first
// by name. See DOCS/MEMORY.md § PT/POC entry mis-selection — FIXED
func TestManagedEntries_SelectorDrivesEntryPick(t *testing.T) {
	entries := []ServiceEntry{
		{Service: "growin_app", Selector: "pt_pool", GitValues: "products/pt/.../values.yaml"},
		{Service: "growin_app", Selector: "poc_pool", GitValues: "products/poc/.../values.yaml"},
	}
	if m := managedEntries(entries, []string{"pt_pool"}); len(m) != 1 || m[0].GitValues != "products/pt/.../values.yaml" {
		t.Errorf("pt_pool: got %+v, want pt entry", m)
	}
	if m := managedEntries(entries, []string{"poc_pool"}); len(m) != 1 || m[0].GitValues != "products/poc/.../values.yaml" {
		t.Errorf("poc_pool: got %+v, want poc entry — this is the PT/POC bug regression", m)
	}
}

// ─── serviceAutoSync ───────────────────────────────────────────────────────

// TestServiceAutoSync_TolerantBooleanParsing pins every accepted truthy form and
// representative falsy / unrecognised values (always disabled).
func TestServiceAutoSync_TolerantBooleanParsing(t *testing.T) {
	cases := []struct {
		name  string
		value interface{}
		want  bool
	}{
		// Bool path.
		{"bool true", true, true},
		{"bool false", false, false},

		// Missing / null.
		{"nil (missing field)", nil, false},

		// String path — case variants.
		{"string true", "true", true},
		{"string True", "True", true},
		{"string TRUE", "TRUE", true},
		{"string tRue", "tRue", true},
		{"string yes", "yes", true},
		{"string Yes", "Yes", true},
		{"string YES", "YES", true},
		{"string yEs", "yEs", true},
		{"string YeS", "YeS", true},
		{"string 1", "1", true},
		{"string with whitespace", "  true  ", true},
		{"string false", "false", false},
		{"string False", "False", false},
		{"string FALSE", "FALSE", false},
		{"string no", "no", false},
		{"string No", "No", false},
		{"string NO", "NO", false},
		{"string nO", "nO", false},
		{"string 0", "0", false},
		{"string foo", "foo", false},
		{"string empty", "", false},

		// Int path: only 1 means true, 0 and other ints mean false.
		{"int 1", 1, true},
		{"int 0", 0, false},
		{"int 2", 2, false},
		{"int -1", -1, false},
		{"int64 1", int64(1), true},
		{"int64 0", int64(0), false},

		// Unsupported types fall through to false.
		{"float", 1.0, false},
		{"slice", []interface{}{true}, false},
		{"map", map[string]interface{}{"x": true}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := serviceAutoSync(ServiceEntry{AutoSync: tc.value})
			if got != tc.want {
				t.Errorf("serviceAutoSync(%T(%v)): got %v, want %v", tc.value, tc.value, got, tc.want)
			}
		})
	}
}

// TestLoadConfig_AutoSync_AcceptedYAMLForms pins how each documented
// `autosync:` YAML form decodes and resolves through serviceAutoSync.
func TestLoadConfig_AutoSync_AcceptedYAMLForms(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want bool
	}{
		// Native YAML booleans.
		{"true", "    autosync: true", true},
		{"True", "    autosync: True", true},
		{"TRUE", "    autosync: TRUE", true},
		{"false", "    autosync: false", false},
		{"False", "    autosync: False", false},
		{"FALSE", "    autosync: FALSE", false},

		// Quoted strings (yaml.v3 decodes as string when target is interface{}).
		{`"true"`, `    autosync: "true"`, true},
		{`"True"`, `    autosync: "True"`, true},
		{`"yes"`, `    autosync: "yes"`, true},
		{`"Yes"`, `    autosync: "Yes"`, true},
		{`"YES"`, `    autosync: "YES"`, true},
		{`"1"`, `    autosync: "1"`, true},
		{`"false"`, `    autosync: "false"`, false},
		{`"no"`, `    autosync: "no"`, false},
		{`"0"`, `    autosync: "0"`, false},
		{`"foo"`, `    autosync: "foo"`, false},

		// Bare yes/no — yaml.v3 (YAML 1.2) decodes these as strings.
		{"bare yes", "    autosync: yes", true},
		{"bare YES", "    autosync: YES", true},
		{"bare no", "    autosync: no", false},
		{"bare NO", "    autosync: NO", false},

		// Numeric forms.
		{"int 1", "    autosync: 1", true},
		{"int 0", "    autosync: 0", false},
		{"int 2", "    autosync: 2", false},

		// Null / omitted / unrecognised — all disabled.
		{"null", "    autosync: null", false},
		{"~", "    autosync: ~", false},
		{"empty", "    autosync:", false},
		{"omitted", "    # autosync omitted", false},
		{"garbage scalar", "    autosync: notabool", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			content := `
continuous_deployment:
  - service: myproject_myapp
    selector: mypool
` + tc.yaml + "\n"
			cfg, err := LoadConfig(mustWriteTemp(t, content))
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			got := serviceAutoSync(cfg.ContinuousDeployment[0])
			if got != tc.want {
				t.Errorf("YAML %q: got %v, want %v (raw=%T %v)",
					tc.yaml, got, tc.want, cfg.ContinuousDeployment[0].AutoSync, cfg.ContinuousDeployment[0].AutoSync)
			}
		})
	}
}

// TestLoadConfig_AutoSyncComplexValue_DoesNotCrash pins that a non-scalar
// autosync still parses (interface{} typing) and resolves to false.
func TestLoadConfig_AutoSyncComplexValue_DoesNotCrash(t *testing.T) {
	content := `
continuous_deployment:
  - service: myproject_myapp
    selector: mypool
    autosync:
      - true
      - false
`
	cfg, err := LoadConfig(mustWriteTemp(t, content))
	if err != nil {
		t.Fatalf("LoadConfig must tolerate non-scalar autosync value: %v", err)
	}
	if serviceAutoSync(cfg.ContinuousDeployment[0]) {
		t.Errorf("non-scalar autosync should disable; got enabled")
	}
}

// ─── EnsureServiceWorkdir ──────────────────────────────────────────────────

func TestEnsureServiceWorkdir_CreatesDirectory(t *testing.T) {
	base := mkTempDir(t)
	cfg := &Config{
		ServiceName:  "myproject_myapp",
		HostSelector: "mypool", // must match the entry's Selector, not the Service name
		ContinuousDeployment: []ServiceEntry{
			{Service: "myproject_myapp", Selector: "mypool", Workdir: base},
		},
	}
	EnsureServiceWorkdir(cfg)
	expected := filepath.Join(base, "myproject_myapp")
	if info, err := os.Stat(expected); err != nil || !info.IsDir() {
		t.Errorf("expected directory %s to exist; err=%v", expected, err)
	}
}

// TestEnsureServiceWorkdir_MultiService_CreatesAllDirs pins that every managed
// service gets its directory, not just the primary.
func TestEnsureServiceWorkdir_MultiService_CreatesAllDirs(t *testing.T) {
	base1 := mkTempDir(t)
	base2 := mkTempDir(t)
	cfg := &Config{
		ServiceName:   "svc1",
		HostSelector:  "sel1",
		HostSelectors: []string{"sel1", "sel2"},
		ContinuousDeployment: []ServiceEntry{
			{Service: "svc1", Selector: "sel1", Workdir: base1},
			{Service: "svc2", Selector: "sel2", Workdir: base2},
		},
	}
	EnsureServiceWorkdir(cfg)
	for _, want := range []string{filepath.Join(base1, "svc1"), filepath.Join(base2, "svc2")} {
		if info, err := os.Stat(want); err != nil || !info.IsDir() {
			t.Errorf("expected directory %s to exist; err=%v", want, err)
		}
	}
}

func TestEnsureServiceWorkdir_NoServiceName_NoOp(t *testing.T) {
	cfg := &Config{}
	EnsureServiceWorkdir(cfg) // should not panic
}

func TestEnsureServiceWorkdir_NoEntry_NoOp(t *testing.T) {
	cfg := &Config{ServiceName: "missing"}
	EnsureServiceWorkdir(cfg) // should not panic
}

// ─── helpers ───────────────────────────────────────────────────────────────

// mustWriteTempConfig loads content from a temp file (removed on cleanup).
func mustWriteTempConfig(t *testing.T, content string) *Config {
	t.Helper()
	path := mustWriteTemp(t, content)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: unexpected error: %v", err)
	}
	return cfg
}

// mustWriteTemp writes content to a temporary file and returns the path.
func mustWriteTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp("", "cystemd_cfg_*.yml")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	f.Close()
	return f.Name()
}

// ─── StartConfigReload ─────────────────────────────────────────────────────

func TestStartConfigReload_PicksUpNewAllowedServices(t *testing.T) {
	v1 := `
continuous_deployment:
  - selector: web
    service: svc_a
node_pools:
  - selector: web
    nodes: ["ignored-host"]
`
	path := mustWriteTemp(t, v1)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.AllowedServices) != 1 {
		t.Fatalf("initial AllowedServices: got %d, want 1", len(cfg.AllowedServices))
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartConfigReload(ctx, path, cfg, 20*time.Millisecond)

	// Write a new config with an additional service.
	v2 := `
continuous_deployment:
  - selector: web
    service: svc_a
  - selector: db
    service: svc_b
node_pools:
  - selector: web
    nodes: ["ignored-host"]
`
	// Ensure mtime advances by writing after a brief sleep.
	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(path, []byte(v2), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		cfg.mu.RLock()
		n := len(cfg.AllowedServices)
		cfg.mu.RUnlock()
		if n == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cfg.mu.RLock()
	got := cfg.AllowedServices
	cfg.mu.RUnlock()
	t.Errorf("AllowedServices after reload: got %v, want 2 entries", got)
}

func TestStartConfigReload_KeepsOldConfigOnParseError(t *testing.T) {
	v1 := `
continuous_deployment:
  - selector: web
    service: original_svc
node_pools:
  - selector: web
    nodes: ["ignored-host"]
`
	path := mustWriteTemp(t, v1)
	cfg, _ := LoadConfig(path)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartConfigReload(ctx, path, cfg, 20*time.Millisecond)

	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(path, []byte(":\tinvalid: yaml::\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	cfg.mu.RLock()
	svcName := cfg.AllowedServices
	cfg.mu.RUnlock()
	if len(svcName) == 0 || svcName[0] != "original_svc" {
		t.Errorf("AllowedServices after bad reload: got %v, want [original_svc]", svcName)
	}
}

func TestStartConfigReload_ZeroIntervalIsNoop(t *testing.T) {
	path := mustWriteTemp(t, "log_level: info\n")
	cfg, _ := LoadConfig(path)
	// Should not panic or start a goroutine; just a smoke test.
	StartConfigReload(context.Background(), path, cfg, 0)
}

// TestStartConfigReload_SelfUpdateDeferredWhileSyncMuHeld pins the reload
// site's syncMu guard: while held, the tick logs "self-update deferred" (Debug)
// without entering RunSelfVersionInstall (its dev-build skip line stays absent);
// once released, the next tick runs it. Dev/unknown builds only: phase 2 would
// otherwise run a real dnf install. See DOCS/CLAUDE.md § Self-update
func TestStartConfigReload_SelfUpdateDeferredWhileSyncMuHeld(t *testing.T) {
	if v := Version(); v != "dev" && v != "unknown" {
		t.Skipf("running build has Version()=%q; phase 2 would invoke dnf — skipping", v)
	}
	buf := captureLogs(t, LevelDebug)

	path := mustWriteTemp(t, "log_level: debug\n")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// Phase 1: hold syncMu, as a CD cycle or /sync would.
	syncMu.Lock()
	locked := true
	defer func() {
		if locked {
			syncMu.Unlock()
		}
	}()

	StartConfigReload(ctx, path, cfg, 20*time.Millisecond)

	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(path, []byte("log_level: debug\nversion: v9.9.9\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	const deferredMsg = "self-update deferred"
	const devSkipMsg = "running build has no injected version"

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(decodeLogMsgs(t, buf), deferredMsg) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs := decodeLogMsgs(t, buf)
	if !strings.Contains(msgs, deferredMsg) {
		t.Fatalf("expected deferred self-update log while syncMu held; got:\n%s", msgs)
	}
	if strings.Contains(msgs, devSkipMsg) {
		t.Fatalf("RunSelfVersionInstall must not run while syncMu is held; got:\n%s", msgs)
	}
	// The deferred line must be Debug level (journal-coverage contract).
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, deferredMsg) && !strings.Contains(line, `"level":"debug"`) {
			t.Errorf("deferred self-update line must be debug level; got: %s", line)
		}
	}

	// Phase 2: released, the next tick's TryLock succeeds (dev-build skip line).
	syncMu.Unlock()
	locked = false

	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(path, []byte("log_level: debug\nversion: v9.9.8\n"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(decodeLogMsgs(t, buf), devSkipMsg) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("expected RunSelfVersionInstall to run after syncMu release; got:\n%s", decodeLogMsgs(t, buf))
}

func TestApplyReloaded_UpdatesAllowedServicesUnderLock(t *testing.T) {
	cfg := &Config{
		ServiceName:     "old_svc",
		HostSelector:    "old_svc",
		AllowedServices: []string{"old_svc"},
	}
	newCfg := &Config{
		ServiceName:     "new_svc",
		HostSelector:    "new_svc",
		AllowedServices: []string{"new_svc", "extra_svc"},
		LogLevel:        "debug",
	}
	cfg.applyReloaded(newCfg)

	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	if cfg.ServiceName != "new_svc" {
		t.Errorf("ServiceName: got %q, want %q", cfg.ServiceName, "new_svc")
	}
	if len(cfg.AllowedServices) != 2 {
		t.Errorf("AllowedServices: got %v, want 2 entries", cfg.AllowedServices)
	}
}

// TestApplyReloaded_ReplacesEntireAuth pins that the whole Auth struct is
// copied, so a future AuthConfig field reloads without code changes.
func TestApplyReloaded_ReplacesEntireAuth(t *testing.T) {
	cfg := &Config{Auth: AuthConfig{Enabled: false, PublicKeyFile: "/old.pub"}}
	newCfg := &Config{Auth: AuthConfig{Enabled: true, PublicKeyFile: "/new.pub"}}
	cfg.applyReloaded(newCfg)

	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled: got false, want true")
	}
	if cfg.Auth.PublicKeyFile != "/new.pub" {
		t.Errorf("Auth.PublicKeyFile: got %q, want %q", cfg.Auth.PublicKeyFile, "/new.pub")
	}
}

// TestApplyReloaded_PreservesEnvForcedAuthEnabled pins that a reload keeps an
// env-forced AUTH_ENABLED=true over config.yml's auth.enabled: false.
func TestApplyReloaded_PreservesEnvForcedAuthEnabled(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "true")
	t.Setenv("AUTH_PUBLIC_KEY_FILE", "")
	t.Setenv("PUBLIC_KEY", "")

	// cfg starts in the env-resolved state InitAuth leaves at startup.
	cfg := &Config{Auth: AuthConfig{Enabled: true, PublicKeyFile: "/old.pub"}}
	// The freshly-parsed YAML — env-ignorant — says auth.enabled: false.
	newCfg := &Config{Auth: AuthConfig{Enabled: false, PublicKeyFile: "/old.pub"}}

	cfg.applyReloaded(newCfg)

	cfg.mu.RLock()
	defer cfg.mu.RUnlock()
	if !cfg.Auth.Enabled {
		t.Error("Auth.Enabled regressed to false after reload despite AUTH_ENABLED=true env override")
	}
}

// TestStartConfigReload_EnvForcedAuthEnabled_NoSpuriousWarning pins the same
// end to end: Enabled stays true and no spurious "auth.enabled changed" warning.
func TestStartConfigReload_EnvForcedAuthEnabled_NoSpuriousWarning(t *testing.T) {
	t.Setenv("AUTH_ENABLED", "true")
	t.Setenv("AUTH_PUBLIC_KEY_FILE", "")
	t.Setenv("PUBLIC_KEY", "")

	v1 := "auth:\n  enabled: false\n  public_key_file: /some/key1.pub\nlog_level: info\n"
	path := mustWriteTemp(t, v1)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Startup InitAuth resolves Enabled=true from the env; its missing-key error
	// is irrelevant (Enabled is resolved before the key load).
	_ = InitAuth(&cfg.Auth)
	if !cfg.Auth.Enabled {
		t.Fatalf("test setup: InitAuth should have resolved Enabled=true from AUTH_ENABLED env")
	}

	buf := captureLogs(t, LevelInfo)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartConfigReload(ctx, path, cfg, 20*time.Millisecond)

	// Rewrite config.yml with an unrelated change (log_level) so the mtime
	// advances and a reload fires; auth.enabled stays false in the file.
	time.Sleep(5 * time.Millisecond)
	v2 := "auth:\n  enabled: false\n  public_key_file: /some/key1.pub\nlog_level: debug\n"
	if err := os.WriteFile(path, []byte(v2), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		cfg.mu.RLock()
		lvl := cfg.LogLevel
		cfg.mu.RUnlock()
		if lvl == "debug" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	cfg.mu.RLock()
	enabled := cfg.Auth.Enabled
	cfg.mu.RUnlock()
	if !enabled {
		t.Error("Auth.Enabled regressed to false after config.yml reload despite AUTH_ENABLED=true env override")
	}
	if strings.Contains(buf.String(), "auth.enabled changed") {
		t.Errorf("spurious 'auth.enabled changed' warning fired even though the effective (env-forced) state never changed; log:\n%s", buf.String())
	}
}

// TestApplyReloaded_UnknownLogLevel_Warns pins that a reloaded log_level
// ParseLevel rejects is warned about, as at startup, not dropped silently.
func TestApplyReloaded_UnknownLogLevel_Warns(t *testing.T) {
	buf := captureLogs(t, LevelWarning)
	cfg := &Config{LogLevel: "info"}
	cfg.applyReloaded(&Config{LogLevel: "not-a-real-level"})
	if !strings.Contains(buf.String(), "unknown log_level") {
		t.Errorf("expected an 'unknown log_level' warning on reload; got:\n%s", buf.String())
	}
}

// TestStartConfigReload_CallsInitAuthOnPublicKeyFileChange pins that a changed
// auth.public_key_file (auth enabled) re-runs InitAuth, rotating the key in-process.
func TestStartConfigReload_CallsInitAuthOnPublicKeyFileChange(t *testing.T) {
	// Neutralise env overrides so the test controls key loading via config.yml only.
	t.Setenv("AUTH_ENABLED", "")
	t.Setenv("AUTH_PUBLIC_KEY_FILE", "")
	t.Setenv("PUBLIC_KEY", "")

	_, _, sshKey1 := mustGenerateSSHKeyPair(t)
	_, _, sshKey2 := mustGenerateSSHKeyPair(t)

	dir := mkTempDir(t)
	keyFile1 := filepath.Join(dir, "key1.pub")
	keyFile2 := filepath.Join(dir, "key2.pub")
	if err := os.WriteFile(keyFile1, []byte(sshKey1+"\n"), 0600); err != nil {
		t.Fatalf("WriteFile key1: %v", err)
	}
	if err := os.WriteFile(keyFile2, []byte(sshKey2+"\n"), 0600); err != nil {
		t.Fatalf("WriteFile key2: %v", err)
	}

	v1 := fmt.Sprintf("auth:\n  enabled: true\n  public_key_file: %s\n", keyFile1)
	path := mustWriteTemp(t, v1)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// Initialise auth with key1; restore publicKey after the test.
	publicKeyMu.RLock()
	prevKey := publicKey
	publicKeyMu.RUnlock()
	t.Cleanup(func() {
		publicKeyMu.Lock()
		publicKey = prevKey
		publicKeyMu.Unlock()
	})
	if err := InitAuth(&cfg.Auth); err != nil {
		t.Fatalf("InitAuth key1: %v", err)
	}

	buf := captureLogs(t, LevelInfo)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartConfigReload(ctx, path, cfg, 20*time.Millisecond)

	// Update config to point at key2.
	v2 := fmt.Sprintf("auth:\n  enabled: true\n  public_key_file: %s\n", keyFile2)
	time.Sleep(5 * time.Millisecond)
	if err := os.WriteFile(path, []byte(v2), 0600); err != nil {
		t.Fatalf("WriteFile config v2: %v", err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "auth re-initialised") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("expected 'auth re-initialised' log after public_key_file change; got:\n%s", buf.String())
}

// TestExampleConfigYML_ParsesAndDocumentsDNFBlock pins example.config.yml (what
// operators copy) to the parser: a key renamed only in Go parses to a zero value.
func TestExampleConfigYML_ParsesAndDocumentsDNFBlock(t *testing.T) {
	cfg, err := LoadConfig("../../example.config.yml")
	if err != nil {
		t.Fatalf("example.config.yml must parse through LoadConfig: %v", err)
	}
	// The example must set the knob (commented-out YAML proves nothing), though
	// the code default is empty/disabled.
	if len(cfg.DNF.MakecacheRepos) == 0 {
		t.Errorf("example.config.yml should demonstrate dnf.makecache_repos; got %v", cfg.DNF.MakecacheRepos)
	}
	if cfg.DNF.MakecacheTimeout == "" {
		t.Error("example.config.yml should demonstrate dnf.makecache_timeout")
	}
}
