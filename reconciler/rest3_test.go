// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// A compare rule inside a list's elements: GitLab's value stream stages
// take startEventLabelId and read back startEventLabel {id}.
func TestNormalizeInsideElements(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"stages[].startEventLabelId": descriptor.CompareSlugObject}}
	desired := map[string]any{"stages": []any{
		map[string]any{"name": "a", "startEventLabelId": "gid://gitlab/GroupLabel/1"},
		map[string]any{"name": "b"},
	}}
	observed := map[string]any{"stages": []any{
		map[string]any{"name": "a", "startEventLabelId": map[string]any{"id": "gid://gitlab/GroupLabel/1"}},
		map[string]any{"name": "b", "startEventLabelId": nil},
	}}
	if f := Drift(desired, Normalize(k, desired, observed), nil); f != "" {
		t.Fatalf("drift at %s", f)
	}
	observed["stages"].([]any)[0].(map[string]any)["startEventLabelId"] = map[string]any{"id": "gid://gitlab/GroupLabel/2"}
	if f := Drift(desired, Normalize(k, desired, observed), nil); f == "" {
		t.Fatal("another label: no drift")
	}
}

// A create that answers no ID (Cloudflare's asynchronous bulk add of a list
// item) finds its resource in the list by what the spec sets; before a
// create, a resource such a create made is found, not made again.
func TestIDFind(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "ListItem", Group: "list.example.hfl.dev", Version: "v1alpha1", Plural: "listitems",
		Shape:    descriptor.CRUD,
		Envelope: "result",
		Create:   &descriptor.Op{Method: "POST", Path: "/lists/{list_id}/items", BodyArray: true},
		Read:     descriptor.Op{Method: "GET", Path: "/lists/{list_id}/items/{id}"},
		Delete:   &descriptor.Op{Method: "DELETE", Path: "/lists/{list_id}/items", Body: map[string]any{"items": []any{map[string]any{"id": "{id}"}}}},
		Params:   []descriptor.Param{{Name: "list_id", Field: "listID"}},
		ID: &descriptor.ID{Param: "id", From: "id", Find: &descriptor.Adopt{
			Op:    descriptor.Op{Method: "GET", Path: "/lists/{list_id}/items", Query: map[string]string{"search": "{ip|asn}"}, Find: &descriptor.Find{List: "result"}},
			Match: map[string]string{"ip": "{ip}", "asn": "{asn}"},
		}},
	}
	var items []any
	creates, deleted := 0, ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == "POST" && req.URL.Path == "/lists/l1/items":
			creates++
			var body []map[string]any
			_ = json.NewDecoder(req.Body).Decode(&body)
			item := body[0]
			item["id"] = "i1"
			items = append(items, map[string]any{"id": "other", "ip": "192.0.2.1", "asn": float64(7)}, item)
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"operation_id": "op1"}})
		case req.Method == "GET" && req.URL.Path == "/lists/l1/items":
			if req.URL.Query().Get("search") != "192.0.2.1" {
				http.Error(w, "bad search", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"result": append([]any{}, items...)})
		case req.Method == "GET" && req.URL.Path == "/lists/l1/items/i1":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "i1", "ip": "192.0.2.1", "comment": "c"}})
		case req.Method == "DELETE" && req.URL.Path == "/lists/l1/items":
			var body map[string]any
			_ = json.NewDecoder(req.Body).Decode(&body)
			deleted = body["items"].([]any)[0].(map[string]any)["id"].(string)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"listID": "l1", "ip": "192.0.2.1", "comment": "c"})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != "i1" || creates != 1 {
		t.Fatalf("status.id = %q after %d creates, want i1 after 1", id, creates)
	}
	// Made by a create whose ID was lost (a status never written): found first.
	unstructured.RemoveNestedField(obj.Object, "status", "id")
	if err := c.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if err := c.Status().Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("resync: %v", err)
	}
	if creates != 1 {
		t.Fatalf("creates = %d, want 1: the item made is found", creates)
	}
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deleted != "i1" {
		t.Fatalf("deleted %q, want i1", deleted)
	}
}
