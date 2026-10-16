// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func ptr[T any](v T) *T { return &v }

func serviceJob(count int, version uint64) *api.Job {
	return &api.Job{
		ID: ptr("web"), Type: ptr("service"), Version: ptr(version),
		SubmitTime: ptr(time.Now().Add(-time.Hour).UnixNano()),
		TaskGroups: []*api.TaskGroup{{Name: ptr("web"), Count: ptr(count)}},
	}
}

func alloc(id, client string, version uint64) *api.AllocationListStub {
	return &api.AllocationListStub{ID: id, ClientStatus: client, DesiredStatus: "run", JobVersion: version}
}

func TestJobHealth(t *testing.T) {
	cases := []struct {
		name string
		s    JobState
		want v1.HealthStatusCode
	}{
		{"missing", JobState{}, v1.HealthStatusMissing},
		{"stopped", JobState{Job: func() *api.Job { j := serviceJob(1, 0); j.Stop = ptr(true); return j }()}, v1.HealthStatusSuspended},
		{"all running", JobState{Job: serviceJob(2, 3), Allocs: []*api.AllocationListStub{alloc("a", "running", 3), alloc("b", "running", 3)}}, v1.HealthStatusHealthy},
		{"old version allocs do not count", JobState{Job: serviceJob(1, 3), Allocs: []*api.AllocationListStub{alloc("a", "running", 2)}}, v1.HealthStatusProgressing},
		{"one pending", JobState{Job: serviceJob(2, 0), Allocs: []*api.AllocationListStub{alloc("a", "running", 0), alloc("b", "pending", 0)}}, v1.HealthStatusProgressing},
		{"one failed", JobState{Job: serviceJob(2, 0), Allocs: []*api.AllocationListStub{alloc("a", "running", 0), alloc("b", "failed", 0)}}, v1.HealthStatusDegraded},
		{"critical checks", JobState{Job: serviceJob(1, 0), Allocs: []*api.AllocationListStub{alloc("a", "running", 0)}, Checks: map[string]string{"a": CheckCritical}}, v1.HealthStatusDegraded},
		{"deployment running", JobState{Job: serviceJob(1, 1), Deployment: &api.Deployment{ID: "d", Status: "running", JobVersion: 1}}, v1.HealthStatusProgressing},
		{"deployment failed", JobState{Job: serviceJob(1, 1), Deployment: &api.Deployment{ID: "d", Status: "failed", JobVersion: 1}}, v1.HealthStatusDegraded},
		{"deployment paused", JobState{Job: serviceJob(1, 1), Deployment: &api.Deployment{ID: "d", Status: "paused", JobVersion: 1}}, v1.HealthStatusSuspended},
		{"old failed deployment ignored", JobState{Job: serviceJob(1, 2), Deployment: &api.Deployment{ID: "d", Status: "failed", JobVersion: 1}, Allocs: []*api.AllocationListStub{alloc("a", "running", 2)}}, v1.HealthStatusHealthy},
		{"scaled to zero", JobState{Job: serviceJob(0, 0)}, v1.HealthStatusHealthy},
		{
			"queued long after submit = cannot schedule",
			JobState{Job: serviceJob(1, 0), Summary: &api.JobSummary{Summary: map[string]api.TaskGroupSummary{"web": {Queued: 1}}}},
			v1.HealthStatusDegraded,
		},
		{
			"queued right after submit = progressing",
			JobState{Job: func() *api.Job { j := serviceJob(1, 0); j.SubmitTime = ptr(time.Now().UnixNano()); return j }(), Summary: &api.JobSummary{Summary: map[string]api.TaskGroupSummary{"web": {Queued: 1}}}},
			v1.HealthStatusProgressing,
		},
		{"batch complete", JobState{Job: &api.Job{Type: ptr("batch"), Version: ptr(uint64(0))}, Allocs: []*api.AllocationListStub{alloc("a", "complete", 0)}}, v1.HealthStatusHealthy},
		{"batch failed", JobState{Job: &api.Job{Type: ptr("batch"), Version: ptr(uint64(0))}, Allocs: []*api.AllocationListStub{alloc("a", "failed", 0)}}, v1.HealthStatusDegraded},
		{"periodic", JobState{Job: &api.Job{Type: ptr("batch"), Periodic: &api.PeriodicConfig{Enabled: ptr(true), Spec: ptr("@daily")}}}, v1.HealthStatusHealthy},
		{"system none yet", JobState{Job: &api.Job{Type: ptr("system"), Version: ptr(uint64(0))}}, v1.HealthStatusProgressing},
		{"system running", JobState{Job: &api.Job{Type: ptr("system"), Version: ptr(uint64(0))}, Allocs: []*api.AllocationListStub{alloc("a", "running", 0)}}, v1.HealthStatusHealthy},
	}
	for _, c := range cases {
		if got := JobHealth(c.s); got.Status != c.want {
			t.Errorf("%s: got %s (%s), want %s", c.name, got.Status, got.Message, c.want)
		}
	}
}

func TestAggregateFollowsArgoOrder(t *testing.T) {
	got := Aggregate([]v1.HealthStatus{
		{Status: v1.HealthStatusHealthy},
		{Status: v1.HealthStatusProgressing},
		{Status: v1.HealthStatusSuspended},
	})
	if got.Status != v1.HealthStatusProgressing {
		t.Fatalf("got %s", got.Status)
	}
	got = Aggregate([]v1.HealthStatus{{Status: v1.HealthStatusMissing}, {Status: v1.HealthStatusDegraded}})
	if got.Status != v1.HealthStatusDegraded {
		t.Fatalf("got %s", got.Status)
	}
	if Aggregate(nil).Status != v1.HealthStatusHealthy {
		t.Fatal("empty aggregate must be healthy")
	}
}

func TestTaskAndAllocHealth(t *testing.T) {
	if TaskHealth(&api.TaskState{State: "dead", Failed: true}).Status != v1.HealthStatusDegraded {
		t.Error("failed task")
	}
	if TaskHealth(&api.TaskState{State: "dead"}).Status != v1.HealthStatusHealthy {
		t.Error("completed task")
	}
	unhealthy := alloc("a", "running", 0)
	unhealthy.DeploymentStatus = &api.AllocDeploymentStatus{Healthy: ptr(false)}
	if AllocHealth(unhealthy, "").Status != v1.HealthStatusDegraded {
		t.Error("unhealthy allocation")
	}
	if AllocHealth(alloc("a", "running", 0), CheckWarning).Status != v1.HealthStatusProgressing {
		t.Error("warning checks")
	}
	if WorstCheck(CheckPassing, CheckCritical, CheckWarning) != CheckCritical {
		t.Error("worst check")
	}
}
