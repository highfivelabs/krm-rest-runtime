// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

const widgets = `
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
`

// A read that misses an object just written (a replica behind) is polled
// with backoff before the object is created a second time.
func TestNotFoundRecheck(t *testing.T) {
	defer func(d time.Duration) { reconciler.NotFoundRecheck = d }(reconciler.NotFoundRecheck)
	reconciler.NotFoundRecheck = 15 * time.Millisecond

	f, err := descriptor.Parse([]byte(widgets))
	if err != nil {
		t.Fatal(err)
	}
	k := &f.Kinds[0]
	api := apifake.New(f)
	var misses atomic.Int32 // GETs of an item still to answer 404
	var emptyPatch atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && strings.HasPrefix(req.URL.Path, "/widgets/") && misses.Add(-1) >= 0 {
			http.NotFound(w, req)
			return
		}
		if req.Method == http.MethodPatch && emptyPatch.Load() {
			api.ServeHTTP(httptest.NewRecorder(), req)
			misses.Store(2)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		api.ServeHTTP(w, req)
	}))
	defer srv.Close()

	gvk := k.GVK()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(gvk, meta.RESTScopeNamespace)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "creds"}, Data: map[string][]byte{"token": []byte("t")}}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace("ns")
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, srv.URL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"name": "w1"}}}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace("ns")
	obj.SetName("w")
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(secret, conn, obj).WithStatusSubresource(obj).Build()
	r := &reconciler.Reconciler{Client: c, Kind: k, Vendor: &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}, Connection: f.ConnectionGVK()}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "w"}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	// Multiple consecutive 404s (e.g. slow replica) still resolve within the
	// backoff window without creating a duplicate.
	misses.Store(3)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := api.Len(); n != 1 {
		t.Fatalf("the API holds %d widgets after 3 missed reads, want 1: %v", n, api.Paths())
	}

	// Empty 204 update followed by 404s on readback also retries via readRecheck.
	if err := c.Get(context.Background(), req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(obj.Object, "w2", "spec", "name")
	if err := c.Update(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	emptyPatch.Store(true)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	emptyPatch.Store(false)

	// Gone for good (deleted outside Kubernetes): created again after budget expires.
	misses.Store(100)
	for _, p := range api.Paths() {
		req2, _ := http.NewRequest(http.MethodDelete, srv.URL+p, nil)
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := api.Len(); n != 1 {
		t.Fatalf("the API holds %d widgets after an outside delete, want 1 (created again): %v", n, api.Paths())
	}

	// NotFoundRecheck = 0 turns rechecking off (1 miss immediately re-creates).
	reconciler.NotFoundRecheck = 0
	misses.Store(1)
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := api.Len(); n != 2 {
		t.Fatalf("the API holds %d widgets with NotFoundRecheck=0, want 2 (re-created): %v", n, api.Paths())
	}

	// Context cancellation aborts the recheck promptly.
	reconciler.NotFoundRecheck = 5 * time.Second
	misses.Store(100)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := r.Reconcile(ctx, req); err == nil {
		t.Fatal("expected error on canceled context, got nil")
	}
}
