// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"strings"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestSyncWindowActive(t *testing.T) {
	nightly := &SyncWindow{Kind: SyncWindowAllow, Schedule: "0 22 * * *", Duration: "2h", Applications: []string{"*"}}
	for now, want := range map[string]bool{
		"2026-10-15T21:59:59Z": false,
		"2026-10-15T22:00:00Z": true,
		"2026-10-15T23:59:59Z": true,
		"2026-10-16T00:00:00Z": false,
		"2026-10-16T12:00:00Z": false,
	} {
		if got := nightly.Active(at(now)); got != want {
			t.Errorf("nightly at %s: %v", now, got)
		}
	}
	// Office hours in Amsterdam (UTC+2 in October): 09:00-17:00 local.
	office := &SyncWindow{Kind: SyncWindowAllow, Schedule: "0 9 * * 1-5", Duration: "8h", TimeZone: "Europe/Amsterdam", Applications: []string{"*"}}
	for now, want := range map[string]bool{
		"2026-10-15T06:30:00Z": false, // 08:30
		"2026-10-15T07:00:00Z": true,  // 09:00
		"2026-10-15T14:59:00Z": true,  // 16:59
		"2026-10-15T15:00:00Z": false, // 17:00
		"2026-10-17T08:00:00Z": false, // Saturday
	} {
		if got := office.Active(at(now)); got != want {
			t.Errorf("office hours at %s: %v", now, got)
		}
	}
}

func TestSyncWindowMatches(t *testing.T) {
	app := &Application{Metadata: ObjectMeta{Name: "payments-api"}, Spec: ApplicationSpec{Destination: ApplicationDestination{Namespace: "payments"}}}
	for _, tc := range []struct {
		w    SyncWindow
		want bool
	}{
		{SyncWindow{Applications: []string{"payments-*"}}, true},
		{SyncWindow{Applications: []string{"billing-*"}, Namespaces: []string{"payments"}}, true},
		{SyncWindow{Clusters: []string{"in-cluster"}}, true},
		{SyncWindow{Clusters: []string{"edge-*"}}, false},
		{SyncWindow{}, false},
		{SyncWindow{AndOperator: true, Applications: []string{"payments-*"}, Namespaces: []string{"payments"}}, true},
		{SyncWindow{AndOperator: true, Applications: []string{"payments-*"}, Namespaces: []string{"default"}}, false},
	} {
		if got := tc.w.Matches(app); got != tc.want {
			t.Errorf("%+v: %v", tc.w, got)
		}
	}
}

func TestSyncWindowsCanSync(t *testing.T) {
	now := at("2026-10-15T12:00:00Z")
	active := func(kind string, manual bool) *SyncWindow {
		return &SyncWindow{Kind: kind, Schedule: "0 11 * * *", Duration: "2h", ManualSync: manual}
	}
	inactive := func(kind string, manual bool) *SyncWindow {
		return &SyncWindow{Kind: kind, Schedule: "0 1 * * *", Duration: "1h", ManualSync: manual}
	}
	for _, tc := range []struct {
		name         string
		ws           SyncWindows
		auto, manual bool
	}{
		{"no windows", nil, true, true},
		{"active allow", SyncWindows{active(SyncWindowAllow, false)}, true, true},
		{"inactive allow", SyncWindows{inactive(SyncWindowAllow, false)}, false, false},
		{"inactive allow, manual sync", SyncWindows{inactive(SyncWindowAllow, true)}, false, true},
		{"active deny", SyncWindows{active(SyncWindowDeny, false)}, false, false},
		{"active deny, manual sync", SyncWindows{active(SyncWindowDeny, true)}, false, true},
		{"deny wins over allow", SyncWindows{active(SyncWindowAllow, true), active(SyncWindowDeny, false)}, false, false},
		{"every active deny must allow manual syncs", SyncWindows{active(SyncWindowDeny, true), active(SyncWindowDeny, false)}, false, false},
		{"inactive deny", SyncWindows{inactive(SyncWindowDeny, false)}, true, true},
	} {
		if auto, manual := tc.ws.CanSync(false, now), tc.ws.CanSync(true, now); auto != tc.auto || manual != tc.manual {
			t.Errorf("%s: auto=%v manual=%v", tc.name, auto, manual)
		}
	}
}

func TestSyncWindowValidate(t *testing.T) {
	good := SyncWindow{Kind: SyncWindowDeny, Schedule: "0 22 * * 5", Duration: "60h", TimeZone: "America/New_York", Namespaces: []string{"*"}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := SyncWindow{Kind: "block", Schedule: "every night", Duration: "-1h", TimeZone: "Mars/Olympus"}
	err := bad.Validate()
	for _, want := range []string{`kind "block"`, `schedule "every night"`, `duration "-1h"`, `time zone "Mars/Olympus"`, "matches nothing"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v lacks %q", err, want)
		}
	}
	p := &AppProject{Metadata: ObjectMeta{Name: "platform"}, Spec: AppProjectSpec{SyncWindows: SyncWindows{&good, &bad}}}
	if err := p.Validate(); err == nil || !strings.Contains(err.Error(), "syncWindows[1]") {
		t.Fatalf("project: %v", err)
	}
}

func TestSyncWindowState(t *testing.T) {
	app := &Application{Metadata: ObjectMeta{Name: "web"}}
	p := &AppProject{Spec: AppProjectSpec{SyncWindows: SyncWindows{
		{Kind: SyncWindowDeny, Schedule: "0 11 * * *", Duration: "2h", Applications: []string{"web"}, ManualSync: true},
		{Kind: SyncWindowDeny, Schedule: "0 11 * * *", Duration: "2h", Applications: []string{"other"}},
	}}}
	st := p.WindowState(app, at("2026-10-15T12:00:00Z"))
	if len(st.AssignedWindows) != 1 || len(st.ActiveWindows) != 1 || !st.CanSync || st.AutoSync || st.Summary() != "Manual only" {
		t.Fatalf("state = %+v (%s)", st, st.Summary())
	}
	if st := p.WindowState(app, at("2026-10-15T14:00:00Z")); st.Summary() != "Allowed" || len(st.ActiveWindows) != 0 {
		t.Fatalf("after the window: %+v", st)
	}
	if st := (&AppProject{}).WindowState(app, time.Now()); st.Summary() != "" || st.AssignedWindows == nil {
		t.Fatalf("no windows: %+v", st)
	}
}
