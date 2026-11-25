// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
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

const clusters = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Cluster
  group: things.example.hfl.dev
  version: v1alpha1
  plural: clusters
  shape: crud
  create: {method: POST, path: "/clusters"}
  read: {method: GET, path: "/clusters/{id}"}
  update: {method: PATCH, path: "/clusters/{id}"}
  delete: {method: DELETE, path: "/clusters/{id}"}
  id: {param: id, from: id}
  omitUnchanged: [config, display_name]
  sample: {display_name: c1, config: {kind: Basic}, environment: {id: env-1}}
`

func TestOmitUnchanged(t *testing.T) {
	f, err := descriptor.Parse([]byte(clusters))
	if err != nil {
		t.Fatal(err)
	}
	k := &f.Kinds[0]
	api := apifake.New(f)
	var patchBodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch {
			raw, _ := io.ReadAll(req.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			patchBodies = append(patchBodies, body)
			if _, hasName := body["display_name"]; hasName {
				if _, hasCfg := body["config"]; hasCfg {
					http.Error(w, `{"message":"Name cannot be updated together with cluster type"}`, http.StatusBadRequest)
					return
				}
			}
			req.Body = io.NopCloser(bytes.NewReader(raw))
		}
		api.ServeHTTP(w, req)
	}))
	defer srv.Close()

	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace("default")
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, srv.URL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "creds"}, Data: map[string][]byte{"t": []byte("tok")}}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	obj.SetNamespace("default")
	obj.SetName("c1")
	_ = unstructured.SetNestedField(obj.Object, map[string]any{
		"display_name": "c1",
		"config":       map[string]any{"kind": "Basic"},
		"environment":  map[string]any{"id": "env-1"},
	}, "spec")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(conn, sec, obj).WithStatusSubresource(obj).Build()
	r := &reconciler.Reconciler{
		Client:     cl,
		Vendor:     &rest.Vendor{Name: "example", CredentialKeys: []string{"t"}, Authorize: func(*http.Request, map[string]string) {}},
		Connection: f.ConnectionGVK(),
		Kind:       k,
	}
	ctx := context.Background()
	nn := types.NamespacedName{Namespace: "default", Name: "c1"}
	reconcile := func() {
		t.Helper()
		for range 3 {
			res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: nn})
			if err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if res.RequeueAfter == 0 {
				break
			}
		}
	}

	// Create.
	reconcile()

	// Rename display_name: config is unchanged so it must be omitted from PATCH.
	_ = cl.Get(ctx, nn, obj)
	_ = unstructured.SetNestedField(obj.Object, "c1-renamed", "spec", "display_name")
	_ = cl.Update(ctx, obj)
	reconcile()

	if len(patchBodies) != 1 {
		t.Fatalf("got %d PATCHes, want 1", len(patchBodies))
	}
	if _, ok := patchBodies[0]["config"]; ok {
		t.Errorf("rename PATCH unexpectedly included unchanged config: %v", patchBodies[0])
	}
	if patchBodies[0]["display_name"] != "c1-renamed" {
		t.Errorf("rename PATCH display_name = %v, want c1-renamed", patchBodies[0]["display_name"])
	}
	if _, ok := patchBodies[0]["environment"]; !ok {
		t.Errorf("rename PATCH missing required environment: %v", patchBodies[0])
	}

	// Upgrade config: display_name is unchanged so it must be omitted from PATCH.
	_ = cl.Get(ctx, nn, obj)
	_ = unstructured.SetNestedField(obj.Object, "Standard", "spec", "config", "kind")
	_ = cl.Update(ctx, obj)
	reconcile()

	if len(patchBodies) != 2 {
		t.Fatalf("got %d PATCHes, want 2", len(patchBodies))
	}
	if _, ok := patchBodies[1]["display_name"]; ok {
		t.Errorf("upgrade PATCH unexpectedly included unchanged display_name: %v", patchBodies[1])
	}
	if cfg, _ := patchBodies[1]["config"].(map[string]any); cfg["kind"] != "Standard" {
		t.Errorf("upgrade PATCH config = %v, want kind=Standard", patchBodies[1]["config"])
	}
}
