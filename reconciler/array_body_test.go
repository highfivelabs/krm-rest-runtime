// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

func TestBodyArrayCRUD(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "AccessListApiKey", Group: "access.example.hfl.dev", Version: "v1alpha1", Plural: "accesslistapikeys",
		Shape:     descriptor.CRUD,
		BodyArray: true,
		Create:    &descriptor.Op{Method: "POST", Path: "/apiKeys/{apiUserId}/accessList"},
		Read:      descriptor.Op{Method: "GET", Path: "/apiKeys/{apiUserId}/accessList/{ipAddress}"},
		Delete:    &descriptor.Op{Method: "DELETE", Path: "/apiKeys/{apiUserId}/accessList/{ipAddress}"},
		Params:    []descriptor.Param{{Name: "apiUserId", Field: "apiuserid"}},
		ID:        &descriptor.ID{Param: "ipAddress", From: "ipAddress"},
	}
	var gotPost []any
	var stored map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == "POST" && req.URL.Path == "/apiKeys/k1/accessList":
			raw, _ := io.ReadAll(req.Body)
			if err := json.Unmarshal(raw, &gotPost); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			stored = gotPost[0].(map[string]any)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{stored}})
		case req.Method == "GET" && req.URL.Path == "/apiKeys/k1/accessList/192.0.2.1":
			if stored == nil {
				http.NotFound(w, req)
				return
			}
			_ = json.NewEncoder(w).Encode(stored)
		case req.Method == "DELETE" && req.URL.Path == "/apiKeys/k1/accessList/192.0.2.1":
			stored = nil
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{
		"apiuserid": "k1",
		"ipAddress": "192.0.2.1",
	})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(gotPost) != 1 || gotPost[0].(map[string]any)["ipAddress"] != "192.0.2.1" {
		t.Fatalf("POST body = %v, want [{ipAddress: 192.0.2.1}]", gotPost)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("read: %v", err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != "192.0.2.1" {
		t.Fatalf("status.id = %q, want 192.0.2.1", id)
	}
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if stored != nil {
		t.Fatal("delete did not remove stored item")
	}
}

func TestBodyFieldSettings(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "CloudConnectorRules", Group: "cloud.example.hfl.dev", Version: "v1alpha1", Plural: "cloudconnectorruleses",
		Shape:     descriptor.Settings,
		Envelope:  "result",
		BodyField: "rules",
		Read:      descriptor.Op{Method: "GET", Path: "/zones/{zone_id}/cloud_connector/rules"},
		Update:    &descriptor.Op{Method: "PUT", Path: "/zones/{zone_id}/cloud_connector/rules"},
		Params:    []descriptor.Param{{Name: "zone_id", Field: "zoneID"}},
	}
	stored := []any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == "GET" && req.URL.Path == "/zones/z1/cloud_connector/rules":
			_ = json.NewEncoder(w).Encode(map[string]any{"result": stored})
		case req.Method == "PUT" && req.URL.Path == "/zones/z1/cloud_connector/rules":
			raw, _ := io.ReadAll(req.Body)
			var arr []any
			if err := json.Unmarshal(raw, &arr); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			stored = arr
			_ = json.NewEncoder(w).Encode(map[string]any{"result": stored})
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	wantRule := map[string]any{"description": "r1", "enabled": true}
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{
		"zoneID": "z1",
		"rules":  []any{wantRule},
	})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if !reflect.DeepEqual(stored, []any{wantRule}) {
		t.Fatalf("stored = %v, want %v", stored, []any{wantRule})
	}
	// Read back: unchanged.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Drift in console, then controller restores it.
	stored = []any{map[string]any{"description": "edited", "enabled": false}}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("drift: %v", err)
	}
	if !reflect.DeepEqual(stored, []any{wantRule}) {
		t.Fatalf("after drift repair: stored = %v, want %v", stored, []any{wantRule})
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	gotRules, _, _ := unstructured.NestedSlice(obj.Object, "status", "atProvider", "rules")
	if !reflect.DeepEqual(gotRules, []any{wantRule}) {
		t.Fatalf("status.atProvider.rules = %v, want %v", gotRules, []any{wantRule})
	}
}

func newTestReconciler(t *testing.T, k *descriptor.Kind, baseURL string, spec map[string]any) (*Reconciler, client.Client, ctrl.Request) {
	t.Helper()
	const ns = "default"
	connGVK := schema.GroupVersionKind{Group: "core.example.hfl.dev", Version: "v1alpha1", Kind: "Connection"}
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(k.GVK(), meta.RESTScopeNamespace)
	mapper.Add(connGVK, meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "creds"}, Data: map[string][]byte{"token": []byte("t")}}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(connGVK)
	conn.SetNamespace(ns)
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, baseURL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	obj.SetNamespace(ns)
	obj.SetName("item")
	obj.Object["spec"] = spec

	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(secret, conn, obj).WithStatusSubresource(obj).Build()
	vendor := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
	r := &Reconciler{Client: c, Kind: k, Vendor: vendor, Connection: connGVK}
	return r, c, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "item"}}
}
