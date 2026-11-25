// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SQLShape: an object the vendor's REST API has no resource for, managed
// with SQL statements sent to its SQL API instead (Snowflake's
// POST /api/v2/statements). The kind's Read op names that endpoint; SQL
// holds the statements.
const SQLShape Shape = "sql"

// SQL modes: what the statements make.
const (
	// SQLObject: an object of its own. CREATE makes it, a read (SHOW,
	// DESCRIBE) finds it by its name, ALTER changes a field, DROP removes it.
	SQLObject = "object"
	// SQLSet: a setting on another object (a parameter, a policy attached to
	// a user): Create sets it (ALTER ... SET), a read finds it set, Delete
	// unsets it. A changed spec is set again. With no Create, a set kind is
	// settings set one field at a time (PerField): the account's own
	// parameters, resource monitor and policies, each by an ALTER ACCOUNT
	// of its own.
	SQLSet = "set"
	// SQLGrant: a link with no name (a privilege granted to a share):
	// Create grants it, a read lists grants and finds it, Delete revokes it.
	// A changed spec revokes what was granted, then grants the new.
	SQLGrant = "grant"
)

// SQL is how a kind of shape sql is managed. Every statement is a template
// (SQLTemplate): {field} is a spec field written as its SQLField.Type says,
// always quoted or checked, never pasted in; [...] is a part left out when
// a field in it is not set.
type SQL struct {
	Mode string `json:"mode"`
	// ID lists the spec fields that name the object, outermost first
	// (database, schema, name). {@id} is them as a quoted qualified
	// identifier, {@parent} all but the last, {@like} the last as a SHOW
	// ... LIKE pattern. An object kind keeps {@id} in status.id.
	ID []string `json:"id,omitempty"`
	// Fields says how each spec field is written in SQL and read back.
	Fields map[string]SQLField `json:"fields"`
	// Create makes the object (object), sets the setting (set) or grants
	// the privilege (grant).
	Create string `json:"create,omitempty"`
	// Read: statements whose answers say what the API holds. The first
	// finds the object: no row that matches is "not found". Each later one
	// adds fields (DESCRIBE).
	Read []SQLRead `json:"read"`
	// Alter: per field, the statement that changes it on an object that
	// exists. A field with none can be set only when the object is created.
	Alter map[string]string `json:"alter,omitempty"`
	// Unset: per field, the statement that puts it back to Snowflake's
	// default when the spec no longer sets it.
	Unset map[string]string `json:"unset,omitempty"`
	// Add and Remove: per list field a read maps, the statements that add
	// the elements the spec has and Snowflake does not, and remove those
	// it has and the spec does not, the field holding just those (ALTER
	// FAILOVER GROUP {@id} ADD {allowed_databases} TO ALLOWED_DATABASES):
	// for a list Snowflake has no SET for.
	Add    map[string]string `json:"add,omitempty"`
	Remove map[string]string `json:"remove,omitempty"`
	// UnsetFirst: fields (of a set kind set field by field) whose alter
	// Snowflake refuses while another value is set (a password policy on the
	// account): when the read finds one set, its unset is sent first.
	UnsetFirst []string `json:"unsetFirst,omitempty"`
	// Delete drops the object (object), unsets the setting (set) or revokes
	// the privilege (grant). A set kind set field by field has none: its
	// delete unsets each field it applied.
	Delete string `json:"delete,omitempty"`
	// CreateColumns: output field -> the column of the create's own answer
	// it is read from: what Snowflake answers once, when it makes the
	// object (a programmatic access token's secret). A secret one is in
	// the kind's SecretOutputs, written to spec.writeSecretRef.
	CreateColumns map[string]string `json:"createColumns,omitempty"`
	// Warehouse: the spec field naming the warehouse the statements run in
	// (a materialized view's CREATE runs its query), sent as the request's
	// warehouse. Unset, the Connection's Secret key "warehouse" names it;
	// neither, the user's default warehouse.
	Warehouse string `json:"warehouse,omitempty"`
	// Settle: the object's statements are asynchronous (an Openflow
	// runtime's): Snowflake answers at once and the object reports a status
	// until the work is done. The reconciler waits for the status after
	// each statement, without blocking: it requeues while it waits.
	Settle *SQLSettle `json:"settle,omitempty"`
}

// SQLSettle says how an object whose statements are asynchronous settles,
// and how it is torn down: the status an output field reads (Field), the
// statuses it settles in (Ready), those that end a wait as a failure
// (Failed), those of work still in flight, while which Snowflake refuses
// another statement (Busy), and the statements a delete runs before its
// DROP (Teardown) until the status is one the DROP takes (Gone).
type SQLSettle struct {
	// Field: the output field the first read maps the status to.
	Field string `json:"field"`
	// Ready: the statuses an object is done in (ACTIVE): after a create or
	// a change, and at every read.
	Ready []string `json:"ready"`
	// Failed: statuses a wait ends in as a failure (CREATE_FAILED).
	Failed []string `json:"failed,omitempty"`
	// Busy: statuses of work in flight (CREATING, UPDATING): no change and
	// no teardown statement is sent in one, as Snowflake refuses it.
	Busy []string `json:"busy,omitempty"`
	// Teardown: what a delete does before its DROP, by status: the first
	// step whose When holds the status (a step with none: any other)
	// runs its statement, if it has one, and the delete waits again.
	Teardown []SQLTeardownStep `json:"teardown,omitempty"`
	// Gone: the statuses the DROP is sent in (DELETED, TERMINATED); none:
	// the DROP is sent once nothing is Busy.
	Gone []string `json:"gone,omitempty"`
	// Timeout: how long a wait may last before it is an error (a Go
	// duration; default 30m). The wait goes on after it, with backoff.
	Timeout string `json:"timeout,omitempty"`
}

// SQLTeardownStep is one step of a delete: in the statuses When, run Run
// (none: wait).
type SQLTeardownStep struct {
	When []string `json:"when,omitempty"`
	Run  string   `json:"run,omitempty"`
}

// DefaultSettleTimeout is a settle's Timeout when it names none: as
// Terraform's provider waits for an Openflow object.
const DefaultSettleTimeout = 30 * time.Minute

// TimeoutOf is how long the wait may last.
func (s *SQLSettle) TimeoutOf() time.Duration {
	if d, err := time.ParseDuration(s.Timeout); err == nil && d > 0 {
		return d
	}
	return DefaultSettleTimeout
}

// SettleIn reports whether status is one of list (ignoring case).
func SettleIn(list []string, status string) bool {
	for _, s := range list {
		if strings.EqualFold(s, status) {
			return true
		}
	}
	return false
}

// SQLField is how one spec field is written into a statement.
type SQLField struct {
	// Type is one of SQLTypes.
	Type string `json:"type"`
	// Output: a value Snowflake makes (an integration's IAM user ARN, a
	// consent URL), read back into status.atProvider; never a spec field,
	// never in a statement.
	Output bool `json:"output,omitempty"`
	// Item, for type object (one) and objects (a list): how one is
	// written, from its own fields (a column's name, type, NOT NULL).
	Item *SQLItem `json:"item,omitempty"`
	// Enum, for type keyword and keywords: the values it may take, each
	// words of letters, digits and _ (CALLED ON NULL INPUT). Any other is
	// refused before it reaches a statement.
	Enum []string `json:"enum,omitempty"`
	// Projection: a {list.sub} placeholder's field (SQLFieldAt), a list of
	// the elements' sub values; never in a descriptor.
	Projection bool `json:"-"`
}

// SQLEnumValue is what an enum value may be: words of letters, digits
// and _, one space between them.
var SQLEnumValue = regexp.MustCompile(`^[A-Z][A-Z0-9_]*( [A-Z][A-Z0-9_]*)*$`)

// SQLArgFields are an args element's fields, for a projection ({arg.type}).
var SQLArgFields = map[string]SQLField{"name": {Type: "identifier"}, "type": {Type: "datatype"}}

// SQLFieldAt is the field a placeholder names: a field, or a projection
// {list.sub}, a structured list's (objects, args) elements' field sub,
// written as a list of them (DROP FUNCTION {@id}({arg.type})).
func SQLFieldAt(fields map[string]SQLField, name string) (SQLField, bool) {
	if fd, ok := fields[name]; ok {
		return fd, true
	}
	base, sub, dotted := strings.Cut(name, ".")
	if !dotted {
		return SQLField{}, false
	}
	fd, ok := fields[base]
	if !ok || fd.Output {
		return SQLField{}, false
	}
	var subs map[string]SQLField
	switch {
	case fd.Type == "args":
		subs = SQLArgFields
	case fd.Type == "objects" && fd.Item != nil:
		subs = fd.Item.Fields
	default:
		return SQLField{}, false
	}
	sf, ok := subs[sub]
	if !ok || sf.Item != nil || SQLListElem(sf.Type) != "" {
		return SQLField{}, false
	}
	return SQLField{Type: sf.Type, Enum: sf.Enum, Projection: true}, true
}

// SQLItem is how a structured value is written: Template, a statement
// template over Fields (the value's own), and between a list's elements
// Sep (default ", ").
type SQLItem struct {
	Template string              `json:"template"`
	Fields   map[string]SQLField `json:"fields"`
	Sep      string              `json:"sep,omitempty"`
}

// Separator is what goes between a list's elements.
func (i *SQLItem) Separator() string {
	if i.Sep == "" {
		return ", "
	}
	return i.Sep
}

// SQLRead is one read statement and how its answer maps to spec fields.
type SQLRead struct {
	Statement string `json:"statement"`
	// Match: the columns a row must have to be the object's, each a
	// {field} (an identifier compared exactly as Snowflake stores it; a
	// list: one row for each element) or a literal (compared ignoring
	// case). No match: the first row. A key a.b.c joins columns as a
	// name's parts (POLICY_DB.POLICY_SCHEMA.POLICY_NAME). On a grant, the
	// Columns too must read back as the spec has them, or it is made
	// again.
	Match map[string]string `json:"match,omitempty"`
	// Properties: the answer is one row per property (DESCRIBE ... of an
	// integration): Name is the column naming the property, Value the one
	// holding its value, and Columns name properties instead of columns.
	Properties *SQLProperties `json:"properties,omitempty"`
	// Columns: spec field -> the column (or property) it is read back from;
	// a.b.c: columns a, b and c joined as an object name's parts. A field no
	// read maps is write-only: never compared.
	Columns map[string]string `json:"columns,omitempty"`
	// ListColumns: list field -> the column each row holds one element of
	// (SHOW DATABASES IN FAILOVER GROUP: a row per database). No rows is
	// an empty list.
	ListColumns map[string]string `json:"listColumns,omitempty"`
	// Without: list field -> the column (a.b joins, as in Columns) whose
	// value the read leaves out of the list: SHOW FAILOVER GROUPS lists the
	// group's own account among its allowed accounts,
	// organization_name.account_name.
	Without map[string]string `json:"without,omitempty"`
}

// SQLProperties names a property-per-row answer's columns.
type SQLProperties struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Null: the value that means "not set" (DESCRIBE USER's "null"): such
	// a property is not read, and a first read none of whose properties
	// is set finds nothing.
	Null string `json:"null,omitempty"`
}

// SQLTypes are the field types a statement can hold, and how each is
// written:
//
//   - string: a string constant, 'it”s', backslashes doubled
//   - identifier: a quoted identifier, "NAME": a plain name (letters,
//     digits, _ and $) is upper-cased as Snowflake does an unquoted one;
//     any other, or one written in double quotes ("\"x\""), is kept as is
//   - name: an object name of up to three identifiers, db.schema.name
//   - identstring: an identifier as a string constant holds it, quoted
//     inside ('"ROLE"': a SCIM integration's RUN_AS_ROLE)
//   - exactstring: an identifier's exact name as a string constant
//     ('MY_SCHEMA': INFORMATION_SCHEMA's TABLE_SCHEMA = ...)
//   - namestring: an object name as a string constant holds it
//     ('"DB"."SC"."T"': POLICY_REFERENCES' REF_ENTITY_NAME)
//   - keyword: a word of letters, digits and _ (an enum value), upper-cased
//   - number, integer, boolean: checked, written bare (TRUE, FALSE)
//   - value: a parameter value: a number or boolean bare, else a string
//   - datatype: a SQL data type, VARCHAR(100), NUMBER(38, 0)
//   - dollarstring: a string constant in $$ (a YAML specification), which
//     may hold no $$
//   - expression: SQL by design (a policy's body): checked to be one
//     balanced expression, with no ; and no comment, and written as is
//   - strings, identifiers, names, keywords: lists, comma-separated
//   - args: a signature, a list of {name, type}: "NAME" TYPE, ...
//   - object, objects: a structured value, or a list of them, each written
//     by its Item template from its own fields (a table's columns)
var SQLTypes = map[string]bool{
	"string": true, "identifier": true, "name": true, "keyword": true, "number": true, "integer": true,
	"boolean": true, "value": true, "datatype": true, "expression": true, "identstring": true, "dollarstring": true, "namestring": true, "exactstring": true,
	"strings": true, "identifiers": true, "names": true, "keywords": true, "args": true,
	"object": true, "objects": true,
}

// SQLListElem is a list type's element type ("" for a scalar type).
func SQLListElem(typ string) string {
	switch typ {
	case "strings":
		return "string"
	case "identifiers":
		return "identifier"
	case "names":
		return "name"
	case "keywords":
		return "keyword"
	case "args":
		return "arg"
	}
	return ""
}

// SQLTemplate is a parsed statement template.
type SQLTemplate struct {
	Parts []SQLPart
}

// SQLPart is literal text, a {placeholder}, or an [optional] section.
type SQLPart struct {
	Text  string
	Field string // a placeholder: a spec field or @id, @parent, @like
	// Flag, for {?field} and {!field}: a boolean field that writes nothing
	// and is set only when true (?) or only when false (!), so that the
	// [optional] part around it is a keyword turned on and off
	// ([ SECURE{?secure}]).
	Flag byte
	// As, for {field:type}: the field written as another type than its
	// own (a parameter's name, a keyword, as the string SHOW ... LIKE takes).
	As       string
	Optional *SQLTemplate
}

// SQLSpecial are the placeholders that are not spec fields.
var SQLSpecial = map[string]bool{"@id": true, "@parent": true, "@like": true}

// sqlLiteral is what a template's literal text may hold: keywords
// (SYSTEM$GET_TAG), operators and punctuation, constant strings and quoted
// identifiers of the same ('ACCOUNT', AS "TAG"), never a quote of anything
// else, a $$, a ; or a comment.
var sqlLiteral = regexp.MustCompile(`^[A-Za-z0-9_ =(),.>*%$-]*$`)

// sqlConst is a constant a template may hold: 'NETWORK_POLICY', "TAG".
var sqlConst = regexp.MustCompile(`'[A-Za-z0-9_ ]*'|"[A-Za-z0-9_]+"`)

// ParseSQLTemplate parses a statement template and checks its literal text.
func ParseSQLTemplate(s string) (*SQLTemplate, error) {
	t, rest, err := parseSQLParts(s, 0)
	if err != nil {
		return nil, err
	}
	if rest != len(s) {
		return nil, fmt.Errorf("unbalanced ] at %d in %q", rest, s)
	}
	return t, nil
}

func parseSQLParts(s string, i int) (*SQLTemplate, int, error) {
	t := &SQLTemplate{}
	var text strings.Builder
	flush := func() error {
		if text.Len() == 0 {
			return nil
		}
		if lit := sqlConst.ReplaceAllString(text.String(), ""); strings.Contains(lit, "--") || strings.Contains(lit, "$$") || !sqlLiteral.MatchString(lit) {
			return fmt.Errorf("literal %q may hold only keywords and punctuation", text.String())
		}
		t.Parts = append(t.Parts, SQLPart{Text: text.String()})
		text.Reset()
		return nil
	}
	for i < len(s) {
		switch c := s[i]; c {
		case '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, 0, fmt.Errorf("unclosed { in %q", s)
			}
			if err := flush(); err != nil {
				return nil, 0, err
			}
			name := s[i+1 : i+end]
			name, as, _ := strings.Cut(name, ":")
			var flag byte
			if name != "" && (name[0] == '?' || name[0] == '!') {
				flag, name = name[0], name[1:]
			}
			if name == "" || strings.ContainsAny(name, "{[]?!") || (as != "" && (flag != 0 || !SQLTypes[as] || SQLListElem(as) != "" || as == "object" || as == "objects")) {
				return nil, 0, fmt.Errorf("bad placeholder {%s} in %q", s[i+1:i+end], s)
			}
			t.Parts = append(t.Parts, SQLPart{Field: name, As: as, Flag: flag})
			i += end + 1
		case '[':
			if err := flush(); err != nil {
				return nil, 0, err
			}
			sub, next, err := parseSQLParts(s, i+1)
			if err != nil {
				return nil, 0, err
			}
			if next >= len(s) || s[next] != ']' {
				return nil, 0, fmt.Errorf("unclosed [ in %q", s)
			}
			t.Parts = append(t.Parts, SQLPart{Optional: sub})
			i = next + 1
		case ']':
			if err := flush(); err != nil {
				return nil, 0, err
			}
			return t, i, nil
		case '}':
			return nil, 0, fmt.Errorf("unbalanced } in %q", s)
		default:
			text.WriteByte(c)
			i++
		}
	}
	if err := flush(); err != nil {
		return nil, 0, err
	}
	return t, i, nil
}

// Fields lists the placeholders of a template, sections included.
func (t *SQLTemplate) Fields() []string {
	var out []string
	for _, p := range t.Parts {
		switch {
		case p.Field != "":
			out = append(out, p.Field)
		case p.Optional != nil:
			out = append(out, p.Optional.Fields()...)
		}
	}
	return out
}

// Placeholders lists the placeholder parts of a template, sections
// included.
func (t *SQLTemplate) Placeholders() []SQLPart {
	var out []SQLPart
	for _, p := range t.Parts {
		switch {
		case p.Field != "":
			out = append(out, p)
		case p.Optional != nil:
			out = append(out, p.Optional.Placeholders()...)
		}
	}
	return out
}

// Required lists the placeholders outside any optional section.
func (t *SQLTemplate) Required() []string {
	var out []string
	for _, p := range t.Parts {
		if p.Field != "" {
			out = append(out, p.Field)
		}
	}
	return out
}

// Templates is every statement template of the kind, by a name for errors.
func (q *SQL) Templates() map[string]string {
	out := map[string]string{}
	if q.Create != "" {
		out["create"] = q.Create
	}
	if q.Delete != "" {
		out["delete"] = q.Delete
	}
	for i, r := range q.Read {
		out[fmt.Sprintf("read[%d]", i)] = r.Statement
	}
	for f, s := range q.Alter {
		out["alter."+f] = s
	}
	for f, s := range q.Unset {
		out["unset."+f] = s
	}
	for f, s := range q.Add {
		out["add."+f] = s
	}
	for f, s := range q.Remove {
		out["remove."+f] = s
	}
	if q.Settle != nil {
		for i, st := range q.Settle.Teardown {
			if st.Run != "" {
				out[fmt.Sprintf("settle.teardown[%d]", i)] = st.Run
			}
		}
	}
	return out
}

// Settable reports whether a field can be changed on an object that exists.
func (q *SQL) Settable(field string) bool {
	if q == nil {
		return false
	}
	if q.PerField() {
		return q.Alter[field] != ""
	}
	if q.Mode == SQLSet {
		return q.Fields[field].Type != ""
	}
	_, ok := q.Alter[field]
	return ok || q.Add[field] != "" || q.Remove[field] != ""
}

// PerField reports whether a set kind is settings set one field at a time:
// no create, an alter per field (sent only for a field that differs), an
// unset per field (sent for one the spec drops, and for each on delete).
// The thing they are set on always exists (the account): a read that
// selects by no spec field and finds nothing finds nothing set; one that
// selects by a field (the organization account by its name) and finds
// nothing is an error.
func (q *SQL) PerField() bool {
	return q != nil && q.Mode == SQLSet && q.Create == ""
}

// Readable reports whether a read maps the field, or selects by it (SHOW
// GRANTS TO SHARE {to_share}): only those are compared.
func (q *SQL) Readable(field string) bool {
	for _, r := range q.Read {
		if t, err := ParseSQLTemplate(r.Statement); err == nil {
			for _, f := range t.Fields() {
				if f == field {
					return true
				}
			}
		}
		if _, ok := r.Columns[field]; ok {
			return true
		}
		if _, ok := r.ListColumns[field]; ok {
			return true
		}
		for _, v := range r.Match {
			if v == "{"+field+"}" {
				return true
			}
		}
	}
	return false
}

func (k *Kind) validateSQL() error {
	q := k.SQL
	if q == nil {
		return fmt.Errorf("sql shape needs sql")
	}
	if k.Read.Method != "POST" || k.Read.Path == "" {
		return fmt.Errorf("sql: read must be the SQL API's endpoint (POST /api/v2/statements)")
	}
	if k.Create != nil || k.Update != nil || k.Delete != nil || k.ID != nil || len(k.Params) > 0 || k.Replace != nil || len(k.BeforeDelete) > 0 {
		return fmt.Errorf("sql: create, update, delete, id, params, replace and beforeDelete are for REST kinds")
	}
	switch q.Mode {
	case SQLObject:
		if len(q.ID) == 0 {
			return fmt.Errorf("sql: an object needs id")
		}
	case SQLSet, SQLGrant:
	default:
		return fmt.Errorf("sql: unknown mode %q", q.Mode)
	}
	switch {
	case q.PerField():
		if len(q.Alter) == 0 || q.Delete != "" || len(q.Read) == 0 {
			return fmt.Errorf("sql: a set kind with no create sets each field by its alter, reads, and unsets each field: alter and read are required, delete is not taken")
		}
	case q.Create == "" || q.Delete == "" || len(q.Read) == 0:
		return fmt.Errorf("sql: create, read and delete are required")
	}
	for _, f := range q.UnsetFirst {
		if !q.PerField() || q.Alter[f] == "" || q.Unset[f] == "" || !q.Readable(f) {
			return fmt.Errorf("sql.unsetFirst.%s: for a field of a set kind with no create that has an alter and an unset and a read maps", f)
		}
	}
	if err := validateSQLFields("sql.fields", q.Fields); err != nil {
		return err
	}
	for _, f := range q.ID {
		// A string: a name Snowflake keeps as written, a procedure's
		// argument (a Postgres mirror's mirror_name => '...').
		if t := q.Fields[f].Type; t != "identifier" && t != "string" {
			return fmt.Errorf("sql.id: %s must be an identifier or string field, is %q", f, t)
		}
	}
	if q.Warehouse != "" {
		if fd, ok := q.Fields[q.Warehouse]; !ok || fd.Type != "identifier" || fd.Output {
			return fmt.Errorf("sql.warehouse: %s must be an identifier field", q.Warehouse)
		}
	}
	if err := q.validateSettle(); err != nil {
		return err
	}
	for f := range q.CreateColumns {
		if !q.Fields[f].Output {
			return fmt.Errorf("sql.createColumns.%s: not an output field", f)
		}
	}
	names := make([]string, 0)
	tmpls := q.Templates()
	for n := range tmpls {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		t, err := ParseSQLTemplate(tmpls[n])
		if err != nil {
			return fmt.Errorf("sql.%s: %w", n, err)
		}
		if err := checkSQLPlaceholders("sql."+n, t, q.Fields, len(q.ID) > 0); err != nil {
			return err
		}
	}
	if q.Mode == SQLObject {
		// Statements on the object name it by {@id}: the name it was made
		// with (status.id), never a spec field that may have changed since.
		// A statement that cannot say {@id} (ALTER USER {user_name} ADD
		// PROGRAMMATIC ACCESS TOKEN {name}) names every ID field instead:
		// they too are the made name's parts there.
		for n, s := range tmpls {
			if strings.HasPrefix(n, "read") || strings.Contains(s, "{@id}") {
				continue
			}
			t, _ := ParseSQLTemplate(s)
			have := map[string]bool{}
			for _, f := range t.Required() {
				have[f] = true
			}
			for _, f := range q.ID {
				if !have[f] {
					return fmt.Errorf("sql.%s: an object's statement names it by {@id}, or by all its id fields outside [...]", n)
				}
			}
		}
	}
	for f := range q.Alter {
		if _, ok := q.Fields[f]; !ok {
			return fmt.Errorf("sql.alter.%s: not a field", f)
		}
	}
	for f := range q.Unset {
		if _, ok := q.Fields[f]; !ok {
			return fmt.Errorf("sql.unset.%s: not a field", f)
		}
	}
	for what, m := range map[string]map[string]string{"add": q.Add, "remove": q.Remove} {
		for f := range m {
			if SQLListElem(q.Fields[f].Type) == "" || !q.Readable(f) {
				return fmt.Errorf("sql.%s.%s: for a list field a read maps", what, f)
			}
		}
	}
	for i, r := range q.Read {
		for f := range r.Columns {
			if _, ok := q.Fields[f]; !ok {
				return fmt.Errorf("sql.read[%d].columns.%s: not a field", i, f)
			}
		}
		for f := range r.ListColumns {
			if SQLListElem(q.Fields[f].Type) == "" {
				return fmt.Errorf("sql.read[%d].listColumns.%s: not a list field", i, f)
			}
		}
		for f := range r.Without {
			if _, mapped := r.Columns[f]; !mapped || SQLListElem(q.Fields[f].Type) == "" {
				return fmt.Errorf("sql.read[%d].without.%s: not a list field the read maps", i, f)
			}
		}
		for col, v := range r.Match {
			if strings.HasPrefix(v, "{") {
				f := strings.Trim(v, "{}")
				if _, ok := q.Fields[f]; !ok {
					return fmt.Errorf("sql.read[%d].match.%s: {%s} is not a field", i, col, f)
				}
			}
		}
	}
	return nil
}

// validateSettle: a settle is an object's; its field an output its first
// read maps; its statuses words, its teardown statements on the object.
func (q *SQL) validateSettle() error {
	st := q.Settle
	if st == nil {
		return nil
	}
	if q.Mode != SQLObject {
		return fmt.Errorf("sql.settle: for an object")
	}
	if fd, ok := q.Fields[st.Field]; !ok || !fd.Output {
		return fmt.Errorf("sql.settle.field: %q must be an output field", st.Field)
	}
	if _, ok := q.Read[0].Columns[st.Field]; !ok {
		return fmt.Errorf("sql.settle.field: the first read must map %s", st.Field)
	}
	if len(st.Ready) == 0 {
		return fmt.Errorf("sql.settle.ready: the statuses it settles in are required")
	}
	for what, list := range map[string][]string{"ready": st.Ready, "failed": st.Failed, "busy": st.Busy, "gone": st.Gone} {
		for _, v := range list {
			if !SQLEnumValue.MatchString(strings.ToUpper(v)) {
				return fmt.Errorf("sql.settle.%s: %q is not a status", what, v)
			}
		}
	}
	for i, step := range st.Teardown {
		for _, v := range step.When {
			if !SQLEnumValue.MatchString(strings.ToUpper(v)) {
				return fmt.Errorf("sql.settle.teardown[%d].when: %q is not a status", i, v)
			}
		}
		if step.Run != "" && !strings.Contains(step.Run, "{@id}") {
			return fmt.Errorf("sql.settle.teardown[%d].run: names the object by {@id}", i)
		}
	}
	if st.Timeout != "" {
		if d, err := time.ParseDuration(st.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("sql.settle.timeout: %q is not a duration", st.Timeout)
		}
	}
	return nil
}

// validateSQLFields checks fields' types, and the items of structured ones.
func validateSQLFields(where string, fields map[string]SQLField) error {
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		fd := fields[f]
		if !SQLTypes[fd.Type] {
			return fmt.Errorf("%s.%s: unknown type %q", where, f, fd.Type)
		}
		if len(fd.Enum) > 0 {
			if fd.Type != "keyword" && fd.Type != "keywords" {
				return fmt.Errorf("%s.%s: enum is for type keyword and keywords", where, f)
			}
			for _, v := range fd.Enum {
				if !SQLEnumValue.MatchString(strings.ToUpper(v)) {
					return fmt.Errorf("%s.%s: enum value %q is not words of letters, digits and _", where, f, v)
				}
			}
		}
		structured := fd.Type == "object" || fd.Type == "objects"
		if structured != (fd.Item != nil) {
			return fmt.Errorf("%s.%s: an item is what type object and objects need, and only they", where, f)
		}
		if fd.Item == nil {
			continue
		}
		if fd.Output {
			return fmt.Errorf("%s.%s: an output is not structured", where, f)
		}
		if !sqlLiteral.MatchString(fd.Item.Sep) {
			return fmt.Errorf("%s.%s.item.sep: may hold only keywords and punctuation", where, f)
		}
		if err := validateSQLFields(where+"."+f+".item.fields", fd.Item.Fields); err != nil {
			return err
		}
		t, err := ParseSQLTemplate(fd.Item.Template)
		if err != nil {
			return fmt.Errorf("%s.%s.item.template: %w", where, f, err)
		}
		if err := checkSQLPlaceholders(where+"."+f+".item.template", t, fd.Item.Fields, false); err != nil {
			return err
		}
	}
	return nil
}

// checkSQLPlaceholders: each placeholder is a field the template can send
// (or @id, @parent, @like where the object has a name), a flag a boolean.
func checkSQLPlaceholders(where string, t *SQLTemplate, fields map[string]SQLField, named bool) error {
	for _, p := range t.Placeholders() {
		if SQLSpecial[p.Field] {
			if !named || p.Flag != 0 {
				return fmt.Errorf("%s: {%s} needs id", where, p.Field)
			}
			continue
		}
		fd, ok := SQLFieldAt(fields, p.Field)
		if !ok || fd.Output {
			return fmt.Errorf("%s: {%s} is not a field it can send", where, p.Field)
		}
		if p.Flag != 0 && (fd.Type != "boolean" || fd.Projection) {
			return fmt.Errorf("%s: {%c%s} is a flag, for a boolean field", where, p.Flag, p.Field)
		}
	}
	return nil
}
