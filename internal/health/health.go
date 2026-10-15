// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package health assesses the health of Nomad objects in Argo CD's
// vocabulary (Healthy, Progressing, Degraded, Suspended, Missing, Unknown),
// so an Application's health reads the same way whether it runs pods or
// systemd units.
package health

import (
	"fmt"
	"time"

	"github.com/hashicorp/nomad/api"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Check states, as Consul and Nomad report them.
const (
	CheckPassing  = "passing"
	CheckWarning  = "warning"
	CheckCritical = "critical"
	CheckPending  = "pending"
)

// JobState is everything JobHealth looks at.
type JobState struct {
	Job        *api.Job
	Deployment *api.Deployment
	Allocs     []*api.AllocationListStub
	Summary    *api.JobSummary
	// Checks maps an allocation ID to the worst state of its service checks.
	Checks map[string]string
	// Now is the assessment time (tests); zero means time.Now().
	Now time.Time
}

// queuedGrace is how long queued placements count as progress after a job
// change before they read as a placement failure.
const queuedGrace = time.Minute

func status(code v1.HealthStatusCode, format string, args ...any) v1.HealthStatus {
	return v1.HealthStatus{Status: code, Message: fmt.Sprintf(format, args...)}
}

// JobHealth assesses a live job.
func JobHealth(s JobState) v1.HealthStatus {
	job := s.Job
	if job == nil {
		return status(v1.HealthStatusMissing, "Job does not exist")
	}
	if job.Stop != nil && *job.Stop {
		return status(v1.HealthStatusSuspended, "Job is stopped")
	}
	now := s.Now
	if now.IsZero() {
		now = time.Now()
	}
	jobType := deref(job.Type)
	switch {
	case job.ParameterizedJob != nil:
		return status(v1.HealthStatusHealthy, "Parameterized job (dispatched on demand)")
	case job.Periodic != nil && job.Periodic.Enabled != nil && *job.Periodic.Enabled:
		return status(v1.HealthStatusHealthy, "Periodic job (%s)", deref(job.Periodic.Spec))
	}

	version := derefU64(job.Version)
	var current []*api.AllocationListStub
	for _, a := range s.Allocs {
		if a.JobVersion == version && a.DesiredStatus == "run" {
			current = append(current, a)
		}
	}

	if jobType == "batch" || jobType == "sysbatch" {
		return batchHealth(job, current)
	}

	if d := s.Deployment; d != nil && d.JobVersion == version {
		switch d.Status {
		case "running", "pending", "blocked", "unblocking", "initializing":
			return status(v1.HealthStatusProgressing, "Deployment %s: %s", short(d.ID), orDefault(d.StatusDescription, d.Status))
		case "failed":
			return status(v1.HealthStatusDegraded, "Deployment %s failed: %s", short(d.ID), d.StatusDescription)
		case "paused":
			return status(v1.HealthStatusSuspended, "Deployment %s is paused", short(d.ID))
		}
	}

	desired := 0
	for _, tg := range job.TaskGroups {
		if tg.Count != nil {
			desired += *tg.Count
		}
	}
	running, pending, failed, critical := 0, 0, 0, 0
	for _, a := range current {
		switch a.ClientStatus {
		case "running":
			if a.DeploymentStatus != nil && a.DeploymentStatus.Healthy != nil && !*a.DeploymentStatus.Healthy {
				failed++
				continue
			}
			running++
			if s.Checks[a.ID] == CheckCritical {
				critical++
			}
		case "pending":
			pending++
		case "failed", "lost":
			failed++
		}
	}
	queued := 0
	if s.Summary != nil {
		for _, tg := range s.Summary.Summary {
			queued += tg.Queued
		}
	}

	switch {
	case failed > 0:
		return status(v1.HealthStatusDegraded, "%d allocation(s) failed or unhealthy", failed)
	case critical > 0:
		return status(v1.HealthStatusDegraded, "%d allocation(s) with critical health checks", critical)
	case queued > 0:
		if job.SubmitTime != nil && now.Sub(time.Unix(0, *job.SubmitTime)) < queuedGrace {
			return status(v1.HealthStatusProgressing, "%d placement(s) queued", queued)
		}
		return status(v1.HealthStatusDegraded, "%d placement(s) cannot be scheduled (no eligible node or insufficient resources)", queued)
	case pending > 0:
		return status(v1.HealthStatusProgressing, "%d allocation(s) pending", pending)
	case jobType == "system":
		if running == 0 {
			return status(v1.HealthStatusProgressing, "No allocation running yet")
		}
		return status(v1.HealthStatusHealthy, "Running on %d node(s)", running)
	case desired == 0:
		return status(v1.HealthStatusHealthy, "Scaled to zero")
	case running < desired:
		return status(v1.HealthStatusProgressing, "%d/%d allocation(s) running", running, desired)
	default:
		return status(v1.HealthStatusHealthy, "%d/%d allocation(s) running", running, desired)
	}
}

func batchHealth(job *api.Job, current []*api.AllocationListStub) v1.HealthStatus {
	failed, running, complete := 0, 0, 0
	for _, a := range current {
		switch a.ClientStatus {
		case "failed", "lost":
			failed++
		case "running", "pending":
			running++
		case "complete":
			complete++
		}
	}
	switch {
	case failed > 0:
		return status(v1.HealthStatusDegraded, "%d allocation(s) failed", failed)
	case running > 0:
		return status(v1.HealthStatusProgressing, "%d allocation(s) running", running)
	case complete > 0:
		return status(v1.HealthStatusHealthy, "Completed")
	default:
		return status(v1.HealthStatusProgressing, "Waiting for placement")
	}
}

// AllocHealth assesses one allocation.
func AllocHealth(a *api.AllocationListStub, checks string) v1.HealthStatus {
	switch a.ClientStatus {
	case "pending":
		return status(v1.HealthStatusProgressing, "Pending")
	case "running":
		if a.DeploymentStatus != nil && a.DeploymentStatus.Healthy != nil && !*a.DeploymentStatus.Healthy {
			return status(v1.HealthStatusDegraded, "Marked unhealthy by its deployment")
		}
		switch checks {
		case CheckCritical:
			return status(v1.HealthStatusDegraded, "Health checks critical")
		case CheckWarning:
			return status(v1.HealthStatusProgressing, "Health checks warning")
		}
		if a.DesiredStatus != "run" {
			return status(v1.HealthStatusProgressing, "Stopping (%s)", a.DesiredStatus)
		}
		return status(v1.HealthStatusHealthy, "Running")
	case "complete":
		return status(v1.HealthStatusHealthy, "Complete")
	case "failed":
		return status(v1.HealthStatusDegraded, "Failed: %s", orDefault(a.ClientDescription, "see task events"))
	case "lost":
		return status(v1.HealthStatusDegraded, "Lost (node down)")
	case "unknown":
		return status(v1.HealthStatusUnknown, "Client disconnected")
	}
	return status(v1.HealthStatusUnknown, "%s", a.ClientStatus)
}

// TaskHealth assesses one task of an allocation.
func TaskHealth(ts *api.TaskState) v1.HealthStatus {
	if ts == nil {
		return status(v1.HealthStatusUnknown, "No state")
	}
	switch ts.State {
	case "pending":
		return status(v1.HealthStatusProgressing, "Pending")
	case "running":
		return status(v1.HealthStatusHealthy, "Running (%d restart(s))", ts.Restarts)
	case "dead":
		if ts.Failed {
			return status(v1.HealthStatusDegraded, "Failed%s", lastEvent(ts))
		}
		return status(v1.HealthStatusHealthy, "Completed")
	}
	return status(v1.HealthStatusUnknown, "%s", ts.State)
}

func lastEvent(ts *api.TaskState) string {
	for i := len(ts.Events) - 1; i >= 0; i-- {
		e := ts.Events[i]
		if e.DisplayMessage != "" {
			return ": " + e.DisplayMessage
		}
	}
	return ""
}

// DeploymentHealth assesses a deployment.
func DeploymentHealth(d *api.Deployment) v1.HealthStatus {
	switch d.Status {
	case "successful":
		return status(v1.HealthStatusHealthy, "%s", d.StatusDescription)
	case "running", "pending", "blocked", "unblocking", "initializing":
		return status(v1.HealthStatusProgressing, "%s", d.StatusDescription)
	case "failed":
		return status(v1.HealthStatusDegraded, "%s", d.StatusDescription)
	case "paused":
		return status(v1.HealthStatusSuspended, "%s", d.StatusDescription)
	case "cancelled":
		return status(v1.HealthStatusSuspended, "%s", d.StatusDescription)
	}
	return status(v1.HealthStatusUnknown, "%s", d.Status)
}

// GroupHealth assesses a task group from its summary and desired count.
func GroupHealth(desired int, s api.TaskGroupSummary, system bool) v1.HealthStatus {
	switch {
	case s.Failed > 0 && s.Running < desired:
		return status(v1.HealthStatusDegraded, "%d running, %d failed", s.Running, s.Failed)
	case s.Queued > 0:
		return status(v1.HealthStatusProgressing, "%d queued", s.Queued)
	case s.Starting > 0:
		return status(v1.HealthStatusProgressing, "%d starting", s.Starting)
	case system:
		return status(v1.HealthStatusHealthy, "%d running", s.Running)
	case s.Running < desired:
		return status(v1.HealthStatusProgressing, "%d/%d running", s.Running, desired)
	}
	return status(v1.HealthStatusHealthy, "%d/%d running", s.Running, desired)
}

// ChecksHealth assesses a service from its check states.
func ChecksHealth(states []string) v1.HealthStatus {
	worst := ""
	rank := map[string]int{"": 0, CheckPassing: 1, CheckPending: 2, CheckWarning: 3, CheckCritical: 4}
	for _, s := range states {
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	switch worst {
	case "", CheckPassing:
		return status(v1.HealthStatusHealthy, "All checks passing")
	case CheckPending:
		return status(v1.HealthStatusProgressing, "Checks pending")
	case CheckWarning:
		return status(v1.HealthStatusProgressing, "Checks warning")
	}
	return status(v1.HealthStatusDegraded, "Checks critical")
}

// WorstCheck returns the worst of the check states.
func WorstCheck(states ...string) string {
	rank := map[string]int{"": 0, CheckPassing: 1, CheckPending: 2, CheckWarning: 3, CheckCritical: 4}
	worst := ""
	for _, s := range states {
		if rank[s] > rank[worst] {
			worst = s
		}
	}
	return worst
}

// Aggregate returns the worst health of statuses (Healthy when empty).
func Aggregate(statuses []v1.HealthStatus) v1.HealthStatus {
	out := v1.HealthStatus{Status: v1.HealthStatusHealthy}
	for _, s := range statuses {
		if v1.IsWorse(out.Status, s.Status) {
			out = s
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefU64(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
