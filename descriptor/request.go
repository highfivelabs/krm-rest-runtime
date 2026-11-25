// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"fmt"
	"strings"
)

// validateRequest checks a call's Request (Op.Request): a REST create,
// update or delete of a crud, upsert or settings kind, which sends nothing
// else as its body.
func (k *Kind) validateRequest(op *Op) error {
	if op.Request == nil {
		return nil
	}
	where := op.Method + " " + op.Path
	if op != k.Create && op != k.Update && op != k.Delete {
		return fmt.Errorf("%s: request is for a create, an update or a delete", where)
	}
	if k.Shape != CRUD && k.Shape != Upsert && k.Shape != Settings || op.GraphQL != nil || k.Element != nil || k.Order != nil {
		return fmt.Errorf("%s: request is for a REST call of a crud, upsert or settings kind", where)
	}
	if op.Body != nil || op.SendBody || op.BodyArray || op.BareBody || op.Carry != nil || k.Multipart != nil {
		return fmt.Errorf("%s: request is the whole body: not with body, sendBody, bodyArray, bareBody, carry or multipart", where)
	}
	for _, n := range RequestFields(op.Request) {
		if strings.HasPrefix(n, "current.") && op == k.Create {
			return fmt.Errorf("%s: request: a create has no current resource ({%s})", where, n)
		}
	}
	return nil
}

// RequestFields are the names a request template's placeholders give:
// spec fields (dotted), path parameters, the ID and current.<field>.
func RequestFields(tmpl any) []string {
	var out []string
	switch t := tmpl.(type) {
	case map[string]any:
		for _, v := range t {
			out = append(out, RequestFields(v)...)
		}
	case []any:
		for _, v := range t {
			out = append(out, RequestFields(v)...)
		}
	case string:
		for _, m := range templateRE.FindAllString(t, -1) {
			out = append(out, m[1:len(m)-1])
		}
	}
	return out
}

// FillRequest is a request template (Op.Request) filled from vals: a
// string that is one {name} takes the value at that name as it is (dotted
// for a nested one), text around templates makes a string; a member or an
// element whose template vals cannot fill is left out.
func FillRequest(tmpl any, vals map[string]any) (any, bool) {
	switch t := tmpl.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for f, v := range t {
			if got, ok := FillRequest(v, vals); ok {
				out[f] = got
			}
		}
		return out, true
	case []any:
		out := make([]any, 0, len(t))
		for _, v := range t {
			if got, ok := FillRequest(v, vals); ok {
				out = append(out, got)
			}
		}
		return out, true
	case string:
		if !IsTemplate(t) {
			return t, true
		}
		if m := templateRE.FindStringSubmatchIndex(t); m != nil && m[0] == 0 && m[1] == len(t) {
			got := Dig(vals, strings.ReplaceAll(t[1:len(t)-1], ".", "/"))
			return got, got != nil
		}
		return (&ID{From: t}).Compose(vals)
	}
	return tmpl, true
}

// RequestValues is what a body sent by a request template (Op.Request)
// says of the names its placeholders give, as FillRequest would have
// filled them: each whole {name}'s value where the template has it, a
// string's parts where text is around them (dotted names nested). An API
// that keeps what it is sent (the fake) reads the call's spec fields back
// so.
func RequestValues(tmpl, sent any) map[string]any {
	out := map[string]any{}
	var walk func(t, v any)
	walk = func(t, v any) {
		switch tt := t.(type) {
		case map[string]any:
			m, _ := v.(map[string]any)
			for f, sub := range tt {
				if got, ok := m[f]; ok {
					walk(sub, got)
				}
			}
		case []any:
			l, _ := v.([]any)
			for i, sub := range tt {
				if i < len(l) {
					walk(sub, l[i])
				}
			}
		case string:
			if !IsTemplate(tt) {
				return
			}
			if m := templateRE.FindStringSubmatchIndex(tt); m != nil && m[0] == 0 && m[1] == len(tt) {
				setDotted(out, tt[1:len(tt)-1], v)
				return
			}
			if s, ok := v.(string); ok {
				if parts, ok := MatchTemplate(tt, s); ok {
					for n, p := range parts {
						setDotted(out, n, p)
					}
				}
			}
		}
	}
	walk(tmpl, sent)
	return out
}

// setDotted sets m's field at a dotted name, making the objects on the way.
func setDotted(m map[string]any, name string, v any) {
	parts := strings.Split(name, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}
