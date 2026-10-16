// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

package values

import (
	"strings"
	"testing"
)

const helmValues = `
myapp:
  global:
    image:
      tag: 1.10
  services:
    myapp:
      deployment:
        resources:
          limits:
            cpu: 200m
            memory: 1Gi
      config:
        config.yml: |
          server:
            port: 8080
          features: [a, b]
`

func TestExtractYAMLKeyUnwrapsHelmConfigBlob(t *testing.T) {
	got, err := ExtractYAMLKey([]byte(helmValues), ".${selector}.services.${selector}.config.[]", "myapp")
	if err != nil {
		t.Fatal(err)
	}
	want := "features:\n  - a\n  - b\nserver:\n  port: 8080\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestExtractYAMLKeyErrors(t *testing.T) {
	for _, key := range []string{".myapp.nope", ".myapp.global.image.tag.deeper", ".myapp.services.[].x.y"} {
		if _, err := ExtractYAMLKey([]byte(helmValues), key, ""); err == nil {
			t.Errorf("%s: expected error", key)
		}
	}
	if _, err := ExtractYAMLKey([]byte("a: [b"), ".a", ""); err == nil {
		t.Error("invalid yaml accepted")
	}
}

func TestExtractYAMLKeyMultilineLeafVerbatim(t *testing.T) {
	data := "cfg: |\n  logging {\n    level = \"info\"\n  }\n"
	got, err := ExtractYAMLKey([]byte(data), ".cfg", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(got), "logging {") {
		t.Fatalf("got %q", got)
	}
}

func TestExtractScalarKeepsVersionText(t *testing.T) {
	// "1.10" must stay "1.10": a numeric decode would install 1.1.
	v, err := ExtractScalar([]byte(helmValues), ".${selector}.global.image.tag", "myapp")
	if err != nil || v != "1.10" {
		t.Fatalf("got %q, %v", v, err)
	}
	cpu, _ := ExtractScalar([]byte(helmValues), ".myapp.services.myapp.deployment.resources.limits.cpu", "")
	mem, _ := ExtractScalar([]byte(helmValues), ".myapp.services.myapp.deployment.resources.limits.memory", "")
	if cpu != "200m" || mem != "1Gi" {
		t.Fatalf("cpu=%q mem=%q", cpu, mem)
	}
}

func TestExtractScalarMergeKeysAndAliases(t *testing.T) {
	data := `
base: &base
  tag: "2.0"
  extra: x
other: &other
  tag: "3.0"
app:
  <<: [*base, *other]
  name: svc
alias: *base
`
	if v, err := ExtractScalar([]byte(data), ".app.tag", ""); err != nil || v != "2.0" {
		t.Fatalf("merge precedence: got %q, %v", v, err)
	}
	if v, err := ExtractScalar([]byte(data), ".alias.extra", ""); err != nil || v != "x" {
		t.Fatalf("alias: got %q, %v", v, err)
	}
	if _, err := ExtractScalar([]byte(data), ".app", ""); err == nil {
		t.Fatal("non-scalar accepted")
	}
}
