// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"log/slog"
	"testing"
	"time"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func TestAutoSyncRespectsSyncWindows(t *testing.T) {
	now := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC) // a Friday night
	c := &Controller{logger: slog.New(slog.DiscardHandler), now: func() time.Time { return now }}
	app := &v1.Application{Metadata: v1.ObjectMeta{Name: "web"}, Spec: v1.ApplicationSpec{
		SyncPolicy: &v1.SyncPolicy{Automated: &v1.SyncPolicyAutomated{SelfHeal: true}},
	}}
	freeze := &v1.SyncWindow{Kind: v1.SyncWindowDeny, Schedule: "0 22 * * 5", Duration: "56h", Applications: []string{"*"}, ManualSync: true}
	cmp := &comparison{sync: v1.SyncStatusOutOfSync, revision: "abc", project: &v1.AppProject{},
		resources: []v1.ResourceStatus{{Kind: v1.KindJob, Namespace: "default", Name: "web", Status: v1.SyncStatusOutOfSync}}}

	if op := c.autoSyncOperation(app, cmp); op == nil {
		t.Fatal("no auto-sync without windows")
	}
	cmp.project.Spec.SyncWindows = v1.SyncWindows{freeze}
	if op := c.autoSyncOperation(app, cmp); op != nil {
		t.Fatal("auto-sync during a deny window")
	}
	now = now.Add(60 * time.Hour) // Monday morning: the freeze is over
	if op := c.autoSyncOperation(app, cmp); op == nil {
		t.Fatal("no auto-sync after the deny window")
	}
	// Only inside an allow window.
	cmp.project.Spec.SyncWindows = v1.SyncWindows{{Kind: v1.SyncWindowAllow, Schedule: "0 2 * * *", Duration: "1h", Applications: []string{"web"}}}
	if op := c.autoSyncOperation(app, cmp); op != nil {
		t.Fatal("auto-sync outside the allow window")
	}
}
