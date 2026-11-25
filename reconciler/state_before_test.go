// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// A state whose call needs others first (Grafana Asserts' dataset
// auto-setup before its enable) makes them in order, only when the state
// is to be set; a call with sendBody sends the spec's other fields, {}
// when there are none (Grafana's keeper activation); deleting sets the
// delete state, its own calls first.
func TestStateBeforeAndBody(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "AssertsStack", Group: "asserts.example.hfl.dev", Version: "v1alpha1", Plural: "assertsstacks",
		Shape: descriptor.Settings,
		Read:  descriptor.Op{Method: "GET", Path: "/v1/stack/status"},
		State: &descriptor.State{Field: "enabled", Delete: "false",
			Calls: map[string]descriptor.Op{
				"true":  {Method: "POST", Path: "/v2/stack/enable", SendBody: true},
				"false": {Method: "POST", Path: "/v2/stack/disable"},
			},
			Before: map[string][]descriptor.Op{"true": {{Method: "POST", Path: "/v2/stack/datasets/auto-setup"}}},
		},
	}
	if err := parseKind(t, k); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	enabled := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		data, _ := io.ReadAll(req.Body)
		calls = append(calls, req.Method+" "+req.URL.Path+" "+string(data))
		switch req.URL.Path {
		case "/v2/stack/enable":
			enabled = true
		case "/v2/stack/disable":
			enabled = false
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": enabled, "status": "COMPLETE"})
	}))
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"enabled": "true"})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, cl := range calls {
		if !strings.HasPrefix(cl, "GET") {
			writes = append(writes, cl)
		}
	}
	if strings.Join(writes, "\n") != "POST /v2/stack/datasets/auto-setup \nPOST /v2/stack/enable {}" {
		t.Fatalf("enable: %q", writes)
	}
	calls = nil
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, cl := range calls {
		if !strings.HasPrefix(cl, "GET") {
			t.Fatalf("resync wrote: %v", calls)
		}
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, obj)
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	calls = nil
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := calls[len(calls)-1]; got != "POST /v2/stack/disable " {
		t.Fatalf("delete: %v", calls)
	}

	k.State.Before = map[string][]descriptor.Op{"maybe": {{Method: "POST", Path: "/x"}}}
	if err := parseKind(t, k); err == nil || !strings.Contains(err.Error(), "state.before.maybe") {
		t.Fatalf("before a value with no call: %v", err)
	}
}
