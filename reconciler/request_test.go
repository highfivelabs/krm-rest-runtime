// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// Each call builds its whole body from the spec (Op.Request: Datadog's
// datastore item): a create of values: [value], an update of item_key and
// item_changes.ops_set, a delete of item_key; none is the spec's body, and
// the fake reads each back into the spec's fields.
func TestRequestBodies(t *testing.T) {
	k := descriptor.Kind{
		Kind: "DatastoreItem", Group: "datastore.example.hfl.dev", Version: "v1alpha1", Plural: "datastoreitems", Shape: descriptor.Upsert,
		Create: &descriptor.Op{Method: "POST", Path: "/stores/{datastore_id}/items/bulk", Request: map[string]any{
			"data": map[string]any{"type": "items", "attributes": map[string]any{"values": []any{"{value}"}, "conflict_mode": "fail_on_conflict", "note": "{note}"}}}},
		Read: descriptor.Op{Method: "GET", Path: "/stores/{datastore_id}/items", Query: map[string]string{"item_key": "{item_key}"}, Envelope: "data/0/attributes"},
		Update: &descriptor.Op{Method: "PATCH", Path: "/stores/{datastore_id}/items", Request: map[string]any{
			"data": map[string]any{"type": "items", "attributes": map[string]any{"item_key": "{item_key}", "item_changes": map[string]any{"ops_set": "{value}"}, "label": "item {item_key} of {datastore_id}"}}}},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/stores/{datastore_id}/items", Request: map[string]any{
			"data": map[string]any{"type": "items", "attributes": map[string]any{"item_key": "{item_key}"}}}},
		Params:   []descriptor.Param{{Name: "datastore_id", Field: "datastore_id"}, {Name: "item_key", Field: "item_key"}},
		Envelope: "data/attributes",
	}
	rec, srv := record(t, k)
	value := map[string]any{"id": "k1", "name": "a"}
	r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"datastore_id": "s1", "item_key": "k1", "value": value})
	obj := run(t, r, c, req)
	create := rec.last(t, "POST")
	want := map[string]any{"data": map[string]any{"type": "items", "attributes": map[string]any{"values": []any{value}, "conflict_mode": "fail_on_conflict"}}}
	if create.Path != "/stores/s1/items/bulk" || !reflect.DeepEqual(create.Body, want) {
		t.Fatalf("create: %s %v, want %v (an unset {note} left out)", create.Path, create.Body, want)
	}
	if get := rec.last(t, "GET"); get.Query["item_key"] != "k1" {
		t.Fatalf("read: %v %d", get.Query, get.Status)
	}
	changed := map[string]any{"id": "k1", "name": "b"}
	setSpec(t, c, obj, "value", changed)
	run(t, r, c, req)
	upd := rec.last(t, "PATCH")
	want = map[string]any{"data": map[string]any{"type": "items", "attributes": map[string]any{"item_key": "k1", "item_changes": map[string]any{"ops_set": changed}, "label": "item k1 of s1"}}}
	if !reflect.DeepEqual(upd.Body, want) {
		t.Fatalf("update: %v, want %v", upd.Body, want)
	}
	if got, _ := rec.api.Get("/stores/s1/items"); !reflect.DeepEqual(got["value"], changed) {
		t.Fatalf("the fake holds %v, want value %v", got, changed)
	}
	remove(t, r, c, req)
	del := rec.last(t, "DELETE")
	want = map[string]any{"data": map[string]any{"type": "items", "attributes": map[string]any{"item_key": "k1"}}}
	if !reflect.DeepEqual(del.Body, want) || del.Status != 204 {
		t.Fatalf("delete: %v %d, want %v", del.Body, del.Status, want)
	}
}

// A find read's envelope is where in the element found the object is
// (Datadog's managed organizations: included[].attributes), the create
// answering it in another shape (org.public_id).
func TestFindElementEnvelope(t *testing.T) {
	k := descriptor.Kind{
		Kind: "ChildOrganization", Group: "child.example.hfl.dev", Version: "v1alpha1", Plural: "childorganizations", Shape: descriptor.CRUD,
		Create: &descriptor.Op{Method: "POST", Path: "/api/v1/org"},
		Read: descriptor.Op{Method: "GET", Path: "/api/v2/org", Envelope: "attributes",
			Find: &descriptor.Find{List: "included", Field: "attributes.public_id", Param: "id"}},
		ID:     &descriptor.ID{Param: "id", From: "org/public_id"},
		Forget: "no delete",
	}
	rec, srv := record(t, k)
	r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"name": "child"})
	run(t, r, c, req)
	obj := run(t, r, c, req) // read back from the list: no drift on name
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id == "" {
		t.Fatalf("no status.id: %v", obj.Object["status"])
	}
	if got := rec.last(t, "GET"); got.Status != 200 {
		t.Fatalf("read: %d", got.Status)
	}
	if n := len(rec.calls); rec.calls[n-1].Method != "GET" {
		t.Fatalf("the second sync wrote: %+v", rec.calls[n-1])
	}
	remove(t, r, c, req)
}
