// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/cronexpr"
)

// Kinds of sync windows.
const (
	SyncWindowAllow = "allow"
	SyncWindowDeny  = "deny"
)

// SyncWindow is Argo CD's: from each time its schedule (a cron expression)
// fires, for its duration, it allows or denies syncs of the Applications it
// matches.
type SyncWindow struct {
	Kind     string `json:"kind" yaml:"kind"`
	Schedule string `json:"schedule" yaml:"schedule"`
	Duration string `json:"duration" yaml:"duration"`
	// Applications, Namespaces and Clusters are globs; a window matches an
	// Application when any of them matches (all of them, with
	// andOperator), and nothing when they are all empty.
	Applications []string `json:"applications,omitempty" yaml:"applications,omitempty"`
	Namespaces   []string `json:"namespaces,omitempty" yaml:"namespaces,omitempty"`
	Clusters     []string `json:"clusters,omitempty" yaml:"clusters,omitempty"`
	// ManualSync lets manual syncs through while a deny window is active,
	// or outside the allow windows.
	ManualSync bool `json:"manualSync,omitempty" yaml:"manualSync,omitempty"`
	// TimeZone of the schedule (an IANA name; default UTC).
	TimeZone    string `json:"timeZone,omitempty" yaml:"timeZone,omitempty"`
	AndOperator bool   `json:"andOperator,omitempty" yaml:"andOperator,omitempty"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

// SyncWindows are a project's sync windows.
type SyncWindows []*SyncWindow

// Validate checks a window.
func (w *SyncWindow) Validate() error {
	var errs []error
	if w.Kind != SyncWindowAllow && w.Kind != SyncWindowDeny {
		errs = append(errs, fmt.Errorf("kind %q is not allow or deny", w.Kind))
	}
	if _, err := cronexpr.Parse(w.Schedule); err != nil || w.Schedule == "" {
		errs = append(errs, fmt.Errorf("schedule %q is not a cron expression", w.Schedule))
	}
	if d, err := time.ParseDuration(w.Duration); err != nil || d <= 0 {
		errs = append(errs, fmt.Errorf("duration %q is not a positive duration", w.Duration))
	}
	if _, err := w.location(); err != nil {
		errs = append(errs, fmt.Errorf("time zone %q: %v", w.TimeZone, err))
	}
	if len(w.Applications)+len(w.Namespaces)+len(w.Clusters) == 0 {
		errs = append(errs, errors.New("the window matches nothing: give applications, namespaces or clusters"))
	}
	for _, list := range [][]string{w.Applications, w.Namespaces, w.Clusters} {
		for _, g := range list {
			if g == "" {
				errs = append(errs, errors.New("empty application, namespace or cluster pattern"))
			}
		}
	}
	return errors.Join(errs...)
}

func (w *SyncWindow) location() (*time.Location, error) {
	if w.TimeZone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(w.TimeZone)
}

// Active reports whether the window is in effect at now: its schedule fired
// within the last duration.
func (w *SyncWindow) Active(now time.Time) bool {
	expr, err := cronexpr.Parse(w.Schedule)
	if err != nil {
		return false
	}
	d, err := time.ParseDuration(w.Duration)
	if err != nil || d <= 0 {
		return false
	}
	loc, err := w.location()
	if err != nil {
		return false
	}
	start := expr.Next(now.In(loc).Add(-d))
	return !start.IsZero() && !start.After(now)
}

// Matches reports whether the window applies to app.
func (w *SyncWindow) Matches(app *Application) bool {
	dest := app.Spec.Destination
	checks := []struct {
		globs []string
		value string
	}{
		{w.Applications, app.Metadata.Name},
		{w.Namespaces, dest.NamespaceOrDefault()},
		{w.Clusters, dest.ClusterName()},
	}
	matched := false
	for _, c := range checks {
		if len(c.globs) == 0 {
			continue
		}
		ok := false
		for _, g := range c.globs {
			ok = ok || globMatch(g, c.value)
		}
		if w.AndOperator && !ok {
			return false
		}
		matched = matched || ok
	}
	return matched
}

// For returns the windows that apply to app.
func (ws SyncWindows) For(app *Application) SyncWindows {
	var out SyncWindows
	for _, w := range ws {
		if w.Matches(app) {
			out = append(out, w)
		}
	}
	return out
}

// Active returns the windows in effect at now.
func (ws SyncWindows) Active(now time.Time) SyncWindows {
	var out SyncWindows
	for _, w := range ws {
		if w.Active(now) {
			out = append(out, w)
		}
	}
	return out
}

// CanSync decides, as Argo CD does, whether windows (those of an
// Application) let a sync through at now: an active deny window blocks it
// (manual syncs pass when every active deny window allows them); otherwise
// an active allow window lets it through; otherwise allow windows that
// exist but are not active block it (manual syncs pass when one of them
// allows them); without windows, everything passes.
func (ws SyncWindows) CanSync(manual bool, now time.Time) bool {
	if len(ws) == 0 {
		return true
	}
	active := ws.Active(now)
	deny, denyManual := false, true
	for _, w := range active {
		if w.Kind == SyncWindowDeny {
			deny = true
			denyManual = denyManual && w.ManualSync
		}
	}
	if deny {
		return manual && denyManual
	}
	for _, w := range active {
		if w.Kind == SyncWindowAllow {
			return true
		}
	}
	allows, allowManual := false, false
	for _, w := range ws {
		if w.Kind == SyncWindowAllow {
			allows = true
			allowManual = allowManual || w.ManualSync
		}
	}
	if allows {
		return manual && allowManual
	}
	return true
}

// SyncWindowState is how windows stand for an Application (Argo CD's
// ApplicationSyncWindowsResponse).
type SyncWindowState struct {
	// AssignedWindows apply to the Application; ActiveWindows are those in
	// effect now.
	AssignedWindows SyncWindows `json:"assignedWindows"`
	ActiveWindows   SyncWindows `json:"activeWindows"`
	// CanSync is for manual syncs; automated syncs pass when AutoSync does.
	CanSync  bool `json:"canSync"`
	AutoSync bool `json:"autoSync"`
}

// WindowState computes the state of a project's windows for app.
func (p *AppProject) WindowState(app *Application, now time.Time) SyncWindowState {
	ws := p.Spec.SyncWindows.For(app)
	st := SyncWindowState{AssignedWindows: ws, ActiveWindows: ws.Active(now),
		CanSync: ws.CanSync(true, now), AutoSync: ws.CanSync(false, now)}
	if st.AssignedWindows == nil {
		st.AssignedWindows = SyncWindows{}
	}
	if st.ActiveWindows == nil {
		st.ActiveWindows = SyncWindows{}
	}
	return st
}

// Summary is one word for the UI: "" (no windows), Allowed, Denied, or
// "Manual only".
func (st SyncWindowState) Summary() string {
	switch {
	case len(st.AssignedWindows) == 0:
		return ""
	case st.AutoSync:
		return "Allowed"
	case st.CanSync:
		return "Manual only"
	}
	return "Denied"
}
