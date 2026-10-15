// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package values reads Helm-style values files the way CYSTEMD v4 did: a
// yq-style dot path selects a sub-document (the service configuration) or a
// scalar (version tag, CPU and memory limits). It is the extraction core of
// the systemd source renderer and keeps v4's semantics, including the Helm
// `config: { config.yml: "<yaml as string>" }` unwrapping.
package values

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ExpandSelector replaces every ${selector} in keyExpr.
func ExpandSelector(keyExpr, selector string) string {
	return strings.ReplaceAll(keyExpr, "${selector}", selector)
}

// ExtractYAMLKey resolves keyExpr (dot path, ${selector} expanded) in data and
// returns the sub-document as YAML (2-space indent, sorted keys). A plain
// segment indexes a mapping; "[]" takes a sequence's first element or a
// single-key map's only value (re-parsed when that value is a YAML string —
// the Helm config-blob case) and passes a multi-key map through. A
// multi-line string leaf is returned verbatim. Errors on a missing key, a
// wrong node type or unparseable YAML.
func ExtractYAMLKey(data []byte, keyExpr, selector string) ([]byte, error) {
	keyExpr = strings.TrimPrefix(ExpandSelector(keyExpr, selector), ".")

	var root any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("yaml unmarshal: %w", err)
	}

	current := root
	for _, seg := range strings.Split(keyExpr, ".") {
		if seg == "" {
			continue
		}
		switch seg {
		case "[]":
			switch v := current.(type) {
			case []any:
				if len(v) == 0 {
					return nil, fmt.Errorf("[] on empty sequence")
				}
				current = v[0]
			case map[string]any:
				if len(v) == 1 {
					for _, only := range v {
						current = only
					}
					if s, ok := current.(string); ok {
						var inner any
						if err := yaml.Unmarshal([]byte(s), &inner); err == nil {
							switch inner.(type) {
							case map[string]any, []any:
								current = inner
							}
						}
					}
				}
			default:
				return nil, fmt.Errorf("expected sequence or mapping at [] but got %T", current)
			}
		default:
			m, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("expected mapping at %q but got %T", seg, current)
			}
			val, exists := m[seg]
			if !exists {
				return nil, fmt.Errorf("key %q not found in mapping", seg)
			}
			current = val
		}
	}

	// A multi-line string leaf (an Alloy/River config, …) is returned
	// verbatim: re-encoding would wrap it in a block scalar.
	if s, ok := current.(string); ok && strings.Contains(s, "\n") {
		return []byte(s), nil
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(current); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("yaml marshal: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("yaml encoder close: %w", err)
	}
	return buf.Bytes(), nil
}

// mergeKeyTag is yaml.v3's tag on a `<<` merge key (a quoted "<<" stays an
// ordinary key).
const mergeKeyTag = "!!merge"

// maxAliasDepth bounds alias chains and merge recursion, so a hand-crafted
// document cannot hang a render. Exceeding it is an error, never "absent".
const maxAliasDepth = 32

// ExtractScalar resolves keyExpr to a scalar leaf and returns its raw text,
// trimmed. It reads the node's text, never a decode + re-encode, so an
// unquoted "1.10" stays "1.10" (a decode yields 1.1: the wrong package).
// Errors on a missing key, bad YAML, a non-scalar or an empty leaf.
func ExtractScalar(data []byte, keyExpr, selector string) (string, error) {
	resolved := ExpandSelector(keyExpr, selector)
	path := strings.TrimPrefix(resolved, ".")

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("yaml unmarshal: %w", err)
	}
	node := &doc
	if node.Kind == yaml.DocumentNode {
		if len(node.Content) == 0 {
			return "", fmt.Errorf("key %q: empty yaml document", resolved)
		}
		node = node.Content[0]
	}
	for _, seg := range strings.Split(path, ".") {
		if seg == "" {
			continue
		}
		next, err := navSegment(node, seg)
		if err != nil {
			return "", fmt.Errorf("key %q: %w", resolved, err)
		}
		node = next
	}
	node = resolveAlias(node)
	if node == nil {
		return "", fmt.Errorf("key %q: unresolvable YAML alias", resolved)
	}
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("key %q did not resolve to a scalar", resolved)
	}
	v := strings.TrimSpace(node.Value)
	if v == "" {
		return "", fmt.Errorf("key %q resolved to empty value", resolved)
	}
	return v, nil
}

// navSegment advances one dot-path segment toward a scalar leaf with
// ExtractYAMLKey's "[]" semantics (minus the Helm string re-parse).
func navSegment(node *yaml.Node, seg string) (*yaml.Node, error) {
	node = resolveAlias(node)
	if node == nil {
		return nil, fmt.Errorf("unresolvable YAML alias at %q", seg)
	}
	if seg == "[]" {
		switch node.Kind {
		case yaml.SequenceNode:
			if len(node.Content) == 0 {
				return nil, fmt.Errorf("[] on empty sequence")
			}
			return node.Content[0], nil
		case yaml.MappingNode:
			if len(node.Content) == 2 {
				return node.Content[1], nil
			}
			return node, nil
		default:
			return nil, fmt.Errorf("expected sequence or mapping at [] but got node kind %d", node.Kind)
		}
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping at %q but got node kind %d", seg, node.Kind)
	}
	if v, ok := mappingValue(node, seg, 0); ok {
		return v, nil
	}
	return nil, fmt.Errorf("key %q not found in mapping", seg)
}

func resolveAlias(node *yaml.Node) *yaml.Node {
	for depth := 0; node != nil && node.Kind == yaml.AliasNode; depth++ {
		if depth >= maxAliasDepth {
			return nil
		}
		node = node.Alias
	}
	return node
}

// mappingValue looks up seg with YAML merge-key precedence, as yaml.v3's
// decode applies it: explicit keys first, then `<<` sources in order.
func mappingValue(node *yaml.Node, seg string, depth int) (*yaml.Node, bool) {
	if node == nil || node.Kind != yaml.MappingNode || depth >= maxAliasDepth {
		return nil, false
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Tag != mergeKeyTag && node.Content[i].Value == seg {
			return node.Content[i+1], true
		}
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Tag != mergeKeyTag {
			continue
		}
		for _, src := range mergeSources(node.Content[i+1]) {
			if v, ok := mappingValue(resolveAlias(src), seg, depth+1); ok {
				return v, true
			}
		}
	}
	return nil, false
}

func mergeSources(v *yaml.Node) []*yaml.Node {
	r := resolveAlias(v)
	if r == nil {
		return nil
	}
	if r.Kind == yaml.SequenceNode {
		return r.Content
	}
	return []*yaml.Node{r}
}
