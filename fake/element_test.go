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

// An account holds its secrets, which another kind reads from the
// account's own URL: with none made, the account answers as its kind does
// (its secrets as its server field has them); with one, the list holds it.
func TestListInAnotherKindsItem(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: ServiceAccount
  group: s.example.hfl.dev
  version: v1alpha1
  plural: serviceaccounts
  shape: crud
  create: {method: POST, path: "/orgs/{orgId}/serviceAccounts"}
  read: {method: GET, path: "/orgs/{orgId}/serviceAccounts/{clientId}"}
  delete: {method: DELETE, path: "/orgs/{orgId}/serviceAccounts/{clientId}"}
  params: [{name: orgId, field: orgId}]
  id: {param: clientId, from: clientId}
  sample: {orgId: o1, name: sa}
  serverFields: {secrets: []}
- kind: ServiceAccountSecret
  group: s.example.hfl.dev
  version: v1alpha1
  plural: serviceaccountsecrets
  shape: crud
  create: {method: POST, path: "/orgs/{orgId}/serviceAccounts/{clientId}/secrets"}
  read: {method: GET, path: "/orgs/{orgId}/serviceAccounts/{clientId}", find: {list: secrets, field: id, param: secretId}}
  delete: {method: DELETE, path: "/orgs/{orgId}/serviceAccounts/{clientId}/secrets/{secretId}"}
  params: [{name: orgId, field: orgId}, {name: clientId, field: clientId}]
  id: {param: secretId, from: id}
  sample: {orgId: o1, clientId: c1}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	post := func(path, body string) map[string]any {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	get := func(path string) string {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		b, _ := json.Marshal(out["secrets"])
		return string(b)
	}
	sa := post("/orgs/o1/serviceAccounts", `{"name":"sa"}`)
	at := "/orgs/o1/serviceAccounts/" + descriptor.Text(sa["clientId"])
	if got := get(at); got != "[]" {
		t.Errorf("no secrets: %s, want [] (the account's own)", got)
	}
	post(at+"/secrets", `{"hours":8}`)
	if got := get(at); !strings.Contains(got, `"hours":8`) {
		t.Errorf("one secret: %s, want it listed", got)
	}
}

// HoldFor makes the resource an element goes in where a spec other than
// the sample names it (a rehearsal's live spec: the run's repository), and
// leaves a resource stored there as it is.
func TestHoldForOtherParams(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: RepositoryCustomProperty
  group: repository.example.hfl.dev
  version: v1alpha1
  plural: repositorycustomproperties
  shape: upsert
  read: {method: GET, path: "/repos/{owner}/{repo}/properties/values"}
  update: {method: PATCH, path: "/repos/{owner}/{repo}/properties/values"}
  delete: {method: PATCH, path: "/repos/{owner}/{repo}/properties/values"}
  element: {key: property_name, patch: properties, unset: {value: null}}
  params: [{name: owner, field: owner}, {name: repo, field: repo}]
  sample: {owner: o, repo: sample, property_name: p, value: v}
`))
	if err != nil {
		t.Fatal(err)
	}
	api := New(f)
	srv := httptest.NewServer(api)
	defer srv.Close()
	get := func(path string) int {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := get("/repos/o/run/properties/values"); code != http.StatusNotFound {
		t.Fatalf("a repository no spec names: %d, want 404", code)
	}
	api.HoldFor(&f.Kinds[0], map[string]any{"owner": "o", "repo": "run", "property_name": "p", "value": "a"})
	if code := get("/repos/o/run/properties/values"); code != http.StatusOK {
		t.Errorf("the repository the spec names: %d, want 200", code)
	}
	if code := get("/repos/o/sample/properties/values"); code != http.StatusOK {
		t.Errorf("the sample's repository: %d, want 200", code)
	}
	api.HoldFor(&f.Kinds[0], map[string]any{"owner": "o", "property_name": "p"}) // no repo: nothing
	if code := get("/repos/o//properties/values"); code == http.StatusOK {
		t.Errorf("a spec without a repo made a holder")
	}
}
