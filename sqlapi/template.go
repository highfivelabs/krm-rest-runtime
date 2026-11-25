// SPDX-License-Identifier: Apache-2.0

package sqlapi

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// Render fills a statement template: each {field} from vals, written as the
// field's type says (Value); @id, @parent and @like from id, the object's
// exact name parts. An [optional] part whose fields are not all set is left
// out (one holding only other optional parts is kept when any of them is);
// a field outside any is required. A flag, {?field} ({!field}), writes
// nothing and is set when the boolean is true (false); an object or a list
// of objects is written by its item's template, element by element.
func Render(q *descriptor.SQL, tmpl string, vals map[string]any, id []string) (string, error) {
	return renderWith(q.Fields, tmpl, vals, id)
}

func renderWith(fields map[string]descriptor.SQLField, tmpl string, vals map[string]any, id []string) (string, error) {
	t, err := descriptor.ParseSQLTemplate(tmpl)
	if err != nil {
		return "", err
	}
	out, _, err := render(fields, t, vals, id, true)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func render(fields map[string]descriptor.SQLField, t *descriptor.SQLTemplate, vals map[string]any, id []string, top bool) (string, bool, error) {
	var b strings.Builder
	direct, nested := 0, false
	for _, p := range t.Parts {
		switch {
		case p.Optional != nil:
			s, kept, err := render(fields, p.Optional, vals, id, false)
			if err != nil {
				return "", false, err
			}
			if kept {
				nested = true
				b.WriteString(s)
			}
		case p.Field != "":
			direct++
			s, ok, err := placeholder(fields, p, vals, id)
			if err != nil {
				return "", false, err
			}
			if !ok {
				if top {
					return "", false, fmt.Errorf("spec.%s is required", p.Field)
				}
				return "", false, nil
			}
			b.WriteString(s)
		default:
			b.WriteString(p.Text)
		}
	}
	return b.String(), top || direct > 0 || nested, nil
}

func placeholder(fields map[string]descriptor.SQLField, p descriptor.SQLPart, vals map[string]any, id []string) (string, bool, error) {
	f, as := p.Field, p.As
	switch f {
	case "@id":
		if len(id) == 0 {
			return "", false, nil
		}
		return quoteParts(id), true, nil
	case "@parent":
		if len(id) < 2 {
			return "", false, nil
		}
		return quoteParts(id[:len(id)-1]), true, nil
	case "@like":
		if len(id) == 0 {
			return "", false, nil
		}
		s, err := LikePattern(id[len(id)-1])
		return s, err == nil, err
	}
	fd, _ := descriptor.SQLFieldAt(fields, f)
	if fd.Projection {
		return projection(fd, f, vals)
	}
	v, ok := vals[f]
	if !ok || v == nil {
		return "", false, nil
	}
	if p.Flag != 0 {
		b, isBool := v.(bool)
		if !isBool {
			return "", false, fmt.Errorf("spec.%s: %v is not a boolean", f, v)
		}
		return "", b == (p.Flag == '?'), nil
	}
	if fd.Item != nil {
		s, ok, err := items(fd, v)
		if err != nil {
			return "", false, fmt.Errorf("spec.%s%w", f, err)
		}
		return s, ok, nil
	}
	typ := fd.Type
	if as != "" {
		typ = as
	}
	if len(fd.Enum) > 0 && as == "" {
		s, err := enumValue(fd, v)
		if err != nil {
			return "", false, fmt.Errorf("spec.%s: %w", f, err)
		}
		return s, true, nil
	}
	s, err := Value(typ, v)
	if err != nil {
		return "", false, fmt.Errorf("spec.%s: %w", f, err)
	}
	return s, true, nil
}

// enumValue writes a keyword (or each of keywords) that must be one of the
// field's enum values, as that value is written.
func enumValue(fd descriptor.SQLField, v any) (string, error) {
	one := func(e any) (string, error) {
		s, _ := e.(string)
		for _, ok := range fd.Enum {
			if strings.EqualFold(s, ok) {
				return strings.ToUpper(ok), nil
			}
		}
		return "", fmt.Errorf("%v is not one of %s", e, strings.Join(fd.Enum, ", "))
	}
	if fd.Type == "keyword" {
		return one(v)
	}
	list, ok := v.([]any)
	if !ok {
		return "", fmt.Errorf("want a list, have %T", v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, err := one(e)
		if err != nil {
			return "", err
		}
		out = append(out, s)
	}
	return strings.Join(out, ", "), nil
}

// projection writes {list.sub}: each element's sub value, comma-separated
// (an external function's argument types); an empty list writes nothing.
func projection(fd descriptor.SQLField, f string, vals map[string]any) (string, bool, error) {
	base, sub, _ := strings.Cut(f, ".")
	v, ok := vals[base]
	if !ok || v == nil {
		return "", false, nil
	}
	list, ok := v.([]any)
	if !ok {
		return "", false, fmt.Errorf("spec.%s: want a list, have %T", base, v)
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		m, _ := e.(map[string]any)
		s, err := Value(fd.Type, m[sub])
		if err != nil {
			return "", false, fmt.Errorf("spec.%s[%d].%s: %w", base, i, sub, err)
		}
		out = append(out, s)
	}
	return strings.Join(out, ", "), true, nil
}

// itemError prefixes an element's error with where it is (spec.columns[1].name).
type itemError struct {
	at  string
	err error
}

func (e *itemError) Error() string {
	msg := e.err.Error()
	if rest, ok := strings.CutPrefix(msg, "spec."); ok {
		return e.at + "." + rest // a field of the element's
	}
	return e.at + ": " + msg
}

func (e *itemError) Unwrap() error { return e.err }

// items writes an object (type object) or each of a list's (objects) by
// the item's template; an empty list is not set.
func items(fd descriptor.SQLField, v any) (string, bool, error) {
	one := func(at string, e any) (string, error) {
		m, ok := e.(map[string]any)
		if !ok {
			return "", &itemError{at, fmt.Errorf("want an object, have %T", e)}
		}
		s, err := renderWith(fd.Item.Fields, fd.Item.Template, m, nil)
		if err != nil {
			return "", &itemError{at, err}
		}
		return s, nil
	}
	if fd.Type == "object" {
		s, err := one("", v)
		return s, err == nil, err
	}
	list, ok := v.([]any)
	if !ok {
		return "", false, fmt.Errorf(": want a list, have %T", v)
	}
	if len(list) == 0 {
		return "", false, nil
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		s, err := one(fmt.Sprintf("[%d]", i), e)
		if err != nil {
			return "", false, err
		}
		out = append(out, s)
	}
	return strings.Join(out, fd.Item.Separator()), true, nil
}

// Token patterns: what Value writes for each type, for the fake to read a
// statement back.
const (
	reString = `'(?:[^'\\]|''|\\.)*'`
	reIdent  = `"(?:[^"]|"")*"`
	reNumber = `-?[0-9]+(?:\.[0-9]+)?`
	reType   = `[A-Z][A-Z0-9_]*(?: [A-Z][A-Z0-9_]*)*?(?: ?\( *[A-Z0-9_]+(?: *, *[A-Z0-9_]+)* *\))?`
)

var reName = reIdent + `(?:\.` + reIdent + `){0,2}`

func tokenRE(fd descriptor.SQLField, typ string) string {
	if fd.Projection {
		e := tokenRE(descriptor.SQLField{}, typ)
		return `(?:` + e + `(?:, ` + e + `)*)?`
	}
	if len(fd.Enum) > 0 && typ == fd.Type {
		alts := make([]string, len(fd.Enum))
		for i, v := range fd.Enum {
			alts[i] = regexp.QuoteMeta(strings.ToUpper(v))
		}
		e := `(?:` + strings.Join(alts, `|`) + `)`
		if typ == "keywords" {
			return `(?:` + e + `(?:, ` + e + `)*)?`
		}
		return e
	}
	if fd.Item != nil && typ == fd.Type {
		var b strings.Builder
		(&Pattern{}).write(fd.Item.Fields, mustParse(fd.Item.Template), &b, false)
		e := b.String()
		if fd.Type == "object" {
			return `(?:` + e + `)`
		}
		return `(?:` + e + `(?:` + regexp.QuoteMeta(fd.Item.Separator()) + e + `)*)`
	}
	if elem := descriptor.SQLListElem(typ); elem != "" {
		e := tokenRE(descriptor.SQLField{}, elem)
		return `(?:` + e + `(?:, ` + e + `)*)?`
	}
	switch typ {
	case "string", "identstring", "namestring", "exactstring":
		return reString
	case "dollarstring":
		return `\$\$.*?\$\$`
	case "identifier":
		return reIdent
	case "name":
		return reName
	case "keyword":
		return `[A-Z][A-Z0-9_]*`
	case "number", "integer":
		return reNumber
	case "boolean":
		return `TRUE|FALSE`
	case "value":
		return reString + `|` + reNumber + `|TRUE|FALSE`
	case "datatype":
		return reType
	case "arg":
		return reIdent + ` ` + reType
	}
	return `.+?` // expression
}

func mustParse(tmpl string) *descriptor.SQLTemplate {
	t, err := descriptor.ParseSQLTemplate(tmpl)
	if err != nil {
		panic(err) // validated with the descriptor
	}
	return t
}

// Pattern is a template compiled to read a statement back: which
// placeholder each group holds.
type Pattern struct {
	re     *regexp.Regexp
	fields []string
	defs   []descriptor.SQLField
	types  []string
	flags  []byte
	lit    int
	// opts: per [optional] part, its group's number and the literal text
	// directly in it, for Used.
	opts []struct{ group, lit int }
	n    int // capturing groups written so far
	// groups: each field's group number.
	groups []int
}

// nestedLit is the literal text in t's own optional parts, at any depth.
func nestedLit(t *descriptor.SQLTemplate) int {
	n := 0
	for _, part := range t.Parts {
		if part.Optional != nil {
			n += litOf(part.Optional)
		}
	}
	return n
}

func litOf(t *descriptor.SQLTemplate) int {
	n := 0
	for _, part := range t.Parts {
		switch {
		case part.Optional != nil:
			n += litOf(part.Optional)
		case part.Field == "":
			n += len(part.Text)
		}
	}
	return n
}

// Literal is how much literal text the template holds: the more, the more
// specific it is.
func (p *Pattern) Literal() int { return p.lit }

// Used is how much of the template's literal text a statement it matches
// holds: its optional parts left out do not count. Of two templates that
// both make a statement, the one using more is the more specific.
func (p *Pattern) Used(stmt string) int {
	m := p.re.FindStringSubmatchIndex(stmt)
	if m == nil {
		return 0
	}
	used := p.lit
	for _, o := range p.opts {
		if m[2*o.group] < 0 {
			used -= o.lit
		}
	}
	return used
}

// Compile turns a template into a Pattern.
func Compile(q *descriptor.SQL, tmpl string) (*Pattern, error) {
	return compile(q.Fields, tmpl, `$`)
}

func compile(fields map[string]descriptor.SQLField, tmpl, end string) (*Pattern, error) {
	t, err := descriptor.ParseSQLTemplate(tmpl)
	if err != nil {
		return nil, err
	}
	p := &Pattern{}
	var b strings.Builder
	p.write(fields, t, &b, true)
	p.re, err = regexp.Compile(`(?s)^` + b.String() + end)
	return p, err
}

// write writes t's regular expression; capture: a group for each
// placeholder (else none: an item's, inside a list's one group).
func (p *Pattern) write(fields map[string]descriptor.SQLField, t *descriptor.SQLTemplate, b *strings.Builder, capture bool) {
	open := `(`
	if !capture {
		open = `(?:`
	}
	for _, part := range t.Parts {
		switch {
		case part.Optional != nil:
			// Left out unless the statement needs it: an expression
			// before it then does not swallow it ([DEFAULT {expression}]
			// before [ COMMENT {comment}]).
			if !capture {
				b.WriteString(`(?:`)
				p.write(fields, part.Optional, b, capture)
				b.WriteString(`)??`)
				continue
			}
			p.n++
			idx := len(p.opts)
			p.opts = append(p.opts, struct{ group, lit int }{group: p.n})
			before := p.lit
			b.WriteString(`(`)
			p.write(fields, part.Optional, b, capture)
			b.WriteString(`)??`)
			p.opts[idx].lit = p.lit - before - nestedLit(part.Optional)
		case part.Field != "":
			typ := ""
			var fd descriptor.SQLField
			switch part.Field {
			case "@id", "@parent":
				typ = "name"
			case "@like":
				typ = "string"
			default:
				fd, _ = descriptor.SQLFieldAt(fields, part.Field)
				typ = fd.Type
			}
			if part.As != "" {
				typ = part.As
			}
			re := tokenRE(fd, typ)
			if part.Flag != 0 {
				re = ``
			}
			if capture {
				p.n++
				p.fields = append(p.fields, part.Field)
				p.groups = append(p.groups, p.n)
				p.defs = append(p.defs, fd)
				p.types = append(p.types, typ)
				p.flags = append(p.flags, part.Flag)
			}
			b.WriteString(open + re + `)`)
		default:
			p.lit += len(part.Text)
			b.WriteString(regexp.QuoteMeta(part.Text))
		}
	}
}

// Match reads a statement back into the values its placeholders were
// filled with (Parse of each), or reports that it is not this template's.
// A flag is true (false for {!field}) where its part is there, and the
// opposite where it is left out.
func (p *Pattern) Match(q *descriptor.SQL, stmt string) (map[string]any, bool) {
	return p.match(stmt)
}

func (p *Pattern) match(stmt string) (map[string]any, bool) {
	m := p.re.FindStringSubmatchIndex(stmt)
	if m == nil {
		return nil, false
	}
	out := map[string]any{}
	for i, f := range p.fields {
		g := p.groups[i]
		if m[2*g] < 0 || p.flags[i] != 0 {
			continue // an optional part left out; a flag, below
		}
		text := stmt[m[2*g]:m[2*g+1]]
		var v any
		var err error
		switch d := p.defs[i]; {
		case d.Projection:
			list := []any{}
			for _, e := range splitTokens(text) {
				var ev any
				if ev, err = Parse(p.types[i], e); err != nil {
					break
				}
				list = append(list, ev)
			}
			v = list
		case d.Item != nil && p.types[i] == d.Type:
			v, err = parseItems(d, text)
		case len(d.Enum) > 0 && p.types[i] == d.Type:
			v, err = parseEnum(d, text)
		default:
			v, err = Parse(p.types[i], text)
		}
		if err != nil {
			return nil, false
		}
		out[f] = v
	}
	for pass := 0; pass < 2; pass++ { // flags there first; then those left out
		for i, f := range p.fields {
			there := m[2*p.groups[i]] >= 0
			if p.flags[i] == 0 || there != (pass == 0) {
				continue
			}
			if _, set := out[f]; !set {
				out[f] = there == (p.flags[i] == '?')
			}
		}
	}
	return out, true
}

// parseEnum reads enum values back: a keywords list is split on ", ".
func parseEnum(fd descriptor.SQLField, s string) (any, error) {
	if fd.Type == "keyword" {
		return s, nil
	}
	out := []any{}
	if s != "" {
		for _, e := range strings.Split(s, ", ") {
			out = append(out, e)
		}
	}
	return out, nil
}

// parseItems reads an object, or a list of them, back element by element.
func parseItems(fd descriptor.SQLField, s string) (any, error) {
	sep := regexp.QuoteMeta(fd.Item.Separator())
	p, err := compile(fd.Item.Fields, fd.Item.Template, `(?:`+sep+`|$)`)
	if err != nil {
		return nil, err
	}
	whole, err := compile(fd.Item.Fields, fd.Item.Template, `$`)
	if err != nil {
		return nil, err
	}
	var out []any
	for rest := s; ; {
		m := p.re.FindStringIndex(rest)
		if m == nil || m[1] == 0 {
			return nil, fmt.Errorf("not an item: %s", rest)
		}
		elem := strings.TrimSuffix(rest[:m[1]], fd.Item.Separator())
		if m[1] == len(rest) {
			elem = rest
		}
		v, ok := whole.match(elem)
		if !ok {
			return nil, fmt.Errorf("not an item: %s", elem)
		}
		out = append(out, v)
		rest = rest[m[1]:]
		if rest == "" {
			break
		}
		if fd.Type == "object" {
			return nil, fmt.Errorf("more than one object: %s", s)
		}
	}
	if fd.Type == "object" {
		return out[0], nil
	}
	return out, nil
}

// Parse reads what Value wrote back into the value it was written from,
// with identifiers as their exact names (a name's parts joined with ., each
// plain or quoted as Snowflake answers it).
func Parse(typ, s string) (any, error) {
	if elem := descriptor.SQLListElem(typ); elem != "" {
		out := []any{}
		for _, e := range splitTokens(s) {
			v, err := Parse(elem, e)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	switch typ {
	case "string":
		return unquoteString(s)
	case "dollarstring":
		if len(s) < 4 || !strings.HasPrefix(s, "$$") || !strings.HasSuffix(s, "$$") {
			return nil, fmt.Errorf("not a $$ string: %s", s)
		}
		return s[2 : len(s)-2], nil
	case "identstring":
		q, err := unquoteString(s)
		if err != nil {
			return nil, err
		}
		return Parse("identifier", q)
	case "namestring":
		q, err := unquoteString(s)
		if err != nil {
			return nil, err
		}
		return Parse("name", q)
	case "exactstring":
		return unquoteString(s)
	case "identifier":
		if len(s) < 2 || s[0] != '"' {
			return nil, fmt.Errorf("not a quoted identifier: %s", s)
		}
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`), nil
	case "name":
		parts := splitName(s)
		for i, part := range parts {
			v, err := Parse("identifier", part)
			if err != nil {
				return nil, err
			}
			parts[i] = Display(v.(string))
		}
		return strings.Join(parts, "."), nil
	case "number", "integer":
		return strconv.ParseFloat(s, 64)
	case "boolean":
		return s == "TRUE", nil
	case "value":
		switch {
		case strings.HasPrefix(s, "'"):
			return unquoteString(s)
		case s == "TRUE" || s == "FALSE":
			return s == "TRUE", nil
		}
		return strconv.ParseFloat(s, 64)
	case "arg":
		end := identEnd(s)
		if end < 0 {
			return nil, fmt.Errorf("not an argument: %s", s)
		}
		name, err := Parse("identifier", s[:end])
		if err != nil {
			return nil, err
		}
		return map[string]any{"name": name, "type": strings.TrimSpace(s[end:])}, nil
	}
	return s, nil // keyword, datatype, expression
}

// Display is an exact name as Snowflake shows it in a SHOW answer's name
// columns: plain when it can be, else in double quotes.
func Display(exact string) string {
	if plainIdent.MatchString(exact) && strings.ToUpper(exact) == exact {
		return exact
	}
	return QuoteExact(exact)
}

func unquoteString(s string) (string, error) {
	if len(s) < 2 || s[0] != '\'' || s[len(s)-1] != '\'' {
		return "", fmt.Errorf("not a string constant: %s", s)
	}
	in := s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] == '\\' && i+1 < len(in):
			i++
			b.WriteByte(in[i])
		case in[i] == '\'' && i+1 < len(in) && in[i+1] == '\'':
			i++
			b.WriteByte('\'')
		default:
			b.WriteByte(in[i])
		}
	}
	return b.String(), nil
}

// identEnd is where a quoted identifier at the start of s ends, or -1.
func identEnd(s string) int {
	if s == "" || s[0] != '"' {
		return -1
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '"' {
			if i+1 < len(s) && s[i+1] == '"' {
				i++
				continue
			}
			return i + 1
		}
	}
	return -1
}

// splitTokens splits a list Value wrote on the ", " outside quotes and
// parentheses.
func splitTokens(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	var quote byte
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && quote == '\'' {
				i++
			} else if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0 && i+1 < len(s) && s[i+1] == ' ':
			out = append(out, s[start:i])
			start = i + 2
			i++
		}
	}
	return append(out, s[start:])
}
