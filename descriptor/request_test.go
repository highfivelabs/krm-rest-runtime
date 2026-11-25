// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"reflect"
	"strings"
	"testing"
)

func TestRequestValidation(t *testing.T) {
	kind := func(extra string) string {
		return `vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Item
  group: x.example.hfl.dev
  version: v1alpha1
  plural: items
  shape: upsert
  read: {method: GET, path: "/items/{key}"` + extra + `}
  update: {method: PUT, path: "/items/{key}", request: {v: "{value}"}` + map[bool]string{true: ", body: {x: 1}"}[strings.Contains(extra, "BODY")] + `}
  delete: {method: DELETE, path: "/items/{key}"}
  params: [{name: key, field: key}]
`
	}
	if _, err := Parse([]byte(kind(""))); err != nil {
		t.Fatalf("valid: %v", err)
	}
	if _, err := Parse([]byte(kind(`, request: {v: "{value}"}`))); err == nil {
		t.Error("request on a read: no error")
	}
	if _, err := Parse([]byte(kind(`, query: {q: BODY}`))); err == nil {
		t.Error("request beside a body: no error")
	}
}

// FillRequest fills a template and leaves out what nothing fills;
// RequestValues reads a sent body back into the names it filled.
func TestFillRequestRoundTrip(t *testing.T) {
	tmpl := map[string]any{"data": map[string]any{
		"values": []any{"{value}", "{missing}"},
		"key":    "{item_key}",
		"label":  "item {item_key} of {store}",
		"deep":   "{a.b}",
		"fixed":  "items",
	}}
	vals := map[string]any{"value": map[string]any{"id": "1"}, "item_key": "k", "store": "s", "a": map[string]any{"b": 2}}
	got, _ := FillRequest(tmpl, vals)
	want := map[string]any{"data": map[string]any{
		"values": []any{map[string]any{"id": "1"}},
		"key":    "k",
		"label":  "item k of s",
		"deep":   2,
		"fixed":  "items",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FillRequest: %v, want %v", got, want)
	}
	back := RequestValues(tmpl, got)
	wantBack := map[string]any{"value": map[string]any{"id": "1"}, "item_key": "k", "store": "s", "a": map[string]any{"b": 2}}
	if !reflect.DeepEqual(back, wantBack) {
		t.Fatalf("RequestValues: %v, want %v", back, wantBack)
	}
}
