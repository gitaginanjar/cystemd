// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package appset

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// fakeRepos serves a directory as every repository, at commit "c1".
type fakeRepos struct {
	root      string
	checkouts int
}

func (f *fakeRepos) Resolve(_ context.Context, _, revision string) (string, error) { return "c1", nil }
func (f *fakeRepos) Checkout(_ context.Context, _, _ string, fn func(string) error) error {
	f.checkouts++
	return fn(f.root)
}

func repoTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func set(gens []v1.ApplicationSetGenerator, name, path string) *v1.ApplicationSet {
	return &v1.ApplicationSet{
		Metadata: v1.ObjectMeta{Name: "apps"},
		Spec: v1.ApplicationSetSpec{
			Generators: gens,
			Template: v1.ApplicationSetTemplate{
				Metadata: v1.ApplicationSetTemplateMeta{Name: name},
				Spec: v1.ApplicationSpec{
					Project: "default",
					Source:  v1.ApplicationSource{RepoURL: "https://git.example.com/ops.git", Path: path},
				},
			},
		},
	}
}

func names(apps []*v1.Application) []string {
	var out []string
	for _, a := range apps {
		out = append(out, a.Metadata.Name)
	}
	return out
}

func TestListGeneratorAndRender(t *testing.T) {
	s := set([]v1.ApplicationSetGenerator{{List: &v1.ListGenerator{Elements: []map[string]any{
		{"env": "dev", "replicas": 1},
		{"env": "Prod EU", "replicas": 3, "values": map[string]any{"tier": "gold"}},
	}}}}, "web-{{ .env | normalize }}", "apps/web/{{ .env | lower | replace \" \" \"-\" }}")
	s.Spec.Template.Metadata.Labels = map[string]string{"env": "{{ .env | normalize }}", "{{ .env | normalize }}-replicas": "{{ .replicas }}"}
	apps, err := (&Generator{}).Applications(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(apps); !slices.Equal(got, []string{"web-dev", "web-prod-eu"}) {
		t.Fatalf("names = %v", got)
	}
	prod := apps[1]
	if prod.Spec.Source.Path != "apps/web/prod-eu" || prod.Metadata.Labels["env"] != "prod-eu" || prod.Metadata.Labels["prod-eu-replicas"] != "3" {
		t.Fatalf("prod = %+v", prod.Metadata)
	}
	if prod.Metadata.Labels[v1.AppSetLabelKey] != "apps" || !prod.Metadata.HasFinalizer(v1.ResourcesFinalizer) || prod.Kind != v1.KindApplication {
		t.Fatalf("ownership: %+v", prod.Metadata)
	}

	// Preserving resources leaves the finalizer out.
	s.Spec.SyncPolicy = &v1.ApplicationSetSyncPolicy{PreserveResourcesOnDeletion: true}
	apps, _ = (&Generator{}).Applications(context.Background(), s)
	if apps[0].Metadata.HasFinalizer(v1.ResourcesFinalizer) {
		t.Fatal("finalizer despite preserveResourcesOnDeletion")
	}

	// A missing parameter is an error, unless the options say otherwise.
	s.Spec.Template.Metadata.Name = "web-{{ .region }}"
	if _, err := (&Generator{}).Applications(context.Background(), s); err == nil || !strings.Contains(err.Error(), "map has no entry for key \"region\"") {
		t.Fatalf("missing key: %v", err)
	}
	// Two parameter sets generating one name.
	s.Spec.Template.Metadata.Name = "web"
	if _, err := (&Generator{}).Applications(context.Background(), s); err == nil || !strings.Contains(err.Error(), `both generate application "web"`) {
		t.Fatalf("duplicate: %v", err)
	}
	// A generated Application must be valid.
	s.Spec.Template.Metadata.Name = "Web {{ .env }}"
	if _, err := (&Generator{}).Applications(context.Background(), s); err == nil || !strings.Contains(err.Error(), "invalid name") {
		t.Fatalf("invalid: %v", err)
	}
	// The environment is out of reach.
	s.Spec.Template.Metadata.Name = `{{ env "HOME" }}`
	if _, err := (&Generator{}).Applications(context.Background(), s); err == nil || !strings.Contains(err.Error(), `function "env" not defined`) {
		t.Fatalf("env: %v", err)
	}
}

func TestClusterGenerator(t *testing.T) {
	g := &Generator{Clusters: func() []Cluster {
		return []Cluster{
			{Name: "in-cluster", Server: "http://nomad:4646", Labels: map[string]string{"env": "dev"}},
			{Name: "EU_Prod", Server: "https://eu:4646", Labels: map[string]string{"env": "prod", "region": "eu"}},
			{Name: "us-prod", Server: "https://us:4646", Labels: map[string]string{"env": "prod"}},
		}
	}}
	s := set([]v1.ApplicationSetGenerator{{Clusters: &v1.ClusterGenerator{
		Selector: &v1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
		Values:   map[string]string{"ns": "web-{{ .metadata.labels.env }}"},
	}}}, "web-{{ .nameNormalized }}", "apps/web")
	s.Spec.Template.Spec.Destination = v1.ApplicationDestination{Name: "{{ .name }}", Namespace: "{{ .values.ns }}"}
	apps, err := g.Applications(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(apps); !slices.Equal(got, []string{"web-eu-prod", "web-us-prod"}) {
		t.Fatalf("names = %v", got)
	}
	if d := apps[0].Spec.Destination; d.Name != "EU_Prod" || d.Namespace != "web-prod" {
		t.Fatalf("destination = %+v", d)
	}
	s.Spec.Generators[0].Clusters.Selector = &v1.LabelSelector{MatchExpressions: []v1.LabelSelectorRequirement{{Key: "region", Operator: "DoesNotExist"}}}
	apps, _ = g.Applications(context.Background(), s)
	if got := names(apps); !slices.Equal(got, []string{"web-in-cluster", "web-us-prod"}) {
		t.Fatalf("names = %v", got)
	}
}

func TestGitDirectoriesAndFiles(t *testing.T) {
	repos := &fakeRepos{root: repoTree(t, map[string]string{
		"apps/web/web.nomad.hcl":           "job",
		"apps/api/api.nomad.hcl":           "job",
		"apps/legacy/old.nomad.hcl":        "job",
		"apps/web/files/index.html":        "x",
		"clusters/eu/config.yaml":          "cluster: eu\nreplicas: 3\nlabels:\n  tier: gold\n",
		"clusters/us/config.json":          `{"cluster": "us", "replicas": 2}`,
		"clusters/list/config.yaml":        "- cluster: ap\n- cluster: sa\n",
		".git/config":                      "ignored",
		"apps/.hidden/x.nomad.hcl":         "job",
		"clusters/eu/nested/deep/config.y": "not matched",
	})}
	g := &Generator{Repos: repos}
	s := set([]v1.ApplicationSetGenerator{{Git: &v1.GitGenerator{RepoURL: "https://git.example.com/ops.git",
		Directories: []v1.GitDirectoryGeneratorItem{{Path: "apps/*"}, {Path: "apps/legacy", Exclude: true}},
		Values:      map[string]string{"owner": "team-{{ .path.basename }}"},
	}}}, "{{ .path.basename }}", "{{ .path.path }}")
	s.Spec.Template.Metadata.Annotations = map[string]string{"owner": "{{ .values.owner }}", "depth": "{{ len .path.segments }}"}
	apps, err := g.Applications(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(apps); !slices.Equal(got, []string{"api", "web"}) {
		t.Fatalf("directories: %v", got)
	}
	if apps[1].Spec.Source.Path != "apps/web" || apps[1].Metadata.Annotations["owner"] != "team-web" || apps[1].Metadata.Annotations["depth"] != "2" {
		t.Fatalf("web = %+v %+v", apps[1].Spec.Source, apps[1].Metadata.Annotations)
	}
	// The same commit is read once.
	if _, err := g.Applications(context.Background(), s); err != nil || repos.checkouts != 1 {
		t.Fatalf("checkouts = %d, %v", repos.checkouts, err)
	}

	files := set([]v1.ApplicationSetGenerator{{Git: &v1.GitGenerator{RepoURL: "https://git.example.com/ops.git",
		Files: []v1.GitFileGeneratorItem{{Path: "clusters/*/config.*"}},
	}}}, "web-{{ .cluster }}", "apps/web")
	files.Spec.Template.Metadata.Annotations = map[string]string{"from": "{{ .path.path }}/{{ .path.filename }}"}
	apps, err = g.Applications(context.Background(), files)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(apps); !slices.Equal(got, []string{"web-eu", "web-ap", "web-sa", "web-us"}) {
		t.Fatalf("files: %v", got)
	}
	if apps[0].Metadata.Annotations["from"] != "clusters/eu/config.yaml" {
		t.Fatalf("annotations = %v", apps[0].Metadata.Annotations)
	}

	// ** crosses directories; pathParamPrefix nests the path parameters.
	deep := set([]v1.ApplicationSetGenerator{{Git: &v1.GitGenerator{RepoURL: "https://git.example.com/ops.git",
		Directories: []v1.GitDirectoryGeneratorItem{{Path: "clusters/**/deep"}}, PathParamPrefix: "repo",
	}}}, "{{ .repo.path.basename }}-{{ index .repo.path.segments 1 }}", "{{ .repo.path.path }}")
	apps, err = g.Applications(context.Background(), deep)
	if err != nil || len(apps) != 1 || apps[0].Metadata.Name != "deep-eu" || apps[0].Spec.Source.Path != "clusters/eu/nested/deep" {
		t.Fatalf("deep: %v %v", names(apps), err)
	}

	if _, err := (&Generator{}).Applications(context.Background(), s); err == nil {
		t.Fatal("git without repositories")
	}
}

func TestMatrixAndMerge(t *testing.T) {
	repos := &fakeRepos{root: repoTree(t, map[string]string{
		"apps/dev/web/a": "", "apps/dev/api/a": "", "apps/prod/web/a": "",
	})}
	g := &Generator{Repos: repos, Clusters: func() []Cluster {
		return []Cluster{{Name: "dev", Labels: map[string]string{"env": "dev"}}, {Name: "prod", Labels: map[string]string{"env": "prod"}}}
	}}
	// Each cluster gets the applications of its environment's directory.
	s := set([]v1.ApplicationSetGenerator{{Matrix: &v1.MatrixGenerator{Generators: []v1.ApplicationSetGenerator{
		{Clusters: &v1.ClusterGenerator{}},
		{Git: &v1.GitGenerator{RepoURL: "https://git.example.com/ops.git",
			Directories: []v1.GitDirectoryGeneratorItem{{Path: "apps/{{ .metadata.labels.env }}/*"}},
			Values:      map[string]string{"app": "{{ .path.basename }}"}}},
	}}}}, "{{ .values.app }}-{{ .name }}", "{{ .path.path }}")
	s.Spec.Template.Spec.Destination.Name = "{{ .name }}"
	apps, err := g.Applications(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(apps); !slices.Equal(got, []string{"api-dev", "web-dev", "web-prod"}) {
		t.Fatalf("matrix: %v", got)
	}
	if apps[2].Spec.Source.Path != "apps/prod/web" || apps[2].Spec.Destination.Name != "prod" {
		t.Fatalf("web-prod = %+v", apps[2].Spec)
	}

	// Merge: defaults from the clusters, overridden per cluster by a list.
	m := set([]v1.ApplicationSetGenerator{{Merge: &v1.MergeGenerator{MergeKeys: []string{"name"}, Generators: []v1.ApplicationSetGenerator{
		{Clusters: &v1.ClusterGenerator{Values: map[string]string{"replicas": "1"}}},
		{List: &v1.ListGenerator{Elements: []map[string]any{{"name": "prod", "values": map[string]any{"replicas": "3"}}}}},
	}}}}, "web-{{ .name }}", "apps/web")
	m.Spec.Template.Metadata.Annotations = map[string]string{"replicas": "{{ .values.replicas }}"}
	apps, err = g.Applications(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if apps[0].Metadata.Annotations["replicas"] != "1" || apps[1].Metadata.Annotations["replicas"] != "3" {
		t.Fatalf("merge: %v %v", apps[0].Metadata.Annotations, apps[1].Metadata.Annotations)
	}
}

func TestGlobAndNormalize(t *testing.T) {
	for _, c := range []struct {
		pattern, name string
		want          bool
	}{
		{"apps/*", "apps/web", true},
		{"apps/*", "apps/web/x", false},
		{"apps/**", "apps/web/x", true},
		{"apps/**/config.yaml", "apps/config.yaml", true},
		{"apps/**/config.yaml", "apps/a/b/config.yaml", true},
		{"**", "x/y", true},
		{"apps/w?b", "apps/web", true},
		{"apps/[ab]*", "apps/api", true},
		{"apps/[", "apps/[", false},
	} {
		if got := globMatch(c.pattern, c.name); got != c.want {
			t.Errorf("%s ~ %s = %v", c.pattern, c.name, got)
		}
	}
	for in, want := range map[string]string{"EU_Prod": "eu-prod", "--a..b--": "a..b", "web": "web", strings.Repeat("x", 70): strings.Repeat("x", 63)} {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q", in, got)
		}
	}
}

func TestValidation(t *testing.T) {
	good := set([]v1.ApplicationSetGenerator{{List: &v1.ListGenerator{}}}, "x", "y")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := set([]v1.ApplicationSetGenerator{
		{},
		{List: &v1.ListGenerator{}, Clusters: &v1.ClusterGenerator{}},
		{Git: &v1.GitGenerator{RepoURL: "r"}},
		{Matrix: &v1.MatrixGenerator{Generators: []v1.ApplicationSetGenerator{{List: &v1.ListGenerator{}}}}},
		{Merge: &v1.MergeGenerator{Generators: []v1.ApplicationSetGenerator{{List: &v1.ListGenerator{}}, {List: &v1.ListGenerator{}}}}},
		{Matrix: &v1.MatrixGenerator{Generators: []v1.ApplicationSetGenerator{
			{Matrix: &v1.MatrixGenerator{Generators: []v1.ApplicationSetGenerator{
				{Matrix: &v1.MatrixGenerator{Generators: []v1.ApplicationSetGenerator{{List: &v1.ListGenerator{}}, {List: &v1.ListGenerator{}}}}},
				{List: &v1.ListGenerator{}}}}},
			{List: &v1.ListGenerator{}}}}},
	}, "", "y")
	bad.Spec.Template.Spec.Project = "{{ .project }}"
	bad.Spec.SyncPolicy = &v1.ApplicationSetSyncPolicy{ApplicationsSync: "everything"}
	bad.Spec.GoTemplateOptions = []string{"missingkey=panic"}
	err := bad.Validate()
	for _, want := range []string{
		"generators[0]: a generator is exactly one",
		"generators[1]: a generator is exactly one",
		"generators[2]: git: give directories or files",
		"generators[3]: matrix: combine exactly two",
		"generators[4]: merge: mergeKeys are required",
		"cannot nest another matrix or merge more than once",
		"metadata.name is required",
		"project cannot be a template",
		`applicationsSync "everything"`,
		`"missingkey=panic"`,
	} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}
