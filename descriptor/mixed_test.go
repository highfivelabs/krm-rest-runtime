// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"strings"
	"testing"
)

func TestMatchTemplate(t *testing.T) {
	got, ok := MatchTemplate("refs/heads/{branch}", "refs/heads/feature/x")
	if !ok || got["branch"] != "feature/x" {
		t.Errorf("got %v, %v", got, ok)
	}
	if _, ok := MatchTemplate("refs/heads/{branch}", "refs/tags/v1"); ok {
		t.Error("refs/tags/v1 matched refs/heads/{branch}")
	}
	id := &ID{Param: "id", From: "id", Match: "gid://gitlab/Boards::EpicBoard/{id}"}
	if p, ok := id.Part("gid://gitlab/Boards::EpicBoard/42"); !ok || p != "42" {
		t.Errorf("Part = %q, %v", p, ok)
	}
	if _, ok := id.Part("gid://gitlab/Group/42"); ok {
		t.Error("another type's global ID matched")
	}
	if p, ok := (&ID{Param: "id", From: "id"}).Part("7"); !ok || p != "7" {
		t.Errorf("no match: Part = %q, %v", p, ok)
	}
	if got := Placeholders("/repos/{owner}/git/refs/heads/{+branch}"); strings.Join(got, ",") != "owner,branch" {
		t.Errorf("Placeholders = %v", got)
	}
}

func TestFindFirstElement(t *testing.T) {
	k := &Kind{Read: Op{Find: &Find{List: "instance_level_scopings"}}}
	if m, ok := k.FindIn(map[string]any{"instance_level_scopings": []any{map[string]any{"created_at": "x"}}}, ""); !ok || m["created_at"] != "x" {
		t.Errorf("FindIn = %v, %v", m, ok)
	}
	if _, ok := k.FindIn(map[string]any{"instance_level_scopings": []any{}}, ""); ok {
		t.Error("an empty list found something")
	}
}

func TestValidateMixedAndSet(t *testing.T) {
	gql := `{method: POST, path: /graphql, graphql: {operation: Make, query: "mutation Make { make { thing { id } } }", result: make/thing}}`
	for name, c := range map[string]struct{ yaml, err string }{
		"mixed crud": {`shape: crud
create: ` + gql + `
read: {method: GET, path: "/things/{id}"}
delete: {method: DELETE, path: "/things/{id}"}
id: {param: id, from: id, match: "gid://x/Thing/{id}"}`, ""},
		"mixed upsert": {`shape: upsert
update: ` + gql + `
read: {method: GET, path: "/things/{name}"}
delete: {method: DELETE, path: "/things/{name}"}
params: [{name: name, field: name}]`, "is crud"},
		"match without the param": {`shape: crud
create: {method: POST, path: /things}
read: {method: GET, path: "/things/{id}"}
delete: {method: DELETE, path: "/things/{id}"}
id: {param: id, from: id, match: "gid://x/Thing/{n}"}`, "has no {id}"},
		"upsert with a create and no update": {`shape: upsert
create: {method: POST, path: "/scopes/{name}"}
read: {method: GET, path: "/scopes", find: {list: items, field: name, param: name}}
delete: {method: DELETE, path: "/scopes/{name}"}
params: [{name: name, field: name}]`, ""},
		"set": {`shape: set
set: {field: labels, key: name, param: name}
read: {method: GET, path: "/repos/{repo}/labels"}
create: {method: POST, path: "/repos/{repo}/labels"}
delete: {method: DELETE, path: "/repos/{repo}/labels/{name}"}
params: [{name: repo, field: repo}]`, ""},
		"set element call not naming the element": {`shape: set
set: {field: labels, key: name, param: name}
read: {method: GET, path: "/repos/{repo}/labels"}
create: {method: POST, path: "/repos/{repo}/labels"}
delete: {method: DELETE, path: "/repos/{repo}/labels"}
params: [{name: repo, field: repo}]`, "names the element"},
		"set keyed by two fields in the query": {`shape: set
set: {field: namespaces, keys: [db, collection]}
read: {method: GET, path: "/clusters/{c}/sharding"}
create: {method: POST, path: "/clusters/{c}/sharding/namespaces"}
delete: {method: DELETE, path: "/clusters/{c}/sharding/namespaces", query: {db: "{db}", collection: "{collection}"}}
params: [{name: c, field: c}]`, ""},
		"set keys a call does not name": {`shape: set
set: {field: namespaces, keys: [db, collection]}
read: {method: GET, path: "/clusters/{c}/sharding"}
create: {method: POST, path: "/clusters/{c}/sharding/namespaces"}
delete: {method: DELETE, path: "/clusters/{c}/sharding/namespaces", query: {db: "{db}"}}
params: [{name: c, field: c}]`, "no {collection}"},
		"set keys beside a key": {`shape: set
set: {field: namespaces, keys: [db, collection], key: db, param: db}
read: {method: GET, path: "/clusters/{c}/sharding"}
create: {method: POST, path: "/clusters/{c}/sharding/namespaces"}
delete: {method: DELETE, path: "/clusters/{c}/sharding/namespaces/{db}", query: {collection: "{collection}"}}
params: [{name: c, field: c}]`, "in place of"},
		"set batch": {`shape: set
set:
  field: namespaces
  keys: [db, collection]
  batch: {field: zones, key: location, at: zone_map, value: zone, add: {method: POST, path: "/clusters/{c}/zones"}, clear: {method: DELETE, path: "/clusters/{c}/zones"}}
read: {method: GET, path: "/clusters/{c}/sharding"}
create: {method: POST, path: "/clusters/{c}/sharding/namespaces"}
delete: {method: DELETE, path: "/clusters/{c}/sharding/namespaces", query: {db: "{db}", collection: "{collection}"}}
params: [{name: c, field: c}]`, ""},
		"set batch without a clear": {`shape: set
set:
  field: namespaces
  keys: [db, collection]
  batch: {field: zones, key: location, at: zone_map, value: zone, add: {method: POST, path: "/clusters/{c}/zones"}}
read: {method: GET, path: "/clusters/{c}/sharding"}
create: {method: POST, path: "/clusters/{c}/sharding/namespaces"}
delete: {method: DELETE, path: "/clusters/{c}/sharding/namespaces", query: {db: "{db}", collection: "{collection}"}}
params: [{name: c, field: c}]`, "set.batch needs"},
		"set batch on an unknown param": {`shape: set
set:
  field: namespaces
  keys: [db, collection]
  batch: {field: zones, key: location, at: zone_map, value: zone, add: {method: POST, path: "/clusters/{x}/zones"}, clear: {method: DELETE, path: "/clusters/{c}/zones"}}
read: {method: GET, path: "/clusters/{c}/sharding"}
create: {method: POST, path: "/clusters/{c}/sharding/namespaces"}
delete: {method: DELETE, path: "/clusters/{c}/sharding/namespaces", query: {db: "{db}", collection: "{collection}"}}
params: [{name: c, field: c}]`, "no param fills {x}"},
		"unread": {`shape: settings
unread: no read names it
update: {method: POST, path: "/owners"}`, ""},
		"unread with a read": {`shape: settings
unread: no read names it
read: {method: GET, path: "/owners"}
update: {method: POST, path: "/owners"}`, "has no read"},
		"unread crud": {`shape: crud
unread: no read names it
create: {method: POST, path: "/owners"}
update: {method: POST, path: "/owners/{id}"}
delete: {method: DELETE, path: "/owners/{id}"}
id: {param: id, from: id}`, "settings kind"},
		"set without its field": {`shape: set
read: {method: GET, path: "/labels"}
create: {method: POST, path: "/labels"}
delete: {method: DELETE, path: "/labels/{name}"}`, "set needs"},
	} {
		doc := "vendor: x\nconnection: {group: core.x, version: v1, kind: Connection}\nkinds:\n- kind: Thing\n  group: things.x\n  version: v1\n  plural: things\n  " +
			strings.ReplaceAll(c.yaml, "\n", "\n  ") + "\n"
		_, err := Parse([]byte(doc))
		switch {
		case c.err == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)):
			t.Errorf("%s: err %v, want %q", name, err, c.err)
		}
	}
}
