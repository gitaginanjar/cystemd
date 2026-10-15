// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package manifest_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/nomad/api"

	"github.com/gitaginanjar/cystemd/internal/manifest"
	"github.com/gitaginanjar/cystemd/internal/render"
	"github.com/gitaginanjar/cystemd/internal/testutil"
	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

func TestDecode(t *testing.T) {
	objs, err := manifest.Decode(strings.NewReader(`
apiVersion: cystemd.io/v1alpha1
kind: AppProject
metadata: {name: p}
---
---
kind: Application
metadata: {name: a}
spec:
  project: p
  source: {repoURL: x}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].Project == nil || objs[1].Application.Spec.Project != "p" || objs[1].Application.APIVersion != v1.APIVersion {
		t.Fatalf("objs = %+v", objs)
	}
	if manifest.Kinds(objs) != "AppProject/p, Application/a" {
		t.Fatal(manifest.Kinds(objs))
	}
	for name, src := range map[string]string{
		"typo":        "kind: Application\nmetadata: {name: a}\nspec: {projct: p}\n",
		"no kind":     "metadata: {name: a}\n",
		"bad kind":    "kind: Job\n",
		"bad version": "apiVersion: argoproj.io/v1alpha1\nkind: Application\n",
		"empty":       "---\n",
	} {
		if _, err := manifest.Decode(strings.NewReader(src)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The example repository must stay valid: its manifests decode and
// validate, and its applications render.
func TestExamples(t *testing.T) {
	root := filepath.Join("..", "..", "examples", "gitops")
	apps := map[string]*v1.Application{}
	for _, f := range []string{"projects/platform.yaml", "applications/hello.yaml", "applications/payments-api.yaml"} {
		objs, err := manifest.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range objs {
			if o.Project != nil {
				o.Project.SetDefaults()
				if err := o.Project.Validate(); err != nil {
					t.Fatalf("%s: %v", f, err)
				}
				if !o.Project.IsSourcePermitted("https://git.example.com/platform/deploy.git") {
					t.Fatalf("%s: the examples' repository is not permitted", f)
				}
				continue
			}
			a := o.Application
			a.SetDefaults()
			if err := a.Validate(); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			apps[a.Metadata.Name] = a
		}
	}

	ctx := context.Background()
	r := &render.Renderer{Opts: render.Options{ConsulEnabled: true, VaultAddress: "https://vault.service.consul:8200"}}
	m, err := r.Render(ctx, apps["payments-api"], filepath.Join(root, "apps", "payments-api"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Jobs) != 1 || *m.Jobs[0].Type != "system" || len(m.KV) == 0 {
		t.Fatalf("payments-api: jobs=%d kv=%d", len(m.Jobs), len(m.KV))
	}

	// HCL jobspecs are parsed by Nomad itself.
	if testing.Short() {
		t.Skip("hello needs a Nomad agent to parse HCL")
	}
	nomad := testutil.StartNomad(t, testutil.NomadOptions{})
	client, err := api.NewClient(&api.Config{Address: nomad.URL})
	if err != nil {
		t.Fatal(err)
	}
	r.Parse = func(_ context.Context, hcl, vars string) (*api.Job, error) {
		return client.Jobs().ParseHCLOpts(&api.JobsParseRequest{JobHCL: hcl, Variables: vars, Canonicalize: true})
	}
	m, err = r.Render(ctx, apps["hello"], filepath.Join(root, "apps", "hello"))
	if err != nil {
		t.Fatal(err)
	}
	j := m.Jobs[0]
	kv := map[string]string{}
	for _, p := range m.KV {
		kv[p.Key] = string(p.Value)
	}
	if *j.ID != "hello" || *j.TaskGroups[0].Count != 2 || *j.Namespace != "platform" || len(kv) != 2 ||
		kv["config/hello/settings"] != "WHOAMI_NAME=hello-production\n" {
		t.Fatalf("hello: job=%s count=%d ns=%s kv=%v", *j.ID, *j.TaskGroups[0].Count, *j.Namespace, kv)
	}
}
