// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"reflect"
	"testing"
)

func TestWithoutFields(t *testing.T) {
	body := map[string]any{
		"type": "fastly-accounts",
		"attributes": map[string]any{
			"name":     "acct-1",
			"api_key":  "secret",
			"services": []any{},
		},
	}
	got := WithoutFields(body, []string{"attributes.name", "attributes.services", "missing.field"})
	want := map[string]any{
		"type": "fastly-accounts",
		"attributes": map[string]any{
			"api_key": "secret",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WithoutFields = %#v, want %#v", got, want)
	}
	// Original must not be mutated.
	attrs := body["attributes"].(map[string]any)
	if _, ok := attrs["name"]; !ok {
		t.Fatal("WithoutFields mutated original body")
	}
}

func TestPickField(t *testing.T) {
	body := map[string]any{
		"attributes": map[string]any{
			"name":    "acct-1",
			"api_key": "secret",
		},
	}
	got, ok := PickField(body, "attributes.name")
	if !ok {
		t.Fatal("PickField(attributes.name) = false, want true")
	}
	want := map[string]any{"attributes": map[string]any{"name": "acct-1"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PickField = %#v, want %#v", got, want)
	}
	if _, ok := PickField(body, "attributes.missing"); ok {
		t.Fatal("PickField(attributes.missing) = true, want false")
	}
	if _, ok := PickField(body, "attributes.name.deep"); ok {
		t.Fatal("PickField(attributes.name.deep) = true, want false")
	}
}
