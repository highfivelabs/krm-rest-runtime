// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// holder answers GET with a held resource and records every other call's
// method, path and body; a write replaces what it sends.
type holder struct {
	mu    sync.Mutex
	held  map[string]any
	calls []string
	sent  []map[string]any
}

func (rc *holder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if req.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(rc.held)
		return
	}
	var body map[string]any
	data, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(data, &body)
	rc.calls = append(rc.calls, req.Method+" "+req.URL.Path)
	rc.sent = append(rc.sent, body)
	for f, v := range body {
		rc.held[f] = v
	}
	w.WriteHeader(http.StatusNoContent)
}

// A Grafana role's assignments are written whole: adding a team sends the
// users and service accounts as read, and the teams with it; a spec naming
// another team takes the old one out in the same write; deleting takes it
// out.
func TestElementWholeWrite(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "RoleAssignmentItem", Group: "role.example.hfl.dev", Version: "v1alpha1", Plural: "roleassignmentitems",
		Shape:   descriptor.Upsert,
		Read:    descriptor.Op{Method: "GET", Path: "/roles/{roleUID}/assignments"},
		Update:  &descriptor.Op{Method: "PUT", Path: "/roles/{roleUID}/assignments"},
		Delete:  &descriptor.Op{Method: "PUT", Path: "/roles/{roleUID}/assignments"},
		Element: &descriptor.Element{In: map[string]string{"user_id": "users", "team_id": "teams", "service_account_id": "service_accounts"}},
		Params:  []descriptor.Param{{Name: "roleUID", Field: "roleUID"}},
	}
	rc := &holder{held: map[string]any{"role_uid": "r1", "users": []any{1.0}, "teams": []any{2.0}, "service_accounts": nil}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"roleUID": "r1", "team_id": int64(3)})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"users": []any{1.0}, "teams": []any{2.0, 3.0}, "service_accounts": []any{}}
	if len(rc.sent) != 1 || !reflect.DeepEqual(rc.sent[0], want) {
		t.Fatalf("create sent %v, want %v", rc.sent, want)
	}
	if _, err := r.Reconcile(ctx, req); err != nil || len(rc.sent) != 1 {
		t.Fatalf("resync: err %v, sent %d writes, want none more", err, len(rc.sent)-1)
	}

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, obj)
	_ = unstructured.SetNestedField(obj.Object, int64(4), "spec", "team_id")
	if err := c.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if want := []any{2.0, 4.0}; !reflect.DeepEqual(rc.sent[1]["teams"], want) {
		t.Fatalf("change sent teams %v, want %v (3 out, 4 in)", rc.sent[1]["teams"], want)
	}

	_ = c.Get(ctx, req.NamespacedName, obj)
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if want := []any{2.0}; len(rc.sent) != 3 || !reflect.DeepEqual(rc.sent[2]["teams"], want) {
		t.Fatalf("delete sent %v, want teams %v", rc.sent, want)
	}
}

// A GitHub custom property value is patched alone, and removed by sending
// it with value null.
func TestElementPatch(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "RepositoryCustomProperty", Group: "repository.example.hfl.dev", Version: "v1alpha1", Plural: "repositorycustomproperties",
		Shape:   descriptor.Upsert,
		Read:    descriptor.Op{Method: "GET", Path: "/repos/{owner}/{repo}/properties/values"},
		Update:  &descriptor.Op{Method: "PATCH", Path: "/repos/{owner}/{repo}/properties/values"},
		Delete:  &descriptor.Op{Method: "PATCH", Path: "/repos/{owner}/{repo}/properties/values"},
		Element: &descriptor.Element{Key: "property_name", Patch: "properties", Unset: map[string]any{"value": nil}},
		Params:  []descriptor.Param{{Name: "owner", Field: "owner"}, {Name: "repo", Field: "repo"}},
	}
	var sent []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte(`[{"property_name": "team", "value": "old"}, {"property_name": "tier", "value": "1"}]`))
			return
		}
		var body any
		data, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(data, &body)
		sent = append(sent, body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"owner": "o", "repo": "r", "property_name": "team", "value": "platform"})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, obj)
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"properties": []any{map[string]any{"property_name": "team", "value": "platform"}}},
		map[string]any{"properties": []any{map[string]any{"property_name": "team", "value": nil}}},
	}
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("sent %v, want %v", sent, want)
	}
}

// A state is set by its call only when the read answers another; deleting
// the object makes the delete state's call.
func TestStateCalls(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "OrgFactor", Group: "org.example.hfl.dev", Version: "v1alpha1", Plural: "orgfactors",
		Shape: descriptor.Settings,
		Read:  descriptor.Op{Method: "GET", Path: "/org/factors/{factorId}"},
		State: &descriptor.State{Field: "status", Delete: "INACTIVE", Calls: map[string]descriptor.Op{
			"ACTIVE":   {Method: "POST", Path: "/org/factors/{factorId}/lifecycle/activate"},
			"INACTIVE": {Method: "POST", Path: "/org/factors/{factorId}/lifecycle/deactivate"},
		}},
		Params: []descriptor.Param{{Name: "factorId", Field: "factorId"}},
	}
	status := "INACTIVE"
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/org/factors/okta_otp/lifecycle/activate":
			status = "active" // read back in another case
		case "/org/factors/okta_otp/lifecycle/deactivate":
			status = "INACTIVE"
		}
		if req.Method != http.MethodGet {
			calls = append(calls, req.Method+" "+req.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "okta_otp", "status": status})
	}))
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"factorId": "okta_otp", "status": "ACTIVE"})
	ctx := context.Background()
	for i := 0; i < 2; i++ { // the second finds it active: no call
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, obj)
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /org/factors/okta_otp/lifecycle/activate", "POST /org/factors/okta_otp/lifecycle/deactivate"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
}

// A whole write sends the resource as read with the list changed (Okta's
// PUT /api/v1/apps/{appId} replaces the app).
func TestElementWriteWhole(t *testing.T) {
	k := &descriptor.Kind{Element: &descriptor.Element{In: map[string]string{"uri": "settings/oauthClient/redirect_uris"}, Whole: true}}
	resp := map[string]any{"label": "app", "signOnMode": "OPENID_CONNECT",
		"settings": map[string]any{"oauthClient": map[string]any{"redirect_uris": []any{"https://a"}, "grant_types": []any{"authorization_code"}}}}
	got, err := ElementWrite(k, resp, map[string]any{"uri": "https://b"}, true)
	if err != nil {
		t.Fatal(err)
	}
	oc := got["settings"].(map[string]any)["oauthClient"].(map[string]any)
	if got["label"] != "app" || got["signOnMode"] != "OPENID_CONNECT" || len(oc["grant_types"].([]any)) != 1 || len(oc["redirect_uris"].([]any)) != 2 {
		t.Fatalf("whole write = %v", got)
	}
}
