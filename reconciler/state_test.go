// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

func TestCarried(t *testing.T) {
	op := &descriptor.Op{Carry: []string{"metadata.resourceVersion", "sha", "branch"}}
	body := map[string]any{"metadata": map[string]any{"name": "a"}, "spec": map[string]any{"x": 1.0}}
	obs := map[string]any{"metadata": map[string]any{"name": "a", "resourceVersion": "7"}, "sha": "abc"}
	got := Carried(op, body, obs, map[string]any{"branch": "main", "sha": "stale"})
	want := map[string]any{
		"metadata": map[string]any{"name": "a", "resourceVersion": "7"},
		"spec":     map[string]any{"x": 1.0},
		"sha":      "abc", // the read's, not the spec's
		"branch":   "main",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Carried = %v, want %v", got, want)
	}
	if _, ok := body["metadata"].(map[string]any)["resourceVersion"]; ok || body["sha"] != nil {
		t.Fatalf("Carried changed its body: %v", body)
	}
	if got := Carried(&descriptor.Op{}, body, obs, nil); !reflect.DeepEqual(got, body) {
		t.Fatalf("no carry: %v, want the body", got)
	}
}

// A GitHub file's delete must send its blob sha, read just before, with
// the commit message and branch from the spec.
func TestDeleteCarriesFromRead(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "RepositoryFile", Group: "repository.example.hfl.dev", Version: "v1alpha1", Plural: "repositoryfiles",
		Shape:  descriptor.Upsert,
		Read:   descriptor.Op{Method: "GET", Path: "/repos/{repo}/contents/{path}"},
		Update: &descriptor.Op{Method: "PUT", Path: "/repos/{repo}/contents/{path}", Carry: []string{"sha"}},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/repos/{repo}/contents/{path}", Carry: []string{"sha", "message", "branch"}},
		Params: []descriptor.Param{{Name: "repo", Field: "repo"}, {Name: "path", Field: "path"}},
	}
	var deleted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.Method {
		case "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": "b1", "message": "add", "branch": "main"})
		case "DELETE":
			data, _ := io.ReadAll(req.Body)
			_ = json.Unmarshal(data, &deleted)
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"repo": "web", "path": "README.md", "message": "add", "branch": "main"})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	// The read answers no message or branch on a real API: the spec's go.
	if err := c.Delete(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"sha": "b1", "message": "add", "branch": "main"}
	if !reflect.DeepEqual(deleted, want) {
		t.Fatalf("DELETE body = %v, want %v", deleted, want)
	}
}

// A read that answers 200 with the absent marker is not found: the first
// write is the create.
func TestAbsentReadCreates(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "PushBasedLogExport", Group: "push.example.hfl.dev", Version: "v1alpha1", Plural: "pushbasedlogexports",
		Shape:  descriptor.Upsert,
		Create: &descriptor.Op{Method: "POST", Path: "/groups/{groupId}/pushBasedLogExport"},
		Read:   descriptor.Op{Method: "GET", Path: "/groups/{groupId}/pushBasedLogExport", Absent: map[string]any{"state": "UNCONFIGURED"}},
		Update: &descriptor.Op{Method: "PATCH", Path: "/groups/{groupId}/pushBasedLogExport"},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/groups/{groupId}/pushBasedLogExport"},
		Params: []descriptor.Param{{Name: "groupId", Field: "groupId"}},
	}
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.Method)
		switch req.Method {
		case "GET":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "UNCONFIGURED", "links": []any{}})
		case "POST":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "ACTIVE", "bucketName": "logs"})
		default:
			http.Error(w, "no", http.StatusBadRequest)
		}
	}))
	defer srv.Close()
	r, _, req := newTestReconciler(t, k, srv.URL, map[string]any{"groupId": "g1", "bucketName": "logs"})
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, " ") != "GET POST" {
		t.Fatalf("calls = %v, want GET POST", calls)
	}
	op := &descriptor.Op{Absent: map[string]any{"id": nil}}
	if !op.IsAbsent(map[string]any{}) || !op.IsAbsent(map[string]any{"id": ""}) || op.IsAbsent(map[string]any{"id": "x"}) {
		t.Fatal("absent {id: null}: no id or an empty one is absent; an id is not")
	}
}

// An adopting kind whose list has no match says what it looked for.
func TestAdoptNoMatch(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "PolicyMfaDefault", Group: "policy.example.hfl.dev", Version: "v1alpha1", Plural: "policymfadefaults",
		Shape:  descriptor.CRUD,
		Read:   descriptor.Op{Method: "GET", Path: "/policies/{policyId}"},
		Update: &descriptor.Op{Method: "PUT", Path: "/policies/{policyId}"},
		Adopt:  &descriptor.Adopt{Op: descriptor.Op{Method: "GET", Path: "/policies", Query: map[string]string{"type": "MFA_ENROLL"}}, Match: map[string]string{"system": "true"}},
		Forget: "Okta makes it.",
		ID:     &descriptor.ID{Param: "policyId", From: "id"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("type") != "MFA_ENROLL" {
			t.Errorf("list query = %q", req.URL.RawQuery)
		}
		_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": "p1", "system": false}})
	}))
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"name": "Default Policy"})
	_, _ = r.Reconcile(context.Background(), req)
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	if err := c.Get(context.Background(), req.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	if len(conds) == 0 || !strings.Contains(conds[0].(map[string]any)["message"].(string), "system=true") {
		t.Fatalf("conditions = %v, want a message naming system=true", conds)
	}
}
