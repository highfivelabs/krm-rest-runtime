// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
)

// sent is one request the fake API answered.
type sent struct {
	Method, Path string
	Query        map[string]string
	Header       http.Header
	Body         any
	Status       int
}

// recorder serves the fake API for k and keeps every request.
type recorder struct {
	mu    sync.Mutex
	calls []sent
	api   *apifake.Server
}

func record(t *testing.T, kinds ...descriptor.Kind) (*recorder, *httptest.Server) {
	t.Helper()
	rec := &recorder{api: apifake.New(&descriptor.File{Kinds: kinds})}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		data, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(strings.NewReader(string(data)))
		var body any
		_ = json.Unmarshal(data, &body)
		q := map[string]string{}
		for k, v := range req.URL.Query() {
			q[k] = v[0]
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		rec.api.ServeHTTP(sw, req)
		rec.mu.Lock()
		rec.calls = append(rec.calls, sent{req.Method, req.URL.Path, q, req.Header.Clone(), body, sw.status})
		rec.mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(s int) { w.status = s; w.ResponseWriter.WriteHeader(s) }

// last is the last call of method, failing the test when there is none.
func (rec *recorder) last(t *testing.T, method string) sent {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i := len(rec.calls) - 1; i >= 0; i-- {
		if rec.calls[i].Method == method {
			return rec.calls[i]
		}
	}
	t.Fatalf("no %s call; calls: %+v", method, rec.calls)
	return sent{}
}

// run reconciles the object and fails the test unless it is Ready.
func run(t *testing.T, r *Reconciler, c client.Client, req ctrl.Request) *unstructured.Unstructured {
	t.Helper()
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.Kind.GVK())
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if len(conds) == 0 || conds[0].(map[string]any)["status"] != "True" {
		t.Fatalf("not Ready: %v", conds)
	}
	return obj
}

// setSpec changes one spec field of the object.
func setSpec(t *testing.T, c client.Client, obj *unstructured.Unstructured, field string, v any) {
	t.Helper()
	_ = unstructured.SetNestedField(obj.Object, v, "spec", field)
	if err := c.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
}

// remove deletes the object and reconciles its finalizer away.
func remove(t *testing.T, r *Reconciler, c client.Client, req ctrl.Request) {
	t.Helper()
	ctx := context.Background()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.Kind.GVK())
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Get(ctx, req.NamespacedName, obj); err == nil {
		t.Fatal("the object is still there: its finalizer was not removed")
	}
}

// A query field fills ?region= on every call and is never in the body.
func TestQueryFields(t *testing.T) {
	q := map[string]string{"region": "{region}"}
	k := descriptor.Kind{
		Kind: "AccessPolicy", Group: "cloud.example.hfl.dev", Version: "v1alpha1", Plural: "accesspolicies", Shape: descriptor.CRUD,
		Create:      &descriptor.Op{Method: "POST", Path: "/v1/accesspolicies", Query: q},
		Read:        descriptor.Op{Method: "GET", Path: "/v1/accesspolicies/{id}", Query: q},
		Update:      &descriptor.Op{Method: "POST", Path: "/v1/accesspolicies/{id}", Query: q},
		Delete:      &descriptor.Op{Method: "DELETE", Path: "/v1/accesspolicies/{id}", Query: q},
		ID:          &descriptor.ID{Param: "id", From: "id"},
		QueryFields: []string{"region"},
	}
	rec, srv := record(t, k)
	r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"region": "us", "name": "a"})
	obj := run(t, r, c, req)
	create := rec.last(t, "POST")
	if create.Query["region"] != "us" || !reflect.DeepEqual(create.Body, map[string]any{"name": "a"}) {
		t.Fatalf("create: query %v body %v, want ?region=us and no region in the body", create.Query, create.Body)
	}
	setSpec(t, c, obj, "name", "b")
	run(t, r, c, req)
	if upd := rec.last(t, "POST"); upd.Path != "/v1/accesspolicies/270000001" || upd.Query["region"] != "us" {
		t.Fatalf("update: %s %v", upd.Path, upd.Query)
	}
	remove(t, r, c, req)
	if del := rec.last(t, "DELETE"); del.Query["region"] != "us" || del.Status != http.StatusNoContent {
		t.Fatalf("delete: %v %d", del.Query, del.Status)
	}
}

// An update and a delete name the scope the object has now, not the one
// the spec asks for (Grafana Assistant's X-Resource-Scope).
func TestCurrentHeader(t *testing.T) {
	h := map[string]string{"X-Resource-Scope": "{current.scope}"}
	k := descriptor.Kind{
		Kind: "Rule", Group: "assistant.example.hfl.dev", Version: "v1alpha1", Plural: "rules", Shape: descriptor.CRUD,
		Create: &descriptor.Op{Method: "POST", Path: "/rules"},
		Read:   descriptor.Op{Method: "GET", Path: "/rules/{id}"},
		Update: &descriptor.Op{Method: "PUT", Path: "/rules/{id}", Headers: h},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/rules/{id}", Headers: h},
		ID:     &descriptor.ID{Param: "id", From: "id"},
	}
	rec, srv := record(t, k)
	r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"scope": "user", "name": "a"})
	obj := run(t, r, c, req)
	if got := rec.last(t, "POST").Header.Get("X-Resource-Scope"); got != "" {
		t.Fatalf("create sent X-Resource-Scope %q; it has none", got)
	}
	setSpec(t, c, obj, "scope", "tenant")
	run(t, r, c, req)
	upd := rec.last(t, "PUT")
	if got := upd.Header.Get("X-Resource-Scope"); got != "user" || upd.Body.(map[string]any)["scope"] != "tenant" {
		t.Fatalf("update: header %q body %v, want the current scope user in the header and tenant in the body", got, upd.Body)
	}
	remove(t, r, c, req)
	if del := rec.last(t, "DELETE"); del.Header.Get("X-Resource-Scope") != "tenant" || del.Status != http.StatusNoContent {
		t.Fatalf("delete: header %q status %d", del.Header.Get("X-Resource-Scope"), del.Status)
	}
}

// Fields beside the envelope on every write, and as the whole body of the
// DELETE (Datadog's {"meta": {}}).
func TestOuterBody(t *testing.T) {
	meta := map[string]any{"meta": map[string]any{}}
	k := descriptor.Kind{
		Kind: "Group", Group: "sds.example.hfl.dev", Version: "v1alpha1", Plural: "groups", Shape: descriptor.CRUD,
		Create:       &descriptor.Op{Method: "POST", Path: "/groups", Outer: meta},
		Read:         descriptor.Op{Method: "GET", Path: "/groups/{id}"},
		Update:       &descriptor.Op{Method: "PATCH", Path: "/groups/{id}", Outer: meta},
		Delete:       &descriptor.Op{Method: "DELETE", Path: "/groups/{id}", Outer: meta},
		ID:           &descriptor.ID{Param: "id", From: "id"},
		Envelope:     "data",
		BodyEnvelope: "data",
	}
	rec, srv := record(t, k)
	r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"name": "a"})
	run(t, r, c, req)
	want := map[string]any{"data": map[string]any{"name": "a"}, "meta": map[string]any{}}
	if got := rec.last(t, "POST").Body; !reflect.DeepEqual(got, want) {
		t.Fatalf("create body %v, want %v", got, want)
	}
	remove(t, r, c, req)
	if del := rec.last(t, "DELETE"); !reflect.DeepEqual(del.Body, meta) || del.Status != http.StatusNoContent {
		t.Fatalf("delete body %v status %d, want %v", del.Body, del.Status, meta)
	}
}

// Deletes that are not a bare DELETE: a POST of a fixed body to the path
// the writes use (Grafana's {"permission": ""}); a POST elsewhere naming the
// item in its body (Datadog's bulk delete); a DELETE that sends the spec.
func TestDeleteBodies(t *testing.T) {
	t.Run("post of a fixed body", func(t *testing.T) {
		k := descriptor.Kind{
			Kind: "PermissionItem", Group: "perm.example.hfl.dev", Version: "v1alpha1", Plural: "permissionitems", Shape: descriptor.Upsert,
			Read:   descriptor.Op{Method: "GET", Path: "/access/{resource}", Find: &descriptor.Find{Field: "userId", Param: "user"}},
			Update: &descriptor.Op{Method: "POST", Path: "/access/{resource}/users/{user}"},
			Delete: &descriptor.Op{Method: "POST", Path: "/access/{resource}/users/{user}", Body: map[string]any{"permission": ""}},
			Params: []descriptor.Param{{Name: "resource", Field: "resource"}, {Name: "user", Field: "user"}},
		}
		rec, srv := record(t, k)
		r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"resource": "d1", "user": "7", "permission": "View"})
		run(t, r, c, req)
		if rec.api.Len() != 1 {
			t.Fatalf("fake holds %d objects, want 1", rec.api.Len())
		}
		remove(t, r, c, req)
		if got := rec.last(t, "POST").Body; !reflect.DeepEqual(got, map[string]any{"permission": ""}) {
			t.Fatalf("delete body %v", got)
		}
		if rec.api.Len() != 0 {
			t.Fatalf("the permission is still there: %v", rec.api.Paths())
		}
	})
	t.Run("post elsewhere naming the item", func(t *testing.T) {
		k := descriptor.Kind{
			Kind: "Suite", Group: "synthetics.example.hfl.dev", Version: "v1alpha1", Plural: "suites", Shape: descriptor.CRUD,
			Create: &descriptor.Op{Method: "POST", Path: "/suites"},
			Read:   descriptor.Op{Method: "GET", Path: "/suites/{public_id}"},
			Delete: &descriptor.Op{Method: "POST", Path: "/suites/bulk-delete", Body: map[string]any{
				"type": "delete_suites_request", "attributes": map[string]any{"public_ids": []any{"{public_id}"}}}},
			ID:           &descriptor.ID{Param: "public_id", From: "id"},
			Envelope:     "data",
			BodyEnvelope: "data",
		}
		rec, srv := record(t, k)
		r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"name": "a"})
		run(t, r, c, req)
		remove(t, r, c, req)
		want := map[string]any{"data": map[string]any{"type": "delete_suites_request", "attributes": map[string]any{"public_ids": []any{"270000001"}}}}
		if del := rec.last(t, "POST"); del.Path != "/suites/bulk-delete" || !reflect.DeepEqual(del.Body, want) {
			t.Fatalf("delete: %s %v, want %v", del.Path, del.Body, want)
		}
		if rec.api.Len() != 0 {
			t.Fatalf("the suite is still there: %v", rec.api.Paths())
		}
	})
	t.Run("delete that sends the spec", func(t *testing.T) {
		k := descriptor.Kind{
			Kind: "Thresholds", Group: "asserts.example.hfl.dev", Version: "v1alpha1", Plural: "thresholds", Shape: descriptor.Upsert,
			Read:   descriptor.Op{Method: "GET", Path: "/thresholds"},
			Update: &descriptor.Op{Method: "POST", Path: "/thresholds"},
			Delete: &descriptor.Op{Method: "DELETE", Path: "/thresholds", SendBody: true},
		}
		rec, srv := record(t, k)
		spec := map[string]any{"requestThresholds": []any{map[string]any{"value": "1"}}}
		r, c, req := newTestReconciler(t, &k, srv.URL, spec)
		run(t, r, c, req)
		remove(t, r, c, req)
		if del := rec.last(t, "DELETE"); !reflect.DeepEqual(del.Body, spec) {
			t.Fatalf("delete body %v, want the spec %v", del.Body, spec)
		}
	})
}

// A link undone by a query naming its fields, with no body (Grafana's
// DELETE /teams/{teamId}/groups?groupId=); and one undone in another
// envelope than it is made in (PagerDuty's {"add": [...]}, {"remove": [...]}).
func TestAssocUndo(t *testing.T) {
	t.Run("by query", func(t *testing.T) {
		k := descriptor.Kind{
			Kind: "TeamGroup", Group: "team.example.hfl.dev", Version: "v1alpha1", Plural: "teamgroups", Shape: descriptor.Assoc,
			Create: &descriptor.Op{Method: "POST", Path: "/teams/{teamId}/groups"},
			Read:   descriptor.Op{Method: "GET", Path: "/teams/{teamId}/groups"},
			Delete: &descriptor.Op{Method: "DELETE", Path: "/teams/{teamId}/groups", Query: map[string]string{"groupId": "{groupId}"}},
			Params: []descriptor.Param{{Name: "teamId", Field: "teamId"}},
		}
		rec, srv := record(t, k)
		r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"teamId": "1", "groupId": "g1"})
		obj := run(t, r, c, req)
		setSpec(t, c, obj, "groupId", "g2") // the old link is undone, the new one made
		run(t, r, c, req)
		if del := rec.last(t, "DELETE"); del.Query["groupId"] != "g1" || del.Body != nil {
			t.Fatalf("undo: query %v body %v, want ?groupId=g1 and no body", del.Query, del.Body)
		}
		remove(t, r, c, req)
		if del := rec.last(t, "DELETE"); del.Query["groupId"] != "g2" {
			t.Fatalf("delete: query %v, want ?groupId=g2", del.Query)
		}
		if rec.api.Len() != 0 {
			t.Fatalf("links remain: %v", rec.api.Paths())
		}
	})
	t.Run("in another envelope", func(t *testing.T) {
		k := descriptor.Kind{
			Kind: "TagAssignment", Group: "tag.example.hfl.dev", Version: "v1alpha1", Plural: "tagassignments", Shape: descriptor.Assoc,
			Create:       &descriptor.Op{Method: "POST", Path: "/{entity_type}/{id}/change_tags"},
			Read:         descriptor.Op{Method: "GET", Path: "/{entity_type}/{id}/tags", Find: &descriptor.Find{List: "tags"}},
			Delete:       &descriptor.Op{Method: "POST", Path: "/{entity_type}/{id}/change_tags", SendBody: true, BodyEnvelope: "remove"},
			Params:       []descriptor.Param{{Name: "entity_type", Field: "entityType"}, {Name: "id", Field: "entityID"}},
			BodyEnvelope: "add",
			BodyArray:    true,
			WriteOnly:    []string{"type"}, // read back as tag, written as tag_reference
		}
		rec, srv := record(t, k)
		r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"entityType": "teams", "entityID": "T1", "type": "tag_reference", "id": "P1"})
		run(t, r, c, req)
		remove(t, r, c, req)
		want := map[string]any{"remove": []any{map[string]any{"type": "tag_reference", "id": "P1"}}}
		if got := rec.last(t, "POST").Body; !reflect.DeepEqual(got, want) {
			t.Fatalf("undo body %v, want %v", got, want)
		}
		if rec.api.Len() != 0 {
			t.Fatalf("links remain: %v", rec.api.Paths())
		}
	})
}

// Written on another path than the item's, the ID in the body: an update
// by POST /check/update (with the tenantId the API gave the check), and a
// create and update by one PUT to the collection.
func TestWritePaths(t *testing.T) {
	t.Run("update elsewhere", func(t *testing.T) {
		k := descriptor.Kind{
			Kind: "Check", Group: "sm.example.hfl.dev", Version: "v1alpha1", Plural: "checks", Shape: descriptor.CRUD,
			Create:       &descriptor.Op{Method: "POST", Path: "/check/add"},
			Read:         descriptor.Op{Method: "GET", Path: "/check/{id}"},
			Update:       &descriptor.Op{Method: "POST", Path: "/check/update", Body: map[string]any{"tenantId": "{current.tenantId}"}},
			Delete:       &descriptor.Op{Method: "DELETE", Path: "/check/delete/{id}"},
			ID:           &descriptor.ID{Param: "id", From: "id"},
			UpdateID:     "id",
			ServerFields: map[string]any{"tenantId": float64(12)},
		}
		rec, srv := record(t, k)
		r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"job": "a"})
		obj := run(t, r, c, req)
		setSpec(t, c, obj, "job", "b")
		run(t, r, c, req)
		want := map[string]any{"job": "b", "id": "270000001", "tenantId": float64(12)}
		if upd := rec.last(t, "POST"); upd.Path != "/check/update" || !reflect.DeepEqual(upd.Body, want) {
			t.Fatalf("update: %s %v, want %v", upd.Path, upd.Body, want)
		}
		if got, _ := rec.api.Get("/check/270000001"); got["job"] != "b" {
			t.Fatalf("stored %v", got)
		}
		remove(t, r, c, req)
		if rec.api.Len() != 0 {
			t.Fatalf("the check is still there: %v", rec.api.Paths())
		}
	})
	t.Run("create and update by PUT to the collection", func(t *testing.T) {
		k := descriptor.Kind{
			Kind: "Budget", Group: "cost.example.hfl.dev", Version: "v1alpha1", Plural: "budgets", Shape: descriptor.CRUD,
			Create:       &descriptor.Op{Method: "PUT", Path: "/cost/budget"},
			Read:         descriptor.Op{Method: "GET", Path: "/cost/budget/{budget_id}"},
			Update:       &descriptor.Op{Method: "PUT", Path: "/cost/budget"},
			Delete:       &descriptor.Op{Method: "DELETE", Path: "/cost/budget/{budget_id}"},
			ID:           &descriptor.ID{Param: "budget_id", From: "id"},
			UpdateID:     "id",
			Envelope:     "data",
			BodyEnvelope: "data",
		}
		rec, srv := record(t, k)
		r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"type": "budget", "name": "a"})
		obj := run(t, r, c, req)
		setSpec(t, c, obj, "name", "b")
		run(t, r, c, req)
		if upd := rec.last(t, "PUT"); !reflect.DeepEqual(upd.Body, map[string]any{"data": map[string]any{"type": "budget", "name": "b", "id": "270000001"}}) {
			t.Fatalf("update body %v", upd.Body)
		}
		if rec.api.Len() != 1 {
			t.Fatalf("fake holds %v, want the one budget", rec.api.Paths())
		}
	})
}

// A path parameter filled from a body field of another name, which the
// body sends too (Confluent's tag binding).
func TestParamInBody(t *testing.T) {
	k := descriptor.Kind{
		Kind: "TagBinding", Group: "tag.example.hfl.dev", Version: "v1alpha1", Plural: "tagbindings", Shape: descriptor.CRUD,
		Create: &descriptor.Op{Method: "POST", Path: "/entity/tags"},
		Read: descriptor.Op{Method: "GET", Path: "/entity/type/{typeName}/name/{qualifiedName}/tags",
			Find: &descriptor.Find{Field: "typeName", Param: "tagName"}},
		Delete:    &descriptor.Op{Method: "DELETE", Path: "/entity/type/{typeName}/name/{qualifiedName}/tags/{tagName}"},
		Params:    []descriptor.Param{{Name: "typeName", Field: "entityType", InBody: true}, {Name: "qualifiedName", Field: "entityName", InBody: true}},
		ID:        &descriptor.ID{Param: "tagName", From: "typeName"},
		BodyArray: true,
	}
	rec, srv := record(t, k)
	spec := map[string]any{"entityType": "sr_schema", "entityName": "lsrc:s1", "typeName": "PII"}
	r, c, req := newTestReconciler(t, &k, srv.URL, spec)
	run(t, r, c, req)
	if got := rec.last(t, "POST").Body; !reflect.DeepEqual(got, []any{spec}) {
		t.Fatalf("create body %v, want [%v]", got, spec)
	}
	run(t, r, c, req) // read back: found in the entity's list
	if read := rec.last(t, "GET"); read.Path != "/entity/type/sr_schema/name/lsrc:s1/tags" || read.Status != http.StatusOK {
		t.Fatalf("read %s", read.Path)
	}
	remove(t, r, c, req)
	if del := rec.last(t, "DELETE"); del.Path != "/entity/type/sr_schema/name/lsrc:s1/tags/PII" || del.Status != http.StatusNoContent {
		t.Fatalf("delete %s: %d", del.Path, del.Status)
	}
}

// Objects named by a body field on their collection (Datadog's v1 Azure
// integration): found in the list by that field, changed by a PUT and
// removed by a DELETE whose body names it.
func TestKeyedByBodyField(t *testing.T) {
	k := descriptor.Kind{
		Kind: "IntegrationAzure", Group: "integration.example.hfl.dev", Version: "v1alpha1", Plural: "integrationazures", Shape: descriptor.Upsert,
		Create: &descriptor.Op{Method: "POST", Path: "/integration/azure"},
		Read:   descriptor.Op{Method: "GET", Path: "/integration/azure", Find: &descriptor.Find{Field: "tenant_name", Param: "tenant_name"}},
		Update: &descriptor.Op{Method: "PUT", Path: "/integration/azure"},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/integration/azure", SendBody: true},
		Params: []descriptor.Param{{Name: "tenant_name", Field: "tenant_name", InBody: true}},
	}
	rec, srv := record(t, k)
	r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"tenant_name": "t1", "client_id": "c1", "automute": "on"})
	obj := run(t, r, c, req)
	if got := rec.last(t, "POST").Body; !reflect.DeepEqual(got, map[string]any{"tenant_name": "t1", "client_id": "c1", "automute": "on"}) {
		t.Fatalf("create body %v", got)
	}
	setSpec(t, c, obj, "automute", "off")
	run(t, r, c, req)
	if upd := rec.last(t, "PUT"); upd.Body.(map[string]any)["tenant_name"] != "t1" || upd.Body.(map[string]any)["automute"] != "off" {
		t.Fatalf("update body %v", upd.Body)
	}
	remove(t, r, c, req)
	if del := rec.last(t, "DELETE"); del.Body.(map[string]any)["tenant_name"] != "t1" || del.Status != http.StatusNoContent {
		t.Fatalf("delete: body %v status %d", del.Body, del.Status)
	}
	if rec.api.Len() != 0 {
		t.Fatalf("still there: %v", rec.api.Paths())
	}
}
