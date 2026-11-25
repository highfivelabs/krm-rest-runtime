// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import "testing"

// A whole number is written as an integer, however large; the rest as is.
func TestText(t *testing.T) {
	for v, want := range map[any]string{float64(12345678): "12345678", float64(1000000): "1000000", float64(1.5): "1.5", "abc": "abc", int64(7): "7"} {
		if got := Text(v); got != want {
			t.Errorf("Text(%v) = %q, want %q", v, got, want)
		}
	}
	id := &ID{From: "{org}/{n}"}
	if got, _ := id.Compose(map[string]any{"org": "a", "n": float64(270000033)}); got != "a/270000033" {
		t.Errorf("Compose: %q", got)
	}
}

// A CRUD kind may have no delete only when it says why (forget), and only then.
func TestForget(t *testing.T) {
	base := "vendor: x\nconnection: {group: core.x.hfl.dev, version: v1alpha1, kind: Connection}\nkinds:\n- {kind: Job, group: j.x.hfl.dev, version: v1alpha1, plural: jobs, shape: crud, create: {method: POST, path: /jobs}, read: {method: GET, path: \"/jobs/{id}\"}, id: {param: id, from: id}"
	for doc, ok := range map[string]bool{
		base + ", forget: no delete}\n": true,
		base + "}\n":                    false,
		base + ", forget: no, delete: {method: DELETE, path: \"/jobs/{id}\"}}\n": false,
	} {
		if _, err := Parse([]byte(doc)); (err == nil) != ok {
			t.Errorf("%s: err %v, want ok=%v", doc, err, ok)
		}
	}
}

func TestValidateRequestShapes(t *testing.T) {
	crud := func() Kind {
		return Kind{Shape: CRUD, Create: &Op{Method: "POST", Path: "/a"}, Read: Op{Method: "GET", Path: "/a/{id}"},
			Delete: &Op{Method: "DELETE", Path: "/a/{id}"}, ID: &ID{Param: "id", From: "id"}}
	}
	both := crud()
	both.Delete = &Op{Method: "POST", Path: "/a/{id}", SendBody: true, Body: map[string]any{"x": 1}}
	env := crud()
	env.Create.BodyEnvelope = "add"
	qf := Kind{Shape: Upsert, Read: Op{Method: "GET", Path: "/a/{n}"}, Update: &Op{Method: "PUT", Path: "/a/{n}"},
		Delete: &Op{Method: "DELETE", Path: "/a/{n}"}, Params: []Param{{Name: "n", Field: "n"}}, QueryFields: []string{"n"}}
	ok := crud()
	ok.QueryFields = []string{"region"}
	ok.Delete.Outer = map[string]any{"meta": map[string]any{}}
	for name, k := range map[string]Kind{"delete body and sendBody": both, "bodyEnvelope off an assoc delete": env, "queryField that is a param": qf} {
		if err := k.validate(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if err := ok.validate(); err != nil {
		t.Errorf("valid kind: %v", err)
	}
}
