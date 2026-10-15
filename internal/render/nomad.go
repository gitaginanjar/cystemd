// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/nomad/api"
	"gopkg.in/yaml.v3"

	"github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// defaultJobPatterns select jobspecs when NomadSource.Files is empty.
var defaultJobPatterns = []string{"*.nomad.hcl", "*.nomad", "*.nomad.json"}

// renderNomad renders the jobspecs under dir.
func (r *Renderer) renderNomad(ctx context.Context, app *v1alpha1.Application, dir string) (*Manifests, error) {
	src := app.Spec.Source.Nomad
	if src == nil {
		src = &v1alpha1.NomadSource{}
	}
	patterns := src.Files
	if len(patterns) == 0 {
		patterns = defaultJobPatterns
	}
	files, err := matchFiles(dir, patterns)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no jobspec matching %s in %q", strings.Join(patterns, ", "), app.Spec.Source.Path)
	}

	var varFiles [][]byte
	for _, vf := range src.VarFiles {
		b, err := readInside(dir, vf)
		if err != nil {
			return nil, fmt.Errorf("var file %s: %w", vf, err)
		}
		varFiles = append(varFiles, b)
	}
	variables, err := mergeVariables(varFiles, src.Vars)
	if err != nil {
		return nil, err
	}

	m := &Manifests{SourceType: v1alpha1.SourceTypeNomad, Sources: map[string]*api.JobSubmission{}}
	for _, f := range files {
		content, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return nil, err
		}
		var job *api.Job
		format := "hcl2"
		if strings.HasSuffix(f, ".json") {
			format = "json"
			job, err = parseJobJSON(content)
		} else {
			if r.Parse == nil {
				return nil, errors.New("no Nomad parser configured")
			}
			job, err = r.Parse(ctx, string(content), variables)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		if job.ID == nil || *job.ID == "" {
			return nil, fmt.Errorf("%s: job has no ID", f)
		}
		sub := &api.JobSubmission{Source: string(content), Format: format}
		if format == "hcl2" && variables != "" {
			sub.Variables = variables
		}
		m.Sources[*job.ID] = sub
		m.Jobs = append(m.Jobs, job)
	}

	if src.ConsulKV != "" {
		kv, err := readConsulKVFile(dir, src.ConsulKV, r.Opts.kvPrefix())
		if err != nil {
			return nil, err
		}
		m.KV = kv
	}
	return m, nil
}

// matchFiles lists the files directly in dir matching any pattern, sorted.
func matchFiles(dir string, patterns []string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read source path: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		for _, p := range patterns {
			if ok, _ := path.Match(p, e.Name()); ok {
				out = append(out, e.Name())
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// readInside reads rel below dir, refusing paths that leave it.
func readInside(dir, rel string) ([]byte, error) {
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return nil, fmt.Errorf("%q must stay inside the source path", rel)
	}
	return os.ReadFile(filepath.Join(dir, clean))
}

// parseJobJSON accepts `{"Job": {...}}` (the API's register body) or a bare
// job object.
func parseJobJSON(content []byte) (*api.Job, error) {
	var wrapper struct {
		Job *api.Job `json:"Job"`
	}
	if err := json.Unmarshal(content, &wrapper); err == nil && wrapper.Job != nil {
		wrapper.Job.Canonicalize()
		return wrapper.Job, nil
	}
	var job api.Job
	dec := json.NewDecoder(bytes.NewReader(content))
	if err := dec.Decode(&job); err != nil {
		return nil, fmt.Errorf("invalid job JSON: %w", err)
	}
	job.Canonicalize()
	return &job, nil
}

// mergeVariables merges HCL2 variable files (later files win) and explicit
// overrides into one variables document for /v1/jobs/parse. A plain
// concatenation would fail on a variable set twice.
func mergeVariables(files [][]byte, overrides map[string]string) (string, error) {
	values := map[string]string{}
	var order []string
	set := func(name, expr string) {
		if _, ok := values[name]; !ok {
			order = append(order, name)
		}
		values[name] = expr
	}
	for i, content := range files {
		f, diags := hclsyntax.ParseConfig(content, fmt.Sprintf("varfile%d.hcl", i), hcl.InitialPos)
		if diags.HasErrors() {
			return "", fmt.Errorf("variable file: %s", diags.Error())
		}
		body, ok := f.Body.(*hclsyntax.Body)
		if !ok {
			return "", errors.New("variable file: unexpected body")
		}
		if len(body.Blocks) > 0 {
			return "", fmt.Errorf("variable file: blocks are not allowed (%s)", body.Blocks[0].Type)
		}
		for name, attr := range body.Attributes {
			rng := attr.Expr.Range()
			set(name, string(content[rng.Start.Byte:rng.End.Byte]))
		}
	}
	names := make([]string, 0, len(overrides))
	for name := range overrides {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !hclsyntax.ValidIdentifier(name) {
			return "", fmt.Errorf("invalid variable name %q", name)
		}
		set(name, hclString(overrides[name]))
	}
	var b strings.Builder
	for _, name := range order {
		fmt.Fprintf(&b, "%s = %s\n", name, values[name])
	}
	return b.String(), nil
}

// hclString quotes s as an HCL string literal with interpolation disabled.
func hclString(s string) string {
	q := strconv.Quote(s)
	q = strings.ReplaceAll(q, "${", "$${")
	return strings.ReplaceAll(q, "%{", "%%{")
}

// consulKVEntry is one entry of a consul-kv file.
type consulKVEntry struct {
	Key   string `yaml:"key"`
	Value string `yaml:"value"`
	File  string `yaml:"file"`
}

// readConsulKVFile reads a YAML list of {key, value | file}. Keys under the
// control plane's own prefix are reserved and rejected.
func readConsulKVFile(dir, rel, kvPrefix string) ([]KVPair, error) {
	b, err := readInside(dir, rel)
	if err != nil {
		return nil, fmt.Errorf("consul kv file: %w", err)
	}
	var entries []consulKVEntry
	if err := yaml.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("consul kv file %s: %w", rel, err)
	}
	var out []KVPair
	seen := map[string]bool{}
	for i, e := range entries {
		key := strings.Trim(e.Key, "/")
		switch {
		case key == "":
			return nil, fmt.Errorf("consul kv file %s: entry %d has no key", rel, i)
		case key == kvPrefix || strings.HasPrefix(key, kvPrefix+"/"):
			return nil, fmt.Errorf("consul kv file %s: key %q is under the reserved prefix %q", rel, key, kvPrefix)
		case seen[key]:
			return nil, fmt.Errorf("consul kv file %s: key %q is defined twice", rel, key)
		case e.File != "" && e.Value != "":
			return nil, fmt.Errorf("consul kv file %s: key %q sets both value and file", rel, key)
		}
		seen[key] = true
		value := []byte(e.Value)
		if e.File != "" {
			if value, err = readInside(dir, e.File); err != nil {
				return nil, fmt.Errorf("consul kv file %s: key %q: %w", rel, key, err)
			}
		}
		out = append(out, KVPair{Key: key, Value: value})
	}
	return out, nil
}
