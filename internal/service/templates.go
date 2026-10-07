package service

// templates.go: renders operator text/template files from Vault KV reads (no
// dynamic generators) into real files — 0600 from creation, content never
// logged; a changed file runs its optional reload hook.
// Why: DOCS/CLAUDE.md § Vault secret templating

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

// TemplateSpec is one source-template → destination-file mapping, with an
// optional reload command run when the rendered output changes.
type TemplateSpec struct {
	Source string // path to the text/template source file
	Dest   string // path to write the rendered output
	Reload string // optional shell command (sh -c) run when Dest changes
}

// DefaultTemplateMode is the permission applied to rendered output files. They
// may contain secrets, so they are owner-only, mirroring the .env contract.
const DefaultTemplateMode = 0o600

// templateReloadTimeout bounds a per-template reload command so a hung reload
// cannot wedge the render loop.
const templateReloadTimeout = 30 * time.Second

// parseTemplateSpecs parses VAULT_TEMPLATES: entries split on ';' or newline,
// each "source:destination[:reload]" split on its first two ':' — the reload
// remainder may contain ':' but never ';'. Fields are trimmed; an entry with no
// ':' or an empty source/destination is skipped with a Warning. "" → nil.
func parseTemplateSpecs(raw string) []TemplateSpec {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	entries := strings.FieldsFunc(raw, func(r rune) bool { return r == ';' || r == '\n' })
	var specs []TemplateSpec
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		idx := strings.Index(entry, ":")
		if idx < 0 {
			Warningf("vault: ignoring malformed VAULT_TEMPLATES entry %q (expected source:destination[:reload])", entry)
			continue
		}
		source := strings.TrimSpace(entry[:idx])
		rest := entry[idx+1:]
		var dest, reload string
		if j := strings.Index(rest, ":"); j >= 0 {
			dest = strings.TrimSpace(rest[:j])
			reload = strings.TrimSpace(rest[j+1:])
		} else {
			dest = strings.TrimSpace(rest)
		}
		if source == "" || dest == "" {
			Warningf("vault: ignoring malformed VAULT_TEMPLATES entry %q (empty source or destination)", entry)
			continue
		}
		specs = append(specs, TemplateSpec{Source: source, Dest: dest, Reload: reload})
	}
	return specs
}

// RenderTemplates renders every cfg.Templates spec from live Vault reads, each
// path read once per pass (cached). Fail-soft: a missing client or any
// per-template error logs and continues.
func RenderTemplates(cfg *VaultConfig) {
	if cfg == nil || len(cfg.Templates) == 0 {
		return
	}
	state := GetVaultClient()
	if state == nil || state.Client == nil {
		Infof("vault: skipping template render — Vault client not initialised (degraded mode or not configured)")
		return
	}

	cache := make(map[string]map[string]string)
	readPath := func(path string) (map[string]string, error) {
		if v, ok := cache[path]; ok {
			return v, nil
		}
		secrets, err := readSecretsV2ThenV1(state.Client, path)
		if err != nil {
			return nil, err
		}
		cache[path] = secrets
		return secrets, nil
	}

	funcs := template.FuncMap{
		// secret PATH → the flattened KV map at PATH, for map access:
		//   {{ (secret "kv/app").DB_PASSWORD }}
		"secret": func(path string) (map[string]string, error) {
			return readPath(path)
		},
		// secretField PATH FIELD → a single field, erroring if absent. Use for
		// field names that are not valid template identifiers (dashes, etc.):
		//   {{ secretField "kv/app" "db-password" }}
		"secretField": func(path, field string) (string, error) {
			m, err := readPath(path)
			if err != nil {
				return "", err
			}
			v, ok := m[field]
			if !ok {
				return "", fmt.Errorf("secret %q has no field %q", path, field)
			}
			return v, nil
		},
	}

	rendered := 0
	for _, spec := range cfg.Templates {
		if renderOneTemplate(spec, funcs) {
			rendered++
		}
	}
	if rendered > 0 {
		Infof("vault: rendered %d template(s)", rendered)
	}
}

// renderOneTemplate renders one spec and returns true only when it wrote a
// CHANGED file. Every failure logs a Warning and leaves the destination
// untouched — never a partial or "<no value>" secret file.
func renderOneTemplate(spec TemplateSpec, funcs template.FuncMap) bool {
	src, err := os.ReadFile(spec.Source)
	if err != nil {
		Warningf("vault: cannot read template source %q: %v — skipping", spec.Source, err)
		recordTemplateRender(false)
		return false
	}

	// missingkey=error: an absent map field aborts the render instead of emitting
	// "<no value>" (secretField checks its own field).
	tmpl, err := template.New(filepath.Base(spec.Source)).
		Option("missingkey=error").
		Funcs(funcs).
		Parse(string(src))
	if err != nil {
		Warningf("vault: cannot parse template %q: %v — skipping", spec.Source, err)
		recordTemplateRender(false)
		return false
	}

	// Write only after Execute fully succeeds: text/template streams partial
	// output before a mid-render error.
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, nil); err != nil {
		Warningf("vault: cannot render template %q → %q: %v — destination left unchanged", spec.Source, spec.Dest, err)
		recordTemplateRender(false)
		return false
	}
	newContent := buf.Bytes()

	// Re-tighten an existing destination, even on the unchanged path.
	ensureEnvFileMode(spec.Dest)

	if existing, readErr := os.ReadFile(spec.Dest); readErr == nil && bytes.Equal(existing, newContent) {
		Debugf("vault: template %q in sync", spec.Dest)
		recordTemplateRender(true)
		return false
	}

	if !writeSecretFileAtomic(spec.Dest, newContent) {
		recordTemplateRender(false)
		return false
	}
	recordTemplateRender(true)
	Infof("vault: rendered template %q → %q (%d bytes)", spec.Source, spec.Dest, len(newContent))
	if spec.Reload != "" {
		runTemplateReload(spec.Dest, spec.Reload)
	}
	return true
}

// runTemplateReload runs a changed template's reload command via sh -c, bounded
// by templateReloadTimeout. It logs the trigger BEFORE running (visible even if
// the command hangs); a failure only warns and never fails the render.
func runTemplateReload(dest, command string) {
	Infof("vault: template %q changed — running reload command: %s", dest, command)
	ctx, cancel := context.WithTimeout(context.Background(), templateReloadTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", command).CombinedOutput()
	if trimmed := strings.TrimSpace(string(out)); trimmed != "" {
		Debugf("vault: reload command output for %q: %s", dest, trimmed)
	}
	if err != nil {
		Warningf("vault: reload command for %q failed: %v — consumer may be using stale config", dest, err)
		recordTemplateReload(false)
		return
	}
	recordTemplateReload(true)
	Infof("vault: reload command for %q completed", dest)
}

// writeSecretFileAtomic writes content to dest via <dest>.tmp + rename, the tmp
// created 0600 (never chmod-after: no world-readable window). Fail-soft: any
// error logs a Warning, leaves dest unchanged and returns false.
// Why: DOCS/CLAUDE.md § Vault secret templating
func writeSecretFileAtomic(dest string, content []byte) bool {
	tmp := dest + ".tmp"
	// The create mode applies only to a NEW file: drop any stale or planted tmp,
	// then O_EXCL, so a path that re-appears fails the open, never written through.
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, DefaultTemplateMode)
	if err != nil {
		Warningf("vault: cannot create temp file %q: %v — destination left unchanged", tmp, err)
		return false
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		Warningf("vault: cannot write temp file %q: %v — destination left unchanged", tmp, err)
		return false
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		Warningf("vault: cannot close temp file %q: %v — destination left unchanged", tmp, err)
		return false
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		Warningf("vault: cannot rename %q → %q: %v — destination left unchanged", tmp, dest, err)
		return false
	}
	// Belt-and-suspenders tighten in case of odd umask/overlay behaviour.
	ensureEnvFileMode(dest)
	return true
}
