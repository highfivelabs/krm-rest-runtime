// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

func rotatingToken() descriptor.Kind {
	q := map[string]string{"region": "{region}"}
	return descriptor.Kind{
		Kind: "CloudAccessPolicyRotatingToken", Group: "cloud.example.hfl.dev", Version: "v1alpha1", Plural: "cloudaccesspolicyrotatingtokens", Shape: descriptor.CRUD,
		Create:        &descriptor.Op{Method: "POST", Path: "/v1/tokens", Query: q},
		Read:          descriptor.Op{Method: "GET", Path: "/v1/tokens/{id}", Query: q},
		Delete:        &descriptor.Op{Method: "DELETE", Path: "/v1/tokens/{id}", Query: q},
		ID:            &descriptor.ID{Param: "id", From: "id"},
		QueryFields:   []string{"region", "name_prefix", "expire_after", "early_rotation_window", "delete_on_destroy"},
		SecretOutputs: []string{"token"},
		ServerFields:  map[string]any{"token": "glc_example"},
		Rotate: &descriptor.Rotate{Prefix: "name_prefix", Lifetime: "expire_after", Window: "early_rotation_window",
			DeleteOnDelete: "delete_on_destroy", Name: "name", ExpiresAt: "expiresAt"},
	}
}

// A rotating token is made named after its prefix and expiry; the object
// is requeued for the time it is to be replaced; then a new token is made,
// written to the Secret, and the old one deleted. Deleting the object
// leaves the last token unless delete_on_destroy.
func TestRotatingToken(t *testing.T) {
	k := rotatingToken()
	rec, srv := record(t, k)
	spec := map[string]any{"region": "us", "accessPolicyId": "ap1", "name_prefix": "ci", "expire_after": "24h",
		"early_rotation_window": "1h", "writeSecretRef": map[string]any{"name": "ci-token"}}
	r, c, req := newTestReconciler(t, &k, srv.URL, spec)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := t0
	r.Now = func() time.Time { return now }
	ctx := context.Background()
	reconcileAt := func(at time.Time) (time.Duration, *unstructured.Unstructured) {
		t.Helper()
		now = at
		res, err := r.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile at %s: %v", at, err)
		}
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(k.GVK())
		if err := c.Get(ctx, req.NamespacedName, obj); err != nil {
			t.Fatal(err)
		}
		conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
		if len(conds) == 0 || conds[0].(map[string]any)["status"] != "True" {
			t.Fatalf("at %s not Ready: %v", at, conds)
		}
		return res.RequeueAfter, obj
	}

	after, obj := reconcileAt(t0)
	create := rec.last(t, "POST")
	body := create.Body.(map[string]any)
	expires := t0.Add(24 * time.Hour)
	if body["name"] != fmt.Sprintf("ci-%d", expires.Unix()) || body["expiresAt"] != expires.Format(time.RFC3339) || body["accessPolicyId"] != "ap1" {
		t.Fatalf("create body %v", body)
	}
	for _, f := range []string{"name_prefix", "expire_after", "early_rotation_window", "region"} {
		if _, ok := body[f]; ok {
			t.Fatalf("create sent %s: %v", f, body)
		}
	}
	first, _, _ := unstructured.NestedString(obj.Object, "status", "id")
	rotateAt, _, _ := unstructured.NestedString(obj.Object, "status", "rotation", "rotateAt")
	if rotateAt != t0.Add(23*time.Hour).Format(time.RFC3339) || after != Resync {
		t.Fatalf("rotateAt %q, requeue %s", rotateAt, after)
	}
	sec := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: "default", Name: "ci-token"}, sec); err != nil || string(sec.Data["token"]) != "glc_example" {
		t.Fatalf("secret: %v %v", err, sec.Data)
	}

	// Ten seconds before the window: read, nothing made, requeued for it.
	posts := len(rec.calls)
	after, obj = reconcileAt(t0.Add(23*time.Hour - 10*time.Second))
	for _, call := range rec.calls[posts:] {
		if call.Method != "GET" {
			t.Fatalf("before the window: %s %s", call.Method, call.Path)
		}
	}
	if after != 10*time.Second {
		t.Fatalf("requeue %s, want 10s", after)
	}

	// In the window: a new token, then the old one deleted.
	after, obj = reconcileAt(t0.Add(23 * time.Hour))
	second, _, _ := unstructured.NestedString(obj.Object, "status", "id")
	if second == "" || second == first {
		t.Fatalf("not rotated: id %q, was %q", second, first)
	}
	del := rec.last(t, "DELETE")
	if del.Path != "/v1/tokens/"+first || del.Query["region"] != "us" {
		t.Fatalf("old token delete: %s %v", del.Path, del.Query)
	}
	if body := rec.last(t, "POST").Body.(map[string]any); body["name"] != fmt.Sprintf("ci-%d", t0.Add(47*time.Hour).Unix()) {
		t.Fatalf("new token's name %v", body["name"])
	}
	if reason := obj.Object["status"].(map[string]any)["conditions"].([]any)[0].(map[string]any)["reason"]; reason != "Rotated" {
		t.Fatalf("reason %v", reason)
	}
	if retired, ok, _ := unstructured.NestedSlice(obj.Object, "status", "rotation", "retired"); ok {
		t.Fatalf("retired left: %v", retired)
	}
	if after != Resync {
		t.Fatalf("requeue after rotating %s", after)
	}

	// Deleting the object leaves the last token (delete_on_destroy unset).
	calls := len(rec.calls)
	remove(t, r, c, req)
	for _, call := range rec.calls[calls:] {
		if call.Method == "DELETE" {
			t.Fatalf("deleted the last token: %s", call.Path)
		}
	}
	if _, ok := rec.api.Get("/v1/tokens/" + second); !ok {
		t.Fatal("the last token is gone")
	}
}

// delete_on_destroy deletes the last token with the object; a window
// longer than the lifetime is refused.
func TestRotatingTokenDeleteAndInvalid(t *testing.T) {
	k := rotatingToken()
	rec, srv := record(t, k)
	r, c, req := newTestReconciler(t, &k, srv.URL, map[string]any{"region": "us", "accessPolicyId": "ap1", "name_prefix": "ci",
		"expire_after": "1h", "early_rotation_window": "10m", "delete_on_destroy": true})
	obj := run(t, r, c, req)
	id, _, _ := unstructured.NestedString(obj.Object, "status", "id")
	remove(t, r, c, req)
	if del := rec.last(t, "DELETE"); del.Path != "/v1/tokens/"+id {
		t.Fatalf("delete %s, want the token %s", del.Path, id)
	}

	r, _, req = newTestReconciler(t, &k, srv.URL, map[string]any{"region": "us", "accessPolicyId": "ap1", "name_prefix": "ci",
		"expire_after": "1h", "early_rotation_window": "2h"})
	_, _ = r.Reconcile(context.Background(), req) // InvalidSpec, retried with backoff
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(k.GVK())
	_ = r.Client.Get(context.Background(), req.NamespacedName, got)
	conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
	if len(conds) == 0 || conds[0].(map[string]any)["reason"] != "InvalidSpec" {
		t.Fatalf("a window longer than the lifetime: %v", conds)
	}
}
