// SPDX-License-Identifier: Apache-2.0

package sqlapi

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Kind is one sql kind's statements, run on one connection.
type Kind struct {
	Q *descriptor.SQL
	R *Runner
}

// New is a kind's statements on api; wait as Runner.Wait.
func New(k *descriptor.Kind, api *rest.Client, wait func(context.Context, time.Duration) error) *Kind {
	return &Kind{Q: k.SQL, R: &Runner{API: api, Endpoint: k.Read.Path, Wait: wait}}
}

// IDOf is the exact name parts the spec's ID fields name.
func IDOf(q *descriptor.SQL, spec map[string]any) ([]string, error) {
	var out []string
	for _, f := range q.ID {
		s, ok := spec[f].(string)
		if !ok || s == "" {
			return nil, fmt.Errorf("spec.%s is required", f)
		}
		exact := s // a string ID is kept as written
		if q.Fields[f].Type != "string" {
			var err error
			if exact, err = Ident(s); err != nil {
				return nil, fmt.Errorf("spec.%s: %w", f, err)
			}
		}
		if len(exact) > MaxIdentifier || strings.ContainsRune(exact, 0) {
			return nil, fmt.Errorf("spec.%s: longer than %d, or holds a NUL", f, MaxIdentifier)
		}
		out = append(out, exact)
	}
	return out, nil
}

// FormatID is an object's status.id: its name as quoted identifiers.
func FormatID(parts []string) string { return quoteParts(parts) }

var quotedID = regexp.MustCompile(`^"(?:[^"]|"")*"(?:\."(?:[^"]|"")*"){0,2}$`)

// ParseID reads a status.id back into exact name parts. Only what FormatID
// writes is taken: a status is not trusted to be SQL.
func ParseID(id string, n int) ([]string, error) {
	if !quotedID.MatchString(id) {
		return nil, fmt.Errorf("status.id %q is not a quoted object name", id)
	}
	var parts []string
	for _, p := range splitName(id) {
		parts = append(parts, strings.ReplaceAll(p[1:len(p)-1], `""`, `"`))
	}
	if len(parts) != n {
		return nil, fmt.Errorf("status.id %q has %d parts, want %d", id, len(parts), n)
	}
	for _, p := range parts {
		if p == "" || strings.ContainsRune(p, 0) || len(p) > MaxIdentifier {
			return nil, fmt.Errorf("status.id %q is not a quoted object name", id)
		}
	}
	return parts, nil
}

// withID is vals with the ID fields set to id's exact names, quoted, so
// that statements and matches use the object's own name.
func (k *Kind) withID(vals map[string]any, id []string) map[string]any {
	if len(id) != len(k.Q.ID) {
		return vals
	}
	out := make(map[string]any, len(vals)+len(id))
	for f, v := range vals {
		out[f] = v
	}
	for i, f := range k.Q.ID {
		if k.Q.Fields[f].Type == "string" {
			out[f] = id[i]
		} else {
			out[f] = QuoteExact(id[i])
		}
	}
	return out
}

// Run renders a template and sends it, in the kind's warehouse if it names
// one.
func (k *Kind) Run(ctx context.Context, tmpl string, vals map[string]any, id []string) (*Result, error) {
	stmt, err := Render(k.Q, tmpl, k.withID(vals, id), id)
	if err != nil {
		return nil, err
	}
	wh, err := k.warehouse(vals)
	if err != nil {
		return nil, err
	}
	return k.R.ExecIn(ctx, stmt, wh)
}

// warehouse is the warehouse a kind's statements run in: the spec's, else
// the Connection's (its Secret's key "warehouse"), else none.
func (k *Kind) warehouse(vals map[string]any) (string, error) {
	if k.Q.Warehouse == "" {
		return "", nil
	}
	name, _ := vals[k.Q.Warehouse].(string)
	if name == "" && k.R.API != nil {
		name = k.R.API.Creds["warehouse"]
	}
	if name == "" {
		return "", nil
	}
	exact, err := Ident(name)
	if err != nil {
		return "", fmt.Errorf("spec.%s: %w", k.Q.Warehouse, err)
	}
	return Display(exact), nil
}

// Create runs the create statement, and returns the output fields its own
// answer holds (createColumns: a token's secret, answered only then).
func (k *Kind) Create(ctx context.Context, vals map[string]any, id []string) (map[string]any, error) {
	res, err := k.Run(ctx, k.Q.Create, vals, id)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	if len(k.Q.CreateColumns) > 0 && len(res.Rows) > 0 {
		row := res.Row(0)
		for f, col := range k.Q.CreateColumns {
			if v := row[col]; v != nil {
				out[f] = FromColumn(k.Q.Fields[f].Type, *v)
			}
		}
	}
	return out, nil
}

// Delete runs the delete statement; for settings set field by field
// (PerField), the unset of each field vals sets.
func (k *Kind) Delete(ctx context.Context, vals map[string]any, id []string) error {
	if k.Q.PerField() {
		for _, f := range sortedKeys(vals) {
			if k.Q.Unset[f] == "" {
				continue
			}
			if err := k.Alter(ctx, f, vals, id, true); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := k.Run(ctx, k.Q.Delete, vals, id)
	return err
}

// Alter runs field's alter statement (its unset when unset).
func (k *Kind) Alter(ctx context.Context, field string, vals map[string]any, id []string, unset bool) error {
	tmpl := k.Q.Alter[field]
	if unset {
		tmpl = k.Q.Unset[field]
	}
	if tmpl == "" {
		return fmt.Errorf("spec.%s cannot be changed", field)
	}
	_, err := k.Run(ctx, tmpl, vals, id)
	return err
}

// Delta runs field's add statement for the elements desired has and
// observed does not, then its remove statement for those observed has and
// desired does not (as Same compares them); either may be nil, an empty
// list for none.
func (k *Kind) Delta(ctx context.Context, field string, vals map[string]any, id []string, desired, observed []any) error {
	elem := descriptor.SQLListElem(k.Q.Fields[field].Type)
	var added, removed []any
	has := func(list []any, match func(any) bool) bool {
		for _, x := range list {
			if match(x) {
				return true
			}
		}
		return false
	}
	for _, d := range desired {
		if !has(observed, func(o any) bool { return Same(elem, d, o) }) {
			added = append(added, d)
		}
	}
	for _, o := range observed {
		if !has(desired, func(d any) bool { return Same(elem, d, o) }) {
			removed = append(removed, exactSpec(elem, o))
		}
	}
	for _, step := range []struct {
		tmpl  string
		elems []any
	}{{k.Q.Add[field], added}, {k.Q.Remove[field], removed}} {
		if len(step.elems) == 0 {
			continue
		}
		if step.tmpl == "" {
			return fmt.Errorf("spec.%s cannot be changed that way", field)
		}
		v := make(map[string]any, len(vals))
		for f, x := range vals {
			v[f] = x
		}
		v[field] = step.elems
		if _, err := k.Run(ctx, step.tmpl, v, id); err != nil {
			return err
		}
	}
	return nil
}

// sameObserved: two names Snowflake answered are the same name.
func sameObserved(a, b string) bool {
	pa, _ := observedParts(a)
	pb, _ := observedParts(b)
	return strings.Join(pa, "\x00") == strings.Join(pb, "\x00")
}

// exactSpec is a value Snowflake answered as a spec would write it: an
// identifier or name in double quotes, so that it keeps its exact case.
func exactSpec(typ string, v any) any {
	str, ok := v.(string)
	if !ok {
		return v
	}
	switch typ {
	case "identifier", "identstring":
		if len(str) >= 2 && str[0] == '"' && str[len(str)-1] == '"' {
			return str
		}
		return QuoteExact(str)
	case "name":
		if parts, err := observedParts(str); err == nil {
			return quoteParts(parts)
		}
	}
	return v
}

// Read runs the reads and returns what they answer, as spec fields:
// rest.ErrNotFound when the first finds no matching row.
func (k *Kind) Read(ctx context.Context, vals map[string]any, id []string) (map[string]any, error) {
	vals = k.withID(vals, id)
	obs := map[string]any{}
	for i, rd := range k.Q.Read {
		res, err := k.Run(ctx, rd.Statement, vals, id)
		if err != nil {
			return nil, err
		}
		// Settings set field by field are on what always exists: a read
		// that selects by nothing and finds nothing finds nothing set.
		optional := k.Q.PerField() && !selectsByField(rd)
		// A field the statement selects by is what it found (SHOW GRANTS
		// TO SHARE {to_share}, DESCRIBE USER {name}), once it found it.
		selected := func() {
			if t, err := descriptor.ParseSQLTemplate(rd.Statement); err == nil {
				for _, f := range t.Fields() {
					if !descriptor.SQLSpecial[f] && vals[f] != nil {
						obs[f] = vals[f]
					}
				}
			}
		}
		if rd.Properties != nil {
			props := map[string]*string{}
			for j := range res.Rows {
				row := res.Row(j)
				if n := row[rd.Properties.Name]; n != nil {
					v := column(row, rd.Properties.Value)
					if v != nil && rd.Properties.Null != "" && *v == rd.Properties.Null {
						v = nil // not set
					}
					props[strings.ToUpper(*n)] = v
				}
			}
			if i == 0 && len(props) == 0 && !optional {
				return nil, rest.ErrNotFound
			}
			found := false
			for f, p := range rd.Columns {
				if v := props[strings.ToUpper(p)]; v != nil {
					obs[f] = FromColumn(k.Q.Fields[f].Type, *v)
					found = true
				}
			}
			if i == 0 && rd.Properties.Null != "" && !found && !optional {
				return nil, rest.ErrNotFound // none of what it maps is set
			}
			selected()
			continue
		}
		if len(rd.ListColumns) > 0 && len(rd.Match) == 0 {
			// A row per element: none is an empty list, not "not found".
			for f, col := range rd.ListColumns {
				elem := descriptor.SQLListElem(k.Q.Fields[f].Type)
				list := []any{}
				for j := range res.Rows {
					if v := res.Row(j)[col]; v != nil {
						list = append(list, FromColumn(elem, *v))
					}
				}
				obs[f] = list
			}
			if i == 0 && len(res.Rows) == 0 && !optional {
				return nil, rest.ErrNotFound
			}
			continue
		}
		rows, ok := k.match(rd, res, vals)
		if !ok && optional {
			continue
		}
		if !ok {
			return nil, rest.ErrNotFound
		}
		selected()
		for col, want := range rd.Match {
			if f, isField := fieldRef(want); isField && descriptor.SQLListElem(k.Q.Fields[f].Type) != "" {
				obs[f] = vals[f] // each element was found
			} else if isField {
				if v := column(rows[0], col); v != nil {
					obs[f] = FromColumn(k.Q.Fields[f].Type, *v)
				}
			}
		}
		for f, col := range rd.Columns {
			if v := column(rows[0], col); v != nil {
				obs[f] = FromColumn(k.Q.Fields[f].Type, *v)
			}
		}
		for f, col := range rd.Without {
			list, _ := obs[f].([]any)
			ex := column(rows[0], col)
			if ex == nil || list == nil {
				continue
			}
			kept := []any{}
			for _, e := range list {
				if s, _ := e.(string); !sameObserved(s, *ex) {
					kept = append(kept, e)
				}
			}
			obs[f] = kept
		}
	}
	return obs, nil
}

// selectsByField: the read's statement or match names a spec field.
func selectsByField(rd descriptor.SQLRead) bool {
	if t, err := descriptor.ParseSQLTemplate(rd.Statement); err == nil {
		for _, f := range t.Fields() {
			if !descriptor.SQLSpecial[f] {
				return true
			}
		}
	}
	for _, v := range rd.Match {
		if _, isField := fieldRef(v); isField {
			return true
		}
	}
	return false
}

// column is a row's value in col; a.b.c joins columns a, b and c as an
// object name's parts (SHOW ... POLICIES ON USER answers a policy's
// database_name, schema_name and name apart).
func column(row map[string]*string, col string) *string {
	cols := strings.Split(col, ".")
	if len(cols) == 1 {
		return row[col]
	}
	parts := make([]string, 0, len(cols))
	for _, c := range cols {
		v := row[c]
		if v == nil || *v == "" {
			return nil
		}
		parts = append(parts, Display(*v))
	}
	joined := strings.Join(parts, ".")
	return &joined
}

func fieldRef(v string) (string, bool) {
	if len(v) > 2 && v[0] == '{' && v[len(v)-1] == '}' {
		return v[1 : len(v)-1], true
	}
	return "", false
}

// match finds the rows that are the object's: those whose match columns
// hold its values; for a list field, one row for each element.
func (k *Kind) match(rd descriptor.SQLRead, res *Result, vals map[string]any) ([]map[string]*string, bool) {
	var list string
	scalar := func(row map[string]*string) bool {
		for _, col := range sortedKeys(rd.Match) {
			want := rd.Match[col]
			got := column(row, col)
			f, isField := fieldRef(want)
			if !isField {
				if got == nil || !strings.EqualFold(*got, want) {
					return false
				}
				continue
			}
			typ := k.Q.Fields[f].Type
			if descriptor.SQLListElem(typ) != "" {
				list = f
				continue
			}
			if vals[f] == nil || got == nil || !Same(typ, vals[f], FromColumn(typ, *got)) {
				return false
			}
		}
		return true
	}
	var rows []map[string]*string
	for i := range res.Rows {
		if row := res.Row(i); scalar(row) {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, false
	}
	if list == "" {
		return rows, true
	}
	var col string
	for c, v := range rd.Match {
		if v == "{"+list+"}" {
			col = c
		}
	}
	elem := descriptor.SQLListElem(k.Q.Fields[list].Type)
	want, _ := vals[list].([]any)
	if len(want) == 0 {
		return nil, false
	}
	var out []map[string]*string
next:
	for _, e := range want {
		for _, row := range rows {
			if got := row[col]; got != nil && Same(elem, e, FromColumn(elem, *got)) {
				out = append(out, row)
				continue next
			}
		}
		return nil, false
	}
	return out, true
}

// Normalize copies observed, taking the spec's own form of each value
// Snowflake keeps the same way (Same): the drift check then compares them
// as equal.
func Normalize(q *descriptor.SQL, desired, observed map[string]any) map[string]any {
	out := make(map[string]any, len(observed))
	for f, v := range observed {
		out[f] = v
	}
	for f, d := range desired {
		if o, ok := observed[f]; ok && SameField(q.Fields[f], d, o) {
			out[f] = d
		}
	}
	return out
}
