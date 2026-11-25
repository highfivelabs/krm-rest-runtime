// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/sqlapi"
)

// Drift returns the first field (as a path) where desired is not reflected in
// observed, or "" if every desired field is. Only fields the user set are
// compared: the API may add defaults and read-only fields freely. Numbers
// compare by value, whatever their JSON form. skip is asked about every
// object field by its dotted path ("settings.clientSecret"), not only the
// top-level ones.
func Drift(desired, observed any, skip func(string) bool) string {
	return drift("", desired, observed, skip)
}

func drift(at string, d, o any, skip func(string) bool) string {
	switch dv := d.(type) {
	case map[string]any:
		om, ok := o.(map[string]any)
		if !ok {
			return orRoot(at)
		}
		for k, v := range dv {
			p := k
			if at != "" {
				p = at + "." + k
			}
			if skip != nil && skip(p) {
				continue
			}
			if f := drift(p, v, om[k], skip); f != "" {
				return f
			}
		}
		return ""
	case []any:
		ol, ok := o.([]any)
		if !ok || len(ol) != len(dv) {
			return orRoot(at)
		}
		for i := range dv {
			if f := drift(fmt.Sprintf("%s[%d]", at, i), dv[i], ol[i], nil); f != "" {
				return f
			}
		}
		return ""
	case nil:
		return ""
	default:
		if num(d) != nil && num(o) != nil {
			if *num(d) != *num(o) {
				return orRoot(at)
			}
			return ""
		}
		if sameDuration(d, o) || sameInstant(d, o) {
			return ""
		}
		if !reflect.DeepEqual(d, o) {
			return orRoot(at)
		}
		return ""
	}
}

// sameDuration: a duration written as text ("1h", "90s") and read back as
// whole seconds (3600), as Vault does with every TTL.
func sameDuration(d, o any) bool {
	ds, ok := d.(string)
	n := num(o)
	if !ok || n == nil {
		return false
	}
	dur, err := time.ParseDuration(ds)
	return err == nil && dur.Seconds() == *n
}

// sameInstant: two RFC 3339 times that name the same instant, read back in
// another zone than written (PagerDuty answers "2026-12-15T18:00:00-06:00",
// the account's zone, for "2026-12-16T00:00:00Z").
func sameInstant(d, o any) bool {
	ds, ok := d.(string)
	os, ok2 := o.(string)
	if !ok || !ok2 {
		return false
	}
	dt, err := time.Parse(time.RFC3339, ds)
	if err != nil {
		return false
	}
	ot, err := time.Parse(time.RFC3339, os)
	return err == nil && dt.Equal(ot)
}

func orRoot(at string) string {
	if at == "" {
		return "."
	}
	return at
}

func num(v any) *float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int64:
		f = float64(x)
	case int:
		f = float64(x)
	case json.Number:
		y, err := x.Float64()
		if err != nil {
			return nil
		}
		f = y
	default:
		return nil
	}
	return &f
}

// Holds reports whether a list of links (grants, memberships) holds every
// field of desired. Scalars and objects must match within one element;
// a list field may be spread across elements that agree on everything else
// (Snowflake lists one grant per privilege).
func Holds(list []any, desired map[string]any, skip func(string) bool) bool {
	scalar, lists := map[string]any{}, map[string][]any{}
	for k, v := range desired {
		if skip != nil && skip(k) {
			continue
		}
		if l, ok := v.([]any); ok {
			lists[k] = l
		} else {
			scalar[k] = v
		}
	}
	seen := map[string]map[string]bool{}
	found := false
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok || drift("", scalar, foldStrings(scalar, m), nil) != "" {
			continue
		}
		found = true
		for k := range lists {
			if seen[k] == nil {
				seen[k] = map[string]bool{}
			}
			if l, ok := m[k].([]any); ok {
				for _, x := range l {
					seen[k][key(x)] = true
				}
			}
		}
	}
	if !found {
		return false
	}
	for k, l := range lists {
		for _, x := range l {
			if !seen[k][key(x)] {
				return false
			}
		}
	}
	return true
}

// foldStrings: string fields equal ignoring case count as equal (identifiers
// the API uppercases).
func foldStrings(desired, observed map[string]any) map[string]any {
	out := make(map[string]any, len(observed))
	for k, v := range observed {
		out[k] = v
		switch d := desired[k].(type) {
		case string:
			if o, ok := v.(string); ok && strings.EqualFold(d, o) {
				out[k] = d
			}
		case map[string]any:
			if o, ok := v.(map[string]any); ok {
				out[k] = foldStrings(d, o)
			}
		}
	}
	return out
}

func key(v any) string {
	if s, ok := v.(string); ok {
		return strings.ToUpper(s)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// Normalize returns observed with each field the API reads back differently
// but equivalently replaced by its desired value: a case-insensitive field
// (k.CaseInsensitive) that differs only in case, and a field with a Compare
// rule that says the two are the same. observed itself is not changed; it is
// returned as is when nothing applies.
func Normalize(k *descriptor.Kind, desired, observed map[string]any) map[string]any {
	if observed == nil {
		return nil
	}
	if k.SQL != nil {
		return sqlapi.Normalize(k.SQL, desired, observed)
	}
	var out map[string]any
	set := func(f string, d any) {
		if out == nil {
			out = make(map[string]any, len(observed))
			for k, v := range observed {
				out[k] = v
			}
		}
		out[f] = d
	}
	for _, f := range k.CaseInsensitive {
		d, ok1 := desired[f].(string)
		o, ok2 := observed[f].(string)
		if ok1 && ok2 && d != o && strings.EqualFold(d, o) {
			set(f, d)
		}
	}
	for f, rule := range k.Compare {
		if strings.ContainsAny(f, ".[") {
			// Inside objects and lists' elements: the observed value at that
			// path, each one the same under the rule as the desired, takes
			// the desired one's form.
			top, rest, _ := strings.Cut(strings.ReplaceAll(f, "[]", ".[]"), ".")
			if d, ok := desired[top]; ok {
				if o, changed := sameAt(strings.Split(rest, "."), rule, d, observed[top]); changed {
					set(top, o)
				}
			}
			continue
		}
		if d, ok := desired[f]; ok && sameUnder(rule, d, observed[f]) {
			set(f, d)
		}
	}
	if o := k.Order; o != nil && o.Partial {
		// Only the IDs the spec lists are compared, in the order the API has them.
		listed := map[string]bool{}
		want, _ := desired[o.Field].([]any)
		for _, id := range want {
			listed[IDString(id)] = true
		}
		got, _ := observed[o.Field].([]any)
		mine := make([]any, 0, len(want))
		for _, id := range got {
			if listed[IDString(id)] {
				mine = append(mine, id)
			}
		}
		set(o.Field, mine)
	}
	if out == nil {
		return observed
	}
	return out
}

// sameAt is a copy of o with the value at path (field names, "[]" for each
// element of a list, element i of d against element i of o) replaced by
// d's where the two are the same under rule; changed is false when none is.
func sameAt(path []string, rule string, d, o any) (any, bool) {
	if len(path) == 0 || path[0] == "" {
		if sameUnder(rule, d, o) {
			return d, true
		}
		return o, false
	}
	if path[0] == "[]" {
		dl, ok := d.([]any)
		ol, ok2 := o.([]any)
		if !ok || !ok2 {
			return o, false
		}
		var out []any
		for i := range ol {
			if i >= len(dl) {
				break
			}
			if v, ch := sameAt(path[1:], rule, dl[i], ol[i]); ch {
				if out == nil {
					out = append([]any(nil), ol...)
				}
				out[i] = v
			}
		}
		if out == nil {
			return o, false
		}
		return out, true
	}
	dm, ok := d.(map[string]any)
	om, ok2 := o.(map[string]any)
	if !ok || !ok2 {
		return o, false
	}
	dv, has := dm[path[0]]
	if !has {
		return o, false
	}
	v, ch := sameAt(path[1:], rule, dv, om[path[0]])
	if !ch {
		return o, false
	}
	out := make(map[string]any, len(om))
	for f, x := range om {
		out[f] = x
	}
	out[path[0]] = v
	return out, true
}

// sameUnder reports whether desired d and observed o are the same under a
// Compare rule.
func sameUnder(rule string, d, o any) bool {
	switch rule {
	case descriptor.CompareSplitSpace:
		ds, ok := d.(string)
		ol, ok2 := o.([]any)
		if !ok || !ok2 {
			return false
		}
		want := strings.Fields(ds)
		got := make([]string, 0, len(ol))
		for _, x := range ol {
			s, ok := x.(string)
			if !ok {
				return false
			}
			got = append(got, s)
		}
		sort.Strings(want)
		sort.Strings(got)
		return reflect.DeepEqual(want, got)
	case descriptor.CompareEnabledObject:
		om, ok := o.(map[string]any)
		if !ok {
			return false
		}
		e, ok := om["enabled"]
		return ok && drift("", d, e, nil) == ""
	case descriptor.CompareNodeIDs:
		dl, ok := d.([]any)
		om, ok2 := o.(map[string]any)
		nodes, ok3 := om["nodes"].([]any)
		if !ok || !ok2 || !ok3 || len(dl) != len(nodes) {
			return false
		}
		want := make([]string, 0, len(dl))
		for _, x := range dl {
			want = append(want, IDString(x))
		}
		got := make([]string, 0, len(nodes))
		for _, n := range nodes {
			nm, _ := n.(map[string]any)
			got = append(got, IDString(nm["id"]))
		}
		sort.Strings(want)
		sort.Strings(got)
		return reflect.DeepEqual(want, got)
	case descriptor.CompareSlugObject:
		ds, ok := d.(string)
		om, ok2 := o.(map[string]any)
		if !ok || !ok2 || ds == "" {
			return false
		}
		return IDString(om["slug"]) == ds || IDString(om["id"]) == ds
	case descriptor.CompareEndsWith:
		ds, ok := d.(string)
		os, ok2 := o.(string)
		if !ok || !ok2 {
			return false
		}
		want, got := strings.Join(strings.Fields(ds), " "), strings.Join(strings.Fields(os), " ")
		return want != "" && (got == want || strings.HasSuffix(got, " "+want))
	case descriptor.CompareSubsetByID:
		dl, ok := d.([]any)
		ol, ok2 := o.([]any)
		if !ok || !ok2 {
			return false
		}
		byID := make(map[string]any, len(ol))
		for _, oe := range ol {
			if m, ok := oe.(map[string]any); ok && m["id"] != nil {
				byID[IDString(m["id"])] = oe
			}
		}
		for _, de := range dl {
			m, ok := de.(map[string]any)
			if !ok || m["id"] == nil {
				return false
			}
			oe, ok := byID[IDString(m["id"])]
			if !ok || drift("", de, oe, nil) != "" {
				return false
			}
		}
		return true
	case descriptor.CompareUnordered:
		dl, ok := d.([]any)
		ol, ok2 := o.([]any)
		if !ok || !ok2 || len(dl) != len(ol) {
			return false
		}
		used := make([]bool, len(ol))
	next:
		for _, de := range dl {
			for j, oe := range ol {
				if !used[j] && drift("", de, oe, nil) == "" {
					used[j] = true
					continue next
				}
			}
			return false
		}
		return true
	}
	return false
}
