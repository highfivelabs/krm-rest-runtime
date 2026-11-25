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
	"strings"
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

func setupReconciler(t *testing.T, typesYAML string, spec map[string]any, handler http.Handler) (*reconciler.Reconciler, fake.ClientBuilder, *unstructured.Unstructured, ctrl.Request) {
	t.Helper()
	f, err := descriptor.Parse([]byte(typesYAML))
	if err != nil {
		t.Fatal(err)
	}
	k := &f.Kinds[0]
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

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
	obj.SetName("item-1")
	_ = unstructured.SetNestedField(obj.Object, spec, "spec")

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(conn, sec, obj).WithStatusSubresource(obj).Build()
	r := &reconciler.Reconciler{
		Client:     cl,
		Vendor:     &rest.Vendor{Name: "example", CredentialKeys: []string{"t"}, Authorize: func(*http.Request, map[string]string) {}},
		Connection: f.ConnectionGVK(),
		Kind:       k,
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "item-1"}}
	return r, fake.ClientBuilder{}, obj, req
}

func TestNestedCreateOnly(t *testing.T) {
	const yml = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: FastlyAccount
  group: things.example.hfl.dev
  version: v1alpha1
  plural: fastlyaccounts
  shape: crud
  create: {method: POST, path: "/accounts"}
  read: {method: GET, path: "/accounts/{id}"}
  update: {method: PATCH, path: "/accounts/{id}"}
  delete: {method: DELETE, path: "/accounts/{id}"}
  id: {param: id, from: id}
  envelope: data
  bodyEnvelope: data
  createOnly: [attributes.name]
  sample: {type: fastly-accounts, attributes: {name: acct-1, api_key: k1}}
`
	f, err := descriptor.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	api := apifake.New(f)
	var patchAttrs map[string]any
	r, _, obj, req := setupReconciler(t, yml, map[string]any{
		"type":       "fastly-accounts",
		"attributes": map[string]any{"name": "acct-1", "api_key": "k1"},
	}, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPatch {
			raw, _ := io.ReadAll(req.Body)
			var env map[string]any
			_ = json.Unmarshal(raw, &env)
			data, _ := env["data"].(map[string]any)
			patchAttrs, _ = data["attributes"].(map[string]any)
			if _, hasName := patchAttrs["name"]; hasName {
				http.Error(w, `{"errors":[{"detail":"Unknown field name"}]}`, http.StatusBadRequest)
				return
			}
			req.Body = io.NopCloser(bytes.NewReader(raw))
		}
		api.ServeHTTP(w, req)
	}))

	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Update mutable nested field (api_key): attributes.name must be stripped.
	_ = r.Client.Get(ctx, req.NamespacedName, obj)
	_ = unstructured.SetNestedField(obj.Object, "k2", "spec", "attributes", "api_key")
	if err := r.Client.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, hasName := patchAttrs["name"]; hasName {
		t.Fatalf("PATCH body still carried createOnly attributes.name: %v", patchAttrs)
	}
	if patchAttrs["api_key"] != "k2" {
		t.Fatalf("PATCH attributes.api_key = %v, want k2", patchAttrs["api_key"])
	}

	// Mutating nested createOnly field (attributes.name) must report drifted.
	_ = r.Client.Get(ctx, req.NamespacedName, obj)
	_ = unstructured.SetNestedField(obj.Object, "acct-2", "spec", "attributes", "name")
	if err := r.Client.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err == nil || !strings.Contains(err.Error(), "spec.attributes.name") {
		t.Fatalf("expected drifted error for spec.attributes.name, got %v", err)
	}
}

func TestOpBody(t *testing.T) {
	const yml = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: RumApp
  group: things.example.hfl.dev
  version: v1alpha1
  plural: rumapps
  shape: crud
  create: {method: POST, path: "/rum"}
  read: {method: GET, path: "/rum/{id}"}
  update:
    method: PATCH
    path: "/rum/{id}"
    body: {type: rum_application_update}
  delete: {method: DELETE, path: "/rum/{id}"}
  id: {param: id, from: id}
  writeOnly: [type]
  sample: {type: rum_application_create, name: app-1}
`
	f, err := descriptor.Parse([]byte(yml))
	if err != nil {
		t.Fatal(err)
	}
	api := apifake.New(f)
	var postType, patchType string
	r, _, obj, req := setupReconciler(t, yml, map[string]any{
		"type": "rum_application_create",
		"name": "app-1",
	}, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost || req.Method == http.MethodPatch {
			raw, _ := io.ReadAll(req.Body)
			var body map[string]any
			_ = json.Unmarshal(raw, &body)
			if req.Method == http.MethodPost {
				postType, _ = body["type"].(string)
			} else {
				patchType, _ = body["type"].(string)
			}
			req.Body = io.NopCloser(bytes.NewReader(raw))
		}
		api.ServeHTTP(w, req)
	}))

	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("create: %v", err)
	}
	if postType != "rum_application_create" {
		t.Fatalf("POST type = %q, want rum_application_create", postType)
	}

	_ = r.Client.Get(ctx, req.NamespacedName, obj)
	_ = unstructured.SetNestedField(obj.Object, "app-2", "spec", "name")
	if err := r.Client.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("update: %v", err)
	}
	if patchType != "rum_application_update" {
		t.Fatalf("PATCH type = %q, want rum_application_update", patchType)
	}
}

func TestSettingsNotFoundCreates(t *testing.T) {
	const yml = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: TeamRoutingRules
  group: things.example.hfl.dev
  version: v1alpha1
  plural: teamroutingrules
  shape: settings
  read: {method: GET, path: "/teams/{team_id}/rules"}
  update: {method: PUT, path: "/teams/{team_id}/rules"}
  params: [{name: team_id, field: teamID}]
  sample: {teamID: t1, urgency: low}
`
	var stored map[string]any
	r, _, _, req := setupReconciler(t, yml, map[string]any{
		"teamID":  "t1",
		"urgency": "low",
	}, http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case http.MethodGet:
			if stored == nil {
				http.NotFound(w, req)
				return
			}
			_ = json.NewEncoder(w).Encode(stored)
		case http.MethodPut:
			_ = json.NewDecoder(req.Body).Decode(&stored)
			_ = json.NewEncoder(w).Encode(stored)
		}
	}))

	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("initial reconcile on 404 settings: %v", err)
	}
	if stored == nil || stored["urgency"] != "low" {
		t.Fatalf("expected PUT to create settings, stored = %#v", stored)
	}
	// Second reconcile should read back Unchanged.
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
}
