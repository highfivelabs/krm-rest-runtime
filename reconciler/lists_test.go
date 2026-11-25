// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// orderAPI is an API that keeps an order of objects: GET lists them by
// position (answered last first), the reorder call takes the IDs in order.
type orderAPI struct {
	mu    sync.Mutex
	ids   []string
	sent  []map[string]any
	extra map[string]any // the read's other fields
}

func (a *orderAPI) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	switch req.Method {
	case http.MethodGet:
		var data []any
		for i := len(a.ids) - 1; i >= 0; i-- {
			data = append(data, map[string]any{"id": a.ids[i], "type": "rule", "attributes": map[string]any{"position": float64(i + 1)}})
		}
		out := map[string]any{"data": data}
		for f, v := range a.extra {
			out[f] = v
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		var body map[string]any
		b, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(b, &body)
		a.sent = append(a.sent, body)
		a.ids = nil
		for _, e := range body["data"].([]any) {
			a.ids = append(a.ids, e.(map[string]any)["id"].(string))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func orderKind(partial bool) *descriptor.Kind {
	return &descriptor.Kind{
		Kind: "RuleOrder", Group: "rule.example.hfl.dev", Version: "v1alpha1", Plural: "ruleorders",
		Shape: descriptor.Settings,
		Read:  descriptor.Op{Method: "GET", Path: "/rules"},
		Update: &descriptor.Op{Method: "POST", Path: "/rules/reorder", Body: map[string]any{"meta": map[string]any{"config": "{current.meta.id}"}},
			Carry: []string{"meta.version"}},
		Order: &descriptor.Order{Field: "rule_ids", List: "data", SortBy: "attributes.position", At: "data",
			Item: map[string]any{"type": "rule"}, Partial: partial},
	}
}

func TestOrderWritesTheWholeOrder(t *testing.T) {
	api := &orderAPI{ids: []string{"a", "b", "c"}, extra: map[string]any{"meta": map[string]any{"id": "cfg-1", "version": float64(3)}}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	r, c, req := newTestReconciler(t, orderKind(false), srv.URL, map[string]any{"rule_ids": []any{"c", "a", "b"}})
	obj := run(t, r, c, req)
	if len(api.sent) != 1 {
		t.Fatalf("sent %d reorders, want 1", len(api.sent))
	}
	want := map[string]any{
		"data": []any{map[string]any{"id": "c", "type": "rule"}, map[string]any{"id": "a", "type": "rule"}, map[string]any{"id": "b", "type": "rule"}},
		"meta": map[string]any{"config": "cfg-1", "version": float64(3)}, // the call's own body and what it carries, from the read
	}
	if !reflect.DeepEqual(api.sent[0], want) {
		t.Fatalf("reorder sent %v, want %v", api.sent[0], want)
	}
	if got, _, _ := unstructured.NestedSlice(obj.Object, "status", "atProvider", "rule_ids"); !reflect.DeepEqual(got, []any{"c", "a", "b"}) {
		t.Fatalf("atProvider.rule_ids = %v", got)
	}
	run(t, r, c, req) // read back by position: nothing to send
	if len(api.sent) != 1 {
		t.Fatalf("the order read back by position was not the same: sent %d reorders", len(api.sent))
	}
	api.ids = []string{"a", "b", "c"} // changed in the console
	run(t, r, c, req)
	if len(api.sent) != 2 || !reflect.DeepEqual(api.ids, []string{"c", "a", "b"}) {
		t.Fatalf("drift not repaired: %v", api.ids)
	}
	remove(t, r, c, req) // an order is never deleted
	if len(api.sent) != 2 {
		t.Fatalf("delete sent a reorder")
	}
}

func TestOrderPartialKeepsTheOthersAfter(t *testing.T) {
	api := &orderAPI{ids: []string{"ui-1", "b", "ui-2", "a"}, extra: map[string]any{"meta": map[string]any{"id": "x"}}}
	srv := httptest.NewServer(api)
	defer srv.Close()
	r, c, req := newTestReconciler(t, orderKind(true), srv.URL, map[string]any{"rule_ids": []any{"a", "b"}})
	run(t, r, c, req)
	if want := []string{"a", "b", "ui-1", "ui-2"}; !reflect.DeepEqual(api.ids, want) {
		t.Fatalf("order is %v, want %v", api.ids, want)
	}
	api.ids = []string{"a", "ui-1", "b", "ui-2"} // the spec's two in order, others between: the same
	run(t, r, c, req)
	if len(api.sent) != 1 {
		t.Fatalf("a partial order compared the others too: sent %d reorders", len(api.sent))
	}
}

func TestCompareSubsetByID(t *testing.T) {
	k := &descriptor.Kind{Compare: map[string]string{"headers": descriptor.CompareSubsetByID}}
	desired := map[string]any{"headers": []any{map[string]any{"id": "b", "enabled": true}, map[string]any{"id": "a", "enabled": false}}}
	observed := map[string]any{"headers": []any{
		map[string]any{"id": "a", "enabled": false, "has_conflict": false},
		map[string]any{"id": "c", "enabled": true},
		map[string]any{"id": "b", "enabled": true},
	}}
	if f := Drift(desired, Normalize(k, desired, observed), nil); f != "" {
		t.Fatalf("a subset by id, in another order, drifted at %s", f)
	}
	observed["headers"].([]any)[2].(map[string]any)["enabled"] = false
	if f := Drift(desired, Normalize(k, desired, observed), nil); f != "headers" {
		t.Fatalf("a changed element: drift at %q, want headers", f)
	}
	missing := map[string]any{"headers": []any{map[string]any{"id": "a", "enabled": false}}}
	if f := Drift(desired, Normalize(k, desired, missing), nil); f == "" {
		t.Fatal("an element the API does not have is not drift")
	}
}

func TestBulkCreateAndListRead(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "Secret", Group: "secrets.example.hfl.dev", Version: "v1alpha1", Plural: "secrets",
		Shape:    descriptor.CRUD,
		Envelope: "result",
		Create:   &descriptor.Op{Method: "POST", Path: "/stores/{store_id}/secrets", BodyArray: true, Envelope: "result/0"},
		Read:     descriptor.Op{Method: "GET", Path: "/stores/{store_id}/secrets/{secret_id}", Envelope: "result/0"},
		Update:   &descriptor.Op{Method: "PATCH", Path: "/stores/{store_id}/secrets/{secret_id}"},
		Delete:   &descriptor.Op{Method: "DELETE", Path: "/stores/{store_id}/secrets/{secret_id}"},
		Params:   []descriptor.Param{{Name: "store_id", Field: "storeID"}},
		ID:       &descriptor.ID{Param: "secret_id", From: "id"},
	}
	rec, srv := record(t, *k)
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"storeID": "s1", "name": "KEY"})
	obj := run(t, r, c, req)
	post := rec.last(t, "POST")
	if !reflect.DeepEqual(post.Body, []any{map[string]any{"name": "KEY"}}) {
		t.Fatalf("create sent %v, want a one-element array", post.Body)
	}
	id, _, _ := unstructured.NestedString(obj.Object, "status", "id")
	if id == "" {
		t.Fatal("no ID taken from the answer's first element")
	}
	setSpec(t, c, obj, "name", "OTHER")
	run(t, r, c, req)
	if patch := rec.last(t, "PATCH"); !reflect.DeepEqual(patch.Body, map[string]any{"name": "OTHER"}) {
		t.Fatalf("update sent %v, want the object", patch.Body)
	}
	remove(t, r, c, req)
}

func TestRawRead(t *testing.T) {
	var mu sync.Mutex
	value := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch req.Method {
		case http.MethodGet:
			if value == "" {
				http.NotFound(w, req)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte(value))
		case http.MethodPut:
			if !strings.HasPrefix(req.Header.Get("Content-Type"), "multipart/form-data") {
				http.Error(w, "want a form", http.StatusBadRequest)
				return
			}
			value = req.FormValue("value")
			_, _ = w.Write([]byte(`{"success": true}`))
		case http.MethodDelete:
			value = ""
		}
	}))
	defer srv.Close()
	k := &descriptor.Kind{
		Kind: "Value", Group: "kv.example.hfl.dev", Version: "v1alpha1", Plural: "values",
		Shape:     descriptor.Upsert,
		Read:      descriptor.Op{Method: "GET", Path: "/values/{key}", Raw: "value"},
		Update:    &descriptor.Op{Method: "PUT", Path: "/values/{key}"},
		Delete:    &descriptor.Op{Method: "DELETE", Path: "/values/{key}"},
		Params:    []descriptor.Param{{Name: "key", Field: "keyName"}},
		Multipart: &descriptor.Multipart{},
	}
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"keyName": "k", "value": "{not json"})
	obj := run(t, r, c, req)
	if value != "{not json" {
		t.Fatalf("stored %q", value)
	}
	if got, _, _ := unstructured.NestedString(obj.Object, "status", "atProvider", "value"); got != "{not json" {
		t.Fatalf("read back %q", got)
	}
	value = "changed"
	run(t, r, c, req)
	if value != "{not json" {
		t.Fatalf("drift not repaired: %q", value)
	}
}

func TestMultipartOnCreateOnly(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "LoadTest", Group: "k6.example.hfl.dev", Version: "v1alpha1", Plural: "loadtests",
		Shape:      descriptor.CRUD,
		Create:     &descriptor.Op{Method: "POST", Path: "/projects/{project_id}/load_tests"},
		Read:       descriptor.Op{Method: "GET", Path: "/load_tests/{id}"},
		Update:     &descriptor.Op{Method: "PATCH", Path: "/load_tests/{id}"},
		Delete:     &descriptor.Op{Method: "DELETE", Path: "/load_tests/{id}"},
		Params:     []descriptor.Param{{Name: "project_id", Field: "projectID"}},
		ID:         &descriptor.ID{Param: "id", From: "id"},
		WriteOnly:  []string{"script"},
		CreateOnly: []string{"script"},
		Multipart:  &descriptor.Multipart{Files: []string{"script"}, Calls: []string{"create"}},
	}
	rec, srv := record(t, *k)
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"projectID": "1", "name": "t", "script": map[string]any{"name": "s.js", "content": "export default function() {}"}})
	obj := run(t, r, c, req)
	if ct := rec.last(t, "POST").Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data") {
		t.Fatalf("create sent %s, want a multipart form", ct)
	}
	setSpec(t, c, obj, "name", "u")
	run(t, r, c, req)
	patch := rec.last(t, "PATCH")
	if ct := patch.Header.Get("Content-Type"); ct != "application/json" || !reflect.DeepEqual(patch.Body, map[string]any{"name": "u"}) {
		t.Fatalf("update sent %s %v, want JSON {name: u}", ct, patch.Body)
	}
}

func TestNestedParamFromTheBody(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "Forecast", Group: "cost.example.hfl.dev", Version: "v1alpha1", Plural: "forecasts",
		Shape:        descriptor.Upsert,
		Envelope:     "data",
		BodyEnvelope: "data",
		Read:         descriptor.Op{Method: "GET", Path: "/budget/{budget_id}/forecast"},
		Update:       &descriptor.Op{Method: "PUT", Path: "/budget/forecast"},
		Delete:       &descriptor.Op{Method: "DELETE", Path: "/budget/{budget_id}/forecast"},
		Params:       []descriptor.Param{{Name: "budget_id", Field: "attributes.budget_uid", InBody: true}},
	}
	rec, srv := record(t, *k)
	spec := map[string]any{"type": "forecast", "attributes": map[string]any{"budget_uid": "b1", "entries": []any{}}}
	r, c, req := newTestReconciler(t, k, srv.URL, spec)
	run(t, r, c, req)
	if put := rec.last(t, "PUT"); put.Path != "/budget/forecast" {
		t.Fatalf("written at %s", put.Path)
	}
	run(t, r, c, req)
	if get := rec.last(t, "GET"); get.Path != "/budget/b1/forecast" || get.Status != http.StatusOK {
		t.Fatalf("read at %s: %d", get.Path, get.Status)
	}
	remove(t, r, c, req)
	if del := rec.last(t, "DELETE"); del.Path != "/budget/b1/forecast" || del.Status != http.StatusNoContent {
		t.Fatalf("deleted at %s: %d", del.Path, del.Status)
	}
}
