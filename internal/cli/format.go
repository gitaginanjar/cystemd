// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gitaginanjar/cystemd/internal/manifest"
)

// table prints aligned columns.
type table struct {
	tw *tabwriter.Writer
}

func newTable(w io.Writer, header ...string) *table {
	t := &table{tw: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)}
	if len(header) > 0 {
		t.row(header...)
	}
	return t
}

func (t *table) row(cols ...string) {
	for i, c := range cols {
		cols[i] = strings.ReplaceAll(c, "\t", " ")
	}
	fmt.Fprintln(t.tw, strings.Join(cols, "\t"))
}

func (t *table) flush() { _ = t.tw.Flush() }

// output writes v as JSON or YAML; false when the format is a table one.
func output(w io.Writer, format string, v any) (bool, error) {
	switch format {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return true, enc.Encode(v)
	case "yaml":
		return true, manifest.Encode(w, v)
	}
	return false, nil
}

func validOutput(format string, allowed ...string) error {
	for _, a := range append([]string{"", "json", "yaml"}, allowed...) {
		if format == a {
			return nil
		}
	}
	return fmt.Errorf("unknown output format %q", format)
}

// ago renders a duration since t ("5m", "3h", "2d").
func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func agoPtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return ago(*t)
}

func shortRev(rev string) string {
	if len(rev) == 40 {
		return rev[:7]
	}
	if rev == "" {
		return "HEAD"
	}
	return rev
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
