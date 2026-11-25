// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

func do(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// The fake refuses to delete a kind with beforeDelete until every call was
// made, as GitLab refuses to delete an environment that is not stopped.
func TestFakeNeedsBeforeDelete(t *testing.T) {
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apifake.New(f))
	defer srv.Close()
	code, obj := do(t, "POST", srv.URL+"/projects/p1/environments", map[string]any{"name": "review"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d", code)
	}
	item := srv.URL + "/projects/p1/environments/" + jsonNum(obj["id"])
	if code, _ := do(t, "DELETE", item, nil); code != http.StatusForbidden {
		t.Fatalf("delete before stop: %d, want 403", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/projects/p1/environments/999/stop", nil); code != http.StatusNotFound {
		t.Fatalf("stop of a missing environment: %d, want 404", code)
	}
	if code, _ := do(t, "POST", item+"/stop", nil); code != http.StatusOK {
		t.Fatalf("stop: %d", code)
	}
	if code, _ := do(t, "DELETE", item, nil); code != http.StatusNoContent {
		t.Fatalf("delete after stop: %d, want 204", code)
	}
}

// The fake reads a splitSpace field back as an array, as GitLab does an
// application's scopes; a kind without the rule would see drift.
func TestFakeReadsSplitSpaceAsArray(t *testing.T) {
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apifake.New(f))
	defer srv.Close()
	_, obj := do(t, "POST", srv.URL+"/applications", map[string]any{"name": "a", "scopes": "api read_user"})
	_, got := do(t, "GET", srv.URL+"/applications/"+jsonNum(obj["id"]), nil)
	l, ok := got["scopes"].([]any)
	if !ok || len(l) != 2 || l[0] != "api" || l[1] != "read_user" {
		t.Fatalf("scopes read back as %#v, want [api read_user]", got["scopes"])
	}
}

// Without its beforeDelete, the Environment kind is not deleted on the fake;
// without its compare rule, the Application kind is updated on every read.
// The fake keeps the full descriptor: only the controller's copy loses the
// option.
func TestOptionsAreNeeded(t *testing.T) {
	v := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	kind := func(name string) *descriptor.Kind {
		for i := range f.Kinds {
			if f.Kinds[i].Kind == name {
				k := f.Kinds[i]
				return &k
			}
		}
		t.Fatalf("no kind %s", name)
		return nil
	}
	ctx := context.Background()

	t.Run("beforeDelete", func(t *testing.T) {
		srv := newServer(apifake.New(f))
		defer srv.Close()
		k := kind("Environment")
		k.BeforeDelete = nil
		c, r, req := setup(t, f, k, v, srv.URL, nil)
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(k.GVK())
		_ = c.Get(ctx, req.NamespacedName, got)
		if err := c.Delete(ctx, got); err != nil {
			t.Fatal(err)
		}
		_, _ = r.Reconcile(ctx, req)
		_ = c.Get(ctx, req.NamespacedName, got)
		if reason := readyReason(got); reason != "DeleteFailed" {
			t.Fatalf("Ready reason %q, want DeleteFailed (403 until stopped)", reason)
		}
	})

	t.Run("compare", func(t *testing.T) {
		srv := newServer(apifake.New(f))
		defer srv.Close()
		k := kind("Application")
		k.Compare = nil
		c, r, req := setup(t, f, k, v, srv.URL, nil)
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(k.GVK())
		_ = c.Get(ctx, req.NamespacedName, got)
		if reason := readyReason(got); reason != "Updated" {
			t.Fatalf("Ready reason %q, want Updated (scopes read back as an array)", reason)
		}
	})

	t.Run("unordered", func(t *testing.T) {
		srv := newServer(apifake.New(f))
		defer srv.Close()
		k := kind("Schedule")
		k.Compare = nil
		c, r, req := setup(t, f, k, v, srv.URL, nil)
		for i := 0; i < 2; i++ {
			if _, err := r.Reconcile(ctx, req); err != nil {
				t.Fatal(err)
			}
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(k.GVK())
		_ = c.Get(ctx, req.NamespacedName, got)
		if reason := readyReason(got); reason != "Updated" {
			t.Fatalf("Ready reason %q, want Updated (layers read back reversed)", reason)
		}
	})

	t.Run("createOnly", func(t *testing.T) {
		srv := newServer(apifake.New(f))
		defer srv.Close()
		k := kind("Field")
		k.CreateOnly = nil
		c, r, req := setup(t, f, k, v, srv.URL, nil)
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(k.GVK())
		_ = c.Get(ctx, req.NamespacedName, got)
		_ = unstructured.SetNestedField(got.Object, "F2", "spec", "display_name")
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		_, _ = r.Reconcile(ctx, req)
		_ = c.Get(ctx, req.NamespacedName, got)
		if reason := readyReason(got); reason != "SyncFailed" {
			t.Fatalf("Ready reason %q, want SyncFailed (the update names data_type)", reason)
		}
	})
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// A hook that sends something different each time for the same spec
// (GitHub seals a secret with a fresh nonce) is not a change: the second
// reconcile finds nothing to do.
func TestHookOutputIsNotAChange(t *testing.T) {
	v := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	var k *descriptor.Kind
	for i := range f.Kinds {
		if f.Kinds[i].Kind == "Secret" {
			k = &f.Kinds[i]
		}
	}
	srv := newServer(apifake.New(f))
	defer srv.Close()
	n := 0
	seal := func(_ context.Context, _ *rest.Client, _ *descriptor.Kind, _ map[string]string, body map[string]any) error {
		n++
		body["value"] = fmt.Sprintf("sealed-%v-%d", body["value"], n)
		return nil
	}
	c, r, req := setup(t, f, k, v, srv.URL, nil)
	r.Hooks = []reconciler.Hook{seal}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, got)
	if reason := readyReason(got); reason != "Unchanged" {
		t.Fatalf("Ready reason %q, want Unchanged (the spec did not change)", reason)
	}
}

// The fake reads an enabledObject field back as {"enabled": value}, as
// GitHub does a branch protection's enforce_admins.
func TestFakeReadsEnabledObject(t *testing.T) {
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(apifake.New(f))
	defer srv.Close()
	if code, _ := do(t, "PUT", srv.URL+"/repos/r/branches/main/protection", map[string]any{"enforce_admins": true}); code >= 300 {
		t.Fatalf("PUT: %d", code)
	}
	_, got := do(t, "GET", srv.URL+"/repos/r/branches/main/protection", nil)
	if m, ok := got["enforce_admins"].(map[string]any); !ok || m["enabled"] != true {
		t.Fatalf("enforce_admins read back as %#v, want {enabled: true}", got["enforce_admins"])
	}
}
