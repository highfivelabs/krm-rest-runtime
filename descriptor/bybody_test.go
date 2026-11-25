// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import "testing"

// A read that sends a body is a crud kind's POST naming {id}; a keyed find
// has a key of params in place of field and param.
func TestReadBodyAndKeyRules(t *testing.T) {
	head := "vendor: x\nconnection: {group: core.x.hfl.dev, version: v1alpha1, kind: Connection}\nkinds:\n"
	crud := "- {kind: C, group: c.x.hfl.dev, version: v1alpha1, plural: cs, shape: crud, id: {param: id, from: id}, create: {method: POST, path: /Create}, delete: {method: POST, path: /Delete, body: {id: \"{id}\"}, bareBody: true}"
	up := "- {kind: A, group: a.x.hfl.dev, version: v1alpha1, plural: as, shape: upsert, params: [{name: t, field: t}, {name: dest, field: dest}], create: {method: POST, path: \"/d/{t}/{dest}/set\"}, delete: {method: POST, path: \"/d/{t}/{dest}/remove\"}"
	for doc, ok := range map[string]bool{
		crud + ", read: {method: POST, path: /Get, body: {id: \"{id}\"}}}\n":                                true,
		crud + ", read: {method: GET, path: /Get, body: {id: \"{id}\"}}}\n":                                 false, // a GET
		crud + ", read: {method: POST, path: /Get, body: {name: x}}}\n":                                     false, // names no {id}
		up + ", read: {method: GET, path: /a, find: {list: data, key: \"{t}/{dest}\"}}}\n":                  true,
		up + ", read: {method: GET, path: /a, find: {list: data, key: \"{t}/{x}\"}}}\n":                     false, // no param x
		up + ", read: {method: GET, path: /a, find: {list: data, key: \"{t}\", field: id, param: dest}}}\n": false,
	} {
		if _, err := Parse([]byte(head + doc)); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", doc, err, ok)
		}
	}
}

func TestFindInKeyed(t *testing.T) {
	k := &Kind{Read: Op{Find: &Find{List: "data/associated_destinations", Key: "{type}/{name}"}}}
	want, ok := k.Read.Find.KeyIn(map[string]string{"type": "gh", "name": "dest"})
	if !ok || want != "gh/dest" {
		t.Fatalf("KeyIn = %q, %v", want, ok)
	}
	resp := map[string]any{"data": map[string]any{"associated_destinations": map[string]any{
		"aws-sm/dest": map[string]any{"type": "aws-sm"}, "gh/dest": map[string]any{"type": "gh", "sync_status": "SYNCED"}}}}
	m, found := k.FindIn(resp, want)
	if !found || m["type"] != "gh" {
		t.Fatalf("FindIn = %v, %v", m, found)
	}
	if _, found := k.FindIn(resp, "gh/other"); found {
		t.Fatal("found a key that is not there")
	}
}
