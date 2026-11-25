// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// A draft's calls are the generator's guesses: the fake has no route for
// them, so nothing can pass against a draft.
func TestNoRoutesForDrafts(t *testing.T) {
	f, err := descriptor.Parse([]byte(`vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Assignment
  group: things.example.hfl.dev
  version: v1alpha1
  plural: assignments
  shape: upsert
  read: {method: GET, path: "/orgs/{org}/assignments/{name}"}
  update: {method: PUT, path: "/orgs/{org}/assignments/{name}"}
  params: [{name: org, field: org}, {name: name, field: name}]
  terraform: [example_assignment]
  readiness: draft
  draft: {open: ["delete: no DELETE on the item path"]}
`))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(f))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/orgs/acme/assignments/a", strings.NewReader(`{"x": 1}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Errorf("PUT on a draft's path: %d", resp.StatusCode)
	}
}
