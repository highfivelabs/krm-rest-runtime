// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package crdcheck

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// A generated kind's required field that a reference may fill: a CEL rule,
// not a plain required.
const gadget = `
type: object
properties:
  apiVersion: {type: string}
  kind: {type: string}
  metadata: {type: object}
  spec:
    type: object
    properties:
      widgetID: {type: string}
      name: {type: string}
      size: {type: integer, nullable: true}
      fieldRefs:
        type: object
        maxProperties: 64
        additionalProperties:
          type: object
          required: [kind, name]
          properties: {kind: {type: string}, name: {type: string}}
    x-kubernetes-validations:
    - rule: has(self.widgetID) || (has(self.fieldRefs) && 'widgetID' in self.fieldRefs)
      message: spec.widgetID is required (or set it from spec.fieldRefs.widgetID)
`

func TestValidate(t *testing.T) {
	s := &apiextensionsv1.JSONSchemaProps{}
	if err := yaml.Unmarshal([]byte(gadget), s); err != nil {
		t.Fatal(err)
	}
	check := func(spec map[string]any) []Problem {
		t.Helper()
		got, err := Validate(s, map[string]any{"apiVersion": "x/v1", "kind": "Gadget", "metadata": map[string]any{"name": "g"}, "spec": spec})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := check(map[string]any{"name": "g"}); len(got) != 1 || !got[0].Error || !strings.Contains(got[0].Message, "spec.widgetID is required") {
		t.Errorf("missing widgetID: %+v", got)
	}
	if got := check(map[string]any{"name": "g", "fieldRefs": map[string]any{"widgetID": map[string]any{"kind": "Widget", "name": "w"}}}); len(got) != 0 {
		t.Errorf("widgetID from fieldRefs: %+v", got)
	}
	// A null the schema allows is kept; an unknown field is a warning.
	if got := check(map[string]any{"widgetID": "1", "size": nil, "colour": "red"}); len(got) != 1 || got[0].Error || got[0].Field != "spec.colour" {
		t.Errorf("nullable size and unknown colour: %+v", got)
	}
	if got := check(map[string]any{"widgetID": "1", "name": 3}); len(got) != 1 || !got[0].Error || got[0].Field != "spec.name" {
		t.Errorf("a number for a string: %+v", got)
	}
}
