// SPDX-License-Identifier: Apache-2.0

// Package sqlapi manages objects a vendor's REST API has no resource for
// with SQL statements sent to its SQL API (Snowflake's POST
// /api/v2/statements): the statements a kind's descriptor (descriptor.SQL)
// holds as templates, filled from the spec, sent one at a time, and their
// answers read back into spec fields.
//
// No spec value reaches a statement as it is written: identifiers are
// double-quoted with " doubled, strings single-quoted with ' doubled and \
// escaped, numbers, booleans, keywords and data types checked against what
// they may be, and an expression (a policy's body, SQL by design) checked to
// be one balanced expression with no ; and no comment. Each request also
// asks for exactly one statement (multi_statement_count 1).
package sqlapi

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// MaxIdentifier is the longest identifier Snowflake takes.
const MaxIdentifier = 255

var (
	plainIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]*$`)
	keywordRE  = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	datatypeRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]*( [A-Z][A-Z0-9_]*)*( ?\( *[A-Z0-9_]+( *, *[A-Z0-9_]+)* *\))?$`)
)

// Ident is the exact name a spec value names, as Snowflake stores it: a
// plain name upper-cased (Snowflake's rule for an unquoted identifier), one
// written in double quotes ("\"x\"", inner quotes doubled) taken as written
// inside them, any other as written.
func Ident(v string) (string, error) {
	exact := v
	switch {
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		inner := v[1 : len(v)-1]
		if strings.Contains(strings.ReplaceAll(inner, `""`, ""), `"`) {
			return "", fmt.Errorf("identifier %q: a \" inside quotes must be doubled", v)
		}
		exact = strings.ReplaceAll(inner, `""`, `"`)
	case plainIdent.MatchString(v):
		exact = strings.ToUpper(v)
	}
	if exact == "" {
		return "", fmt.Errorf("identifier is empty")
	}
	if len(exact) > MaxIdentifier {
		return "", fmt.Errorf("identifier %.20q...: longer than %d", exact, MaxIdentifier)
	}
	if strings.ContainsRune(exact, 0) {
		return "", fmt.Errorf("identifier %q holds a NUL", exact)
	}
	return exact, nil
}

// QuoteExact writes an exact name as a quoted identifier.
func QuoteExact(exact string) string {
	return `"` + strings.ReplaceAll(exact, `"`, `""`) + `"`
}

// QuoteIdent is a spec value as a quoted identifier.
func QuoteIdent(v string) (string, error) {
	exact, err := Ident(v)
	if err != nil {
		return "", err
	}
	return QuoteExact(exact), nil
}

// NameParts splits an object name (db.schema.name, each part plain or in
// double quotes) into its exact parts.
func NameParts(v string) ([]string, error) {
	var parts []string
	for i := 0; ; {
		start := i
		if i < len(v) && v[i] == '"' {
			i++
			for i < len(v) {
				if v[i] == '"' {
					if i+1 < len(v) && v[i+1] == '"' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			if i >= len(v) {
				return nil, fmt.Errorf("name %q: unclosed quote", v)
			}
			i++
		} else {
			for i < len(v) && v[i] != '.' && v[i] != '"' {
				i++
			}
		}
		exact, err := Ident(v[start:i])
		if err != nil {
			return nil, fmt.Errorf("name %q: %w", v, err)
		}
		parts = append(parts, exact)
		if i == len(v) {
			break
		}
		if v[i] != '.' {
			return nil, fmt.Errorf("name %q: want . between its parts", v)
		}
		i++
	}
	if len(parts) > 3 {
		return nil, fmt.Errorf("name %q: more than three parts", v)
	}
	return parts, nil
}

// QuoteName is an object name as quoted identifiers, "DB"."SCHEMA"."NAME".
func QuoteName(v string) (string, error) {
	parts, err := NameParts(v)
	if err != nil {
		return "", err
	}
	return quoteParts(parts), nil
}

func quoteParts(parts []string) string {
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = QuoteExact(p)
	}
	return strings.Join(q, ".")
}

// QuoteString is a string constant: 'it”s', a backslash doubled (Snowflake
// reads backslash escapes in single-quoted strings).
func QuoteString(v string) (string, error) {
	if strings.ContainsRune(v, 0) {
		return "", fmt.Errorf("string holds a NUL")
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(v, `\`, `\\`), "'", "''") + "'", nil
}

// DollarString is a string constant in $$ (Snowflake takes it as written,
// no escapes): the value may hold no $$ and no NUL.
func DollarString(v string) (string, error) {
	if strings.Contains(v, "$$") || strings.ContainsRune(v, 0) {
		return "", fmt.Errorf("a $$ string may hold no $$ and no NUL")
	}
	if strings.HasSuffix(v, "$") {
		return "", fmt.Errorf("a $$ string may not end in $")
	}
	return "$$" + v + "$$", nil
}

// LikePattern is a SHOW ... LIKE pattern matching at least the exact name:
// _ and % are wildcards, and a \, % or _ in the name becomes _ (any one
// character) rather than an escape, so what the pattern also matches is
// sorted out by comparing names exactly.
func LikePattern(exact string) (string, error) {
	p := strings.Map(func(r rune) rune {
		if r == '\\' || r == '%' {
			return '_'
		}
		return r
	}, exact)
	return QuoteString(p)
}

// Keyword is an enum value: letters, digits and _, upper-cased.
func Keyword(v string) (string, error) {
	u := strings.ToUpper(v)
	if !keywordRE.MatchString(u) {
		return "", fmt.Errorf("%q is not a keyword (letters, digits and _)", v)
	}
	return u, nil
}

// DataType is a SQL data type: VARCHAR, VARCHAR(100), NUMBER(38, 0).
func DataType(v string) (string, error) {
	u := strings.ToUpper(strings.Join(strings.Fields(v), " "))
	if !datatypeRE.MatchString(u) {
		return "", fmt.Errorf("%q is not a data type", v)
	}
	return u, nil
}

// Number is a JSON number written bare; integer: a whole one.
func Number(v any, integer bool) (string, error) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		var err error
		if f, err = n.Float64(); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("%v is not a number", v)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("%v is not a finite number", v)
	}
	if integer {
		if f != math.Trunc(f) || math.Abs(f) >= 1<<53 {
			return "", fmt.Errorf("%v is not a whole number", v)
		}
		return strconv.FormatInt(int64(f), 10), nil
	}
	return strconv.FormatFloat(f, 'f', -1, 64), nil
}

// Expression checks SQL that is SQL by design (a policy's body, a view's
// query): one balanced expression, with no ; and no comment outside its
// string constants, quoted identifiers and $$ strings. It is written as is.
func Expression(v string) (string, error) {
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("expression is empty")
	}
	depth := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case 0:
			return "", fmt.Errorf("expression holds a NUL")
		case '\'':
			j := i + 1
			for ; j < len(v); j++ {
				if v[j] == '\\' {
					j++
					continue
				}
				if v[j] == '\'' {
					if j+1 < len(v) && v[j+1] == '\'' {
						j++
						continue
					}
					break
				}
			}
			if j >= len(v) {
				return "", fmt.Errorf("expression: unclosed string")
			}
			i = j
		case '"':
			j := i + 1
			for ; j < len(v); j++ {
				if v[j] == '"' {
					if j+1 < len(v) && v[j+1] == '"' {
						j++
						continue
					}
					break
				}
			}
			if j >= len(v) {
				return "", fmt.Errorf("expression: unclosed quoted identifier")
			}
			i = j
		case '$':
			if i+1 < len(v) && v[i+1] == '$' {
				end := strings.Index(v[i+2:], "$$")
				if end < 0 {
					return "", fmt.Errorf("expression: unclosed $$")
				}
				i += end + 3
			}
		case ';':
			return "", fmt.Errorf("expression: no ; (one statement only)")
		case '-':
			if i+1 < len(v) && v[i+1] == '-' {
				return "", fmt.Errorf("expression: no -- comment")
			}
		case '/':
			if i+1 < len(v) && (v[i+1] == '*' || v[i+1] == '/') {
				return "", fmt.Errorf("expression: no comment")
			}
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return "", fmt.Errorf("expression: unbalanced )")
			}
		}
	}
	if depth != 0 {
		return "", fmt.Errorf("expression: unbalanced (")
	}
	return v, nil
}

// Value writes a spec value as its type says. It never returns a value
// unquoted or unchecked.
func Value(typ string, v any) (string, error) {
	if elem := descriptor.SQLListElem(typ); elem != "" {
		list, ok := v.([]any)
		if !ok {
			return "", fmt.Errorf("want a list, have %T", v)
		}
		out := make([]string, 0, len(list))
		for _, e := range list {
			s, err := Value(elem, e)
			if err != nil {
				return "", err
			}
			out = append(out, s)
		}
		return strings.Join(out, ", "), nil
	}
	switch typ {
	case "number", "integer":
		return Number(v, typ == "integer")
	case "boolean":
		b, ok := v.(bool)
		if !ok {
			return "", fmt.Errorf("%v is not a boolean", v)
		}
		if b {
			return "TRUE", nil
		}
		return "FALSE", nil
	case "value":
		switch x := v.(type) {
		case bool:
			return Value("boolean", x)
		case string:
			return QuoteString(x)
		default:
			return Number(v, false)
		}
	case "arg":
		m, ok := v.(map[string]any)
		if !ok {
			return "", fmt.Errorf("an argument is {name, type}, have %T", v)
		}
		name, _ := m["name"].(string)
		dt, _ := m["type"].(string)
		qn, err := QuoteIdent(name)
		if err != nil {
			return "", fmt.Errorf("argument name: %w", err)
		}
		qt, err := DataType(dt)
		if err != nil {
			return "", fmt.Errorf("argument %s: %w", name, err)
		}
		return qn + " " + qt, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%v is not a string", v)
	}
	switch typ {
	case "string":
		return QuoteString(s)
	case "dollarstring":
		return DollarString(s)
	case "identifier":
		return QuoteIdent(s)
	case "identstring":
		q, err := QuoteIdent(s)
		if err != nil {
			return "", err
		}
		return QuoteString(q)
	case "namestring":
		q, err := QuoteName(s)
		if err != nil {
			return "", err
		}
		return QuoteString(q)
	case "exactstring":
		exact, err := Ident(s)
		if err != nil {
			return "", err
		}
		return QuoteString(exact)
	case "name":
		return QuoteName(s)
	case "keyword":
		return Keyword(s)
	case "datatype":
		return DataType(s)
	case "expression":
		return Expression(s)
	}
	return "", fmt.Errorf("unknown SQL type %q", typ)
}

// FromColumn is a value a read answers (always text, or null) as the spec
// holds it: a number, a boolean, a list; else the text.
func FromColumn(typ, s string) any {
	if elem := descriptor.SQLListElem(typ); elem != "" {
		out := []any{}
		for _, e := range splitList(s) {
			out = append(out, FromColumn(elem, e))
		}
		return out
	}
	switch typ {
	case "number", "integer":
		// A percentage reads back with its sign (a resource monitor's 90%).
		if f, err := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64); err == nil {
			return f
		}
	case "boolean":
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "y", "yes", "on":
			return true
		case "false", "n", "no", "off":
			return false
		}
	}
	return s
}

// splitList reads a list Snowflake answers in one column: ["a","b"] (JSON),
// [a, b] or a,b.
func splitList(s string) []string {
	s = strings.TrimSpace(s)
	var js []any
	if strings.HasPrefix(s, "[") && json.Unmarshal([]byte(s), &js) == nil {
		out := make([]string, 0, len(js))
		for _, e := range js {
			out = append(out, descriptor.Text(e))
		}
		return out
	}
	s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, e := range strings.Split(s, ",") {
		out = append(out, strings.Trim(strings.TrimSpace(e), `'`))
	}
	return out
}

// Same reports whether a value read back is the spec's, as Snowflake keeps
// it: an identifier by its exact name, a keyword or data type ignoring
// case, an expression ignoring runs of white space, a list in any order.
func Same(typ string, desired, observed any) bool {
	if elem := descriptor.SQLListElem(typ); elem != "" {
		d, _ := desired.([]any)
		o, _ := observed.([]any)
		if len(d) != len(o) {
			return false
		}
		used := make([]bool, len(o))
	next:
		for _, de := range d {
			for i, oe := range o {
				if !used[i] && Same(elem, de, oe) {
					used[i] = true
					continue next
				}
			}
			return false
		}
		return true
	}
	if typ == "arg" {
		d, _ := desired.(map[string]any)
		o, _ := observed.(map[string]any)
		return d != nil && o != nil && Same("identifier", d["name"], o["name"]) && Same("datatype", d["type"], o["type"])
	}
	ds, dIsStr := desired.(string)
	os, oIsStr := observed.(string)
	switch typ {
	case "identifier", "identstring", "exactstring":
		if !dIsStr || !oIsStr {
			return false
		}
		want, err := Ident(ds)
		if err != nil {
			return false
		}
		got := os
		if len(os) >= 2 && os[0] == '"' && os[len(os)-1] == '"' {
			got = strings.ReplaceAll(os[1:len(os)-1], `""`, `"`)
		}
		return want == got
	case "name", "namestring":
		if !dIsStr || !oIsStr {
			return false
		}
		want, err := NameParts(ds)
		if err != nil {
			return false
		}
		got, err := observedParts(os)
		if err != nil {
			return false
		}
		return strings.Join(want, "\x00") == strings.Join(got, "\x00")
	case "keyword", "datatype":
		return dIsStr && oIsStr && strings.EqualFold(strings.Join(strings.Fields(ds), " "), strings.Join(strings.Fields(os), " "))
	case "expression":
		return dIsStr && oIsStr && strings.Join(strings.Fields(ds), " ") == strings.Join(strings.Fields(os), " ")
	case "value":
		return strings.EqualFold(descriptor.Text(desired), descriptor.Text(observed))
	case "number", "integer":
		df, ok1 := desired.(float64)
		of, ok2 := observed.(float64)
		return ok1 && ok2 && df == of
	}
	return fmt.Sprint(desired) == fmt.Sprint(observed) && dIsStr == oIsStr
}

// SameField is Same for one field: an object field by its item's fields
// (those the spec sets), a list of them element by element, in order.
func SameField(fd descriptor.SQLField, desired, observed any) bool {
	if fd.Item == nil {
		return Same(fd.Type, desired, observed)
	}
	if fd.Type == "object" {
		return sameObject(fd.Item.Fields, desired, observed)
	}
	d, _ := desired.([]any)
	o, _ := observed.([]any)
	if len(d) != len(o) {
		return false
	}
	for i := range d {
		if !sameObject(fd.Item.Fields, d[i], o[i]) {
			return false
		}
	}
	return true
}

func sameObject(fields map[string]descriptor.SQLField, desired, observed any) bool {
	d, _ := desired.(map[string]any)
	o, _ := observed.(map[string]any)
	if d == nil || o == nil {
		return false
	}
	for k, v := range d {
		if v != nil && !SameField(fields[k], v, o[k]) {
			return false
		}
	}
	return true
}

// observedParts splits a name Snowflake answers: parts as it stores them,
// plain or in double quotes, never upper-cased.
func observedParts(v string) ([]string, error) {
	var parts []string
	for _, p := range splitName(v) {
		if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
			p = strings.ReplaceAll(p[1:len(p)-1], `""`, `"`)
		}
		parts = append(parts, p)
	}
	return parts, nil
}

// splitName splits on the dots outside double quotes.
func splitName(v string) []string {
	var out []string
	in := false
	start := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"':
			in = !in
		case '.':
			if !in {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	return append(out, v[start:])
}

// sortedKeys is a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
