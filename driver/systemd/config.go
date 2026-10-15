// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package systemd

import (
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/nomad/plugins/shared/hclspec"
)

// PluginName is the driver name used in job specs: driver = "systemd".
const PluginName = "systemd"

// Defaults of the plugin configuration.
const (
	defaultInstallTimeout   = 10 * time.Minute
	defaultMakecacheTimeout = 60 * time.Second
	defaultPollInterval     = time.Second
)

// defaultAllowedProperties are the [Service] directives a job may set through
// `properties`. Resource controls and restart tuning only: directives that
// change what runs or as whom (ExecStart, User, …) would turn "may manage this
// unit" into "may run anything as root".
var defaultAllowedProperties = []string{
	"CPUWeight", "CPUQuota", "CPUQuotaPeriodSec", "AllowedCPUs",
	"MemoryMin", "MemoryLow", "MemoryHigh", "MemoryMax", "MemorySwapMax",
	"TasksMax", "IOWeight", "LimitNOFILE", "LimitNPROC", "LimitCORE",
	"LimitMEMLOCK", "Nice", "OOMScoreAdjust", "OOMPolicy", "Restart",
	"RestartSec", "TimeoutStartSec", "TimeoutStopSec", "UMask",
}

// configSpec is the plugin configuration (client `plugin "systemd" { config {} }`).
var configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"enabled": hclspec.NewDefault(
		hclspec.NewAttr("enabled", "bool", false),
		hclspec.NewLiteral("true"),
	),
	"allowed_units":             hclspec.NewAttr("allowed_units", "list(string)", false),
	"allowed_packages":          hclspec.NewAttr("allowed_packages", "list(string)", false),
	"allowed_file_destinations": hclspec.NewAttr("allowed_file_destinations", "list(string)", false),
	"allowed_properties":        hclspec.NewAttr("allowed_properties", "list(string)", false),
	"allow_transient": hclspec.NewDefault(
		hclspec.NewAttr("allow_transient", "bool", false),
		hclspec.NewLiteral("false"),
	),
	"package_manager":   hclspec.NewAttr("package_manager", "string", false),
	"makecache_repos":   hclspec.NewAttr("makecache_repos", "list(string)", false),
	"makecache_timeout": hclspec.NewAttr("makecache_timeout", "string", false),
	"install_timeout":   hclspec.NewAttr("install_timeout", "string", false),
	"transient_slice":   hclspec.NewAttr("transient_slice", "string", false),
})

// taskConfigSpec is the task `config {}` block.
var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	// unit mode
	"unit": hclspec.NewAttr("unit", "string", false),
	"package": hclspec.NewBlock("package", false, hclspec.NewObject(map[string]*hclspec.Spec{
		"name":    hclspec.NewAttr("name", "string", false),
		"version": hclspec.NewAttr("version", "string", true),
	})),
	"adopt_running": hclspec.NewAttr("adopt_running", "bool", false),
	// transient mode
	"command":  hclspec.NewAttr("command", "string", false),
	"args":     hclspec.NewAttr("args", "list(string)", false),
	"work_dir": hclspec.NewAttr("work_dir", "string", false),
	// both modes
	"file": hclspec.NewBlockList("file", hclspec.NewObject(map[string]*hclspec.Spec{
		"source":      hclspec.NewAttr("source", "string", true),
		"destination": hclspec.NewAttr("destination", "string", true),
		"mode":        hclspec.NewAttr("mode", "string", false),
		"owner":       hclspec.NewAttr("owner", "string", false),
		"group":       hclspec.NewAttr("group", "string", false),
	})),
	"environment_files": hclspec.NewAttr("environment_files", "list(string)", false),
	"cpu_quota":         hclspec.NewAttr("cpu_quota", "string", false),
	"memory_max":        hclspec.NewAttr("memory_max", "string", false),
	"properties":        hclspec.NewAttr("properties", "list(map(string))", false),
})

// Config is the plugin configuration set by SetConfig.
type Config struct {
	Enabled bool `codec:"enabled"`

	// AllowedUnits are glob patterns (path.Match) of the units unit-mode
	// tasks may manage. Empty disables unit mode.
	AllowedUnits []string `codec:"allowed_units"`

	// AllowedPackages are glob patterns of the packages a task may install.
	// Empty disables the package block.
	AllowedPackages []string `codec:"allowed_packages"`

	// AllowedFileDestinations are path patterns `file` blocks may write to:
	// path.Match globs, or "dir/**" for anything below dir. Empty disables
	// file installation.
	AllowedFileDestinations []string `codec:"allowed_file_destinations"`

	// AllowedProperties replaces defaultAllowedProperties when set.
	AllowedProperties []string `codec:"allowed_properties"`

	// AllowTransient enables transient mode (arbitrary commands as root,
	// like raw_exec). Off by default.
	AllowTransient bool `codec:"allow_transient"`

	// PackageManager is dnf, yum, apt or none; empty auto-detects.
	PackageManager string `codec:"package_manager"`

	// MakecacheRepos are refreshed (scoped, one repo per call) right before
	// each install, so a just-published build is visible. Empty disables.
	MakecacheRepos   []string `codec:"makecache_repos"`
	MakecacheTimeout string   `codec:"makecache_timeout"`
	InstallTimeout   string   `codec:"install_timeout"`

	// TransientSlice is the slice transient units run in (default
	// cystemd.slice).
	TransientSlice string `codec:"transient_slice"`
}

func (c *Config) installTimeout() time.Duration {
	return parseDurationDefault(c.InstallTimeout, defaultInstallTimeout)
}

func (c *Config) makecacheTimeout() time.Duration {
	return parseDurationDefault(c.MakecacheTimeout, defaultMakecacheTimeout)
}

func (c *Config) transientSlice() string {
	if c.TransientSlice == "" {
		return "cystemd.slice"
	}
	return c.TransientSlice
}

func (c *Config) allowedProperties() []string {
	if len(c.AllowedProperties) > 0 {
		return c.AllowedProperties
	}
	return defaultAllowedProperties
}

// validate rejects a configuration the driver cannot honour.
func (c *Config) validate() error {
	var errs []error
	switch c.PackageManager {
	case "", "dnf", "yum", "apt", "none":
	default:
		errs = append(errs, fmt.Errorf("package_manager %q: must be dnf, yum, apt or none", c.PackageManager))
	}
	for _, d := range []struct{ name, v string }{
		{"makecache_timeout", c.MakecacheTimeout},
		{"install_timeout", c.InstallTimeout},
	} {
		if d.v == "" {
			continue
		}
		if v, err := time.ParseDuration(d.v); err != nil || v <= 0 {
			errs = append(errs, fmt.Errorf("%s %q: must be a positive duration", d.name, d.v))
		}
	}
	for _, p := range append(append(append([]string{}, c.AllowedUnits...), c.AllowedPackages...), c.AllowedFileDestinations...) {
		if _, err := path.Match(strings.TrimSuffix(p, "/**"), ""); err != nil {
			errs = append(errs, fmt.Errorf("invalid pattern %q: %v", p, err))
		}
	}
	for _, d := range c.AllowedFileDestinations {
		if !filepath.IsAbs(d) {
			errs = append(errs, fmt.Errorf("allowed_file_destinations %q: must be absolute", d))
		}
	}
	if c.TransientSlice != "" && !strings.HasSuffix(c.TransientSlice, ".slice") {
		errs = append(errs, fmt.Errorf("transient_slice %q: must end in .slice", c.TransientSlice))
	}
	return errors.Join(errs...)
}

// PackageConfig is the task `package {}` block.
type PackageConfig struct {
	// Name defaults to the unit name without its .service suffix.
	Name    string `codec:"name"`
	Version string `codec:"version"`
}

// FileConfig is one task `file {}` block: Source (relative to the task
// directory, e.g. "local/app.yml" or "secrets/.env") is kept in sync with the
// host path Destination for the task's whole lifetime.
type FileConfig struct {
	Source      string `codec:"source"`
	Destination string `codec:"destination"`
	Mode        string `codec:"mode"`
	Owner       string `codec:"owner"`
	Group       string `codec:"group"`
}

// TaskConfig is the task `config {}` block.
type TaskConfig struct {
	Unit         string         `codec:"unit"`
	Package      *PackageConfig `codec:"package"`
	AdoptRunning bool           `codec:"adopt_running"`

	Command string   `codec:"command"`
	Args    []string `codec:"args"`
	WorkDir string   `codec:"work_dir"`

	Files            []FileConfig        `codec:"file"`
	EnvironmentFiles []string            `codec:"environment_files"`
	CPUQuota         string              `codec:"cpu_quota"`
	MemoryMax        string              `codec:"memory_max"`
	Properties       []map[string]string `codec:"properties"`
}

// taskMode is how a task maps onto systemd.
type taskMode string

const (
	modeUnit      taskMode = "unit"
	modeTransient taskMode = "transient"
)

func (t *TaskConfig) mode() taskMode {
	if t.Unit != "" {
		return modeUnit
	}
	return modeTransient
}

// validUnitName matches systemd unit names: alphanumerics plus ":_.\-@".
var validUnitName = regexp.MustCompile(`^[A-Za-z0-9:_.\\@-]+$`)

// validPropertyName matches systemd directive names.
var validPropertyName = regexp.MustCompile(`^[A-Z][A-Za-z0-9]+$`)

// normalizeUnit appends ".service" to a bare unit name (a name with no
// unit-type suffix), as systemctl does.
func normalizeUnit(u string) string {
	if strings.Contains(u, ".") {
		return u
	}
	return u + ".service"
}

// validate checks the task configuration against the plugin policy. It runs
// on the client (StartTask): job validation on the server cannot know the
// node's allow lists.
func (t *TaskConfig) validate(cfg *Config) error {
	var errs []error
	switch {
	case t.Unit != "" && t.Command != "":
		errs = append(errs, errors.New("set either unit (unit mode) or command (transient mode), not both"))
	case t.Unit == "" && t.Command == "":
		errs = append(errs, errors.New("one of unit or command is required"))
	}

	if t.Unit != "" {
		unit := normalizeUnit(t.Unit)
		switch {
		case !validUnitName.MatchString(unit) || strings.Contains(unit, ".."):
			errs = append(errs, fmt.Errorf("unit %q: invalid unit name", t.Unit))
		case !strings.HasSuffix(unit, ".service"):
			errs = append(errs, fmt.Errorf("unit %q: only .service units are supported", t.Unit))
		case !matchAny(cfg.AllowedUnits, unit):
			errs = append(errs, fmt.Errorf("unit %q is not allowed on this node (plugin allowed_units)", unit))
		}
		if len(t.Args) > 0 || t.WorkDir != "" {
			errs = append(errs, errors.New("args and work_dir apply to transient mode only"))
		}
	}

	if t.Command != "" {
		if !cfg.AllowTransient {
			errs = append(errs, errors.New("transient mode is disabled on this node (plugin allow_transient)"))
		}
		if t.WorkDir != "" && !filepath.IsAbs(t.WorkDir) {
			errs = append(errs, fmt.Errorf("work_dir %q: must be absolute", t.WorkDir))
		}
		if t.Package != nil || t.AdoptRunning {
			errs = append(errs, errors.New("package and adopt_running apply to unit mode only"))
		}
	}

	if t.Package != nil {
		name := t.packageName()
		switch {
		case name == "":
			errs = append(errs, errors.New("package: name is required"))
		case !validPackageName.MatchString(name):
			errs = append(errs, fmt.Errorf("package name %q: invalid", name))
		case !matchAny(cfg.AllowedPackages, name):
			errs = append(errs, fmt.Errorf("package %q is not allowed on this node (plugin allowed_packages)", name))
		}
		if !validPackageVersion.MatchString(t.Package.Version) {
			errs = append(errs, fmt.Errorf("package version %q: invalid", t.Package.Version))
		}
	}

	for i, f := range t.Files {
		if err := f.validate(cfg); err != nil {
			errs = append(errs, fmt.Errorf("file[%d]: %w", i, err))
		}
	}
	for _, e := range t.EnvironmentFiles {
		if err := validateTaskRelative(e); err != nil {
			errs = append(errs, fmt.Errorf("environment_files %q: %w", e, err))
		}
	}
	if t.CPUQuota != "" {
		if _, err := cpuQuotaPerSecUSec(t.CPUQuota); err != nil {
			errs = append(errs, fmt.Errorf("cpu_quota: %w", err))
		}
	}
	if t.MemoryMax != "" {
		if _, err := memoryBytes(t.MemoryMax); err != nil {
			errs = append(errs, fmt.Errorf("memory_max: %w", err))
		}
	}
	allowed := cfg.allowedProperties()
	for k, v := range t.flatProperties() {
		switch {
		case !validPropertyName.MatchString(k):
			errs = append(errs, fmt.Errorf("properties: invalid directive name %q", k))
		case !containsString(allowed, k):
			errs = append(errs, fmt.Errorf("properties: directive %q is not allowed on this node (plugin allowed_properties)", k))
		case strings.ContainsAny(v, "\n\r"):
			errs = append(errs, fmt.Errorf("properties: value of %q must be a single line", k))
		}
	}
	return errors.Join(errs...)
}

// packageName returns the package block's name, defaulting to the unit name
// without its suffix.
func (t *TaskConfig) packageName() string {
	if t.Package == nil {
		return ""
	}
	if t.Package.Name != "" {
		return t.Package.Name
	}
	return strings.TrimSuffix(normalizeUnit(t.Unit), ".service")
}

// flatProperties merges the properties maps (HCL decodes `properties = {}`
// into a one-element list).
func (t *TaskConfig) flatProperties() map[string]string {
	out := map[string]string{}
	for _, m := range t.Properties {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func (f *FileConfig) validate(cfg *Config) error {
	if err := validateTaskRelative(f.Source); err != nil {
		return fmt.Errorf("source %q: %w", f.Source, err)
	}
	dst := filepath.Clean(f.Destination)
	if !filepath.IsAbs(f.Destination) || dst != f.Destination {
		return fmt.Errorf("destination %q: must be an absolute, clean path", f.Destination)
	}
	if !matchAnyPath(cfg.AllowedFileDestinations, dst) {
		return fmt.Errorf("destination %q is not allowed on this node (plugin allowed_file_destinations)", f.Destination)
	}
	if f.Mode != "" {
		if _, err := parseMode(f.Mode); err != nil {
			return err
		}
	}
	return nil
}

// validateTaskRelative accepts a clean relative path that stays inside the
// task directory.
func validateTaskRelative(p string) error {
	if p == "" {
		return errors.New("must not be empty")
	}
	if filepath.IsAbs(p) {
		return errors.New("must be relative to the task directory")
	}
	c := filepath.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return errors.New("must stay inside the task directory")
	}
	return nil
}

// parseMode parses an octal file mode such as "0640".
func parseMode(s string) (uint32, error) {
	m, err := strconv.ParseUint(s, 8, 32)
	if err != nil || m > 0o7777 {
		return 0, fmt.Errorf("mode %q: must be octal, e.g. \"0640\"", s)
	}
	return uint32(m), nil
}

func parseDurationDefault(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
