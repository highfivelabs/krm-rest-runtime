// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// An item with no path of its own (Datadog's team connections): made in
// bulk on its collection, listed there, removed by a DELETE of the
// collection whose body names it.
func TestItemlessDelete(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: TeamConnection
  group: team.example.hfl.dev
  version: v1alpha1
  plural: teamconnections
  shape: crud
  envelope: data
  bodyEnvelope: data
  create: {method: POST, path: "/connections", bodyArray: true, envelope: data/0}
  read: {method: GET, path: "/connections", query: {"filter[ids]": "{id}"}, find: {list: data, field: id, param: id}}
  delete: {method: DELETE, path: "/connections", bodyArray: true, body: {id: "{id}", type: team_connection}}
  id: {param: id, from: id}
`))
	if err != nil {
		t.Fatal(err)
	}
	api := New(f)
	srv := httptest.NewServer(api)
	defer srv.Close()
	do := func(method, path, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := do("POST", "/connections", `{"data":[{"type":"team_connection"}]}`); st != http.StatusCreated {
		t.Fatalf("create: %d", st)
	}
	if api.Len() != 1 {
		t.Fatalf("stored %v", api.Paths())
	}
	if st := do("DELETE", "/connections", `{"data":[{"id":"nope","type":"team_connection"}]}`); st != http.StatusNotFound {
		t.Fatalf("delete of another: %d, want 404", st)
	}
	if st := do("DELETE", "/connections", `{"data":[{"id":"270000001","type":"team_connection"}]}`); st != http.StatusOK {
		t.Fatalf("delete: %d", st)
	}
	if api.Len() != 0 {
		t.Fatalf("still stored: %v", api.Paths())
	}
}
