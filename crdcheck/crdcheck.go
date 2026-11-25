// SPDX-License-Identifier: Apache-2.0

// Package crdcheck checks an object against a CRD's schema the way the
// API server does when it is created: nulls the schema does not allow are
// dropped, then the OpenAPI schema and its CEL rules
// (x-kubernetes-validations) are checked. It also lists fields the schema
// does not have, which the API server would silently prune.
package crdcheck

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"math"
	"path"
	"sort"
	"strings"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
)

// Problem is one field the API server would refuse (Error) or drop.
type Problem struct {
	Field   string
	Message string
	// Error: the object is refused. Otherwise a warning: the field is
	// accepted but dropped.
	Error bool
}

// DraftsDir is where, under a vendor's config/crds, the generator writes
// the draft kinds' CRDs: validated and described with the others, and left
// out of the install bundle.
const DraftsDir = "drafts"

// Load reads every CRD in a directory and its drafts subdirectory, keyed
// by "Kind.group".
func Load(fsys fs.FS) (map[string]*apiextensionsv1.CustomResourceDefinition, error) {
	out := map[string]*apiextensionsv1.CustomResourceDefinition{}
	files, err := fs.Glob(fsys, "*.yaml")
	if err != nil {
		return nil, err
	}
	drafts, _ := fs.Glob(fsys, DraftsDir+"/*.yaml")
	files = append(files, drafts...)
	for _, name := range files {
		if strings.HasSuffix(name, "kustomization.yaml") {
			continue
		}
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096).Decode(crd); err != nil {
			return nil, fmt.Errorf("%s: %w", path.Base(name), err)
		}
		if crd.Spec.Names.Kind != "" {
			out[crd.Spec.Names.Kind+"."+crd.Spec.Group] = crd
		}
	}
	return out, nil
}

// Schema is the CRD's openAPIV3Schema at version, or nil.
func Schema(crd *apiextensionsv1.CustomResourceDefinition, version string) *apiextensionsv1.JSONSchemaProps {
	if crd == nil {
		return nil
	}
	for _, v := range crd.Spec.Versions {
		if v.Name == version && v.Schema != nil {
			return v.Schema.OpenAPIV3Schema
		}
	}
	return nil
}

// Validate checks obj (a whole object: apiVersion, kind, metadata, spec)
// against the schema. Paths are like "spec.size.width".
func Validate(s *apiextensionsv1.JSONSchemaProps, obj map[string]any) ([]Problem, error) {
	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(s, internal, nil); err != nil {
		return nil, err
	}
	sv, _, err := validation.NewSchemaValidator(internal)
	if err != nil {
		return nil, err
	}
	obj, _ = PruneNulls(s, obj).(map[string]any)
	// The API server decodes a whole number as an integer; CEL types them
	// apart.
	obj, _ = wholeNumbers(obj).(map[string]any)
	var out []Problem
	for _, e := range validation.ValidateCustomResource(nil, obj, sv) {
		out = append(out, Problem{Field: e.Field, Message: e.ErrorBody(), Error: true})
	}
	// CEL rules (x-kubernetes-validations), as the API server runs them on
	// create: a generated kind's required fields that a reference may fill.
	structural, err := structuralschema.NewStructural(internal)
	if err != nil {
		return nil, err
	}
	celErrs, _ := cel.NewValidator(structural, true, celconfig.PerCallLimit).
		Validate(context.Background(), nil, structural, obj, nil, celconfig.RuntimeCELCostBudget)
	for _, e := range celErrs {
		out = append(out, Problem{Field: e.Field, Message: e.ErrorBody(), Error: true})
	}
	for _, f := range UnknownFields(s, obj, "") {
		if f == "metadata" || strings.HasPrefix(f, "metadata.") || f == "status" || strings.HasPrefix(f, "status.") {
			continue
		}
		out = append(out, Problem{Field: f, Message: "not in the CRD: the API server drops it"})
	}
	return out, nil
}

// PruneNulls drops null values the schema does not mark nullable, as the
// API server does before it validates.
func PruneNulls(s *apiextensionsv1.JSONSchemaProps, obj any) any {
	switch x := obj.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			var p *apiextensionsv1.JSONSchemaProps
			if s != nil {
				if pp, ok := s.Properties[k]; ok {
					p = &pp
				} else if s.AdditionalProperties != nil {
					p = s.AdditionalProperties.Schema
				}
			}
			if e == nil && (p == nil || !p.Nullable) {
				continue
			}
			out[k] = PruneNulls(p, e)
		}
		return out
	case []any:
		var it *apiextensionsv1.JSONSchemaProps
		if s != nil && s.Items != nil {
			it = s.Items.Schema
		}
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = PruneNulls(it, e)
		}
		return out
	}
	return obj
}

// UnknownFields lists the paths in obj that the schema does not declare
// and would prune.
func UnknownFields(s *apiextensionsv1.JSONSchemaProps, obj any, prefix string) []string {
	if s == nil || (s.XPreserveUnknownFields != nil && *s.XPreserveUnknownFields && len(s.Properties) == 0) {
		return nil
	}
	var out []string
	switch x := obj.(type) {
	case map[string]any:
		if len(s.Properties) == 0 {
			if s.AdditionalProperties != nil && s.AdditionalProperties.Schema != nil {
				for k, e := range x {
					out = append(out, UnknownFields(s.AdditionalProperties.Schema, e, join(prefix, k))...)
				}
			}
			break
		}
		for k, e := range x {
			p, ok := s.Properties[k]
			if !ok {
				if s.XPreserveUnknownFields == nil || !*s.XPreserveUnknownFields {
					out = append(out, join(prefix, k))
				}
				continue
			}
			out = append(out, UnknownFields(&p, e, join(prefix, k))...)
		}
	case []any:
		if s.Items != nil && s.Items.Schema != nil {
			for i, e := range x {
				out = append(out, UnknownFields(s.Items.Schema, e, fmt.Sprintf("%s[%d]", prefix, i))...)
			}
		}
	}
	sort.Strings(out)
	return out
}

// NumberPlaceholders replaces a "<placeholder>" string (a value an example
// cannot know) in a field the schema types as a number with 0, so the rest
// of the object can still be checked, and returns the paths it replaced.
func NumberPlaceholders(s *apiextensionsv1.JSONSchemaProps, v any, prefix string) []string {
	if s == nil {
		return nil
	}
	var out []string
	numeric := func(p *apiextensionsv1.JSONSchemaProps) bool { return p.Type == "integer" || p.Type == "number" }
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			p, ok := s.Properties[k]
			if !ok {
				continue
			}
			if str, isStr := e.(string); isStr && IsPlaceholder(str) && numeric(&p) {
				x[k] = 0
				out = append(out, join(prefix, k))
				continue
			}
			out = append(out, NumberPlaceholders(&p, e, join(prefix, k))...)
		}
	case []any:
		if s.Items == nil || s.Items.Schema == nil {
			return nil
		}
		it := s.Items.Schema
		for i, e := range x {
			if str, isStr := e.(string); isStr && IsPlaceholder(str) && numeric(it) {
				x[i] = 0
				out = append(out, fmt.Sprintf("%s[%d]", prefix, i))
				continue
			}
			out = append(out, NumberPlaceholders(it, e, fmt.Sprintf("%s[%d]", prefix, i))...)
		}
	}
	sort.Strings(out)
	return out
}

// IsPlaceholder reports whether s is a single "<name>".
func IsPlaceholder(s string) bool {
	return len(s) > 2 && s[0] == '<' && s[len(s)-1] == '>' && !strings.ContainsAny(s[1:len(s)-1], "<> ")
}

func join(prefix, k string) string {
	if prefix == "" {
		return k
	}
	return prefix + "." + k
}

// wholeNumbers turns float64 values that are whole into int64, as the API
// server's JSON decoding does.
func wholeNumbers(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = wholeNumbers(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = wholeNumbers(e)
		}
		return out
	case float64:
		if x == math.Trunc(x) && math.Abs(x) < 1<<53 {
			return int64(x)
		}
	}
	return v
}
