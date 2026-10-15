// Copyright 2026 G.G (https://github.com/gitaginanjar)
// SPDX-License-Identifier: Apache-2.0

// Package manifest reads Application and AppProject manifests: YAML (or
// JSON) files, several documents separated by "---", as `cystemd app
// create -f` and the examples use them. Decoding is strict: an unknown
// field is an error, so a typo never silently drops a setting.
package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	v1 "github.com/gitaginanjar/cystemd/pkg/apis/v1alpha1"
)

// Object is one decoded manifest.
type Object struct {
	Kind        string
	Application *v1.Application
	Project     *v1.AppProject
}

// Name is the object's name.
func (o Object) Name() string {
	if o.Application != nil {
		return o.Application.Metadata.Name
	}
	if o.Project != nil {
		return o.Project.Metadata.Name
	}
	return ""
}

// ReadFile decodes a manifest file ("-" reads standard input).
func ReadFile(path string) ([]Object, error) {
	var r io.Reader
	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r = f
	}
	objs, err := Decode(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return objs, nil
}

// Decode reads every document of r.
func Decode(r io.Reader) ([]Object, error) {
	dec := yaml.NewDecoder(r)
	var out []Object
	for i := 1; ; i++ {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if len(doc.Content) == 0 || doc.Content[0].Tag == "!!null" {
			continue // empty document
		}
		obj, err := decodeDoc(&doc)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		out = append(out, obj)
	}
	if len(out) == 0 {
		return nil, errors.New("no manifests found")
	}
	return out, nil
}

func decodeDoc(doc *yaml.Node) (Object, error) {
	var head struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
	}
	if err := doc.Decode(&head); err != nil {
		return Object{}, err
	}
	if head.APIVersion != "" && head.APIVersion != v1.APIVersion {
		return Object{}, fmt.Errorf("apiVersion %q: expected %s", head.APIVersion, v1.APIVersion)
	}
	// Re-encode the document to decode it strictly.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	if err := enc.Encode(doc); err != nil {
		return Object{}, err
	}
	strict := func(v any) error {
		d := yaml.NewDecoder(&buf)
		d.KnownFields(true)
		return d.Decode(v)
	}
	switch head.Kind {
	case v1.KindApplication:
		var a v1.Application
		if err := strict(&a); err != nil {
			return Object{}, err
		}
		a.APIVersion, a.Kind = v1.APIVersion, v1.KindApplication
		return Object{Kind: head.Kind, Application: &a}, nil
	case v1.KindAppProject:
		var p v1.AppProject
		if err := strict(&p); err != nil {
			return Object{}, err
		}
		p.APIVersion, p.Kind = v1.APIVersion, v1.KindAppProject
		return Object{Kind: head.Kind, Project: &p}, nil
	case "":
		return Object{}, errors.New("kind is required (Application or AppProject)")
	}
	return Object{}, fmt.Errorf("kind %q: expected %s or %s", head.Kind, v1.KindApplication, v1.KindAppProject)
}

// Encode writes objects as YAML documents.
func Encode(w io.Writer, objs ...any) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	for _, o := range objs {
		if err := enc.Encode(o); err != nil {
			return err
		}
	}
	return enc.Close()
}

// Kinds lists the kinds of objs ("Application/web, AppProject/platform").
func Kinds(objs []Object) string {
	var parts []string
	for _, o := range objs {
		parts = append(parts, o.Kind+"/"+o.Name())
	}
	return strings.Join(parts, ", ")
}
