// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package views

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/a-h/templ"
)

// YAML renders YAML with light syntax highlighting: keys, strings, numbers
// and comments. Every piece of text is escaped.
func YAML(src string) templ.Component {
	return templ.ComponentFunc(func(_ context.Context, w io.Writer) error {
		var b strings.Builder
		b.WriteString(`<pre class="code">`)
		for i, line := range strings.Split(strings.TrimRight(src, "\n"), "\n") {
			if i > 0 {
				b.WriteByte('\n')
			}
			highlightYAMLLine(&b, line)
		}
		b.WriteString(`</pre>`)
		_, err := io.WriteString(w, b.String())
		return err
	})
}

func span(b *strings.Builder, class, text string) {
	if text == "" {
		return
	}
	b.WriteString(`<span class="` + class + `">`)
	b.WriteString(templ.EscapeString(text))
	b.WriteString(`</span>`)
}

func highlightYAMLLine(b *strings.Builder, line string) {
	trimmed := strings.TrimLeft(line, " ")
	indent := line[:len(line)-len(trimmed)]
	b.WriteString(indent)
	if strings.HasPrefix(trimmed, "#") {
		span(b, "y-com", trimmed)
		return
	}
	if strings.HasPrefix(trimmed, "- ") {
		b.WriteString("- ")
		trimmed = trimmed[2:]
	} else if trimmed == "-" {
		b.WriteString("-")
		return
	}
	key, rest, hasColon := cutKey(trimmed)
	if hasColon {
		span(b, "y-key", key)
		b.WriteString(":")
		value := strings.TrimLeft(rest, " ")
		b.WriteString(rest[:len(rest)-len(value)])
		yamlValue(b, value)
		return
	}
	yamlValue(b, trimmed)
}

// cutKey splits "key: value" (and "key:"), ignoring colons inside quotes and
// URLs ("http://...").
func cutKey(s string) (key, rest string, ok bool) {
	if s == "" || s[0] == '"' || s[0] == '\'' || s[0] == '|' || s[0] == '>' {
		return "", s, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] == ':' && (i+1 == len(s) || s[i+1] == ' ') {
			return s[:i], s[i+1:], true
		}
		if s[i] == ' ' && i+1 < len(s) && s[i+1] == '#' {
			break
		}
	}
	return "", s, false
}

func yamlValue(b *strings.Builder, v string) {
	if v == "" {
		return
	}
	switch {
	case v[0] == '"' || v[0] == '\'':
		span(b, "y-str", v)
	case v == "true" || v == "false" || v == "null" || v == "~":
		span(b, "y-num", v)
	case isNumber(v):
		span(b, "y-num", v)
	case v == "|" || v == ">" || v == "|-" || v == ">-" || v == "{}" || v == "[]":
		b.WriteString(templ.EscapeString(v))
	default:
		span(b, "y-str", v)
	}
}

func isNumber(s string) bool {
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}
