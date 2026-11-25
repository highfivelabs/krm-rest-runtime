// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"net/http/httptest"
	"strings"
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

const apiKeys = `
vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: ApiKey
  group: things.example.hfl.dev
  version: v1alpha1
  plural: apikeys
  shape: crud
  create: {method: POST, path: "/keys"}
  read: {method: GET, path: "/keys/{key_id}"}
  update: {method: PATCH, path: "/keys/{key_id}"}
  delete: {method: DELETE, path: "/keys/{key_id}"}
  id: {param: key_id, from: id}
  serverFields: {privateKey: s3cr3t-private-key-value, publicKey: pk-1}
  secretOutputs: [privateKey]
  sample: {desc: k1}
`

// A secret output the API answers goes to the Secret writeSecretRef names,
// which the object owns, and never to the status; a Secret of that name the
// object does not own is not written.
func TestSecretOutputs(t *testing.T) {
	f, err := descriptor.Parse([]byte(apiKeys))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apifake.New(f))
	defer srv.Close()
	k := &f.Kinds[0]
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(k.GVK(), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "creds"}, Data: map[string][]byte{"token": []byte("t")}}
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "taken"}, Data: map[string][]byte{"x": []byte("y")}}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace("ns")
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, srv.URL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")
	mk := func(name, secret string) *unstructured.Unstructured {
		o := &unstructured.Unstructured{}
		o.SetGroupVersionKind(k.GVK())
		o.SetNamespace("ns")
		o.SetName(name)
		o.SetUID(types.UID("uid-" + name))
		o.Object["spec"] = map[string]any{"desc": name, "writeSecretRef": map[string]any{"name": secret}}
		return o
	}
	a, b := mk("a", "a-key"), mk("b", "taken")
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(creds, foreign, conn, a, b).WithStatusSubresource(a, b).Build()
	rec := &reconciler.Reconciler{Client: c, Kind: k, Vendor: &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}},
		Connection: f.ConnectionGVK(), Kinds: reconciler.KindsByName(f)}
	ctx := context.Background()
	run := func(name string) *unstructured.Unstructured {
		_, _ = rec.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: name}})
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(k.GVK())
		if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := run("a")
	sec := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "a-key"}, sec); err != nil {
		t.Fatalf("no Secret written: %v", err)
	}
	if string(sec.Data["privateKey"]) != "s3cr3t-private-key-value" || len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].UID != "uid-a" {
		t.Errorf("Secret %v, owners %v", sec.Data, sec.OwnerReferences)
	}
	at, _, _ := unstructured.NestedMap(got.Object, "status", "atProvider")
	if _, has := at["privateKey"]; has || at["publicKey"] != "pk-1" {
		t.Errorf("atProvider %v", at)
	}
	run("a") // again: unchanged, still fine

	got = run("b")
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	if len(conds) == 0 || !strings.Contains(conds[0].(map[string]any)["message"].(string), "not this object's") {
		t.Errorf("a foreign Secret: %v", conds)
	}
	_ = c.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "taken"}, sec)
	if string(sec.Data["x"]) != "y" || sec.Data["privateKey"] != nil {
		t.Errorf("the foreign Secret was written: %v", sec.Data)
	}
}

// A secretRefs field inside an object (Confluent's Flink connection,
// "spec.auth_data.data") fills that nested field, beside its siblings.
func TestSecretRefsDotted(t *testing.T) {
	f, err := descriptor.Parse([]byte(apiKeys))
	if err != nil {
		t.Fatal(err)
	}
	api := apifake.New(f)
	srv := httptest.NewServer(api)
	defer srv.Close()
	k := &f.Kinds[0]
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(k.GVK(), meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	creds := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "creds"}, Data: map[string][]byte{"token": []byte("t"), "data": []byte(`{"API_KEY":"k"}`)}}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace("ns")
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, srv.URL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(k.GVK())
	o.SetNamespace("ns")
	o.SetName("conn")
	o.Object["spec"] = map[string]any{"desc": "d", "spec": map[string]any{"endpoint": "https://example.com", "auth_data": map[string]any{"kind": "PlaintextProvider"}},
		"secretRefs": map[string]any{"spec.auth_data.data": map[string]any{"name": "creds", "key": "data"}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).WithObjects(creds, conn, o).WithStatusSubresource(o).Build()
	rec := &reconciler.Reconciler{Client: c, Kind: k, Vendor: &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}},
		Connection: f.ConnectionGVK(), Kinds: reconciler.KindsByName(f)}
	if _, err := rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "conn"}}); err != nil {
		t.Fatal(err)
	}
	paths := api.Paths()
	if len(paths) != 1 {
		t.Fatalf("stored: %v", paths)
	}
	got, _ := api.Get(paths[0])
	ad, _, _ := unstructured.NestedMap(got, "spec", "auth_data")
	if ad["data"] != `{"API_KEY":"k"}` || ad["kind"] != "PlaintextProvider" {
		t.Fatalf("auth_data = %v", ad)
	}
}
