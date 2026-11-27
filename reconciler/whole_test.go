// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
)

// A kind that is one part of a resource whose write replaces it (Okta's
// SAML app settings, settings.app of an app): the update sends the whole
// resource as just read, the part replaced, not merged; a key the spec
// drops goes.
func TestUpdateWhole(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "AppSamlAppSettings", Group: "app.example.hfl.dev", Version: "v1alpha1", Plural: "appsamlappsettingses",
		Shape: descriptor.Settings, Envelope: "settings", BodyEnvelope: "settings",
		Read:   descriptor.Op{Method: "GET", Path: "/api/v1/apps/{appId}"},
		Update: &descriptor.Op{Method: "PUT", Path: "/api/v1/apps/{appId}", Whole: true},
		Params: []descriptor.Param{{Name: "appId", Field: "appId"}},
	}
	if err := parseKind(t, k); err != nil {
		t.Fatal(err)
	}
	rc := &holder{held: map[string]any{"id": "a1", "label": "My app", "signOnMode": "SAML_2_0",
		"settings": map[string]any{"app": map[string]any{"old": "x", "keep": "y"}, "signOn": map[string]any{"ssoAcsUrl": "https://a"}}}}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"appId": "a1", "app": map[string]any{"keep": "z"}})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"id": "a1", "label": "My app", "signOnMode": "SAML_2_0",
		"settings": map[string]any{"app": map[string]any{"keep": "z"}, "signOn": map[string]any{"ssoAcsUrl": "https://a"}}}
	if len(rc.sent) != 1 || rc.calls[0] != "PUT /api/v1/apps/a1" || !reflect.DeepEqual(rc.sent[0], want) {
		t.Fatalf("sent %v %v, want %v", rc.calls, rc.sent, want)
	}
	if _, err := r.Reconcile(ctx, req); err != nil || len(rc.sent) != 1 {
		t.Fatalf("resync: err %v, %d writes", err, len(rc.sent))
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, obj)
	if v, _, _ := unstructured.NestedString(obj.Object, "status", "atProvider", "app", "keep"); v != "z" {
		t.Fatalf("status %v", obj.Object["status"])
	}

	k.Read.Whole = true
	if err := parseKind(t, k); err == nil || !strings.Contains(err.Error(), "whole is for an update") {
		t.Fatalf("whole on a read: %v", err)
	}
	k.Read.Whole = false
	k.BodyArray = true
	if err := parseKind(t, k); err == nil || !strings.Contains(err.Error(), "update.whole") {
		t.Fatalf("whole with an array body: %v", err)
	}
}

// A set whose list names each element only by a URL (Okta's linked
// objects, {"_links": {"self": {"href": ".../users/<id>"}}}), each kept on
// a path of its own outside the list's: elements are keyed by the URL's
// last segment, added and removed one call each on their own paths.
func TestSetKeyURL(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "LinkValue", Group: "link.example.hfl.dev", Version: "v1alpha1", Plural: "linkvalues",
		Shape:  descriptor.SetShape,
		Set:    &descriptor.Set{Field: "associatedUserIds", Key: "userId", Param: "userId", KeyURL: "_links/self/href"},
		Read:   descriptor.Op{Method: "GET", Path: "/api/v1/users/{primaryUserId}/linkedObjects/{associatedName}"},
		Create: &descriptor.Op{Method: "PUT", Path: "/api/v1/users/{userId}/linkedObjects/{primaryName}/{primaryUserId}"},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/api/v1/users/{userId}/linkedObjects/{primaryName}"},
		Params: []descriptor.Param{{Name: "primaryName", Field: "primaryName"}, {Name: "associatedName", Field: "associatedName"}, {Name: "primaryUserId", Field: "primaryUserId"}},
	}
	if err := parseKind(t, k); err != nil {
		t.Fatal(err)
	}
	api := apifake.New(&descriptor.File{Kinds: []descriptor.Kind{*k}})
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.Method+" "+req.URL.Path)
		api.ServeHTTP(w, req)
	}))
	defer srv.Close()
	// Linked elsewhere, not listed: unlinked.
	api.Put("/api/v1/users/u9/linkedObjects/manager", map[string]any{"userId": "u9"})
	r, c, req := newTestReconciler(t, k, srv.URL, map[string]any{"primaryName": "manager", "associatedName": "subordinate", "primaryUserId": "p1",
		"associatedUserIds": []any{"u1", "u2"}})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(calls, "\n")
	for _, want := range []string{"PUT /api/v1/users/u1/linkedObjects/manager/p1", "PUT /api/v1/users/u2/linkedObjects/manager/p1", "DELETE /api/v1/users/u9/linkedObjects/manager"} {
		if !strings.Contains(got, want) {
			t.Fatalf("no %s in %v", want, calls)
		}
	}
	calls = nil
	if _, err := r.Reconcile(ctx, req); err != nil || len(calls) != 1 {
		t.Fatalf("resync: err %v, calls %v (the list keys u1 and u2 by their links)", err, calls)
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
	if got := strings.Join(calls, "\n"); got != "DELETE /api/v1/users/u1/linkedObjects/manager\nDELETE /api/v1/users/u2/linkedObjects/manager" {
		t.Fatalf("delete: %v", calls)
	}
}

// A set whose list names each element but leaves a field out (GitHub's team
// members, listed by login without the role each one's membership holds):
// Set.ReadEach reads each element and its fields join the list's, so a
// role that differs is changed and one that matches is left.
func TestSetReadEachMergesListedElements(t *testing.T) {
	k := &descriptor.Kind{
		Kind: "TeamMembers", Group: "team.example.hfl.dev", Version: "v1alpha1", Plural: "teammemberses",
		Shape: descriptor.SetShape,
		Set: &descriptor.Set{Field: "members", Key: "login", Param: "username",
			ReadEach: &descriptor.Op{Method: "GET", Path: "/teams/{team}/memberships/{username}"}},
		Read:   descriptor.Op{Method: "GET", Path: "/teams/{team}/members"},
		Create: &descriptor.Op{Method: "PUT", Path: "/teams/{team}/memberships/{username}"},
		Update: &descriptor.Op{Method: "PUT", Path: "/teams/{team}/memberships/{username}"},
		Delete: &descriptor.Op{Method: "DELETE", Path: "/teams/{team}/memberships/{username}"},
		Params: []descriptor.Param{{Name: "team", Field: "team"}},
	}
	if err := parseKind(t, k); err != nil {
		t.Fatal(err)
	}
	role := "maintainer"
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.Method+" "+req.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == "GET" && req.URL.Path == "/teams/t1/members":
			_, _ = w.Write([]byte(`[{"login":"octocat","id":1}]`))
		case req.Method == "GET" && req.URL.Path == "/teams/t1/memberships/octocat":
			_, _ = w.Write([]byte(`{"role":"` + role + `","state":"active"}`))
		case req.Method == "PUT" && req.URL.Path == "/teams/t1/memberships/octocat":
			var b map[string]any
			_ = json.NewDecoder(req.Body).Decode(&b)
			role, _ = b["role"].(string)
			_, _ = w.Write([]byte(`{"role":"` + role + `","state":"active"}`))
		default:
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	r, _, req := newTestReconciler(t, k, srv.URL, map[string]any{"team": "t1",
		"members": []any{map[string]any{"login": "octocat", "role": "member"}}})
	ctx := context.Background()
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(calls, "\n"); !strings.Contains(got, "PUT /teams/t1/memberships/octocat") || role != "member" {
		t.Fatalf("the listed member's role, read by its membership, was not changed: %v (role %s)", calls, role)
	}
	calls = nil
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatal(err)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "PUT ") {
			t.Fatalf("resync with the role matching: %v", calls)
		}
	}
}
