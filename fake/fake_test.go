// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// The fake answers a kind's server fields beside what was sent; what was
// sent wins over a server field of the same name.
func TestServerFields(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Widget
  group: things.example.hfl.dev
  version: v1alpha1
  plural: widgets
  shape: crud
  create: {method: POST, path: "/widgets"}
  read: {method: GET, path: "/widgets/{widget_id}"}
  update: {method: PATCH, path: "/widgets/{widget_id}"}
  delete: {method: DELETE, path: "/widgets/{widget_id}"}
  id: {param: widget_id, from: id}
  sample: {name: w1}
  serverFields: {created_at: "2000-01-01T00:00:00Z", url: "https://example.com/", name: server}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	do := func(method, path, body string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	created := do("POST", "/widgets", `{"name":"w1"}`)
	if created["created_at"] != "2000-01-01T00:00:00Z" || created["url"] != "https://example.com/" || created["name"] != "w1" {
		t.Errorf("create answered %v", created)
	}
	got := do("GET", "/widgets/270000001", "")
	if got["created_at"] != "2000-01-01T00:00:00Z" || got["name"] != "w1" {
		t.Errorf("read answered %v", got)
	}
	if stored, _ := New(f).Get("/widgets/270000001"); stored != nil {
		t.Error("a new fake holds something")
	}
}

// Two kinds on one URL: a CRUD item and a settings kind that writes some of
// its fields. With nothing stored the settings kind answers; once the item
// exists, its own kind does.
func TestTwoKindsOneURL(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Group
  group: g.example.hfl.dev
  version: v1alpha1
  plural: groups
  shape: crud
  create: {method: POST, path: "/groups"}
  read: {method: GET, path: "/groups/{id}"}
  update: {method: PUT, path: "/groups/{id}"}
  delete: {method: DELETE, path: "/groups/{id}"}
  id: {param: id, from: id}
  sample: {name: g1}
- kind: GroupFileTemplate
  group: g.example.hfl.dev
  version: v1alpha1
  plural: groupfiletemplates
  shape: settings
  read: {method: GET, path: "/groups/{id}"}
  update: {method: PUT, path: "/groups/{id}"}
  params: [{name: id, field: id}]
  sample: {id: "7", file_template_project_id: 3}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/groups/7"); code != http.StatusOK {
		t.Errorf("settings read of nothing stored: %d, want 200", code)
	}
	resp, err := http.Post(srv.URL+"/groups", "application/json", strings.NewReader(`{"name":"g1"}`))
	if err != nil {
		t.Fatal(err)
	}
	var made map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&made)
	resp.Body.Close()
	if code := get("/groups/" + descriptor.Text(made["id"])); code != http.StatusOK {
		t.Errorf("the group's own read: %d", code)
	}
}

// An update must send the version a read answers (Op.Carry): the fake
// refuses one without it, or with an old one, and moves it on after each.
func TestCarryRefusesStale(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Playlist
  group: apps.example.hfl.dev
  version: v1alpha1
  plural: playlists
  shape: crud
  create: {method: POST, path: "/playlists"}
  read: {method: GET, path: "/playlists/{name}"}
  update: {method: PUT, path: "/playlists/{name}", carry: [metadata.resourceVersion]}
  delete: {method: DELETE, path: "/playlists/{name}"}
  id: {param: name, from: metadata/name}
  sample: {metadata: {name: p1}}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	do := func(method, path, body string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	_, made := do("POST", "/playlists", `{"metadata":{"name":"p1"},"title":"a"}`)
	rv := made["metadata"].(map[string]any)["resourceVersion"]
	if rv == nil {
		t.Fatalf("create answered no resourceVersion: %v", made)
	}
	if code, _ := do("PUT", "/playlists/p1", `{"metadata":{"name":"p1"},"title":"b"}`); code != http.StatusConflict {
		t.Fatalf("update without the version: %d, want 409", code)
	}
	code, upd := do("PUT", "/playlists/p1", `{"metadata":{"name":"p1","resourceVersion":"`+rv.(string)+`"},"title":"b"}`)
	if code != http.StatusOK || upd["metadata"].(map[string]any)["resourceVersion"] == rv {
		t.Fatalf("update with the version: %d %v, want 200 and a new version", code, upd)
	}
	if code, _ := do("PUT", "/playlists/p1", `{"metadata":{"name":"p1","resourceVersion":"`+rv.(string)+`"},"title":"c"}`); code != http.StatusConflict {
		t.Fatalf("update with an old version: %d, want 409", code)
	}
}

// A CRUD item deleted on a URL other kinds share (GitLab's /users/{id}: a
// user, a service account at /users/{user_id}, the user's avatar setting
// and its identities, found in the user) is gone for all of them: a read
// answers 404, not the settings kind's object nor the list's.
func TestDeletedItemOnSharedURL(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: ServiceAccount
  group: u.example.hfl.dev
  version: v1alpha1
  plural: serviceaccounts
  shape: crud
  create: {method: POST, path: "/service_accounts"}
  read: {method: GET, path: "/users/{user_id}"}
  update: {method: PUT, path: "/users/{user_id}"}
  delete: {method: DELETE, path: "/users/{user_id}"}
  id: {param: user_id, from: id}
  sample: {name: sa}
- kind: User
  group: u.example.hfl.dev
  version: v1alpha1
  plural: users
  shape: crud
  create: {method: POST, path: "/users"}
  read: {method: GET, path: "/users/{id}"}
  update: {method: PUT, path: "/users/{id}"}
  delete: {method: DELETE, path: "/users/{id}"}
  id: {param: id, from: id}
  sample: {name: u}
- kind: UserAvatar
  group: u.example.hfl.dev
  version: v1alpha1
  plural: useravatars
  shape: settings
  read: {method: GET, path: "/users/{id}"}
  update: {method: PUT, path: "/users/{id}"}
  params: [{name: id, field: id}]
  sample: {id: "7", avatar: a.png}
- kind: UserIdentity
  group: u.example.hfl.dev
  version: v1alpha1
  plural: useridentities
  shape: upsert
  read: {method: GET, path: "/users/{id}", find: {list: identities, field: provider, param: provider}}
  update: {method: PUT, path: "/users/{id}"}
  delete: {method: DELETE, path: "/users/{id}/identities/{provider}"}
  params: [{name: id, field: id}, {name: provider, field: provider}]
  sample: {id: "7", provider: p, extern_uid: x}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	do := func(method, path, body string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	for _, create := range []string{"/users", "/service_accounts"} {
		code, made := do("POST", create, `{"name":"n"}`)
		if code != http.StatusCreated {
			t.Fatalf("POST %s: %d", create, code)
		}
		item := "/users/" + descriptor.Text(made["id"])
		if code, got := do("GET", item, ""); code != http.StatusOK || got["name"] != "n" {
			t.Errorf("GET %s after create: %d %v", item, code, got)
		}
		if code, _ := do("DELETE", item, ""); code != http.StatusNoContent {
			t.Errorf("DELETE %s: %d", item, code)
		}
		if code, got := do("GET", item, ""); code != http.StatusNotFound {
			t.Errorf("GET %s after delete: %d %v, want 404", item, code, got)
		}
	}
	if code, _ := do("GET", "/users/7", ""); code != http.StatusOK {
		t.Errorf("the avatar setting of a user never deleted: %d, want 200", code)
	}
}

// A kind that is a mark in another kind's object (Cloudflare's email
// routing DNS: on by POST and off by DELETE .../email/routing/dns, read as
// enabled in the zone's email routing settings, GET .../email/routing): the
// read answers its absent values until it is made, in the settings kind's
// object as in its own, and again once it is deleted; the settings object
// stays.
func TestMarkInAnotherKindsObject(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: RoutingSettings
  group: e.example.hfl.dev
  version: v1alpha1
  plural: routingsettings
  shape: settings
  envelope: result
  read: {method: GET, path: "/zones/{zone_id}/email/routing"}
  update: {method: PATCH, path: "/zones/{zone_id}/email/routing"}
  params: [{name: zone_id, field: zone_id}]
  sample: {zone_id: z, skip_wizard: true}
  serverFields: {name: example.net}
- kind: RoutingDNS
  group: e.example.hfl.dev
  version: v1alpha1
  plural: routingdnses
  shape: upsert
  envelope: result
  create: {method: POST, path: "/zones/{zone_id}/email/routing/dns"}
  read: {method: GET, path: "/zones/{zone_id}/email/routing", absent: {enabled: false}}
  delete: {method: DELETE, path: "/zones/{zone_id}/email/routing/dns"}
  params: [{name: zone_id, field: zone_id}]
  sample: {zone_id: z}
  serverFields: {enabled: true, status: ready}
`))
	if err != nil {
		t.Fatal(err)
	}
	for _, settingsFirst := range []bool{false, true} {
		srv := httptest.NewServer(New(f))
		do := func(method, path, body string) (int, map[string]any) {
			t.Helper()
			req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var out struct {
				Result map[string]any `json:"result"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			return resp.StatusCode, out.Result
		}
		const read, dns = "/zones/z/email/routing", "/zones/z/email/routing/dns"
		if settingsFirst {
			if code, _ := do("PATCH", read, `{"skip_wizard":true}`); code != http.StatusOK {
				t.Fatalf("settings PATCH: %d", code)
			}
		}
		if code, got := do("GET", read, ""); code != http.StatusOK || got["enabled"] != false {
			t.Errorf("settings first %v: read before the create: %d %v, want enabled false", settingsFirst, code, got)
		}
		if code, _ := do("DELETE", dns, ""); code != http.StatusNotFound {
			t.Errorf("settings first %v: delete of none: %d, want 404", settingsFirst, code)
		}
		if code, _ := do("POST", dns, `{}`); code != http.StatusCreated {
			t.Fatalf("settings first %v: create: %d", settingsFirst, code)
		}
		if code, got := do("GET", read, ""); code != http.StatusOK || got["enabled"] != true {
			t.Errorf("settings first %v: read after the create: %d %v, want enabled true", settingsFirst, code, got)
		}
		if code, _ := do("POST", dns, `{}`); code < 400 {
			t.Errorf("settings first %v: second create: %d, want refused", settingsFirst, code)
		}
		if code, _ := do("DELETE", dns, ""); code != http.StatusNoContent {
			t.Errorf("settings first %v: delete: %d", settingsFirst, code)
		}
		code, got := do("GET", read, "")
		if code != http.StatusOK || got["enabled"] != false {
			t.Errorf("settings first %v: read after the delete: %d %v, want enabled false", settingsFirst, code, got)
		}
		if settingsFirst && got["skip_wizard"] != true {
			t.Errorf("the settings kind's object is gone after the mark's delete: %v", got)
		}
		srv.Close()
	}
}

// Two kinds that find their objects in one list (a service account's
// tokens, plain and rotating) answer it empty, not 404, when it holds none.
func TestSharedListEmpty(t *testing.T) {
	kind := func(name string) string {
		return `- kind: ` + name + `
  group: t.example.hfl.dev
  version: v1alpha1
  plural: ` + strings.ToLower(name) + `s
  shape: crud
  create: {method: POST, path: "/serviceaccounts/{sa}/tokens"}
  read: {method: GET, path: "/serviceaccounts/{sa}/tokens", find: {field: id, param: id}}
  delete: {method: DELETE, path: "/serviceaccounts/{sa}/tokens/{id}"}
  params: [{name: sa, field: sa}]
  id: {param: id, from: id}
  sample: {sa: "1", name: t}
`
	}
	f, err := descriptor.Parse([]byte("vendor: example\nconnection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}\nkinds:\n" + kind("Token") + kind("RotatingToken")))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/serviceaccounts/1/tokens")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got []any
	if err := json.NewDecoder(resp.Body).Decode(&got); resp.StatusCode != http.StatusOK || err != nil || len(got) != 0 {
		t.Fatalf("empty shared list: %d %v %v, want 200 []", resp.StatusCode, got, err)
	}
}

// A mixed kind (made by GraphQL, read and changed by REST) shares its item
// path with a settings kind that writes its own path (GitHub's
// EnterpriseOrganization and OrganizationSettings on /orgs/{id}): once the
// mixed kind's object exists, a write to the path is that object's, not a
// second object kept under the path.
func TestMixedKindOwnsItsPath(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Org
  group: o.example.hfl.dev
  version: v1alpha1
  plural: orgs
  shape: crud
  create:
    method: POST
    path: /graphql
    graphql:
      operation: CreateOrg
      query: "mutation CreateOrg($input: CreateOrgInput!) { createOrg(input: $input) { org { login } } }"
      result: createOrg/org
      variables: {input: {login: "{login}"}}
  read: {method: GET, path: "/orgs/{id}"}
  update: {method: PATCH, path: "/orgs/{id}"}
  delete: {method: DELETE, path: "/orgs/{id}"}
  id: {param: id, from: login}
  sample: {login: o1}
- kind: OrgSettings
  group: o.example.hfl.dev
  version: v1alpha1
  plural: orgsettingses
  shape: settings
  read: {method: GET, path: "/orgs/{id}"}
  update: {method: PATCH, path: "/orgs/{id}"}
  params: [{name: id, field: org}]
  sample: {org: o2, description: d}
`))
	if err != nil {
		t.Fatal(err)
	}
	api := New(f)
	srv := httptest.NewServer(api)
	defer srv.Close()
	q := `{"operationName":"CreateOrg","query":"mutation CreateOrg($input: CreateOrgInput!) { createOrg(input: $input) { org { login } } }","variables":{"input":{"login":"o1"}}}`
	resp, err := http.Post(srv.URL+"/graphql", "application/json", strings.NewReader(q))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if api.Len() != 1 {
		t.Fatalf("after the create: %d objects, want 1: %v", api.Len(), api.Paths())
	}
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/orgs/o1", strings.NewReader(`{"description":"changed"}`))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /orgs/o1: %d", resp.StatusCode)
	}
	if api.Len() != 1 {
		t.Errorf("after the PATCH: %d objects, want the org alone: %v", api.Len(), api.Paths())
	}
}
