// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// A Gadget lives under a Widget: its path takes the Widget's server-made ID.
const widgetsAndGadgets = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Widget
  group: things.example.hfl.dev
  version: v1alpha1
  plural: widgets
  shape: crud
  create: {method: POST, path: "/widgets"}
  read: {method: GET, path: "/widgets/{widget_id}"}
  update: {method: PATCH, path: "/widgets/{widget_id}"}
  delete: {method: DELETE, path: "/widgets/{widget_id}"}
  id: {param: widget_id, from: id}
  sample: {name: w1}
- kind: Gadget
  group: things.example.hfl.dev
  version: v1alpha1
  plural: gadgets
  shape: crud
  create: {method: POST, path: "/widgets/{widget_id}/gadgets"}
  read: {method: GET, path: "/widgets/{widget_id}/gadgets/{gadget_id}"}
  update: {method: PATCH, path: "/widgets/{widget_id}/gadgets/{gadget_id}"}
  delete: {method: DELETE, path: "/widgets/{widget_id}/gadgets/{gadget_id}"}
  params: [{name: widget_id, field: widgetID}]
  id: {param: gadget_id, from: id}
  sample: {widgetID: "1", name: g1}
`

type refEnv struct {
	t      *testing.T
	c      client.Client
	f      *descriptor.File
	recs   map[string]*reconciler.Reconciler
	mu     sync.Mutex
	calls  []string
	ctx    context.Context
	vendor *rest.Vendor
}

func newRefEnv(t *testing.T, objs ...client.Object) *refEnv {
	t.Helper()
	f, err := descriptor.Parse([]byte(widgetsAndGadgets))
	if err != nil {
		t.Fatal(err)
	}
	e := &refEnv{t: t, f: f, recs: map[string]*reconciler.Reconciler{}, ctx: context.Background()}
	api := apifake.New(f)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		e.calls = append(e.calls, r.Method+" "+r.URL.Path)
		e.mu.Unlock()
		api.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)

	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	for i := range f.Kinds {
		mapper.Add(f.Kinds[i].GVK(), meta.RESTScopeNamespace)
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "creds"}, Data: map[string][]byte{"token": []byte("t")}}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace("team-a")
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, srv.URL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")
	b := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(append([]client.Object{secret, conn}, objs...)...)
	for _, o := range objs {
		b = b.WithStatusSubresource(o)
	}
	e.c = b.Build()
	e.vendor = &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
	for i := range f.Kinds {
		k := &f.Kinds[i]
		e.recs[k.Kind] = &reconciler.Reconciler{Client: e.c, Kind: k, Vendor: e.vendor, Connection: f.ConnectionGVK(), Kinds: reconciler.KindsByName(f)}
	}
	return e
}

func obj(kind, name string, spec map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("things.example.hfl.dev/v1alpha1")
	u.SetKind(kind)
	u.SetNamespace("team-a")
	u.SetName(name)
	u.Object["spec"] = spec
	return u
}

// reconcile runs kind's controller on name once and returns the object,
// its Ready reason, and the API calls made.
func (e *refEnv) reconcile(kind, name string) (*unstructured.Unstructured, string, []string) {
	e.t.Helper()
	e.mu.Lock()
	e.calls = nil
	e.mu.Unlock()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "team-a", Name: name}}
	// An invalid spec is returned as an error too (retried with backoff);
	// the tests look at the Ready condition.
	_, _ = e.recs[kind].Reconcile(e.ctx, req)
	got := &unstructured.Unstructured{}
	got.SetAPIVersion("things.example.hfl.dev/v1alpha1")
	got.SetKind(kind)
	if err := e.c.Get(e.ctx, req.NamespacedName, got); err != nil {
		e.mu.Lock()
		defer e.mu.Unlock()
		return nil, "", append([]string(nil), e.calls...)
	}
	reason := ""
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	for _, c := range conds {
		if m := c.(map[string]any); m["type"] == "Ready" {
			reason, _ = m["reason"].(string)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return got, reason, append([]string(nil), e.calls...)
}

func (e *refEnv) delete(kind, name string) {
	e.t.Helper()
	got := &unstructured.Unstructured{}
	got.SetAPIVersion("things.example.hfl.dev/v1alpha1")
	got.SetKind(kind)
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "team-a", Name: name}, got); err != nil {
		e.t.Fatal(err)
	}
	if err := e.c.Delete(e.ctx, got); err != nil {
		e.t.Fatal(err)
	}
}

func TestFieldRefsWaitThenResolve(t *testing.T) {
	gadget := obj("Gadget", "g", map[string]any{"name": "g1",
		"fieldRefs": map[string]any{"widgetID": map[string]any{"kind": "Widget", "name": "w"}}})
	widget := obj("Widget", "w", map[string]any{"name": "w1"})
	e := newRefEnv(t, gadget, widget)

	// The child first: it waits, and sends nothing.
	_, reason, calls := e.reconcile("Gadget", "g")
	if reason != reconciler.WaitingForReference || len(calls) != 0 {
		t.Fatalf("child before parent: reason %q, calls %v; want WaitingForReference and none", reason, calls)
	}

	w, reason, _ := e.reconcile("Widget", "w")
	if reason != "Created" {
		t.Fatalf("parent: reason %q", reason)
	}
	wid, _, _ := unstructured.NestedString(w.Object, "status", "id")

	g, reason, calls := e.reconcile("Gadget", "g")
	if reason != "Created" {
		t.Fatalf("child after parent: reason %q", reason)
	}
	if len(calls) == 0 || calls[0] != "POST /widgets/"+wid+"/gadgets" {
		t.Fatalf("child's calls = %v, want a POST under widget %s", calls, wid)
	}
	if got, _, _ := unstructured.NestedFieldNoCopy(g.Object, "status", "resolvedRefs", "widgetID"); got != wid {
		t.Errorf("status.resolvedRefs.widgetID = %v, want %s", got, wid)
	}
	if spec, _, _ := unstructured.NestedMap(g.Object, "spec"); spec["widgetID"] != nil {
		t.Errorf("the resolved value was written into the spec: %v", spec)
	}

	// The parent deleted first: the child still deletes, with the value it
	// last used.
	e.delete("Widget", "w")
	e.reconcile("Widget", "w")
	e.delete("Gadget", "g")
	got, _, calls := e.reconcile("Gadget", "g")
	if got != nil {
		t.Fatalf("the child was not let go after its parent was deleted: %v", got.Object["status"])
	}
	if !strings.Contains(strings.Join(calls, ","), "DELETE /widgets/"+wid+"/gadgets/") {
		t.Errorf("child delete calls = %v, want a DELETE under widget %s", calls, wid)
	}
}

func TestFieldRefsNeverMadeDeletesAtOnce(t *testing.T) {
	gadget := obj("Gadget", "g", map[string]any{"name": "g1",
		"fieldRefs": map[string]any{"widgetID": map[string]any{"kind": "Widget", "name": "missing"}}})
	e := newRefEnv(t, gadget)
	e.reconcile("Gadget", "g") // waits; adds no finalizer, sends nothing
	e.delete("Gadget", "g")
	got, _, calls := e.reconcile("Gadget", "g")
	if got != nil || len(calls) != 0 {
		t.Fatalf("a child that never resolved: object %v, calls %v; want it gone with no calls", got != nil, calls)
	}
}

func TestFieldRefsRefuseBothAndUnknownKinds(t *testing.T) {
	both := obj("Gadget", "both", map[string]any{"name": "g1", "widgetID": "1",
		"fieldRefs": map[string]any{"widgetID": map[string]any{"kind": "Widget", "name": "w"}}})
	unknown := obj("Gadget", "unknown", map[string]any{"name": "g1",
		"fieldRefs": map[string]any{"widgetID": map[string]any{"kind": "Sprocket", "name": "s"}}})
	e := newRefEnv(t, both, unknown)
	for _, name := range []string{"both", "unknown"} {
		got, reason, calls := e.reconcile("Gadget", name)
		msg, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
		if reason != "InvalidSpec" || len(calls) != 0 {
			t.Errorf("%s: reason %q, calls %v, conditions %v; want InvalidSpec and no calls", name, reason, calls, msg)
		}
	}
}

// The parent Ready before the child is first reconciled: the reference
// resolves at once, on an object with no status yet.
func TestFieldRefsResolveOnFirstReconcile(t *testing.T) {
	gadget := obj("Gadget", "g", map[string]any{"name": "g1",
		"fieldRefs": map[string]any{"widgetID": map[string]any{"kind": "Widget", "name": "w"}}})
	widget := obj("Widget", "w", map[string]any{"name": "w1"})
	e := newRefEnv(t, gadget, widget)
	if _, reason, _ := e.reconcile("Widget", "w"); reason != "Created" {
		t.Fatalf("parent: %q", reason)
	}
	g, reason, calls := e.reconcile("Gadget", "g")
	if reason != "Created" || len(calls) == 0 {
		t.Fatalf("child: reason %q, calls %v, status %v", reason, calls, g.Object["status"])
	}
	if rr, _, _ := unstructured.NestedMap(g.Object, "status", "resolvedRefs"); rr["widgetID"] == nil {
		t.Errorf("status.resolvedRefs = %v", rr)
	}
}
