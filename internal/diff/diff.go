// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package diff compares desired and live Nomad jobs for humans. Whether a
// job is in sync is decided by Nomad's own plan diff (authoritative: it sees
// the job after server-side defaulting); this package produces what the UI
// and CLI show — normalised manifests, the live state reduced to the fields
// Git declares (Argo CD's "normalized live state"), the plan diff as text,
// and a unified line diff.
package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/api"
	"gopkg.in/yaml.v3"
)

// serverFields are set by Nomad, never by a jobspec.
var serverFields = []string{
	"Status", "StatusDescription", "Stable", "Version", "SubmitTime",
	"CreateIndex", "ModifyIndex", "JobModifyIndex", "Dispatched",
	"NomadTokenID", "VaultToken", "ConsulToken", "VersionTag",
}

// ToMap converts a job (or any JSON-able value) to a generic map.
func ToMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// FromMap converts a generic map back to a job.
func FromMap(m map[string]any) (*api.Job, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var job api.Job
	if err := json.Unmarshal(b, &job); err != nil {
		return nil, err
	}
	return &job, nil
}

// Normalize returns the job as a map without server-managed fields and
// without empty values (null, "", [] and {}), for display.
func Normalize(job *api.Job) (map[string]any, error) {
	if job == nil {
		return nil, nil
	}
	m, err := ToMap(job)
	if err != nil {
		return nil, err
	}
	for _, f := range serverFields {
		delete(m, f)
	}
	// The job-level update block only seeds the task groups' (where Nomad
	// keeps the effective strategy): parsing fills it with defaults that
	// the server zeroes, keeping Stagger and MaxParallel. Compare those.
	if u, ok := m["Update"].(map[string]any); ok {
		m["Update"] = map[string]any{"Stagger": u["Stagger"], "MaxParallel": u["MaxParallel"]}
	}
	pruned, _ := dropEmpty(m).(map[string]any)
	if pruned == nil {
		pruned = map[string]any{}
	}
	return pruned, nil
}

// dropEmpty removes nulls, empty strings, and empty lists and maps.
func dropEmpty(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range t {
			if c := dropEmpty(val); c != nil {
				out[k] = c
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		var out []any
		for _, val := range t {
			if c := dropEmpty(val); c != nil {
				out = append(out, c)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		if t == "" {
			return nil
		}
		return t
	case nil:
		return nil
	}
	return v
}

// PruneToShape returns live reduced to the keys desired declares, so server
// defaults (identities, implicit constraints, timestamps…) disappear from
// the comparison view. Lists of objects pair up by their Name or Label
// field, else by position.
func PruneToShape(live, desired any) any {
	switch d := desired.(type) {
	case map[string]any:
		l, ok := live.(map[string]any)
		if !ok {
			return live
		}
		out := map[string]any{}
		for k, dv := range d {
			if lv, ok := l[k]; ok {
				out[k] = PruneToShape(lv, dv)
			}
		}
		return out
	case []any:
		l, ok := live.([]any)
		if !ok {
			return live
		}
		out := make([]any, 0, len(l))
		byKey := map[string]any{}
		for _, dv := range d {
			if k := itemKey(dv); k != "" {
				byKey[k] = dv
			}
		}
		for i, lv := range l {
			var dv any
			if k := itemKey(lv); k != "" {
				dv = byKey[k]
			} else if i < len(d) {
				dv = d[i]
			}
			if dv == nil {
				out = append(out, lv) // present live only: keep, it is a difference
				continue
			}
			out = append(out, PruneToShape(lv, dv))
		}
		return out
	}
	return live
}

func itemKey(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, f := range []string{"Name", "Label"} {
		if s, ok := m[f].(string); ok && s != "" {
			return f + "=" + s
		}
	}
	return ""
}

// ToYAML renders a normalised map as YAML (keys sorted).
func ToYAML(v any) string {
	if v == nil {
		return ""
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	_ = enc.Encode(numbersToNative(v))
	_ = enc.Close()
	return buf.String()
}

// numbersToNative converts json.Number to int64/float64 for YAML output.
func numbersToNative(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = numbersToNative(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = numbersToNative(val)
		}
		return out
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	}
	return v
}

// ApplyIgnoreDifferences copies the live values at the dotted paths (with *
// matching any key or index, e.g. "TaskGroups.*.Count") into desired, so
// fields owned by someone else (an autoscaler) never cause drift.
func ApplyIgnoreDifferences(desired, live map[string]any, paths []string) {
	for _, p := range paths {
		p = strings.TrimPrefix(strings.ReplaceAll(strings.TrimPrefix(p, "/"), "/", "."), ".")
		if p == "" {
			continue
		}
		copyPath(desired, live, strings.Split(p, "."))
	}
}

func copyPath(dst, src any, segs []string) {
	if len(segs) == 0 {
		return
	}
	seg, rest := segs[0], segs[1:]
	switch d := dst.(type) {
	case map[string]any:
		s, ok := src.(map[string]any)
		if !ok {
			return
		}
		keys := []string{seg}
		if seg == "*" {
			keys = keys[:0]
			for k := range s {
				keys = append(keys, k)
			}
		}
		for _, k := range keys {
			sv, ok := s[k]
			if !ok {
				continue
			}
			if len(rest) == 0 {
				d[k] = sv
				continue
			}
			if _, ok := d[k]; !ok {
				continue
			}
			copyPath(d[k], sv, rest)
		}
	case []any:
		s, ok := src.([]any)
		if !ok {
			return
		}
		for i := range d {
			if seg != "*" && seg != strconv.Itoa(i) {
				continue
			}
			// Pair list items by Name when both have one.
			sv := pairItem(d[i], s, i)
			if sv == nil {
				continue
			}
			if len(rest) == 0 {
				d[i] = sv
				continue
			}
			copyPath(d[i], sv, rest)
		}
	}
}

func pairItem(item any, src []any, i int) any {
	if k := itemKey(item); k != "" {
		for _, s := range src {
			if itemKey(s) == k {
				return s
			}
		}
		return nil
	}
	if i < len(src) {
		return src[i]
	}
	return nil
}

// HasChanges reports whether a plan diff carries any change.
func HasChanges(d *api.JobDiff) bool {
	return d != nil && d.Type != "" && d.Type != "None"
}

// FormatJobDiff renders a plan diff like `nomad job plan` does.
func FormatJobDiff(d *api.JobDiff) string {
	if !HasChanges(d) {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s Job: %q\n", marker(d.Type), d.ID)
	writeFields(&b, d.Fields, 1)
	writeObjects(&b, d.Objects, 1)
	for _, tg := range d.TaskGroups {
		if tg.Type == "None" {
			continue
		}
		updates := ""
		if len(tg.Updates) > 0 {
			var parts []string
			keys := make([]string, 0, len(tg.Updates))
			for k := range tg.Updates {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				parts = append(parts, fmt.Sprintf("%d %s", tg.Updates[k], k))
			}
			updates = " (" + strings.Join(parts, ", ") + ")"
		}
		fmt.Fprintf(&b, "%s Task Group: %q%s\n", marker(tg.Type), tg.Name, updates)
		writeFields(&b, tg.Fields, 1)
		writeObjects(&b, tg.Objects, 1)
		for _, task := range tg.Tasks {
			if task.Type == "None" {
				continue
			}
			ann := ""
			if len(task.Annotations) > 0 {
				ann = " (" + strings.Join(task.Annotations, ", ") + ")"
			}
			fmt.Fprintf(&b, "  %s Task: %q%s\n", marker(task.Type), task.Name, ann)
			writeFields(&b, task.Fields, 2)
			writeObjects(&b, task.Objects, 2)
		}
	}
	return b.String()
}

func marker(t string) string {
	switch t {
	case "Added":
		return "+"
	case "Deleted":
		return "-"
	case "Edited":
		return "+/-"
	}
	return " "
}

func writeFields(b *strings.Builder, fields []*api.FieldDiff, depth int) {
	pad := strings.Repeat("  ", depth)
	for _, f := range fields {
		switch f.Type {
		case "Added":
			fmt.Fprintf(b, "%s+ %s: %q\n", pad, f.Name, f.New)
		case "Deleted":
			fmt.Fprintf(b, "%s- %s: %q\n", pad, f.Name, f.Old)
		case "Edited":
			fmt.Fprintf(b, "%s+/- %s: %q => %q\n", pad, f.Name, f.Old, f.New)
		}
	}
}

func writeObjects(b *strings.Builder, objects []*api.ObjectDiff, depth int) {
	pad := strings.Repeat("  ", depth)
	for _, o := range objects {
		if o.Type == "None" {
			continue
		}
		fmt.Fprintf(b, "%s%s %s {\n", pad, marker(o.Type), o.Name)
		writeFields(b, o.Fields, depth+1)
		writeObjects(b, o.Objects, depth+1)
		fmt.Fprintf(b, "%s}\n", pad)
	}
}

// Unified renders a unified line diff of a → b with n lines of context.
func Unified(aName, bName, a, b string, n int) string {
	al, bl := splitLines(a), splitLines(b)
	ops := lcsOps(al, bl)
	if len(ops) == 0 {
		return ""
	}
	changed := false
	for _, op := range ops {
		if op.kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}
	var out strings.Builder
	fmt.Fprintf(&out, "--- %s\n+++ %s\n", aName, bName)
	// Group ops into hunks with n lines of context.
	for i := 0; i < len(ops); {
		if ops[i].kind == ' ' {
			i++
			continue
		}
		start := max(0, i-n)
		end := i
		for end < len(ops) {
			if ops[end].kind != ' ' {
				end++
				continue
			}
			// Extend through up to 2n context lines if another change follows.
			j := end
			for j < len(ops) && ops[j].kind == ' ' && j-end < 2*n {
				j++
			}
			if j < len(ops) && ops[j].kind != ' ' {
				end = j
				continue
			}
			end = min(len(ops), end+n)
			break
		}
		aStart, bStart := ops[start].ai+1, ops[start].bi+1
		aCount, bCount := 0, 0
		for _, op := range ops[start:end] {
			if op.kind != '+' {
				aCount++
			}
			if op.kind != '-' {
				bCount++
			}
		}
		fmt.Fprintf(&out, "@@ -%d,%d +%d,%d @@\n", aStart, aCount, bStart, bCount)
		for _, op := range ops[start:end] {
			fmt.Fprintf(&out, "%c%s\n", op.kind, op.text)
		}
		i = end
	}
	return out.String()
}

type lineOp struct {
	kind   byte // ' ', '-', '+'
	text   string
	ai, bi int
}

func splitLines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// lcsOps computes an LCS-based edit script (fine for manifest-sized input).
func lcsOps(a, b []string) []lineOp {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	var ops []lineOp
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, lineOp{' ', a[i], i, j})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, lineOp{'-', a[i], i, j})
			i++
		default:
			ops = append(ops, lineOp{'+', b[j], i, j})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, lineOp{'-', a[i], i, j})
	}
	for ; j < m; j++ {
		ops = append(ops, lineOp{'+', b[j], i, j})
	}
	return ops
}

// Line is one line of a structured diff: Op is ' ' (context), '-'
// (removed), '+' (added) or '@' (a hunk separator, Text says where). A and
// B are 1-based line numbers in the old and new text (0 when absent).
type Line struct {
	Op   byte
	Text string
	A, B int
}

// Lines diffs a → b line by line. With context < 0 every line is returned;
// otherwise only changes with that many lines around them, hunks separated
// by '@' lines. It returns nil when the texts are equal.
func Lines(a, b string, context int) []Line {
	ops := lcsOps(splitLines(a), splitLines(b))
	changed := false
	for _, op := range ops {
		if op.kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	line := func(op lineOp) Line {
		l := Line{Op: op.kind, Text: op.text}
		if op.kind != '+' {
			l.A = op.ai + 1
		}
		if op.kind != '-' {
			l.B = op.bi + 1
		}
		return l
	}
	var out []Line
	if context < 0 {
		for _, op := range ops {
			out = append(out, line(op))
		}
		return out
	}
	keep := make([]bool, len(ops))
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		for j := max(0, i-context); j <= min(len(ops)-1, i+context); j++ {
			keep[j] = true
		}
	}
	for i := 0; i < len(ops); i++ {
		if !keep[i] {
			continue
		}
		if i == 0 || !keep[i-1] {
			out = append(out, Line{Op: '@', Text: fmt.Sprintf("@@ -%d +%d @@", ops[i].ai+1, ops[i].bi+1)})
		}
		out = append(out, line(ops[i]))
	}
	return out
}
