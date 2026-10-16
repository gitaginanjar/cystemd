// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package migrate

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/gitaginanjar/cystemd/internal/vaultagent"
)

// v4 template functions: secretField "path" "field", (secret "path") and a
// bare secret "path".
var (
	templateCall = regexp.MustCompile(`secretField\s+"((?:[^"\\]|\\.)*)"\s+"((?:[^"\\]|\\.)*)"|\(\s*secret\s+"((?:[^"\\]|\\.)*)"\s*\)|\bsecret\s+"((?:[^"\\]|\\.)*)"`)
	leftover     = regexp.MustCompile(`\bsecret(Field)?\s+[^"\s]`)
	identifier   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	action       = regexp.MustCompile(`(?s)\{\{.*?\}\}`)
	assignment   = regexp.MustCompile(`^\$[A-Za-z0-9_]*\s*:?=`)
)

// ConvertTemplate rewrites a v4 template for Vault Agent (consul-template).
//
// v4's secret returned the KV data map and secretField one field of it.
// Vault Agent's secret returns the response at the API path, whose data
// map is .Data.data (KV v2) or .Data (KV v1), and nil during the first
// pass, in which consul-template only discovers what a template reads. So
// each action reading a secret is guarded:
//
//	{{ secretField "kv/app" "user" }}
//	→ {{ $cystemd_s1 := secret "kv/data/app" }}{{ if $cystemd_s1 }}{{ $cystemd_s1.Data.data.user }}{{ end }}
//
// and a block (if, with, range) whose pipeline reads one is enclosed in
// the guard up to its end. A field read as a field access fails when it
// is missing under error_on_missing_key, like secretField did; a field
// that is no template identifier uses index, which does not. Only actions
// are rewritten, never comments or text. The notes list what needs a look.
func ConvertTemplate(src string, kvVersion int) (string, []string) {
	data := ".Data.data"
	if kvVersion == 1 {
		data = ".Data"
	}
	var (
		notes []string
		out   strings.Builder
		last  int
		n     int   // guard variables so far
		ends  []int // extra ends owed by each open block
	)
	for _, loc := range action.FindAllStringIndex(src, -1) {
		out.WriteString(src[last:loc[0]])
		last = loc[1]
		raw := src[loc[0]:loc[1]]
		line := strings.Count(src[:loc[0]], "\n") + 1
		inner, ltrim, rtrim := splitAction(raw)
		if strings.HasPrefix(inner, "/*") {
			out.WriteString(raw)
			continue
		}
		kw := keyword(inner)
		if kw == "end" {
			extra := 0
			if len(ends) > 0 {
				extra, ends = ends[len(ends)-1], ends[:len(ends)-1]
			}
			if extra == 0 {
				out.WriteString(raw)
				continue
			}
			out.WriteString(open(ltrim) + "end }}")
			for i := 1; i <= extra; i++ {
				out.WriteString("{{ end" + closing(rtrim && i == extra))
			}
			continue
		}
		block := kw == "if" || kw == "with" || kw == "range" || kw == "block" || kw == "define"
		if !templateCall.MatchString(inner) {
			if block {
				ends = append(ends, 0)
			}
			if leftover.MatchString(inner) {
				notes = append(notes, fmt.Sprintf("line %d calls secret with a non-literal path: convert it by hand", line))
			}
			out.WriteString(raw)
			continue
		}

		// One guard variable per secret the action reads.
		vars := map[string]string{}
		var paths []string
		converted := templateCall.ReplaceAllStringFunc(inner, func(m string) string {
			g := templateCall.FindStringSubmatch(m)
			p, field := g[4], ""
			switch {
			case strings.HasPrefix(m, "secretField"):
				p, field = g[1], unescape(g[2])
			case g[3] != "":
				p = g[3]
			}
			dp := vaultagent.KVDataPath(unescape(p), kvVersion)
			v, ok := vars[dp]
			if !ok {
				n++
				v = fmt.Sprintf("$cystemd_s%d", n)
				vars[dp] = v
				paths = append(paths, dp)
			}
			switch {
			case field == "":
				return v + data
			case identifier.MatchString(field):
				return v + data + "." + field
			}
			notes = append(notes, fmt.Sprintf("line %d: field %q is read with index, which renders an empty value instead of failing when it is missing", line, field))
			return fmt.Sprintf("(index %s%s %q)", v, data, field)
		})
		if leftover.MatchString(converted) {
			notes = append(notes, fmt.Sprintf("line %d calls secret with a non-literal path: convert it by hand", line))
		}
		if kw == "else" || kw == "block" || kw == "define" || kw == "template" || assignment.MatchString(inner) {
			// No guard fits these: the secret is read inline, which fails
			// while Vault Agent discovers the template's secrets.
			notes = append(notes, fmt.Sprintf("line %d reads a secret in an else, assignment or template action: rewrite it with a with block (see docs/MIGRATION.md)", line))
			for i := len(paths) - 1; i >= 0; i-- { // $cystemd_s12 before $cystemd_s1
				converted = strings.ReplaceAll(converted, vars[paths[i]], fmt.Sprintf("(secret %q)", paths[i]))
			}
			if block {
				ends = append(ends, 0)
			}
			out.WriteString(open(ltrim) + converted + closing(rtrim))
			continue
		}
		guard := make([]string, len(paths))
		for i, dp := range paths {
			lt := ltrim && i == 0
			out.WriteString(fmt.Sprintf("%s%s := secret %q }}", open(lt), vars[dp], dp))
			guard[i] = vars[dp]
		}
		cond := guard[0]
		if len(guard) > 1 {
			cond = "and " + strings.Join(guard, " ")
		}
		out.WriteString("{{ if " + cond + " }}")
		if block {
			out.WriteString("{{ " + converted + closing(rtrim))
			ends = append(ends, 1)
			continue
		}
		out.WriteString("{{ " + converted + " }}{{ end" + closing(rtrim))
	}
	out.WriteString(src[last:])
	return out.String(), notes
}

// splitAction returns an action's text without delimiters and trim
// markers, and whether it trims the space before and after it.
func splitAction(raw string) (inner string, ltrim, rtrim bool) {
	s := raw[2 : len(raw)-2]
	if len(s) >= 2 && s[0] == '-' && isSpace(s[1]) {
		ltrim, s = true, s[1:]
	}
	if len(s) >= 2 && s[len(s)-1] == '-' && isSpace(s[len(s)-2]) {
		rtrim, s = true, s[:len(s)-1]
	}
	return strings.TrimSpace(s), ltrim, rtrim
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// keyword is an action's first word (if, with, range, end, …).
func keyword(inner string) string {
	i := 0
	for i < len(inner) && (inner[i] >= 'a' && inner[i] <= 'z' || inner[i] >= 'A' && inner[i] <= 'Z') {
		i++
	}
	return inner[:i]
}

func open(trim bool) string {
	if trim {
		return "{{- "
	}
	return "{{ "
}

func closing(trim bool) string {
	if trim {
		return " -}}"
	}
	return " }}"
}

func unescape(s string) string {
	var out string
	if err := json.Unmarshal([]byte(`"`+s+`"`), &out); err != nil {
		return s
	}
	return out
}
