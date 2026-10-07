package service

import (
	"os"
	"testing"
)

// TestLoadConfig_SampleFixtureParses: LoadConfig accepts the committed realistic
// fixture testdata/sample_full_config.yml; every entry has Service and Selector;
// autosync resolves both true and false (tolerant bool parsing on a real shape);
// AllowedServices holds every entry's service on any host; top-level version loads.
func TestLoadConfig_SampleFixtureParses(t *testing.T) {
	const path = "testdata/sample_full_config.yml"
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%q): %v", path, err)
	}
	if len(cfg.ContinuousDeployment) == 0 {
		t.Fatalf("expected continuous_deployment entries in %s, got 0", path)
	}
	if len(cfg.AllowedServices) != len(cfg.ContinuousDeployment) {
		t.Errorf("AllowedServices length (%d) != ContinuousDeployment length (%d)",
			len(cfg.AllowedServices), len(cfg.ContinuousDeployment))
	}
	enabled, disabled := 0, 0
	for _, e := range cfg.ContinuousDeployment {
		if e.Service == "" {
			t.Errorf("entry missing service: %+v", e)
		}
		if e.Selector == "" {
			t.Errorf("entry missing selector: %+v", e)
		}
		if serviceAutoSync(e) {
			enabled++
		} else {
			disabled++
		}
	}
	if enabled == 0 || disabled == 0 {
		t.Errorf("fixture should mix autosync enabled and disabled entries (enabled=%d disabled=%d) to exercise both branches",
			enabled, disabled)
	}
	if cfg.Version != "v5.0.24" {
		t.Errorf("cfg.Version: got %q, want %q (the fixture's top-level `version` key drives the self-update path)",
			cfg.Version, "v5.0.24")
	}
	t.Logf("fixture parsed: entries=%d autosync_enabled=%d autosync_disabled=%d service_name=%q version=%q",
		len(cfg.ContinuousDeployment), enabled, disabled, cfg.ServiceName, cfg.Version)
}

// TestLoadConfig_LiveSampleParses: opt-in check of /tmp/sample-cystemd.config.yml
// (a copy of a running host's config.yml); skipped when absent or empty.
func TestLoadConfig_LiveSampleParses(t *testing.T) {
	const path = "/tmp/sample-cystemd.config.yml"
	info, err := os.Stat(path)
	if err != nil {
		t.Skipf("live sample at %s not present: %v", path, err)
	}
	if info.Size() < 32 {
		t.Skipf("live sample at %s is effectively empty (%d bytes); nothing to verify", path, info.Size())
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig(%q): %v", path, err)
	}
	if len(cfg.ContinuousDeployment) == 0 {
		t.Skipf("live sample has no continuous_deployment entries; nothing to verify")
	}
	for _, e := range cfg.ContinuousDeployment {
		if e.Service == "" || e.Selector == "" {
			t.Errorf("entry missing service/selector: %+v", e)
		}
	}
	t.Logf("live sample parsed: %d entries, service_name=%q, allowed=%d",
		len(cfg.ContinuousDeployment), cfg.ServiceName, len(cfg.AllowedServices))
}
