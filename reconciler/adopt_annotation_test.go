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

func variableKind() *descriptor.Kind {
	return &descriptor.Kind{
		Kind: "Variable", Group: "vars.example.hfl.dev", Version: "v1alpha1", Plural: "variables",
		Shape:  descriptor.CRUD,
		Create: &descriptor.Op{Method: "POST", Path: "/repos/{repo}/variables"},
		Read:   descriptor.Op{Method: "GET", Path: "/repos/{repo}/variables/{id}"},
		Update: &descriptor.Op{Method: "PATCH", Path: "/repos/{repo}/variables/{id}"},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/repos/{repo}/variables/{id}"},
		Params: []descriptor.Param{{Name: "repo", Field: "repo"}},
		ID:     &descriptor.ID{Param: "id", From: "id"},
	}
}

// calls counts a fake's requests by method, holding one existing variable.
type calls map[string]int

func existingVariable(t *testing.T, value string) (*httptest.Server, calls) {
	t.Helper()
	n := calls{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n[req.Method]++
		switch {
		case req.URL.Path == "/repos/r1/variables/v9" && (req.Method == "GET" || req.Method == "PATCH"):
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "v9", "value": value})
		case req.URL.Path == "/repos/r1/variables/v9" && req.Method == "DELETE":
			w.WriteHeader(http.StatusNoContent)
		case req.URL.Path == "/repos/r1/variables" && req.Method == "POST":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "new", "value": value})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, n
}

func annotate(t *testing.T, ctx context.Context, r *Reconciler, req interface{ String() string }, obj *unstructured.Unstructured, ann map[string]string) {
	t.Helper()
	obj.SetAnnotations(ann)
	if err := r.Client.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
}

// An object with krm.hfl.dev/adopt-id takes over the resource it names:
// read, not created, and its ID kept in status.id. With the same spec as
// the resource, nothing is written.
func TestAdoptIDAnnotation(t *testing.T) {
	srv, n := existingVariable(t, "x")
	r, c, req := newTestReconciler(t, variableKind(), srv.URL, map[string]any{"repo": "r1", "value": "x"})
	ctx := context.Background()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(r.Kind.GVK())
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	annotate(t, ctx, r, req, obj, map[string]string{AnnAdoptID: "v9"})
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if n["POST"] != 0 {
		t.Errorf("POST calls = %d, want 0: an adopted resource is not made again", n["POST"])
	}
	if n["PATCH"] != 0 {
		t.Errorf("PATCH calls = %d, want 0: the resource already is as the spec says", n["PATCH"])
	}
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := unstructured.NestedString(obj.Object, "status", "id"); id != "v9" {
		t.Errorf("status.id = %q, want v9", id)
	}
}

// krm.hfl.dev/deletion-policy: abandon lets the object go and leaves the
// resource: no DELETE is sent.
func TestDeletionPolicyAbandon(t *testing.T) {
	for _, tc := range []struct {
		policy      string
		wantDeletes int
	}{{"abandon", 0}, {"", 1}} {
		t.Run("policy="+tc.policy, func(t *testing.T) {
			srv, n := existingVariable(t, "x")
			r, c, req := newTestReconciler(t, variableKind(), srv.URL, map[string]any{"repo": "r1", "value": "x"})
			ctx := context.Background()
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(r.Kind.GVK())
			if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
				t.Fatal(err)
			}
			ann := map[string]string{AnnAdoptID: "v9"}
			if tc.policy != "" {
				ann[AnnDeletionPolicy] = tc.policy
			}
			annotate(t, ctx, r, req, obj, ann)
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(ctx, obj); err != nil {
				t.Fatal(err)
			}
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
			if n["DELETE"] != tc.wantDeletes {
				t.Errorf("DELETE calls = %d, want %d", n["DELETE"], tc.wantDeletes)
			}
			if err := c.Get(ctx, req.NamespacedName, obj); err == nil {
				t.Error("the object is still there: its finalizer was not removed")
			}
		})
	}
}
