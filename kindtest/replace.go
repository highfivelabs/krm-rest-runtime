// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// replaceSteps, for a kind with no update and a Replace: a changed field
// reports drift and stops while the object has not opted in; with
// spec.replaceOnChange it is re-created in place, and drift is repaired the
// same way; a change to a field that names it is refused.
func replaceSteps(t *testing.T, api *apifake.Server, k *descriptor.Kind, spec map[string]any, c client.Client,
	r *reconciler.Reconciler, req ctrl.Request, path string, reconcile func(string) *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	field := replaceable(k, spec)
	if field == "" {
		return
	}
	get := func() *unstructured.Unstructured {
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(k.GVK())
		if err := c.Get(ctx, req.NamespacedName, got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	set := func(value any, fields ...string) {
		got := get()
		if err := unstructured.SetNestedField(got.Object, value, fields...); err != nil {
			t.Fatal(err)
		}
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
	}
	refused := func(step, want string) {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err == nil {
			t.Fatalf("%s: reconcile succeeded, want it refused", step)
		}
		conds, _, _ := unstructured.NestedSlice(get().Object, "status", "conditions")
		if len(conds) == 0 {
			t.Fatalf("%s: no condition", step)
		}
		cond := conds[0].(map[string]any)
		if cond["status"] != "False" || !strings.Contains(cond["message"].(string), want) {
			t.Fatalf("%s: condition %v, want Ready=False saying %q", step, cond, want)
		}
	}
	before, _ := api.Get(path)
	old := before[field]

	// Not opted in: drift is reported and nothing is sent.
	set("changed-"+field, "spec", field)
	refused("change without replaceOnChange", "spec.replaceOnChange")
	if stored, _ := api.Get(path); stored[field] != old || api.ReplaceCalls(k) != 0 {
		t.Fatalf("change without replaceOnChange: %s = %v in the API after %d replaces, want it untouched", field, stored[field], api.ReplaceCalls(k))
	}

	// Opted in: re-created in place.
	set(true, "spec", reconciler.ReplaceOnChange)
	got := reconcile("replace")
	if reason := readyReason(got); reason != "Replaced" {
		t.Fatalf("replace: Ready reason %q, want Replaced", reason)
	}
	if stored, _ := api.Get(path); stored[field] != "changed-"+field || api.ReplaceCalls(k) != 1 || api.Len() != 1 {
		t.Fatalf("replace: %s = %v after %d replaces with %d objects; want the new value, 1 replace, 1 object: %v",
			field, stored[field], api.ReplaceCalls(k), api.Len(), api.Paths())
	}
	reconcile("resync")
	if api.ReplaceCalls(k) != 1 {
		t.Fatalf("resync: replaced again with nothing changed")
	}

	// Drift is repaired by replacing too.
	stored, _ := api.Get(path)
	stored[field] = "edited-in-the-console"
	reconcile("drift")
	if stored, _ := api.Get(path); stored[field] != "changed-"+field || api.ReplaceCalls(k) != 2 {
		t.Fatalf("drift: %s = %v, want it restored by a replace", field, stored[field])
	}

	// A field that names the resource is not changed by replacing.
	for _, id := range k.Replace.Identity {
		v, ok := spec[id].(string)
		if !ok {
			continue
		}
		set("other-"+v, "spec", id)
		refused("change of "+id, "delete and recreate")
		if api.Len() != 1 || api.ReplaceCalls(k) != 2 {
			t.Fatalf("change of %s: %d objects after %d replaces, want 1 and 2", id, api.Len(), api.ReplaceCalls(k))
		}
		set(v, "spec", id)
		reconcile("change of " + id + " undone")
	}
}

// replaceable picks a top-level string body field to change that does not
// name the resource.
func replaceable(k *descriptor.Kind, spec map[string]any) string {
	skip := map[string]bool{}
	if k.ID != nil {
		skip[k.ID.From], skip[k.ID.Param] = true, true
	}
	for _, f := range k.Replace.Identity {
		skip[f] = true
	}
	var cands []string
	for f, v := range bodyOf(k, spec) {
		if _, ok := v.(string); ok && !skip[f] && !k.IsWriteOnly(f) && !strings.HasSuffix(f, "_id") {
			cands = append(cands, f)
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		return ""
	}
	return cands[0]
}

func readyReason(obj *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == "Ready" {
			s, _ := m["reason"].(string)
			return s
		}
	}
	return ""
}

// RefusesReplace checks that each named kind cannot be re-created in place:
// its descriptor has no Replace, and an object that sets
// spec.replaceOnChange: true is refused before anything reaches the API.
func RefusesReplace(t *testing.T, typesYAML []byte, vendor *rest.Vendor, kinds ...string) {
	t.Helper()
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range kinds {
		t.Run(name, func(t *testing.T) {
			var k *descriptor.Kind
			for i := range f.Kinds {
				if f.Kinds[i].Kind == name {
					k = &f.Kinds[i]
				}
			}
			if k == nil {
				t.Fatalf("no kind %s", name)
			}
			if k.Replace != nil {
				t.Fatalf("%s can be re-created in place; it must not", name)
			}
			api := apifake.New(f)
			srv := newServer(api)
			defer srv.Close()
			c, r, req := setup(t, f, k, vendor, srv.URL, map[string]any{reconciler.ReplaceOnChange: true})
			if _, err := r.Reconcile(context.Background(), req); err == nil {
				t.Fatalf("reconcile succeeded with spec.replaceOnChange set")
			}
			got := &unstructured.Unstructured{}
			got.SetGroupVersionKind(k.GVK())
			if err := c.Get(context.Background(), req.NamespacedName, got); err != nil {
				t.Fatal(err)
			}
			if reason := readyReason(got); reason != "InvalidSpec" {
				t.Fatalf("Ready reason %q, want InvalidSpec", reason)
			}
			if api.Len() != 0 {
				t.Fatalf("the API holds %v, want nothing", api.Paths())
			}
		})
	}
}

// setup is the fake cluster one object of kind k lives in, with extra spec
// fields over its sample.
func setup(t *testing.T, f *descriptor.File, k *descriptor.Kind, vendor *rest.Vendor, baseURL string, extra map[string]any) (client.Client, *reconciler.Reconciler, ctrl.Request) {
	t.Helper()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(k.GVK(), meta.RESTScopeNamespace)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	creds := map[string][]byte{}
	for _, key := range vendor.CredentialKeys {
		creds[key] = []byte("test-" + key)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "creds"}, Data: creds}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace(ns)
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, baseURL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	obj.SetNamespace(ns)
	obj.SetName("sample")
	spec, _ := runtime.DeepCopyJSONValue(map[string]any(k.Sample)).(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	for f, v := range extra {
		spec[f] = v
	}
	obj.Object["spec"] = spec
	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(secret, conn, obj).WithStatusSubresource(obj).Build()
	r := &reconciler.Reconciler{Client: c, Kind: k, Vendor: vendor, Connection: f.ConnectionGVK()}
	return c, r, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)}
}

func newServer(api *apifake.Server) *httptest.Server { return httptest.NewServer(api) }

// Replaces runs each named kind through EveryKind's steps and requires that
// the replace steps ran: the kind has a Replace and its sample a top-level
// string field to change that does not name the resource (add one to the
// sample in typesYAML if the generated sample has none).
func Replaces(t *testing.T, typesYAML []byte, vendor *rest.Vendor, kinds ...string) {
	t.Helper()
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range kinds {
		var k *descriptor.Kind
		for i := range f.Kinds {
			if f.Kinds[i].Kind == name {
				k = &f.Kinds[i]
			}
		}
		switch {
		case k == nil:
			t.Fatalf("no kind %s", name)
		case k.Replace == nil:
			t.Fatalf("%s cannot be re-created in place", name)
		case replaceable(k, k.Sample) == "":
			t.Fatalf("%s: the sample has no string field to change that does not name it", name)
		}
		t.Run(k.Kind+"."+k.Group, func(t *testing.T) { one(t, f, k, vendor, nil) })
	}
}
