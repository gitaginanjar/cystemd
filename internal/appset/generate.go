// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package appset generates the Applications of ApplicationSets (Argo CD's
// ApplicationSet controller): generators produce sets of parameters, a Go
// template of an Application is executed with each.
package appset

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Params is one set of parameters: the data of a template.
type Params = map[string]any

// Cluster describes a destination cluster to the clusters generator.
type Cluster struct {
	Name   string
	Server string
	Labels map[string]string
}

// Repos is what the git generator reads of repositories.
type Repos interface {
	// Resolve turns a revision into a commit.
	Resolve(ctx context.Context, repoURL, revision string) (string, error)
	// Checkout calls fn with the work tree of repoURL at sha.
	Checkout(ctx context.Context, repoURL, sha string, fn func(dir string) error) error
}

// Generator produces the parameters and Applications of ApplicationSets.
type Generator struct {
	Repos    Repos
	Clusters func() []Cluster

	mu    sync.Mutex
	cache map[string]cachedGit // the git generator's findings, by commit
}

type cachedGit struct {
	params []Params
	at     time.Time
}

const (
	// maxParams bounds the parameter sets of one ApplicationSet.
	maxParams = 5000
	// maxFileSize bounds a git generator's file.
	maxFileSize = 1 << 20
	gitCacheTTL = time.Hour
	gitCacheMax = 256
)

// Applications generates an ApplicationSet's Applications.
func (g *Generator) Applications(ctx context.Context, set *v1.ApplicationSet) ([]*v1.Application, error) {
	params, err := g.Params(ctx, set)
	if err != nil {
		return nil, err
	}
	return Render(set, params)
}

// Params runs an ApplicationSet's generators: the parameter sets of each,
// one after the other.
func (g *Generator) Params(ctx context.Context, set *v1.ApplicationSet) ([]Params, error) {
	var out []Params
	for i := range set.Spec.Generators {
		ps, err := g.generate(ctx, &set.Spec.Generators[i], set.Spec.GoTemplateOptions)
		if err != nil {
			return nil, fmt.Errorf("generators[%d]: %w", i, err)
		}
		out = append(out, ps...)
		if len(out) > maxParams {
			return nil, fmt.Errorf("the generators produce more than %d parameter sets", maxParams)
		}
	}
	return out, nil
}

func (g *Generator) generate(ctx context.Context, gen *v1.ApplicationSetGenerator, opts []string) ([]Params, error) {
	switch {
	case gen.List != nil:
		out := make([]Params, 0, len(gen.List.Elements))
		for _, e := range gen.List.Elements {
			out = append(out, copyParams(jsonable(map[string]any(e)).(map[string]any)))
		}
		return out, nil
	case gen.Clusters != nil:
		return g.clusters(gen.Clusters, opts)
	case gen.Git != nil:
		return g.git(ctx, gen.Git, opts)
	case gen.Matrix != nil:
		return g.matrix(ctx, gen.Matrix, opts)
	case gen.Merge != nil:
		return g.merge(ctx, gen.Merge, opts)
	}
	return nil, errors.New("a generator is one of list, clusters, git, matrix, merge")
}

// withValues adds a generator's values, executed with the parameters, as
// values.<key>.
func withValues(p Params, values map[string]string, opts []string) (Params, error) {
	if len(values) == 0 {
		return p, nil
	}
	out := map[string]any{}
	for _, k := range sortedKeys(values) {
		v, err := execute(values[k], p, opts)
		if err != nil {
			return nil, fmt.Errorf("values.%s: %w", k, err)
		}
		out[k] = v
	}
	p["values"] = out
	return p, nil
}

func (g *Generator) clusters(gen *v1.ClusterGenerator, opts []string) ([]Params, error) {
	if g.Clusters == nil {
		return nil, nil
	}
	var out []Params
	for _, c := range g.Clusters() {
		if !gen.Selector.Matches(c.Labels) {
			continue
		}
		labels := map[string]any{}
		for k, v := range c.Labels {
			labels[k] = v
		}
		p := Params{"name": c.Name, "nameNormalized": Normalize(c.Name), "server": c.Server,
			"metadata": map[string]any{"labels": labels}}
		p, err := withValues(p, gen.Values, opts)
		if err != nil {
			return nil, fmt.Errorf("clusters: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

// pathParams describe a directory of a repository (path, basename,
// basenameNormalized, segments).
func pathParams(dir string) map[string]any {
	segs := []any{}
	for _, s := range strings.Split(dir, "/") {
		if s != "" && s != "." {
			segs = append(segs, s)
		}
	}
	base := path.Base(dir)
	return map[string]any{"path": dir, "basename": base, "basenameNormalized": Normalize(base), "segments": segs}
}

func (g *Generator) git(ctx context.Context, gen *v1.GitGenerator, opts []string) ([]Params, error) {
	if g.Repos == nil {
		return nil, errNoRepos
	}
	rev := cmp.Or(gen.Revision, "HEAD")
	sha, err := g.Repos.Resolve(ctx, gen.RepoURL, rev)
	if err != nil {
		return nil, fmt.Errorf("git %s at %s: %w", gen.RepoURL, rev, err)
	}
	spec, _ := json.Marshal(struct {
		D []v1.GitDirectoryGeneratorItem
		F []v1.GitFileGeneratorItem
	}{gen.Directories, gen.Files})
	sum := sha256.Sum256(append([]byte(gen.RepoURL+"@"+sha+"\x00"), spec...))
	key := hex.EncodeToString(sum[:])

	found, ok := g.cached(key)
	if !ok {
		err = g.Repos.Checkout(ctx, gen.RepoURL, sha, func(root string) error {
			var err error
			if len(gen.Directories) > 0 {
				found, err = directories(root, gen.Directories)
			} else {
				found, err = files(root, gen.Files)
			}
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("git %s at %s: %w", gen.RepoURL, rev, err)
		}
		g.remember(key, found)
	}
	out := make([]Params, 0, len(found))
	for _, f := range found {
		p := copyParams(f)
		if gen.PathParamPrefix != "" {
			p[gen.PathParamPrefix] = map[string]any{"path": p["path"]}
			delete(p, "path")
		}
		p, err := withValues(p, gen.Values, opts)
		if err != nil {
			return nil, fmt.Errorf("git: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
}

func (g *Generator) cached(key string) ([]Params, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.cache[key]
	if !ok || time.Since(c.at) > gitCacheTTL {
		return nil, false
	}
	return c.params, true
}

func (g *Generator) remember(key string, params []Params) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cache == nil || len(g.cache) >= gitCacheMax {
		g.cache = map[string]cachedGit{}
	}
	g.cache[key] = cachedGit{params: params, at: time.Now()}
}

// walk visits the work tree's entries, as slash-separated paths relative
// to root, down to depth (-1: any). Hidden ones (.git, .github...) are
// skipped.
func walk(root string, depth int, fn func(rel string, d fs.DirEntry) error) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, root+string(filepath.Separator)))
		hidden := strings.HasPrefix(d.Name(), ".")
		if d.IsDir() && (hidden || (depth >= 0 && strings.Count(rel, "/")+1 > depth)) {
			return filepath.SkipDir
		}
		if hidden {
			return nil
		}
		return fn(rel, d)
	})
}

// directories finds the directories the items match: some include item
// matches, and no exclude item does.
func directories(root string, items []v1.GitDirectoryGeneratorItem) ([]Params, error) {
	depth := 0
	for _, it := range items {
		if d := maxDepth(it.Path); d < 0 || depth < 0 {
			depth = -1
		} else if d > depth {
			depth = d
		}
	}
	var found []string
	err := walk(root, depth, func(rel string, d fs.DirEntry) error {
		if !d.IsDir() {
			return nil
		}
		included := false
		for _, it := range items {
			if globMatch(it.Path, rel) {
				if it.Exclude {
					return nil
				}
				included = true
			}
		}
		if included {
			found = append(found, rel)
		}
		if len(found) > maxParams {
			return fmt.Errorf("more than %d directories match", maxParams)
		}
		return nil
	})
	sort.Strings(found)
	out := make([]Params, 0, len(found))
	for _, dir := range found {
		out = append(out, Params{"path": pathParams(dir)})
	}
	return out, err
}

// files reads the files the items match: one set of parameters per file
// holding a YAML or JSON object, or per element of a file holding a list.
func files(root string, items []v1.GitFileGeneratorItem) ([]Params, error) {
	depth := 0
	for _, it := range items {
		if d := maxDepth(it.Path); d < 0 || depth < 0 {
			depth = -1
		} else if d > depth {
			depth = d
		}
	}
	var found []string
	err := walk(root, depth, func(rel string, d fs.DirEntry) error {
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		for _, it := range items {
			if globMatch(it.Path, rel) {
				found = append(found, rel)
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	var out []Params
	for _, rel := range found {
		fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		if fi.Size() > maxFileSize {
			return nil, fmt.Errorf("%s is larger than %d bytes", rel, maxFileSize)
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return nil, err
		}
		var doc any
		if err := yaml.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		var objs []map[string]any
		switch x := jsonable(doc).(type) {
		case map[string]any:
			objs = append(objs, x)
		case []any:
			for i, e := range x {
				m, ok := e.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("%s: element %d is not an object", rel, i)
				}
				objs = append(objs, m)
			}
		case nil:
		default:
			return nil, fmt.Errorf("%s holds neither an object nor a list of objects", rel)
		}
		dir := path.Dir(rel)
		pp := pathParams(dir)
		pp["filename"] = path.Base(rel)
		pp["filenameNormalized"] = Normalize(path.Base(rel))
		for _, o := range objs {
			p := Params(o)
			p["path"] = copyValue(pp)
			out = append(out, p)
		}
		if len(out) > maxParams {
			return nil, fmt.Errorf("the files produce more than %d parameter sets", maxParams)
		}
	}
	return out, nil
}

// matrix combines its two generators' parameter sets; the second's own
// fields are templates of the first's parameters.
func (g *Generator) matrix(ctx context.Context, m *v1.MatrixGenerator, opts []string) ([]Params, error) {
	first, err := g.generate(ctx, &m.Generators[0], opts)
	if err != nil {
		return nil, fmt.Errorf("matrix.generators[0]: %w", err)
	}
	second, err := tree(m.Generators[1])
	if err != nil {
		return nil, err
	}
	var out []Params
	for _, p1 := range first {
		rendered, err := renderTree(second, p1, opts, true)
		if err != nil {
			return nil, err
		}
		var gen v1.ApplicationSetGenerator
		if err := fromTree(rendered, &gen); err != nil {
			return nil, fmt.Errorf("matrix.generators[1]: %w", err)
		}
		ps, err := g.generate(ctx, &gen, opts)
		if err != nil {
			return nil, fmt.Errorf("matrix.generators[1] with %s: %w", Describe(p1), err)
		}
		for _, p2 := range ps {
			out = append(out, mergeParams(p1, p2))
		}
		if len(out) > maxParams {
			return nil, fmt.Errorf("matrix: more than %d parameter sets", maxParams)
		}
	}
	return out, nil
}

// merge merges the other generators' parameter sets into the first's,
// where the values of every merge key are equal.
func (g *Generator) merge(ctx context.Context, m *v1.MergeGenerator, opts []string) ([]Params, error) {
	base, err := g.generate(ctx, &m.Generators[0], opts)
	if err != nil {
		return nil, fmt.Errorf("merge.generators[0]: %w", err)
	}
	key := func(p Params) (string, bool) {
		var parts []string
		for _, k := range m.MergeKeys {
			v, ok := lookup(p, k)
			if !ok {
				return "", false
			}
			b, _ := json.Marshal(v)
			parts = append(parts, string(b))
		}
		return strings.Join(parts, "\x00"), true
	}
	for i := 1; i < len(m.Generators); i++ {
		ps, err := g.generate(ctx, &m.Generators[i], opts)
		if err != nil {
			return nil, fmt.Errorf("merge.generators[%d]: %w", i, err)
		}
		byKey := map[string]Params{}
		for _, p := range ps {
			if k, ok := key(p); ok {
				byKey[k] = p
			}
		}
		for j, b := range base {
			if k, ok := key(b); ok {
				if p, ok := byKey[k]; ok {
					base[j] = mergeParams(b, p)
				}
			}
		}
	}
	return base, nil
}
