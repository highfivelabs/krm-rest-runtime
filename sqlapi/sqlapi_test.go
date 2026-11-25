// SPDX-License-Identifier: Apache-2.0

package sqlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

func TestIdentifiers(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"my_db", `"MY_DB"`},
		{"MyDb$1", `"MYDB$1"`},
		{"my-db", `"my-db"`},
		{`"lower"`, `"lower"`},
		{`"a""b"`, `"a""b"`},
		// Injection attempts end up inside one quoted identifier.
		{`x" ; DROP DATABASE prod; --`, `"x"" ; DROP DATABASE prod; --"`},
		{`a"."b`, `"a"".""b"`},
		{`"`, `""""`},
	} {
		got, err := QuoteIdent(c.in)
		if err != nil || got != c.want {
			t.Errorf("QuoteIdent(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if exact, _ := Ident(c.in); QuoteExact(exact) != got {
			t.Errorf("Ident(%q) does not round-trip", c.in)
		}
	}
	for _, bad := range []string{"", `""`, `"a"b"`, "a\x00b", strings.Repeat("x", 256)} {
		if got, err := QuoteIdent(bad); err == nil {
			t.Errorf("QuoteIdent(%q) = %q, want an error", bad, got)
		}
	}
}

func TestNames(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"db.schema.rule", `"DB"."SCHEMA"."RULE"`},
		{`db."My Schema".r`, `"DB"."My Schema"."R"`},
		{`"a.b".c`, `"a.b"."C"`},
		{"rule", `"RULE"`},
	} {
		got, err := QuoteName(c.in)
		if err != nil || got != c.want {
			t.Errorf("QuoteName(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"a.b.c.d", `"open`, `a."b"c`, "a..b", `x"; DROP TABLE t; --`} {
		if got, err := QuoteName(bad); err == nil {
			t.Errorf("QuoteName(%q) = %q, want an error", bad, got)
		}
	}
}

func TestStrings(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", `'plain'`},
		{"it's", `'it''s'`},
		{`back\slash`, `'back\\slash'`},
		// A quote, a backslash before a quote, and a statement after it all
		// stay inside the one string constant.
		{`'; DROP DATABASE prod; --`, `'''; DROP DATABASE prod; --'`},
		{`\'; DROP DATABASE prod; --`, `'\\''; DROP DATABASE prod; --'`},
		{"multi\nline", "'multi\nline'"},
	} {
		got, err := QuoteString(c.in)
		if err != nil || got != c.want {
			t.Errorf("QuoteString(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		back, err := unquoteString(got)
		if err != nil || back != c.in {
			t.Errorf("unquoteString(%q) = %q, %v; want %q", got, back, err, c.in)
		}
	}
	if _, err := QuoteString("a\x00"); err == nil {
		t.Error("a NUL is refused")
	}
	if p, _ := LikePattern(`a\b%c_d'e`); p != `'a_b_c_d''e'` {
		t.Errorf("LikePattern = %s", p)
	}
}

func TestCheckedValues(t *testing.T) {
	ok := []struct {
		typ  string
		v    any
		want string
	}{
		{"keyword", "monthly", "MONTHLY"},
		{"number", 1.5, "1.5"},
		{"integer", float64(100), "100"},
		{"boolean", true, "TRUE"},
		{"value", "UTC", "'UTC'"},
		{"value", float64(60), "60"},
		{"value", false, "FALSE"},
		{"datatype", "varchar(100)", "VARCHAR(100)"},
		{"datatype", "NUMBER(38, 0)", "NUMBER(38, 0)"},
		{"strings", []any{"a", "b'c"}, `'a', 'b''c'`},
		{"identifiers", []any{"r1", `"r 2"`}, `"R1", "r 2"`},
		{"keywords", []any{"usage", "select"}, "USAGE, SELECT"},
		{"args", []any{map[string]any{"name": "val", "type": "varchar"}}, `"VAL" VARCHAR`},
		{"expression", "CASE WHEN a = 'x;y' THEN 1 ELSE 2 END", "CASE WHEN a = 'x;y' THEN 1 ELSE 2 END"},
		{"expression", "$$a;b--c$$", "$$a;b--c$$"},
	}
	for _, c := range ok {
		got, err := Value(c.typ, c.v)
		if err != nil || got != c.want {
			t.Errorf("Value(%s, %v) = %q, %v; want %q", c.typ, c.v, got, err, c.want)
		}
	}
	bad := []struct {
		typ string
		v   any
	}{
		{"keyword", "MONTHLY; DROP DATABASE prod"},
		{"keyword", "a b"},
		{"keyword", "'x'"},
		{"number", "1; DROP DATABASE prod"},
		{"number", "1"},
		{"integer", 1.5},
		{"boolean", "true; DROP DATABASE prod"},
		{"datatype", "VARCHAR); DROP DATABASE prod; --"},
		{"datatype", "VARCHAR(10) --"},
		{"expression", "true; DROP DATABASE prod"},
		{"expression", "true -- COMMENT = 'x'"},
		{"expression", "true /* x */"},
		{"expression", "f(x"},
		{"expression", "x) RETURNS y -> (z"},
		{"expression", "'unclosed"},
		{"expression", "\"unclosed"},
		{"expression", "$$unclosed"},
		{"expression", "   "},
		{"strings", "not a list"},
		{"keywords", []any{"USAGE", "SELECT ON ALL"}},
		{"args", []any{map[string]any{"name": "v", "type": "INT) RETURNS INT -> 1; DROP x; --"}}},
		{"string", 5.0},
		{"identifier", true},
	}
	for _, c := range bad {
		if got, err := Value(c.typ, c.v); err == nil {
			t.Errorf("Value(%s, %#v) = %q, want an error", c.typ, c.v, got)
		}
	}
}

var policy = &descriptor.SQL{
	Mode: descriptor.SQLObject,
	ID:   []string{"database", "schema", "name"},
	Fields: map[string]descriptor.SQLField{
		"database": {Type: "identifier"}, "schema": {Type: "identifier"}, "name": {Type: "identifier"},
		"comment": {Type: "string"}, "quota": {Type: "integer"}, "roles": {Type: "identifiers"},
	},
	Create: "CREATE THING {@id}[ WITH[ QUOTA = {quota}][ ROLES = ({roles})]][ COMMENT = {comment}]",
	Read:   []descriptor.SQLRead{{Statement: "SHOW THINGS LIKE {@like} IN SCHEMA {@parent}", Match: map[string]string{"name": "{name}"}, Columns: map[string]string{"comment": "comment"}}},
	Delete: "DROP THING IF EXISTS {@id}",
}

func TestRender(t *testing.T) {
	id := []string{"DB", "SC", "it's"}
	for _, c := range []struct {
		vals map[string]any
		want string
	}{
		{nil, `CREATE THING "DB"."SC"."it's"`},
		{map[string]any{"comment": "c"}, `CREATE THING "DB"."SC"."it's" COMMENT = 'c'`},
		{map[string]any{"quota": 5.0}, `CREATE THING "DB"."SC"."it's" WITH QUOTA = 5`},
		{map[string]any{"quota": 5.0, "roles": []any{"a"}}, `CREATE THING "DB"."SC"."it's" WITH QUOTA = 5 ROLES = ("A")`},
		{map[string]any{"roles": []any{}}, `CREATE THING "DB"."SC"."it's" WITH ROLES = ()`},
	} {
		got, err := Render(policy, policy.Create, c.vals, id)
		if err != nil || got != c.want {
			t.Errorf("Render(%v) = %q, %v; want %q", c.vals, got, err, c.want)
		}
	}
	got, _ := Render(policy, policy.Read[0].Statement, nil, id)
	if got != `SHOW THINGS LIKE 'it''s' IN SCHEMA "DB"."SC"` {
		t.Errorf("read = %s", got)
	}
	if _, err := Render(policy, "ALTER THING {@id} SET COMMENT = {comment}", nil, id); err == nil || !strings.Contains(err.Error(), "spec.comment is required") {
		t.Errorf("a field outside [] is required: %v", err)
	}
	if _, err := Render(policy, policy.Create, map[string]any{"quota": "5; DROP DATABASE prod"}, id); err == nil {
		t.Error("a string where a number goes is refused")
	}
	// What Render writes, Compile reads back.
	vals := map[string]any{"quota": 5.0, "roles": []any{`"x, y"`, "b"}, "comment": "a ' \\ b"}
	stmt, _ := Render(policy, policy.Create, vals, id)
	p, err := Compile(policy, policy.Create)
	if err != nil {
		t.Fatal(err)
	}
	back, ok := p.Match(policy, stmt)
	if !ok || back["comment"] != vals["comment"] || back["quota"] != 5.0 || back["@id"] != `DB.SC."it's"` {
		t.Fatalf("Match(%s) = %v", stmt, back)
	}
	if roles := back["roles"].([]any); len(roles) != 2 || roles[0] != "x, y" || roles[1] != "B" {
		t.Errorf("roles read back %v", roles)
	}
}

// A status.id is taken only as a quoted object name: a status edited to
// hold SQL is refused before it reaches a statement.
func TestParseID(t *testing.T) {
	if parts, err := ParseID(`"DB"."S""C"."N.x"`, 3); err != nil || strings.Join(parts, "|") != `DB|S"C|N.x` {
		t.Errorf("ParseID = %v, %v", parts, err)
	}
	for _, bad := range []string{`DB.SC.N`, `"DB"."SC"."N"; DROP DATABASE prod`, `"DB"."SC"`, `"DB"."SC"."N"."X"`, `""."SC"."N"`} {
		if _, err := ParseID(bad, 3); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
}

func TestTemplateLiterals(t *testing.T) {
	for _, ok := range []string{"SHOW PARAMETERS LIKE 'NETWORK_POLICY' IN USER {user}", "CREATE X {@id} AS ({a}) RETURNS {b} -> {c}", `SELECT SYSTEM$GET_TAG ({a}, {b}, {c}) AS "TAG"`} {
		if _, err := descriptor.ParseSQLTemplate(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"DROP X {@id}; DROP DATABASE prod", "SET COMMENT = 'x;y'", "DROP X {@id} -- x", "CREATE X [{a}", "CREATE X {a]}", `CREATE "X Y"`, "CREATE X AS $$ {a} $$", `CREATE "x"."y`} {
		if _, err := descriptor.ParseSQLTemplate(bad); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestSame(t *testing.T) {
	for _, c := range []struct {
		typ  string
		d, o any
		want bool
	}{
		{"identifier", "my_role", "MY_ROLE", true},
		{"identifier", `"my_role"`, "my_role", true},
		{"identifier", "my_role", "my_role", false},
		{"name", "db.sc.rule", "DB.SC.RULE", true},
		{"name", `db."sc".rule`, `DB."sc".RULE`, true},
		{"keyword", "monthly", "MONTHLY", true},
		{"expression", "a  =\n b", "a = b", true},
		{"identifiers", []any{"a", "b"}, []any{"B", "A"}, true},
		{"identifiers", []any{"a"}, []any{"A", "B"}, false},
		{"value", 60.0, "60", true},
		{"value", true, "true", true},
		{"integer", 5.0, 5.0, true},
		{"string", "x", "X", false},
	} {
		if got := Same(c.typ, c.d, c.o); got != c.want {
			t.Errorf("Same(%s, %v, %v) = %v", c.typ, c.d, c.o, got)
		}
	}
	if got := FromColumn("strings", `["a","b"]`); len(got.([]any)) != 2 {
		t.Errorf("JSON list: %v", got)
	}
	if got := FromColumn("identifiers", "[A, B]"); len(got.([]any)) != 2 || got.([]any)[1] != "B" {
		t.Errorf("bracket list: %v", got)
	}
	if got := FromColumn("boolean", "Y"); got != true {
		t.Errorf("Y: %v", got)
	}
}

// Exec sends one statement asking for exactly one, polls a 202 until the
// answer is there, reads every partition, and tells "does not exist" (a
// 422 Snowflake says so in) from any other refusal.
func TestExec(t *testing.T) {
	var polls, parts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == "POST":
			if r.URL.Query().Get("nullable") != "true" {
				t.Errorf("no nullable=true: %s", r.URL)
			}
			b, _ := io.ReadAll(r.Body)
			var req Request
			_ = json.Unmarshal(b, &req)
			if req.Parameters["multi_statement_count"] != 1.0 || req.Timeout != 60 {
				t.Errorf("request %s", b)
			}
			switch req.Statement {
			case "SLOW":
				w.WriteHeader(202)
				_, _ = w.Write([]byte(`{"code":"333334","statementHandle":"h-1"}`))
			case "GONE":
				w.WriteHeader(422)
				_, _ = w.Write([]byte(`{"code":"002003","message":"SQL compilation error:\nObject 'X' does not exist or not authorized.","statementHandle":"h-2"}`))
			default:
				w.WriteHeader(422)
				_, _ = w.Write([]byte(`{"code":"001003","message":"syntax error","statementHandle":"h-3"}`))
			}
		case r.URL.Query().Get("partition") == "1":
			parts++
			_, _ = w.Write([]byte(`{"data":[["c"]]}`))
		default:
			polls++
			if polls < 3 {
				w.WriteHeader(202)
				_, _ = w.Write([]byte(`{"code":"333334","statementHandle":"h-1"}`))
				return
			}
			_, _ = w.Write([]byte(`{"resultSetMetaData":{"rowType":[{"name":"name"}],"partitionInfo":[{},{}]},"data":[["a"],[null]],"statementHandle":"h-1"}`))
		}
	}))
	defer srv.Close()
	var waited time.Duration
	run := &Runner{API: rest.New(&rest.Vendor{Name: "t"}, srv.URL, nil), Endpoint: "/api/v2/statements",
		Wait: func(_ context.Context, d time.Duration) error { waited += d; return nil }}
	res, err := run.Exec(context.Background(), "SLOW")
	if err != nil {
		t.Fatal(err)
	}
	if polls != 3 || parts != 1 || len(res.Rows) != 3 || *res.Rows[0][0] != "a" || res.Rows[1][0] != nil || res.Columns[0] != "name" || waited == 0 {
		t.Errorf("polls %d parts %d waited %s rows %v", polls, parts, waited, res.Rows)
	}
	if _, err := run.Exec(context.Background(), "GONE"); !errors.Is(err, rest.ErrNotFound) {
		t.Errorf("GONE: %v, want ErrNotFound", err)
	}
	_, err = run.Exec(context.Background(), "BAD 'secret'")
	var se *SQLError
	if !errors.As(err, &se) || se.Code != "001003" || errors.Is(err, rest.ErrNotFound) || strings.Contains(err.Error(), "secret") {
		t.Errorf("BAD: %v", err)
	}
}

func TestIdentString(t *testing.T) {
	got, err := Value("identstring", `it's"x`)
	if err != nil || got != `'"it''s""x"'` {
		t.Fatalf("identstring = %s, %v", got, err)
	}
	back, err := Parse("identstring", got)
	if err != nil || back != `it's"x` {
		t.Fatalf("Parse = %v, %v", back, err)
	}
	if !Same("identstring", "scim_role", "SCIM_ROLE") {
		t.Error("an identstring compares as an identifier")
	}
}

var table = &descriptor.SQL{
	Mode: descriptor.SQLObject,
	ID:   []string{"name"},
	Fields: map[string]descriptor.SQLField{
		"name": {Type: "identifier"}, "secure": {Type: "boolean"}, "comment": {Type: "string"},
		"columns": {Type: "objects", Item: &descriptor.SQLItem{
			Template: "{name} {type}[ NOT NULL{?not_null}][ DEFAULT {default}][ COMMENT {comment}]",
			Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "type": {Type: "datatype"},
				"not_null": {Type: "boolean"}, "default": {Type: "expression"}, "comment": {Type: "string"}}}},
		"primary_key": {Type: "object", Item: &descriptor.SQLItem{
			Template: "[CONSTRAINT {name} ]PRIMARY KEY ({columns})",
			Fields:   map[string]descriptor.SQLField{"name": {Type: "identifier"}, "columns": {Type: "identifiers"}}}},
		"tags": {Type: "objects", Item: &descriptor.SQLItem{Template: "{name} = {value}", Sep: " AND ",
			Fields: map[string]descriptor.SQLField{"name": {Type: "name"}, "value": {Type: "string"}}}},
	},
	Create: "CREATE[ SECURE{?secure}] TABLE {@id} ({columns}[, {primary_key}])[ WITH TAG ({tags})][ COMMENT = {comment}]",
	Alter:  map[string]string{"secure": "ALTER TABLE {@id}[ SET SECURE{?secure}][ UNSET SECURE{!secure}]"},
	Read:   []descriptor.SQLRead{{Statement: "SHOW TABLES LIKE {@like}", Match: map[string]string{"name": "{name}"}}},
	Delete: "DROP TABLE IF EXISTS {@id}",
}

// A flag turns a keyword on and off; an object, or each of a list of them,
// is written by its item's template; Compile reads it all back.
func TestRenderItems(t *testing.T) {
	id := []string{"T"}
	vals := map[string]any{
		"secure": true,
		"columns": []any{
			map[string]any{"name": "id", "type": "number(38, 0)", "not_null": true, "comment": "it's, the key"},
			map[string]any{"name": "v", "type": "varchar", "default": "CURRENT_TIMESTAMP()", "not_null": false},
		},
		"primary_key": map[string]any{"columns": []any{"id"}},
		"tags":        []any{map[string]any{"name": "db.s.t1", "value": "a"}, map[string]any{"name": "db.s.t2", "value": "b AND c"}},
	}
	want := `CREATE SECURE TABLE "T" ("ID" NUMBER(38, 0) NOT NULL COMMENT 'it''s, the key', "V" VARCHAR DEFAULT CURRENT_TIMESTAMP(), PRIMARY KEY ("ID")) WITH TAG ("DB"."S"."T1" = 'a' AND "DB"."S"."T2" = 'b AND c')`
	got, err := Render(table, table.Create, vals, id)
	if err != nil || got != want {
		t.Fatalf("Render = %q, %v\nwant     %q", got, err, want)
	}
	p, err := Compile(table, table.Create)
	if err != nil {
		t.Fatal(err)
	}
	back, ok := p.Match(table, got)
	if !ok {
		t.Fatalf("Match(%s) failed", got)
	}
	for _, f := range []string{"secure", "columns", "primary_key", "tags"} {
		if !SameField(table.Fields[f], vals[f], back[f]) {
			t.Errorf("%s read back %#v, want %#v", f, back[f], vals[f])
		}
	}
	// Not secure: the keyword is left out, and reads back false.
	vals["secure"] = false
	got, _ = Render(table, table.Create, vals, id)
	if !strings.HasPrefix(got, `CREATE TABLE "T"`) {
		t.Errorf("secure false: %s", got)
	}
	if back, _ := p.Match(table, got); back["secure"] != false {
		t.Errorf("secure read back %v", back["secure"])
	}
	for v, want := range map[bool]string{true: `ALTER TABLE "T" SET SECURE`, false: `ALTER TABLE "T" UNSET SECURE`} {
		got, err := Render(table, table.Alter["secure"], map[string]any{"secure": v}, id)
		if err != nil || got != want {
			t.Errorf("alter secure=%v: %q, %v", v, got, err)
		}
		alter, _ := Compile(table, table.Alter["secure"])
		if back, ok := alter.Match(table, got); !ok || back["secure"] != v {
			t.Errorf("alter secure=%v read back %v", v, back)
		}
	}
	// An element's own fields are checked, and errors say where.
	for _, c := range []struct {
		vals map[string]any
		want string
	}{
		{map[string]any{"columns": []any{}}, "spec.columns is required"},
		{map[string]any{"columns": []any{map[string]any{"name": "a"}}}, "spec.columns[0].type is required"},
		{map[string]any{"columns": []any{map[string]any{"name": "a", "type": "INT); DROP TABLE x; --"}}}, "spec.columns[0].type"},
		{map[string]any{"columns": []any{"a"}}, "spec.columns[0]: want an object"},
		{map[string]any{"columns": []any{map[string]any{"name": "a", "type": "INT", "not_null": "yes"}}}, "spec.columns[0].not_null"},
	} {
		if _, err := Render(table, table.Create, c.vals, id); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Render(%v): %v, want %q", c.vals, err, c.want)
		}
	}
}

// A kind with a warehouse names it in each request; createColumns are read
// from the create's own answer; a DESCRIBE property of the Null value is
// not set.
func TestWarehouseCreateColumnsNull(t *testing.T) {
	var warehouses []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		b, _ := io.ReadAll(r.Body)
		var req Request
		_ = json.Unmarshal(b, &req)
		warehouses = append(warehouses, req.Warehouse)
		switch {
		case strings.HasPrefix(req.Statement, "ADD"):
			_, _ = w.Write([]byte(`{"resultSetMetaData":{"rowType":[{"name":"token_name"},{"name":"token_secret"}]},"data":[["T","s3cr3t"]]}`))
		case strings.Contains(req.Statement, "UNSET"):
			_, _ = w.Write([]byte(`{"resultSetMetaData":{"rowType":[{"name":"property"},{"name":"value"}]},"data":[["RSA_PUBLIC_KEY_FP","null"],["NAME","U"]]}`))
		default:
			_, _ = w.Write([]byte(`{"resultSetMetaData":{"rowType":[{"name":"property"},{"name":"value"}]},"data":[["RSA_PUBLIC_KEY_FP","SHA256:x"]]}`))
		}
	}))
	defer srv.Close()
	k := &descriptor.Kind{Read: descriptor.Op{Path: "/api/v2/statements"}, SQL: &descriptor.SQL{
		Mode: descriptor.SQLObject, ID: []string{"name"}, Warehouse: "warehouse",
		Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "warehouse": {Type: "identifier"},
			"token": {Type: "string", Output: true}, "fp": {Type: "string", Output: true}},
		Create:        "ADD {@id}",
		CreateColumns: map[string]string{"token": "token_secret"},
		Read:          []descriptor.SQLRead{{Statement: "DESCRIBE {@id}", Properties: &descriptor.SQLProperties{Name: "property", Value: "value", Null: "null"}, Columns: map[string]string{"fp": "RSA_PUBLIC_KEY_FP"}}},
		Delete:        "DROP {@id}",
	}}
	sk := New(k, rest.New(&rest.Vendor{Name: "t"}, srv.URL, map[string]string{"warehouse": "conn_wh"}), nil)
	made, err := sk.Create(context.Background(), map[string]any{"warehouse": "my wh"}, []string{"T"})
	if err != nil || made["token"] != "s3cr3t" {
		t.Fatalf("Create = %v, %v", made, err)
	}
	obs, err := sk.Read(context.Background(), map[string]any{}, []string{"T"})
	if err != nil || obs["fp"] != "SHA256:x" {
		t.Fatalf("Read = %v, %v", obs, err)
	}
	if _, err := sk.Read(context.Background(), map[string]any{}, []string{"UNSET"}); !errors.Is(err, rest.ErrNotFound) {
		t.Errorf("all properties null: %v, want ErrNotFound", err)
	}
	if strings.Join(warehouses, "|") != `"my wh"|CONN_WH|CONN_WH` {
		t.Errorf("warehouses %q: the spec's, else the Connection's", warehouses)
	}
	k.SQL.Warehouse = ""
	warehouses = nil
	_, _ = sk.Read(context.Background(), map[string]any{}, []string{"T"})
	if warehouses[0] != "" {
		t.Errorf("a kind with no warehouse names none: %q", warehouses)
	}
}

// An enum keyword may be words, but only the enum's; a projection writes
// each element's field ({arg.type}: DROP FUNCTION f(VARCHAR, NUMBER)).
func TestEnumAndProjection(t *testing.T) {
	q := &descriptor.SQL{
		Mode: descriptor.SQLObject, ID: []string{"name"},
		Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "arg": {Type: "args"},
			"behavior": {Type: "keyword", Enum: []string{"CALLED ON NULL INPUT", "STRICT"}},
			"kinds":    {Type: "keywords", Enum: []string{"DATABASES", "SHARES"}}},
		Create: "CREATE F {@id}({arg})[ {behavior}][ KINDS = {kinds}]",
		Delete: "DROP F {@id}({arg.type})",
	}
	vals := map[string]any{"arg": []any{map[string]any{"name": "a", "type": "varchar"}, map[string]any{"name": "b", "type": "number(38, 0)"}},
		"behavior": "called on null input", "kinds": []any{"shares", "DATABASES"}}
	id := []string{"F"}
	for tmpl, want := range map[string]string{
		q.Create: `CREATE F "F"("A" VARCHAR, "B" NUMBER(38, 0)) CALLED ON NULL INPUT KINDS = SHARES, DATABASES`,
		q.Delete: `DROP F "F"(VARCHAR, NUMBER(38, 0))`,
	} {
		got, err := Render(q, tmpl, vals, id)
		if err != nil || got != want {
			t.Errorf("Render = %q, %v; want %q", got, err, want)
			continue
		}
		p, err := Compile(q, tmpl)
		if err != nil {
			t.Fatal(err)
		}
		back, ok := p.Match(q, got)
		if !ok {
			t.Errorf("Match(%s) failed", got)
		}
		if tmpl == q.Delete && fmt.Sprint(back["arg.type"]) != "[VARCHAR NUMBER(38, 0)]" {
			t.Errorf("projection read back %v", back["arg.type"])
		}
		if tmpl == q.Create && (back["behavior"] != "CALLED ON NULL INPUT" || fmt.Sprint(back["kinds"]) != "[SHARES DATABASES]") {
			t.Errorf("enums read back %v", back)
		}
	}
	for _, bad := range []any{"STRICT RETURNS NULL", "strict; DROP DATABASE prod", 5.0} {
		vals["behavior"] = bad
		if _, err := Render(q, q.Create, vals, id); err == nil || !strings.Contains(err.Error(), "spec.behavior") {
			t.Errorf("behavior %v: %v", bad, err)
		}
	}
}

// A read leaves the group's own account out of its allowed accounts.
func TestReadWithout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"resultSetMetaData":{"rowType":[{"name":"name"},{"name":"allowed_accounts"},{"name":"organization_name"},{"name":"account_name"}]},` +
			`"data":[["FG","ORG.ME,ORG.OTHER","ORG","ME"]]}`))
	}))
	defer srv.Close()
	k := &descriptor.Kind{Read: descriptor.Op{Path: "/api/v2/statements"}, SQL: &descriptor.SQL{
		Mode: descriptor.SQLObject, ID: []string{"name"},
		Fields: map[string]descriptor.SQLField{"name": {Type: "identifier"}, "allowed_accounts": {Type: "names"}},
		Read: []descriptor.SQLRead{{Statement: "SHOW FAILOVER GROUPS", Match: map[string]string{"name": "{name}"},
			Columns: map[string]string{"allowed_accounts": "allowed_accounts"}, Without: map[string]string{"allowed_accounts": "organization_name.account_name"}}},
	}}
	obs, err := New(k, rest.New(&rest.Vendor{Name: "t"}, srv.URL, nil), nil).Read(context.Background(), map[string]any{}, []string{"FG"})
	if err != nil || fmt.Sprint(obs["allowed_accounts"]) != "[ORG.OTHER]" {
		t.Fatalf("Read = %v, %v", obs, err)
	}
}

func TestDollarString(t *testing.T) {
	got, err := Value("dollarstring", "models:\n  orchestration: auto\n'quoted' \\ back")
	if err != nil || got != "$$models:\n  orchestration: auto\n'quoted' \\ back$$" {
		t.Fatalf("Value = %q, %v", got, err)
	}
	if back, err := Parse("dollarstring", got); err != nil || back != "models:\n  orchestration: auto\n'quoted' \\ back" {
		t.Errorf("Parse = %q, %v", back, err)
	}
	for _, bad := range []string{"a$$; DROP DATABASE prod; $$", "ends in $", "nul\x00"} {
		if _, err := Value("dollarstring", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNameStrings(t *testing.T) {
	for _, c := range []struct{ typ, in, want, back string }{
		{"namestring", `db."My Sc".t`, `'"DB"."My Sc"."T"'`, `DB."My Sc".T`},
		{"exactstring", "my_schema", `'MY_SCHEMA'`, "MY_SCHEMA"},
		{"exactstring", `"it's"`, `'it''s'`, "it's"},
	} {
		got, err := Value(c.typ, c.in)
		if err != nil || got != c.want {
			t.Errorf("Value(%s, %s) = %s, %v; want %s", c.typ, c.in, got, err, c.want)
		}
		if back, err := Parse(c.typ, got); err != nil || back != c.back {
			t.Errorf("Parse(%s, %s) = %v, %v", c.typ, got, back, err)
		}
	}
}
