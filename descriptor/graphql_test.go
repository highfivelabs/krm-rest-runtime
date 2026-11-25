// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"reflect"
	"testing"
)

func TestGraphQLRequestAndExtract(t *testing.T) {
	g := &GraphQL{
		Operation: "Create",
		Query:     "mutation Create($groupId: GroupID!, $input: X!) { x }",
		Variables: map[string]any{
			"groupId": "gid://gitlab/Group/{groupId}",
			"path":    "{groupPath}",
			"input":   map[string]any{"params": BodyTemplate, "ttl": "{ttl}", "missing": "{nope}", "on": true},
		},
	}
	params := map[string]string{"groupId": "42", "groupPath": "a/b"}
	body := map[string]any{"name": "n", "ttl": float64(30)}
	req, err := g.Request(params, body)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"query": g.Query, "operationName": "Create",
		"variables": map[string]any{
			"groupId": "gid://gitlab/Group/42",
			"path":    "a/b",
			"input":   map[string]any{"params": body, "ttl": float64(30), "on": true},
		},
	}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("Request = %#v, want %#v", req, want)
	}
	isParam := func(n string) bool { return n == "groupId" || n == "groupPath" }
	gotParams, gotBody := g.Extract(req["variables"].(map[string]any), isParam)
	if !reflect.DeepEqual(gotParams, params) {
		t.Errorf("Extract params = %v, want %v", gotParams, params)
	}
	if !reflect.DeepEqual(gotBody, body) {
		t.Errorf("Extract body = %v, want %v", gotBody, body)
	}
	// Text around a placeholder needs its value.
	if _, err := g.Request(map[string]string{"groupPath": "a"}, nil); err == nil {
		t.Error("Request without groupId: no error")
	}
}

func TestGraphQLValidate(t *testing.T) {
	ok := `vendor: v
connection: {group: core.v, version: v1alpha1, kind: Connection}
kinds:
- kind: S
  group: g.v
  version: v1alpha1
  plural: ss
  shape: settings
  read: {method: POST, path: /graphql, graphql: {operation: R, query: "query R($p: ID!) { x(p: $p) { a } }", variables: {p: "{p}"}, result: x}}
  update: {method: POST, path: /graphql, graphql: {operation: U, query: "mutation U($p: ID!) { u }", variables: {p: "gid://x/{p}"}}}
  params: [{name: p, field: p}]
`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]string{
		"rest op":      `{method: PATCH, path: "/x/{p}"}`,
		"interpolated": `{method: POST, path: /graphql, graphql: {operation: U, query: "mutation U { u }", variables: {p: "gid://x/{name}"}}}`,
		"unnamed":      `{method: POST, path: /graphql, graphql: {operation: U, query: "mutation { u }"}}`,
		"not a post":   `{method: GET, path: /graphql, graphql: {operation: U, query: "mutation U { u }"}}`,
	} {
		y := ok[:len(ok)-len("  params: [{name: p, field: p}]\n")]
		y = y[:len(y)-len(`  update: {method: POST, path: /graphql, graphql: {operation: U, query: "mutation U($p: ID!) { u }", variables: {p: "gid://x/{p}"}}}`+"\n")]
		y += "  update: " + bad + "\n  params: [{name: p, field: p}]\n"
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}
