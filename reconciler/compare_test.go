// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"fmt"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

func TestCompareReturnedOnly(t *testing.T) {
	body := map[string]any{"ttl": "1h", "secret": "x"}
	obs := map[string]any{"ttl": "1h"} // the API does not echo secret

	strict := &Reconciler{Kind: &descriptor.Kind{}}
	if f := Drift(body, obs, strict.skip(obs)); f != "secret" {
		t.Fatalf("strict: drift %q, want secret", f)
	}
	lenient := &Reconciler{Kind: &descriptor.Kind{CompareReturnedOnly: true}}
	if f := Drift(body, obs, lenient.skip(obs)); f != "" {
		t.Fatalf("returned-only: drift %q, want none", f)
	}
	// ...but a field it does return is still compared,
	if f := Drift(map[string]any{"ttl": "2h"}, obs, lenient.skip(obs)); f != "ttl" {
		t.Fatalf("returned-only: drift %q, want ttl", f)
	}
	// and an unreturned field makes a changed spec write once.
	if !lenient.hasUnseen(body, obs) || lenient.hasUnseen(map[string]any{"ttl": "1h"}, obs) {
		t.Fatal("hasUnseen")
	}
}

func TestHoldsSpreadsListsAcrossElements(t *testing.T) {
	// Snowflake lists one grant per privilege, and uppercases names.
	list := []any{
		map[string]any{"privileges": []any{"USAGE"}, "securable_type": "DATABASE", "securable": map[string]any{"name": "DB1"}},
		map[string]any{"privileges": []any{"MONITOR"}, "securable_type": "DATABASE", "securable": map[string]any{"name": "DB1"}},
		map[string]any{"privileges": []any{"CREATE SCHEMA"}, "securable_type": "DATABASE", "securable": map[string]any{"name": "OTHER"}},
	}
	want := map[string]any{"privileges": []any{"usage", "MONITOR"}, "securable_type": "DATABASE", "securable": map[string]any{"name": "db1"}}
	if !Holds(list, want, nil) {
		t.Fatal("grant spread over two rows not found")
	}
	want["privileges"] = []any{"USAGE", "CREATE SCHEMA"} // CREATE SCHEMA is on another database
	if Holds(list, want, nil) {
		t.Fatal("privilege from another securable counted")
	}
}

// A duration sent as text and read back in seconds (Vault's TTLs) is the
// same value; a different one is drift.
func TestDurationsReadBackInSeconds(t *testing.T) {
	desired := map[string]any{"ttl": "1h", "max": "90s", "name": "1h"}
	if f := Drift(desired, map[string]any{"ttl": float64(3600), "max": int64(90), "name": "1h"}, nil); f != "" {
		t.Errorf("drift at %s", f)
	}
	if f := Drift(desired, map[string]any{"ttl": float64(7200), "max": int64(90), "name": "1h"}, nil); f != "ttl" {
		t.Errorf("drift at %q, want ttl", f)
	}
	if f := Drift(map[string]any{"name": "abc"}, map[string]any{"name": float64(3)}, nil); f != "name" {
		t.Errorf("drift at %q, want name", f)
	}
}

// A write-only field inside an object (Grafana's SSO settings.clientSecret,
// read back masked) is not drift; its siblings still are.
func TestNestedWriteOnly(t *testing.T) {
	r := &Reconciler{Kind: &descriptor.Kind{WriteOnly: []string{"settings.clientSecret"}}}
	body := map[string]any{"settings": map[string]any{"clientId": "a", "clientSecret": "s"}}
	obs := map[string]any{"settings": map[string]any{"clientId": "a", "clientSecret": "*********", "enabled": false}}
	if f := Drift(body, obs, r.skip(obs)); f != "" {
		t.Fatalf("drift %q, want none", f)
	}
	obs["settings"].(map[string]any)["clientId"] = "b"
	if f := Drift(body, obs, r.skip(obs)); f != "settings.clientId" {
		t.Fatalf("drift %q, want settings.clientId", f)
	}
	if !r.hasWriteOnly(body) || r.hasWriteOnly(map[string]any{"settings": map[string]any{"clientId": "a"}}) {
		t.Fatal("hasWriteOnly")
	}
}

func TestNormalizeSplitSpace(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"scopes": descriptor.CompareSplitSpace}}
	for _, tc := range []struct {
		desired, observed any
		same              bool
	}{
		{"api", []any{"api"}, true},
		{"api read_user", []any{"read_user", "api"}, true}, // any order
		{" api  read_user ", []any{"api", "read_user"}, true},
		{"api", []any{"api", "read_user"}, false},
		{"api read_user", []any{"api"}, false},
		{"api", "api", true}, // read back as sent: compared as usual
		{"api", []any{1.0}, false},
	} {
		body := map[string]any{"scopes": tc.desired, "name": "a"}
		obs := map[string]any{"scopes": tc.observed, "name": "a"}
		got := Drift(body, Normalize(k, body, obs), nil) == ""
		if got != tc.same {
			t.Errorf("%q vs %#v: same %v, want %v", tc.desired, tc.observed, got, tc.same)
		}
		if _, ok := obs["scopes"].([]any); ok && fmt.Sprint(obs["scopes"]) != fmt.Sprint(tc.observed) {
			t.Errorf("Normalize changed observed")
		}
	}
	// Without the rule a string never equals an array.
	if Drift(map[string]any{"scopes": "api"}, Normalize(&descriptor.Kind{}, nil, map[string]any{"scopes": []any{"api"}}), nil) == "" {
		t.Error("no rule: want drift")
	}
}

func TestNormalizeEnabledObject(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"enforce_admins": descriptor.CompareEnabledObject}}
	for _, tc := range []struct {
		desired, observed any
		same              bool
	}{
		{true, map[string]any{"url": "u", "enabled": true}, true},
		{false, map[string]any{"enabled": false}, true},
		{true, map[string]any{"enabled": false}, false},
		{true, map[string]any{"url": "u"}, false},
		{true, true, true}, // read back as sent: compared as usual
		{true, nil, false},
	} {
		body := map[string]any{"enforce_admins": tc.desired}
		obs := map[string]any{"enforce_admins": tc.observed}
		if got := Drift(body, Normalize(k, body, obs), nil) == ""; got != tc.same {
			t.Errorf("%v vs %#v: same %v, want %v", tc.desired, tc.observed, got, tc.same)
		}
	}
}

func TestNormalizeSlugObject(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"project": descriptor.CompareSlugObject}}
	for _, tc := range []struct {
		desired, observed any
		same              bool
	}{
		{"web", map[string]any{"id": "4512", "slug": "web", "name": "Web"}, true},
		{"4512", map[string]any{"id": "4512", "slug": "web"}, true},
		{"4512", map[string]any{"id": float64(4512), "slug": "web"}, true},
		{"web", map[string]any{"id": "4512", "slug": "api"}, false},
		{"web", map[string]any{"name": "web"}, false},
		{"", map[string]any{"slug": ""}, false},
		{"web", "web", true}, // read back as sent: compared as usual
		{"web", nil, false},
	} {
		body := map[string]any{"project": tc.desired}
		obs := map[string]any{"project": tc.observed}
		if got := Drift(body, Normalize(k, body, obs), nil) == ""; got != tc.same {
			t.Errorf("%v vs %#v: same %v, want %v", tc.desired, tc.observed, got, tc.same)
		}
	}
}

func TestIDString(t *testing.T) {
	for v, want := range map[any]string{
		float64(270123456): "270123456",
		float64(42):        "42",
		int64(270123456):   "270123456",
		"abc":              "abc",
		1.5:                "1.5",
	} {
		if got := IDString(v); got != want {
			t.Errorf("IDString(%#v) = %q, want %q", v, got, want)
		}
	}
}

func TestNormalizeEndsWith(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"query": descriptor.CompareEndsWith}}
	for _, tc := range []struct {
		desired, observed any
		same              bool
	}{
		{"SELECT 1", "CREATE VIEW V AS SELECT 1", true},
		{"SELECT  ID\nFROM T", "CREATE     VIEW  D.S.V  (  ID  )  AS SELECT ID FROM T", true},
		{"SELECT 1", "SELECT 1", true},
		{"SELECT 1", "CREATE VIEW V AS SELECT 2", false},
		{"1", "CREATE VIEW V AS SELECT 11", false}, // a whole word, not part of one
		{"", "CREATE VIEW V AS SELECT 1", false},
	} {
		body := map[string]any{"query": tc.desired}
		obs := map[string]any{"query": tc.observed}
		if got := Drift(body, Normalize(k, body, obs), nil) == ""; got != tc.same {
			t.Errorf("%q vs %q: same %v, want %v", tc.desired, tc.observed, got, tc.same)
		}
	}
}

func TestNormalizeCaseInsensitive(t *testing.T) {
	k := &descriptor.Kind{CaseInsensitive: []string{"name", "warehouse"}}
	body := map[string]any{"name": "hfl_a", "warehouse": "wh", "comment": "x"}
	obs := map[string]any{"name": "HFL_A", "warehouse": "WH", "comment": "x"}
	if f := Drift(body, Normalize(k, body, obs), nil); f != "" {
		t.Errorf("drift in %s", f)
	}
	if obs["name"] != "HFL_A" {
		t.Error("Normalize changed observed")
	}
	obs["comment"] = "X"
	if f := Drift(body, Normalize(k, body, obs), nil); f != "comment" {
		t.Errorf("drift in %q, want comment (not case-insensitive)", f)
	}
}

// A time read back in another zone than written is the same instant
// (PagerDuty answers in the account's zone).
func TestSameInstantInAnotherZone(t *testing.T) {
	desired := map[string]any{"start_time": "2026-12-16T00:00:00Z", "layers": []any{map[string]any{"start": "2026-12-16T00:00:00Z"}}}
	obs := map[string]any{"start_time": "2026-12-15T18:00:00-06:00", "layers": []any{map[string]any{"start": "2026-12-15T18:00:00-06:00"}}}
	if f := Drift(desired, obs, nil); f != "" {
		t.Errorf("drift at %s", f)
	}
	obs["start_time"] = "2026-12-15T19:00:00-06:00"
	if f := Drift(desired, obs, nil); f != "start_time" {
		t.Errorf("drift at %q, want start_time", f)
	}
	if f := Drift(map[string]any{"name": "2026-12-16"}, map[string]any{"name": "2026-12-16T00:00:00Z"}, nil); f != "name" {
		t.Errorf("a date is not an instant: drift at %q, want name", f)
	}
}

func TestNormalizeUnordered(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"layers": descriptor.CompareUnordered}}
	a := map[string]any{"name": "a"}
	b := map[string]any{"name": "b"}
	readA := map[string]any{"name": "a", "id": "P1"}
	readB := map[string]any{"name": "b", "id": "P2"}
	for _, tc := range []struct {
		desired, observed []any
		same              bool
	}{
		{[]any{a, b}, []any{readB, readA}, true},
		{[]any{a, b}, []any{readA, readB}, true},
		{[]any{a, a}, []any{readA, readB}, false}, // each read element once
		{[]any{a}, []any{readA, readB}, false},
		{[]any{a, b}, []any{readA}, false},
	} {
		body := map[string]any{"layers": tc.desired}
		obs := map[string]any{"layers": tc.observed}
		if got := Drift(body, Normalize(k, body, obs), nil) == ""; got != tc.same {
			t.Errorf("%v vs %v: same %v, want %v", tc.desired, tc.observed, got, tc.same)
		}
	}
}

func TestCompareNodeIDs(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"ids": descriptor.CompareNodeIDs}}
	desired := map[string]any{"ids": []any{"gid://x/1", "gid://x/2"}}
	same := map[string]any{"ids": map[string]any{"nodes": []any{map[string]any{"id": "gid://x/2"}, map[string]any{"id": "gid://x/1"}}}}
	if f := Drift(desired, Normalize(k, desired, same), nil); f != "" {
		t.Errorf("the same IDs in another order: drift %q", f)
	}
	other := map[string]any{"ids": map[string]any{"nodes": []any{map[string]any{"id": "gid://x/1"}}}}
	if f := Drift(desired, Normalize(k, desired, other), nil); f == "" {
		t.Error("one ID missing: no drift")
	}
}
