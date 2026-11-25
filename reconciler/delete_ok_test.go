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

// A Worker's latest version cannot be deleted (409); it goes with the
// Worker. With delete.ok: [409] the object is let go; without it the
// delete fails and the object stays.
func TestDeleteOK(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ok       []int
		wantGone bool
	}{
		{"ok 409", []int{409}, true},
		{"no ok", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := &descriptor.Kind{
				Kind: "WorkerVersion", Group: "worker.example.hfl.dev", Version: "v1alpha1", Plural: "workerversions",
				Shape:  descriptor.CRUD,
				Create: &descriptor.Op{Method: "POST", Path: "/workers/{worker_id}/versions"},
				Read:   descriptor.Op{Method: "GET", Path: "/workers/{worker_id}/versions/{version_id}"},
				Delete: &descriptor.Op{Method: "DELETE", Path: "/workers/{worker_id}/versions/{version_id}", OK: tc.ok},
				Params: []descriptor.Param{{Name: "worker_id", Field: "workerID"}},
				ID:     &descriptor.ID{Param: "version_id", From: "id"},
			}
			deletes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch {
				case req.Method == "POST" && req.URL.Path == "/workers/w1/versions":
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "v1", "main_module": "index.js"})
				case req.Method == "GET" && req.URL.Path == "/workers/w1/versions/v1":
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "v1", "main_module": "index.js"})
				case req.Method == "DELETE" && req.URL.Path == "/workers/w1/versions/v1":
					deletes++
					http.Error(w, `{"errors":[{"message":"The latest version cannot be deleted"}]}`, http.StatusConflict)
				default:
					http.NotFound(w, req)
				}
			}))
			defer srv.Close()

			r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"workerID": "w1", "main_module": "index.js"})
			ctx := context.Background()
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatalf("create: %v", err)
			}
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(k.GVK())
			if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
				t.Fatal(err)
			}
			if err := c.Delete(ctx, obj); err != nil {
				t.Fatal(err)
			}
			_, _ = r.Reconcile(ctx, req)
			if deletes != 1 {
				t.Fatalf("DELETE calls = %d, want 1", deletes)
			}
			err := c.Get(ctx, req.NamespacedName, obj)
			if gone := err != nil; gone != tc.wantGone {
				t.Fatalf("object gone = %v, want %v (get: %v)", gone, tc.wantGone, err)
			}
		})
	}
}

func TestOpAllows(t *testing.T) {
	op := &descriptor.Op{OK: []int{409}}
	if !op.Allows(409) || op.Allows(400) {
		t.Fatal("Allows(409) should be true and Allows(400) false")
	}
	var nilOp *descriptor.Op
	if nilOp.Allows(409) {
		t.Fatal("a nil op allows nothing")
	}
}
