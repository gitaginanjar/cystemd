// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"
)

func ptr[T any](v T) *T { return &v }

func job(count int, image string) *api.Job {
	return &api.Job{
		ID: ptr("web"), Name: ptr("web"), Type: ptr("service"),
		TaskGroups: []*api.TaskGroup{{
			Name: ptr("web"), Count: ptr(count),
			Tasks: []*api.Task{{Name: "web", Driver: "docker", Config: map[string]any{"image": image}}},
		}},
	}
}

func TestNormalizeDropsServerFieldsAndEmpties(t *testing.T) {
	j := job(2, "nginx")
	j.Version, j.ModifyIndex, j.Status = ptr(uint64(4)), ptr(uint64(99)), ptr("running")
	m, err := Normalize(j)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"Version", "ModifyIndex", "Status"} {
		if _, ok := m[f]; ok {
			t.Errorf("%s not dropped", f)
		}
	}
	y := ToYAML(m)
	if !strings.Contains(y, "Count: 2") || strings.Contains(y, "null") {
		t.Fatalf("yaml:\n%s", y)
	}
}

func TestPruneToShape(t *testing.T) {
	desired, _ := Normalize(job(2, "nginx"))
	liveJob := job(2, "nginx")
	liveJob.TaskGroups[0].Tasks[0].Identities = []*api.WorkloadIdentity{{Name: "default"}} // server default
	liveJob.Meta = map[string]string{"set-by": "server"}
	live, _ := Normalize(liveJob)
	pruned := PruneToShape(live, desired)
	if ToYAML(pruned) != ToYAML(desired) {
		t.Fatalf("server defaults survived pruning:\n%s\nvs\n%s", ToYAML(pruned), ToYAML(desired))
	}
	// A real difference stays visible.
	changed, _ := Normalize(job(3, "nginx"))
	if ToYAML(PruneToShape(changed, desired)) == ToYAML(desired) {
		t.Fatal("count change hidden")
	}
}

func TestApplyIgnoreDifferences(t *testing.T) {
	desired, _ := ToMap(job(2, "nginx"))
	live, _ := ToMap(job(7, "nginx")) // an autoscaler owns the count
	ApplyIgnoreDifferences(desired, live, []string{"TaskGroups.*.Count"})
	out, err := FromMap(desired)
	if err != nil {
		t.Fatal(err)
	}
	if *out.TaskGroups[0].Count != 7 {
		t.Fatalf("count = %d", *out.TaskGroups[0].Count)
	}
	// JSON-pointer spelling works too.
	desired2, _ := ToMap(job(2, "nginx"))
	ApplyIgnoreDifferences(desired2, live, []string{"/TaskGroups/0/Count"})
	out2, _ := FromMap(desired2)
	if *out2.TaskGroups[0].Count != 7 {
		t.Fatalf("pointer path: count = %d", *out2.TaskGroups[0].Count)
	}
}

func TestFormatJobDiff(t *testing.T) {
	d := &api.JobDiff{Type: "Edited", ID: "web", TaskGroups: []*api.TaskGroupDiff{{
		Type: "Edited", Name: "web", Updates: map[string]uint64{"create/destroy update": 1},
		Tasks: []*api.TaskDiff{{Type: "Edited", Name: "web", Annotations: []string{"forces create/destroy update"},
			Objects: []*api.ObjectDiff{{Type: "Edited", Name: "Config", Fields: []*api.FieldDiff{{Type: "Edited", Name: "image", Old: "nginx:1", New: "nginx:2"}}}},
		}},
	}}}
	out := FormatJobDiff(d)
	for _, want := range []string{`+/- Job: "web"`, `(1 create/destroy update)`, `+/- image: "nginx:1" => "nginx:2"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if FormatJobDiff(&api.JobDiff{Type: "None"}) != "" || HasChanges(nil) {
		t.Error("no-change diff rendered")
	}
}

func TestUnified(t *testing.T) {
	a := "a\nb\nc\nd\ne\nf\ng\n"
	b := "a\nb\nC\nd\ne\nf\ng\nh\n"
	out := Unified("live", "desired", a, b, 1)
	for _, want := range []string{"--- live", "+++ desired", "-c", "+C", "+h"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if Unified("x", "y", a, a, 3) != "" {
		t.Error("identical input produced a diff")
	}
}
