// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package appset

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"gopkg.in/yaml.v3"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// funcs are the templates' functions: Sprig's (as Argo CD's), without
// those that read the environment or the network, and normalize, toYaml
// and fromYaml.
var funcs = func() template.FuncMap {
	f := sprig.TxtFuncMap()
	for _, unsafe := range []string{"env", "expandenv", "getHostByName"} {
		delete(f, unsafe)
	}
	f["normalize"] = Normalize
	f["toYaml"] = func(v any) (string, error) {
		b, err := yaml.Marshal(v)
		return strings.TrimSuffix(string(b), "\n"), err
	}
	f["fromYaml"] = func(s string) (any, error) {
		var v any
		err := yaml.Unmarshal([]byte(s), &v)
		return jsonable(v), err
	}
	return f
}()

var nonDNS = regexp.MustCompile(`[^a-z0-9.]+`)

// Normalize makes a name fit an Application's (Argo CD's nameNormalized):
// lowercase, other characters as '-', at most 63 characters.
func Normalize(s string) string {
	s = strings.Trim(nonDNS.ReplaceAllString(strings.ToLower(s), "-"), "-.")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-.")
	}
	return s
}

// options are the templates' options; missingkey=error by default.
func options(opts []string) []string {
	if len(opts) == 0 {
		return []string{"missingkey=error"}
	}
	return opts
}

// execute runs one template string.
func execute(text string, p Params, opts []string) (string, error) {
	t, err := template.New("").Funcs(funcs).Option(options(opts)...).Parse(text)
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := t.Execute(&b, p); err != nil {
		return "", err
	}
	return b.String(), nil
}

// renderTree executes the templates of a JSON tree: its strings and its
// object keys. lenient keeps what fails as it is (the second generator of
// a matrix: its own templates are for its own parameters).
func renderTree(v any, p Params, opts []string, lenient bool) (any, error) {
	switch x := v.(type) {
	case string:
		if !strings.Contains(x, "{{") {
			return x, nil
		}
		out, err := execute(x, p, opts)
		if err != nil {
			if lenient {
				return x, nil
			}
			return nil, fmt.Errorf("%q: %w", x, err)
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			key, err := renderTree(k, p, opts, lenient)
			if err != nil {
				return nil, err
			}
			if out[key.(string)], err = renderTree(val, p, opts, lenient); err != nil {
				return nil, err
			}
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			var err error
			if out[i], err = renderTree(val, p, opts, lenient); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return v, nil
}

// tree is v as a JSON tree.
func tree(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var t any
	return t, json.Unmarshal(b, &t)
}

// fromTree decodes a JSON tree into out.
func fromTree(t, out any) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// Render stamps out the template once per set of parameters. The
// Applications carry the ApplicationSet's label and, unless it preserves
// resources on deletion, the resources finalizer (deleting one deletes its
// jobs and keys).
func Render(set *v1.ApplicationSet, params []Params) ([]*v1.Application, error) {
	t, err := tree(set.Spec.Template)
	if err != nil {
		return nil, err
	}
	preserve := set.Spec.SyncPolicy != nil && set.Spec.SyncPolicy.PreserveResourcesOnDeletion
	from := map[string]int{}
	out := make([]*v1.Application, 0, len(params))
	for i, p := range params {
		rendered, err := renderTree(t, p, set.Spec.GoTemplateOptions, false)
		if err != nil {
			return nil, fmt.Errorf("parameters %s: %w", Describe(p), err)
		}
		var tm v1.ApplicationSetTemplate
		if err := fromTree(rendered, &tm); err != nil {
			return nil, fmt.Errorf("parameters %s: %w", Describe(p), err)
		}
		app := &v1.Application{
			Metadata: v1.ObjectMeta{Name: tm.Metadata.Name, Labels: maps.Clone(tm.Metadata.Labels),
				Annotations: tm.Metadata.Annotations, Finalizers: tm.Metadata.Finalizers},
			Spec: tm.Spec,
		}
		if app.Metadata.Labels == nil {
			app.Metadata.Labels = map[string]string{}
		}
		app.Metadata.Labels[v1.AppSetLabelKey] = set.Metadata.Name
		if !preserve && !app.Metadata.HasFinalizer(v1.ResourcesFinalizer) {
			app.Metadata.Finalizers = append(app.Metadata.Finalizers, v1.ResourcesFinalizer)
		}
		app.SetDefaults()
		if err := app.Validate(); err != nil {
			return nil, fmt.Errorf("application %q (parameters %s): %w", app.Metadata.Name, Describe(p), err)
		}
		if j, dup := from[app.Metadata.Name]; dup {
			return nil, fmt.Errorf("parameter sets %d and %d both generate application %q", j, i, app.Metadata.Name)
		}
		from[app.Metadata.Name] = i
		out = append(out, app)
	}
	return out, nil
}

// Describe summarizes a set of parameters for messages.
func Describe(p Params) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(p)) {
		v, _ := json.Marshal(p[k])
		parts = append(parts, k+"="+string(v))
	}
	s := "{" + strings.Join(parts, " ") + "}"
	if len(s) > 200 {
		s = s[:197] + "...}"
	}
	return s
}

// jsonable turns what YAML decodes into JSON-compatible values.
func jsonable(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = jsonable(val)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[fmt.Sprint(k)] = jsonable(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = jsonable(val)
		}
		return out
	}
	return v
}

// mergeParams merges b into a copy of a, b winning (maps merge deeply).
func mergeParams(a, b Params) Params {
	out := copyParams(a)
	for k, v := range b {
		if am, ok := out[k].(map[string]any); ok {
			if bm, ok := v.(map[string]any); ok {
				out[k] = map[string]any(mergeParams(am, bm))
				continue
			}
		}
		out[k] = copyValue(v)
	}
	return out
}

func copyParams(p Params) Params {
	return copyValue(map[string]any(p)).(map[string]any)
}

func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = copyValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = copyValue(val)
		}
		return out
	}
	return v
}

// lookup reads a dotted key ("values.region") of a set of parameters.
func lookup(p Params, key string) (any, bool) {
	var cur any = map[string]any(p)
	for _, part := range strings.Split(key, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// sortedKeys of a map, for stable output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var errNoRepos = errors.New("the git generator has no access to repositories here")
