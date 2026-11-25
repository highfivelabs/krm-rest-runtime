// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import "testing"

// An adopting CRUD kind has no create, an update, a match and forget; carry
// is for an update or a delete; createOnly is for an upsert too.
func TestStateRules(t *testing.T) {
	head := "vendor: x\nconnection: {group: core.x.hfl.dev, version: v1alpha1, kind: Connection}\nkinds:\n"
	adopt := "- {kind: P, group: p.x.hfl.dev, version: v1alpha1, plural: ps, shape: crud, read: {method: GET, path: \"/p/{id}\"}, id: {param: id, from: id}, forget: made by the vendor"
	for doc, ok := range map[string]bool{
		adopt + ", update: {method: PUT, path: \"/p/{id}\"}, adopt: {method: GET, path: /p, match: {system: \"true\"}}}\n":                                                                                                                   true,
		adopt + ", adopt: {method: GET, path: /p, match: {system: \"true\"}}}\n":                                                                                                                                                             false, // no update
		adopt + ", update: {method: PUT, path: \"/p/{id}\"}, adopt: {method: GET, path: /p}}\n":                                                                                                                                              false, // no match
		adopt + ", update: {method: PUT, path: \"/p/{id}\"}, create: {method: POST, path: /p}, adopt: {method: GET, path: /p, match: {name: \"{name}\"}}}\n":                                                                                 false, // both
		"- {kind: U, group: u.x.hfl.dev, version: v1alpha1, plural: us, shape: upsert, read: {method: GET, path: /u, carry: [v]}, update: {method: PUT, path: /u}, delete: {method: DELETE, path: /u}}\n":                                    false,
		"- {kind: U, group: u.x.hfl.dev, version: v1alpha1, plural: us, shape: upsert, read: {method: GET, path: /u}, update: {method: PUT, path: /u, carry: [v]}, delete: {method: DELETE, path: /u, carry: [v]}, createOnly: [purpose]}\n": true,
	} {
		if _, err := Parse([]byte(head + doc)); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", doc, err, ok)
		}
	}
}

// A kind reads elsewhere when its update is on another path, or a write
// path names what the read path does not; a delete elsewhere alone is not.
func TestReadsElsewhere(t *testing.T) {
	for _, tc := range []struct {
		k    Kind
		want string
	}{
		{Kind{Shape: CRUD, Read: Op{Path: "/d/{d}/public"}, Update: &Op{Path: "/d/{d}/public/{uid}"}, Delete: &Op{Path: "/d/{d}/public/{uid}"}}, "/d/{d}/public/{uid}"},
		{Kind{Shape: Settings, Read: Op{Path: "/k/{name}"}, Update: &Op{Path: "/k/{name}/config"}}, "/k/{name}/config"},
		{Kind{Shape: CRUD, Read: Op{Path: "/users/{id}"}, Update: &Op{Path: "/users/{id}"}, Delete: &Op{Path: "/admin/users/{id}"}}, "/users/{id}"},
		{Kind{Shape: Upsert, Read: Op{Path: "/s", Find: &Find{Param: "s"}}, Update: &Op{Path: "/s/{s}"}}, "/s"},
	} {
		if got := tc.k.ItemPath(); got != tc.want {
			t.Errorf("%s: ItemPath %s, want %s", tc.k.Read.Path, got, tc.want)
		}
	}
}

// A find with no field matches a bare string in the list.
func TestFindBareName(t *testing.T) {
	k := &Kind{Read: Op{Find: &Find{List: "data/keys", Param: "scope"}}}
	resp := map[string]any{"data": map[string]any{"keys": []any{"a", "Payments"}}}
	if m, ok := k.FindIn(resp, "payments"); !ok || len(m) != 0 {
		t.Fatalf("FindIn = %v, %v; want an empty object", m, ok)
	}
	if _, ok := k.FindIn(resp, "b"); ok {
		t.Fatal("b is not listed")
	}
}
