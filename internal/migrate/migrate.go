// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package migrate converts a CYSTEMD v4 installation into CYSTEMD v5
// configuration.
//
// v4 ran one daemon per host. It pulled config.yml from Git, matched the
// host name against node_pools, and installed, configured and restarted the
// continuous_deployment services of the host's pools, with settings and
// credentials in an .env file. v5 states the same intent declaratively:
//
//	continuous_deployment[]  Applications with a systemd source, one instance
//	                         on every node of the entry's pool
//	node_pools[]             client meta cystemd_selector_<pool> per host
//	secrets                  spec.source.systemd.secrets, read with the job's
//	                         workload identity (a Vault policy and JWT role
//	                         per application)
//	dnf, CYSTEMD_DNF_*       the systemd driver's makecache settings
//	GIT_*, auth, AUTH_*      the control plane's repository, git,
//	                         controller and auth settings
//	VAULT_TEMPLATES          a host Vault Agent configuration
//
// Nothing secret is copied: credentials are referenced by file path, and
// the report says where each must be installed.
package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/gitaginanjar/cystemd/internal/render"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Legacy is a CYSTEMD v4 config.yml.
type Legacy struct {
	Auth struct {
		Enabled       bool   `yaml:"enabled"`
		PublicKeyFile string `yaml:"public_key_file"`
	} `yaml:"auth"`
	ContinuousDeployment []Entry `yaml:"continuous_deployment"`
	DNF                  struct {
		MakecacheRepos   []string `yaml:"makecache_repos"`
		MakecacheTimeout string   `yaml:"makecache_timeout"`
	} `yaml:"dnf"`
	ListenAddress string     `yaml:"listen_address"`
	LogLevel      string     `yaml:"log_level"`
	NodePools     []NodePool `yaml:"node_pools"`
	Vault         struct {
		RefreshInterval string `yaml:"refresh_interval"`
		RenewalInterval string `yaml:"renewal_interval"`
		RenewalFloor    string `yaml:"renewal_floor"`
	} `yaml:"vault"`
	Version string `yaml:"version"`
}

// NodePool is a v4 node_pools[] entry.
type NodePool struct {
	Selector string   `yaml:"selector"`
	Nodes    []string `yaml:"nodes"`
}

// Entry is a v4 continuous_deployment[] entry.
type Entry struct {
	AutoSync            any    `yaml:"autosync"`
	GitBranch           string `yaml:"git_branch"`
	GitValues           string `yaml:"git_values"`
	GitValuesConfigKey  string `yaml:"git_values_config_key"`
	GitValuesCPUKey     string `yaml:"git_values_cpu_key"`
	GitValuesMemoryKey  string `yaml:"git_values_memory_key"`
	GitValuesVersionKey string `yaml:"git_values_version_key"`
	Replica             int    `yaml:"replica"`
	Secrets             string `yaml:"secrets"`
	Selector            string `yaml:"selector"`
	SelectorKey         string `yaml:"selectorkey"`
	Service             string `yaml:"service"`
	ServiceConfig       string `yaml:"service_config"`
	Workdir             string `yaml:"workdir"`
}

// autoSync is v4's tolerant reading of autosync: true only for bool true,
// 1, or "true"/"yes"/"1" in any case.
func (e Entry) autoSync() bool {
	switch v := e.AutoSync.(type) {
	case bool:
		return v
	case int:
		return v == 1
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "1":
			return true
		}
	}
	return false
}

// selectorValue is the ${selector} of the values keys: selectorkey, else
// selector.
func (e Entry) selectorValue() string {
	if e.SelectorKey != "" {
		return e.SelectorKey
	}
	return e.Selector
}

// ParseConfig reads a v4 config.yml. Unknown keys are reported as warnings
// (v4 ignored them too).
func ParseConfig(data []byte) (*Legacy, []string, error) {
	var cfg Legacy
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(&cfg)
	if err == nil {
		return &cfg, nil, nil
	}
	if errors.Is(err, io.EOF) {
		return &cfg, nil, nil
	}
	var warnings []string
	var te *yaml.TypeError
	if !errors.As(err, &te) {
		return nil, nil, fmt.Errorf("config.yml: %w", err)
	}
	for _, e := range te.Errors {
		warnings = append(warnings, "config.yml: "+e+" (ignored, as v4 did)")
	}
	cfg = Legacy{}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, nil, fmt.Errorf("config.yml: %w", err)
	}
	return &cfg, warnings, nil
}

// ParseEnv reads a v4 .env file: KEY=VALUE lines, # comments, optional
// "export ", double-quoted values unquoted (v4 wrote them with
// strconv.Quote), single quotes stripped.
func ParseEnv(data []byte) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		v = strings.TrimSpace(v)
		switch {
		case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
			if u, err := strconv.Unquote(v); err == nil {
				v = u
			} else {
				v = v[1 : len(v)-1]
			}
		case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
			v = v[1 : len(v)-1]
		}
		env[k] = v
	}
	return env
}

// Options steer the conversion.
type Options struct {
	// RepoURL is the GitOps repository (default: GIT_REPOSITORY).
	RepoURL string
	// Project of the Applications (default "default").
	Project string
	// Destination of the Applications (default: in-cluster, "default").
	DestName, DestNamespace, DestRegion string
	// KVVersion of the secrets' KV mounts, 1 or 2 (default 2). v4 tried
	// v2, then v1, on every read; v5 renders a fixed path.
	KVVersion int
	// SecretsDelivery is "nomad" (default) or "vault-agent".
	SecretsDelivery string
	// JWTAuthMount is the Vault JWT auth mount of Nomad workload
	// identities (default "jwt-nomad").
	JWTAuthMount string
	// VaultCACert is the CA certificate host Vault Agents verify Vault
	// with (v4 skipped verification by default).
	VaultCACert string
	// TemplatesDir is searched for VAULT_TEMPLATES sources (by file name)
	// that are not at their original path, e.g. copied from a host.
	TemplatesDir string
	// ReadFile reads template sources (default os.ReadFile).
	ReadFile func(string) ([]byte, error)
}

// Result is the converted installation.
type Result struct {
	Applications []*v1.Application
	Hosts        []Host
	// Files are the generated files, by path relative to the output
	// directory.
	Files map[string][]byte
	// Warnings are things to check before the cut-over.
	Warnings []string
}

// Host is a v4 host: a node_pools[] member.
type Host struct {
	Name      string
	Selectors []string
	Apps      []string
}

// app is one converted entry.
type app struct {
	entry Entry
	app   *v1.Application
	role  string // Vault JWT role ("" without secrets)
}

// Convert converts a v4 configuration and environment.
func Convert(cfg *Legacy, env map[string]string, o Options) (*Result, error) {
	if o.Project == "" {
		o.Project = v1.DefaultProject
	}
	if o.KVVersion == 0 {
		o.KVVersion = 2
	}
	if o.KVVersion != 1 && o.KVVersion != 2 {
		return nil, fmt.Errorf("kv version %d: 1 or 2", o.KVVersion)
	}
	switch o.SecretsDelivery {
	case "", "nomad", "vault-agent":
	default:
		return nil, fmt.Errorf("secrets delivery %q: nomad or vault-agent", o.SecretsDelivery)
	}
	if o.JWTAuthMount == "" {
		o.JWTAuthMount = "jwt-nomad"
	}
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	repo := firstNonEmpty(o.RepoURL, env["GIT_REPOSITORY"])
	if repo == "" && len(cfg.ContinuousDeployment) > 0 {
		return nil, errors.New("no GitOps repository: set GIT_REPOSITORY in the .env or give the repository URL")
	}

	r := &Result{Files: map[string][]byte{}}
	warn := func(format string, args ...any) { r.Warnings = append(r.Warnings, fmt.Sprintf(format, args...)) }

	pools := map[string]bool{}
	for _, p := range cfg.NodePools {
		pools[p.Selector] = true
	}
	declared := map[string]int{}
	for _, e := range cfg.ContinuousDeployment {
		declared[e.Service]++
	}

	var apps []*app
	names := map[string]bool{}
	for i, e := range cfg.ContinuousDeployment {
		if e.Service == "" {
			warn("continuous_deployment[%d] has no service: skipped", i)
			continue
		}
		if e.GitValues == "" {
			warn("%s: no git_values; v5 renders a systemd source from a values file, so it is skipped", e.Service)
			continue
		}
		name := AppName(e.Service)
		if declared[e.Service] > 1 {
			name = AppName(e.Service + "-" + e.Selector)
		}
		for base, n := name, 2; names[name]; n++ {
			name = fmt.Sprintf("%s-%d", base, n)
		}
		names[name] = true
		a, role := convertEntry(name, e, repo, o)
		if err := a.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Service, err)
		}
		if !pools[e.Selector] {
			warn("%s: selector %q is in no node_pools entry: no host runs it until a client has meta %s = \"true\"",
				name, e.Selector, render.SelectorMetaKey(e.Selector))
		}
		apps = append(apps, &app{entry: e, app: a, role: role})
		r.Applications = append(r.Applications, a)
	}

	r.Hosts = hosts(cfg, apps, warn)

	var buf bytes.Buffer
	buf.WriteString("# CYSTEMD v4 continuous_deployment, converted by `cystemd migrate`.\n")
	buf.WriteString("# Create with: cystemd app create -f applications.yaml\n")
	for _, a := range r.Applications {
		buf.WriteString("---\n")
		if err := encodeYAML(&buf, a); err != nil {
			return nil, err
		}
	}
	r.Files["applications.yaml"] = buf.Bytes()

	dnf := dnfSettings(cfg, env)
	for _, h := range r.Hosts {
		b, err := hostConfig(h, apps, dnf, o)
		if err != nil {
			return nil, err
		}
		r.Files["hosts/"+fileName(h.Name)+".hcl"] = b
	}
	if err := vaultRoles(r, apps, o); err != nil {
		return nil, err
	}
	cp, notes := controlPlane(cfg, env, repo)
	r.Files["control-plane.hcl"] = cp
	r.Warnings = append(r.Warnings, notes...)
	if err := hostVaultAgent(r, cfg, env, o); err != nil {
		return nil, err
	}
	r.Files["MIGRATION.md"] = report(r, cfg, env, apps, o)
	return r, nil
}

// unsafeName matches characters not allowed in an Application name.
var unsafeName = regexp.MustCompile(`[^a-z0-9._-]+`)

// AppName turns a v4 service name into an Application name: lowercase,
// other characters as '-', starting and ending alphanumeric.
func AppName(service string) string {
	n := unsafeName.ReplaceAllString(strings.ToLower(service), "-")
	n = strings.TrimFunc(n, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') })
	if len(n) > 253 {
		n = strings.TrimRight(n[:253], "-._")
	}
	if n == "" {
		n = "app"
	}
	return n
}

func convertEntry(name string, e Entry, repo string, o Options) (*v1.Application, string) {
	dir, file := path.Split(strings.TrimPrefix(e.GitValues, "/"))
	branch := e.GitBranch
	if branch == "" {
		branch = "master"
	}
	zero := 0
	sd := &v1.SystemdSource{
		ValuesFile:      file,
		Selector:        e.selectorValue(),
		ConfigKey:       e.GitValuesConfigKey,
		VersionKey:      e.GitValuesVersionKey,
		CPUKey:          e.GitValuesCPUKey,
		MemoryKey:       e.GitValuesMemoryKey,
		ServiceConfig:   e.ServiceConfig,
		Replicas:        &zero,
		NodeSelector:    e.Selector,
		SecretsDelivery: o.SecretsDelivery,
	}
	if e.Service != name {
		sd.Unit = e.Service
	}
	if e.Workdir != "" && path.Clean(e.Workdir) != "/opt" {
		sd.Workdir = e.Workdir
	}
	role := ""
	if s := strings.Trim(e.Secrets, "/"); s != "" {
		sec := v1.SystemdSecret{VaultPath: s}
		if o.KVVersion == 1 {
			sec.KVVersion = 1
		}
		sd.Secrets = []v1.SystemdSecret{sec}
		role = "cystemd-app-" + name
		sd.VaultRole = role
	}
	a := &v1.Application{
		APIVersion: v1.APIVersion,
		Kind:       v1.KindApplication,
		Metadata: v1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"cystemd.io/migrated-from": "v4",
				"cystemd.io/v4-selector":   e.Selector,
			},
		},
		Spec: v1.ApplicationSpec{
			Project: o.Project,
			Source: v1.ApplicationSource{
				RepoURL:        repo,
				Path:           strings.TrimSuffix(dir, "/"),
				TargetRevision: branch,
				Systemd:        sd,
			},
			Destination: v1.ApplicationDestination{Name: o.DestName, Namespace: o.DestNamespace, Region: o.DestRegion},
		},
	}
	if e.autoSync() {
		// v4 autosync installed, configured and restarted on change, and
		// never removed a unit: self-heal without prune.
		a.Spec.SyncPolicy = &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{SelfHeal: true}}
	}
	return a, role
}

// hosts lists the hosts of node_pools (matched case-insensitively, as v4
// matched host names) with their pools and Applications.
func hosts(cfg *Legacy, apps []*app, warn func(string, ...any)) []Host {
	byKey := map[string]*Host{}
	var order []string
	for _, p := range cfg.NodePools {
		for _, n := range p.Nodes {
			k := strings.ToLower(strings.TrimSpace(n))
			if k == "" {
				continue
			}
			h, ok := byKey[k]
			if !ok {
				h = &Host{Name: strings.TrimSpace(n)}
				byKey[k] = h
				order = append(order, k)
			}
			if !contains(h.Selectors, p.Selector) {
				h.Selectors = append(h.Selectors, p.Selector)
			}
		}
	}
	sort.Strings(order)
	out := make([]Host, 0, len(order))
	for _, k := range order {
		h := byKey[k]
		units := map[string]string{}
		for _, a := range apps {
			if !contains(h.Selectors, a.entry.Selector) {
				continue
			}
			h.Apps = append(h.Apps, a.app.Metadata.Name)
			u := unitOf(a.app)
			if other, dup := units[u]; dup {
				warn("host %s: %s and %s both manage %s (v4 managed the first entry only); keep the host in one of their pools",
					h.Name, other, a.app.Metadata.Name, u)
			}
			units[u] = a.app.Metadata.Name
		}
		out = append(out, *h)
	}
	return out
}

// unitOf is the unit an Application manages, as the renderer derives it.
func unitOf(a *v1.Application) string {
	u := a.Spec.Source.Systemd.Unit
	if u == "" {
		u = a.Metadata.Name
	}
	if !strings.Contains(u, ".") {
		u += ".service"
	}
	return u
}

// hostDir is the directory the renderer installs an Application's files
// into.
func hostDir(a *v1.Application) string {
	sd := a.Spec.Source.Systemd
	w := sd.Workdir
	if w == "" {
		w = "/opt"
	}
	return path.Join(w, strings.TrimSuffix(unitOf(a), ".service"))
}

type dnf struct {
	repos   []string
	timeout string
}

// dnfSettings merges dnf with CYSTEMD_DNF_* (the environment wins, as in v4).
func dnfSettings(cfg *Legacy, env map[string]string) dnf {
	var d dnf
	src := cfg.DNF.MakecacheRepos
	if v := strings.TrimSpace(env["CYSTEMD_DNF_MAKECACHE_REPOS"]); v != "" {
		src = strings.Split(v, ",")
	}
	for _, r := range src {
		if r = strings.TrimSpace(r); r != "" {
			d.repos = append(d.repos, r)
		}
	}
	d.timeout = firstNonEmpty(strings.TrimSpace(env["CYSTEMD_DNF_MAKECACHE_TIMEOUT"]), strings.TrimSpace(cfg.DNF.MakecacheTimeout))
	return d
}

// fileName makes a host name safe as a file name.
func fileName(s string) string {
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(s, "_")
}

func encodeYAML(buf *bytes.Buffer, v any) error {
	enc := yaml.NewEncoder(buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return err
	}
	return enc.Close()
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
