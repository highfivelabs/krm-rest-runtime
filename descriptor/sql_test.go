// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"strings"
	"testing"
	"time"
)

func sqlKind(mutate func(q *SQL)) *Kind {
	q := &SQL{
		Mode: SQLObject, ID: []string{"name"},
		Fields: map[string]SQLField{"name": {Type: "identifier"}, "comment": {Type: "string"}, "arn": {Type: "string", Output: true}},
		Create: "CREATE THING {@id}[ COMMENT = {comment}]",
		Read:   []SQLRead{{Statement: "SHOW THINGS LIKE {@like}", Match: map[string]string{"name": "{name}"}, Columns: map[string]string{"arn": "arn"}}},
		Alter:  map[string]string{"comment": "ALTER THING {@id} SET COMMENT = {comment}"},
		Delete: "DROP THING IF EXISTS {@id}",
	}
	if mutate != nil {
		mutate(q)
	}
	return &Kind{Kind: "Thing", Group: "g", Version: "v1", Shape: SQLShape, Read: Op{Method: "POST", Path: "/api/v2/statements"}, SQL: q}
}

func TestValidateSQL(t *testing.T) {
	if err := sqlKind(nil).validate(); err != nil {
		t.Fatalf("valid kind: %v", err)
	}
	for _, c := range []struct {
		name   string
		mutate func(q *SQL)
		want   string
	}{
		{"unknown field", func(q *SQL) { q.Create = "CREATE THING {@id} X = {nope}" }, "not a field"},
		{"output sent", func(q *SQL) { q.Create = "CREATE THING {@id} ARN = {arn}" }, "not a field it can send"},
		{"no @id", func(q *SQL) { q.Delete = "DROP THING IF EXISTS {comment}" }, "names it by {@id}"},
		{"id field optional", func(q *SQL) { q.Delete = "DROP THING IF EXISTS[ {name}]" }, "names it by {@id}"},
		{"flag not boolean", func(q *SQL) { q.Create = "CREATE[ SECURE{?comment}] THING {@id}" }, "for a boolean"},
		{"flag with type", func(q *SQL) { q.Create = "CREATE THING {@id}[{?comment:string}]" }, "bad placeholder"},
		{"object without item", func(q *SQL) { q.Fields["cols"] = SQLField{Type: "objects"} }, "an item is"},
		{"item on a scalar", func(q *SQL) { q.Fields["comment"] = SQLField{Type: "string", Item: &SQLItem{Template: "{x}"}} }, "an item is"},
		{"item field unknown", func(q *SQL) {
			q.Fields["cols"] = SQLField{Type: "objects", Item: &SQLItem{Template: "{name} {kind}", Fields: map[string]SQLField{"name": {Type: "identifier"}}}}
		}, "{kind} is not a field"},
		{"item names @id", func(q *SQL) {
			q.Fields["cols"] = SQLField{Type: "objects", Item: &SQLItem{Template: "{@id}", Fields: map[string]SQLField{}}}
		}, "needs id"},
		{"item sep", func(q *SQL) {
			q.Fields["cols"] = SQLField{Type: "objects", Item: &SQLItem{Template: "X", Sep: "; ", Fields: map[string]SQLField{}}}
		}, "item.sep"},
		{"warehouse", func(q *SQL) { q.Warehouse = "comment" }, "sql.warehouse"},
		{"enum not words", func(q *SQL) { q.Fields["k"] = SQLField{Type: "keyword", Enum: []string{"A; B"}} }, "not words"},
		{"enum on a string", func(q *SQL) { q.Fields["comment"] = SQLField{Type: "string", Enum: []string{"A"}} }, "enum is for"},
		{"projection of a scalar", func(q *SQL) { q.Delete = "DROP THING {@id}({comment.type})" }, "not a field"},
		{"createColumns", func(q *SQL) { q.CreateColumns = map[string]string{"comment": "c"} }, "not an output"},
		{"statement in literal", func(q *SQL) { q.Delete = "DROP THING {@id}; DROP DATABASE x" }, "only keywords"},
		{"quote in literal", func(q *SQL) { q.Delete = `DROP THING "x y" {@id}` }, "only keywords"},
		{"dollar string in literal", func(q *SQL) { q.Delete = `DROP THING {@id} $$x$$` }, "only keywords"},
		{"comment in literal", func(q *SQL) { q.Delete = "DROP THING {@id} -- x" }, "only keywords"},
		{"unknown type", func(q *SQL) { q.Fields["comment"] = SQLField{Type: "raw"} }, "unknown type"},
		{"id not an identifier", func(q *SQL) { q.Fields["name"] = SQLField{Type: "integer"} }, "must be an identifier"},
		{"unknown mode", func(q *SQL) { q.Mode = "other" }, "unknown mode"},
		{"unbalanced", func(q *SQL) { q.Create = "CREATE THING {@id}[ COMMENT = {comment}" }, "unclosed ["},
		{"settle on a grant", func(q *SQL) { q.Mode = SQLGrant; q.Settle = &SQLSettle{Field: "arn", Ready: []string{"A"}} }, "sql.settle: for an object"},
		{"settle field not output", func(q *SQL) { q.Settle = &SQLSettle{Field: "comment", Ready: []string{"A"}} }, "must be an output"},
		{"settle field not read", func(q *SQL) {
			q.Fields["state"] = SQLField{Type: "string", Output: true}
			q.Settle = &SQLSettle{Field: "state", Ready: []string{"A"}}
		}, "first read must map"},
		{"settle without ready", func(q *SQL) { q.Settle = &SQLSettle{Field: "arn"} }, "settle.ready"},
		{"settle status", func(q *SQL) { q.Settle = &SQLSettle{Field: "arn", Ready: []string{"A'; DROP"}} }, "not a status"},
		{"settle timeout", func(q *SQL) { q.Settle = &SQLSettle{Field: "arn", Ready: []string{"A"}, Timeout: "soon"} }, "not a duration"},
		{"teardown not on the object", func(q *SQL) {
			q.Settle = &SQLSettle{Field: "arn", Ready: []string{"A"}, Teardown: []SQLTeardownStep{{Run: "ALTER THING {comment} STOP"}}}
		}, "{@id}"},
		{"teardown literal", func(q *SQL) {
			q.Settle = &SQLSettle{Field: "arn", Ready: []string{"A"}, Teardown: []SQLTeardownStep{{Run: "ALTER THING {@id} STOP; DROP DATABASE x"}}}
		}, "only keywords"},
	} {
		err := sqlKind(c.mutate).validate()
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	ok := sqlKind(func(q *SQL) {
		q.Fields["secure"] = SQLField{Type: "boolean"}
		q.Fields["wh"] = SQLField{Type: "identifier"}
		q.Warehouse = "wh"
		q.CreateColumns = map[string]string{"arn": "arn"}
		q.Fields["cols"] = SQLField{Type: "objects", Item: &SQLItem{Template: "{name}[ NOT NULL{?nn}]", Fields: map[string]SQLField{
			"name": {Type: "identifier"}, "nn": {Type: "boolean"}}}}
		q.Create = "CREATE[ SECURE{?secure}] THING {@id} ({cols})"
		q.Delete = "DROP THING IF EXISTS {name}"
	})
	if err := ok.validate(); err != nil {
		t.Errorf("flags, items, a warehouse, createColumns and a statement naming the id fields: %v", err)
	}
	settled := sqlKind(func(q *SQL) {
		q.Settle = &SQLSettle{Field: "arn", Ready: []string{"ACTIVE"}, Busy: []string{"CREATING"}, Gone: []string{"DELETED"}, Timeout: "45m",
			Teardown: []SQLTeardownStep{{When: []string{"DELETING"}}, {Run: "ALTER THING IF EXISTS {@id} TERMINATE"}}}
	})
	if err := settled.validate(); err != nil {
		t.Errorf("a settle: %v", err)
	}
	if d := settled.SQL.Settle.TimeoutOf(); d != 45*time.Minute {
		t.Errorf("timeout %s", d)
	}
	if d := (&SQLSettle{}).TimeoutOf(); d != DefaultSettleTimeout {
		t.Errorf("default timeout %s", d)
	}
	k := sqlKind(nil)
	k.Create = &Op{Method: "POST", Path: "/x"}
	if err := k.validate(); err == nil {
		t.Error("a sql kind with a REST create is refused")
	}
}

func TestSQLFieldsReadable(t *testing.T) {
	q := sqlKind(func(q *SQL) {
		q.Read = append(q.Read, SQLRead{Statement: "SHOW GRANTS TO SHARE {comment}"})
	}).SQL
	if !q.Readable("name") || !q.Readable("arn") || !q.Readable("comment") {
		t.Error("a field a read matches, maps or selects by is readable")
	}
	if !q.Settable("comment") || q.Settable("name") {
		t.Error("only a field with an alter is settable")
	}
}
