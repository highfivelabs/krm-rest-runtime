// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"strings"
	"testing"
)

const replaceBase = `vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Script
  group: core.example.hfl.dev
  version: v1alpha1
  plural: scripts
  shape: crud
  create: {method: POST, path: "/scripts"}
  read: {method: GET, path: "/scripts/{name}"}
  delete: {method: DELETE, path: "/scripts/{name}"}
  id: {param: name, from: name}
`

// Replace is only for CRUD kinds with no update, and its path must be
// fillable like any other operation's.
func TestReplaceIsOnlyForCRUDWithNoUpdate(t *testing.T) {
	ok := replaceBase + `  replace: {method: POST, path: "/scripts", query: {createMode: orReplace}, identity: [name]}
`
	f, err := Parse([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if r := f.Kinds[0].Replace; r == nil || r.Method != "POST" || r.Path != "/scripts" || r.Query["createMode"] != "orReplace" || len(r.Identity) != 1 {
		t.Fatalf("replace parsed as %+v", r)
	}
	for name, doc := range map[string]string{
		"with an update": replaceBase + `  update: {method: PATCH, path: "/scripts/{name}"}
  replace: {method: POST, path: "/scripts", query: {createMode: orReplace}}
`,
		"unfilled path": replaceBase + `  replace: {method: POST, path: "/dbs/{db}/scripts", query: {createMode: orReplace}}
`,
	} {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		} else if name == "with an update" && !strings.Contains(err.Error(), "no update") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBeforeDeleteAndCompareChecks(t *testing.T) {
	ok := replaceBase + `  beforeDelete: [{method: POST, path: "/scripts/{name}/stop"}]
  compare: {tags: splitSpace, "stages[].labelId": slugObject, a.tags: splitSpace}
`
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	for name, extra := range map[string]string{
		"unknown placeholder": `  beforeDelete: [{method: POST, path: "/scripts/{other}/stop"}]
`,
		"unknown rule": `  compare: {tags: splitComma}
`,
		"a list's own path": `  compare: {"tags[]": splitSpace}
`,
		"a path with a slash": `  compare: {a/tags: splitSpace}
`,
	} {
		if _, err := Parse([]byte(replaceBase + extra)); err == nil {
			t.Errorf("%s: parsed, want an error", name)
		}
	}
}

func TestIDCompose(t *testing.T) {
	id := &ID{Param: "nameWithArgs", From: "{name}({arguments[].datatype})"}
	if !id.IsTemplate() || (&ID{From: "id"}).IsTemplate() {
		t.Fatal("IsTemplate")
	}
	for _, tc := range []struct {
		body map[string]any
		want string
		ok   bool
	}{
		{map[string]any{"name": "f", "arguments": []any{map[string]any{"datatype": "NUMBER"}, map[string]any{"datatype": "VARCHAR"}}}, "f(NUMBER,VARCHAR)", true},
		{map[string]any{"name": "f", "arguments": []any{}}, "f()", true},
		{map[string]any{"name": "f"}, "f()", true},
		{map[string]any{"arguments": []any{}}, "()", false},
		{map[string]any{"name": "f", "arguments": []any{map[string]any{"name": "x"}}}, "f()", false},
	} {
		got, ok := id.Compose(tc.body)
		if got != tc.want || ok != tc.ok {
			t.Errorf("Compose(%v) = %q, %v; want %q, %v", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

func TestQueryTemplates(t *testing.T) {
	k := &Kind{
		Read:   Op{Query: map[string]string{"environment": "{environment.id}", "full": "true"}},
		Delete: &Op{Query: map[string]string{"environment": "{environment.id}"}},
	}
	params := map[string]string{"id": "lkc-1"}
	if err := k.FillQuery(map[string]any{"environment": map[string]any{"id": "env-1"}}, params); err != nil {
		t.Fatal(err)
	}
	got := k.Read.QueryOf(params)
	if got["environment"] != "env-1" || got["full"] != "true" || len(got) != 2 {
		t.Errorf("Read.QueryOf = %v", got)
	}
	if got := k.Delete.QueryOf(params); got["environment"] != "env-1" {
		t.Errorf("Delete.QueryOf = %v", got)
	}
	if (&Op{}).QueryOf(params) != nil || (*Op)(nil).QueryOf(params) != nil {
		t.Error("QueryOf of an op with no query should be nil")
	}
	if err := k.FillQuery(map[string]any{"display_name": "c"}, map[string]string{}); err == nil || !strings.Contains(err.Error(), "spec.environment.id is required") {
		t.Errorf("FillQuery without the field: %v", err)
	}
}
