// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"fmt"
	"sort"
	"strings"
)

// Order makes a settings kind the order of other objects: the spec's Field
// lists their IDs in order, the read is a list of them, and the update is a
// reorder call that takes the IDs in the order wanted (Datadog's APM
// retention filters: GET .../retention-filters lists them,
// PUT .../retention-filters-execution-order takes
// {"data": [{"id": ..., "type": "apm_retention_filter"}, ...]}). Nothing is
// deleted: deleting the object leaves the order as it is.
type Order struct {
	// Field is the spec field holding the IDs, in order.
	Field string `json:"field"`
	// List is where the read's array is, as a slash path ("" for an answer
	// that is the array).
	List string `json:"list,omitempty"`
	// ID is the element field (dotted for a nested one) that holds an
	// element's ID; default "id".
	ID string `json:"id,omitempty"`
	// SortBy is an element field (dotted) the API orders the elements by,
	// for a list answered in another order (Datadog's tag indexing rules,
	// by attributes.rule_order); "" keeps the list's own order.
	SortBy string `json:"sortBy,omitempty"`
	// At is where the IDs go in the update's body, as a slash path.
	At string `json:"at"`
	// Item, if set, is what each ID is sent as: this object with "id" set
	// to the ID ({"type": "apm_retention_filter"}); nil sends bare IDs
	// (Datadog's tag indexing rules: attributes.rule_ids).
	Item map[string]any `json:"item,omitempty"`
	// Partial: the spec lists some of the objects, which go first, in its
	// order; those it does not list keep their order after them (Datadog's
	// custom allocation rules and tag pipeline rulesets, whose provider
	// keeps rules made in the UI at the end). Only the listed IDs are
	// compared. Otherwise the spec lists every object, as the API wants.
	Partial bool `json:"partial,omitempty"`
}

func (k *Kind) validateOrder() error {
	o := k.Order
	if k.Shape != Settings || k.Update == nil || k.Create != nil || k.Delete != nil {
		return fmt.Errorf("order is for a settings kind with an update and no create or delete")
	}
	if o.Field == "" || o.At == "" {
		return fmt.Errorf("order needs field and at")
	}
	if k.Envelope != "" || k.BodyEnvelope != "" || k.BodyArray || k.BodyField != "" || k.Multipart != nil || len(k.Body) > 0 {
		return fmt.Errorf("order: the reorder call's body is made from order.at and order.item; no envelope, bodyEnvelope, bodyArray, bodyField, multipart or body")
	}
	if k.Read.Find != nil || k.Read.Raw != "" || k.Read.Envelope != "" {
		return fmt.Errorf("order: the read is a list at order.list; no find, raw or envelope")
	}
	for _, p := range k.Params {
		if p.Field == o.Field {
			return fmt.Errorf("order.field %s fills a path parameter", o.Field)
		}
	}
	return nil
}

func (o *Order) idPath() string {
	if o.ID == "" {
		return "id"
	}
	return strings.ReplaceAll(o.ID, ".", "/")
}

// IDs are the IDs a read lists, in the order the API has them (by SortBy
// where it says so); an answer with no list there lists none.
func (o *Order) IDs(resp any) []any {
	list, _ := Dig(resp, o.List).([]any)
	elems := make([]any, 0, len(list))
	for _, e := range list {
		if Dig(e, o.idPath()) != nil {
			elems = append(elems, e)
		}
	}
	if o.SortBy != "" {
		at := strings.ReplaceAll(o.SortBy, ".", "/")
		sort.SliceStable(elems, func(i, j int) bool { return orderKey(Dig(elems[i], at)) < orderKey(Dig(elems[j], at)) })
	}
	ids := make([]any, 0, len(elems))
	for _, e := range elems {
		ids = append(ids, Text(Dig(e, o.idPath())))
	}
	return ids
}

func orderKey(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 1 << 62 // no position: last
}

// Observed is the object a read answers: its IDs at Field.
func (o *Order) Observed(resp any) map[string]any {
	return map[string]any{o.Field: o.IDs(resp)}
}

// Wanted is the IDs the update sends: the spec's, and for a Partial order
// after them those the API has (cur, its last answer) that the spec does
// not list, in their order.
func (o *Order) Wanted(spec []any, cur any) []any {
	out := append([]any{}, spec...)
	if !o.Partial || cur == nil {
		return out
	}
	listed := map[string]bool{}
	for _, id := range spec {
		listed[Text(id)] = true
	}
	for _, id := range o.IDs(cur) {
		if !listed[Text(id)] {
			out = append(out, id)
		}
	}
	return out
}

// Request is the reorder call's body: body's other fields (the call's own
// fixed body, merged in already) with the IDs of body's Field at At, each as
// Item with its "id".
func (o *Order) Request(body map[string]any) map[string]any {
	ids, _ := body[o.Field].([]any)
	items := make([]any, 0, len(ids))
	for _, id := range ids {
		if o.Item == nil {
			items = append(items, id)
			continue
		}
		it := make(map[string]any, len(o.Item)+1)
		for f, v := range o.Item {
			it[f] = v
		}
		it["id"] = id
		items = append(items, it)
	}
	out := map[string]any{}
	for f, v := range body {
		if f != o.Field {
			out[f] = v
		}
	}
	return MergeAt(out, o.At, items)
}

// Sent is the IDs a reorder call's body carries at At (what Request made).
func (o *Order) Sent(body any) []any {
	l, _ := Dig(body, o.At).([]any)
	ids := make([]any, 0, len(l))
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			ids = append(ids, m["id"])
		} else {
			ids = append(ids, e)
		}
	}
	return ids
}

// Answer is a stored order as the API's read answers it: obj's other
// fields (the API's own), with the IDs of its Field as list elements at
// List, each element its ID (and Item's fields) and, where the API orders
// by a field, its position there. reversed lists them last first, as an API
// that orders by SortBy may.
func (o *Order) Answer(obj map[string]any, reversed bool) any {
	ids, _ := obj[o.Field].([]any)
	elems := make([]any, len(ids))
	for i, id := range ids {
		e := map[string]any{}
		for f, v := range o.Item {
			e[f] = v
		}
		e = MergeAt(e, o.idPath(), id)
		if o.SortBy != "" {
			e = MergeAt(e, strings.ReplaceAll(o.SortBy, ".", "/"), float64(i+1))
		}
		at := i
		if reversed {
			at = len(ids) - 1 - i
		}
		elems[at] = e
	}
	if o.List == "" {
		return elems
	}
	out := map[string]any{}
	for f, v := range obj {
		if f != o.Field {
			out[f] = v
		}
	}
	return MergeAt(out, o.List, elems)
}

// MergeAt is m with v at a slash path inside it, the objects on the way
// copied rather than changed (m's own and those it shares).
func MergeAt(m map[string]any, path string, v any) map[string]any {
	parts := strings.Split(path, "/")
	out := make(map[string]any, len(m)+1)
	for f, x := range m {
		out[f] = x
	}
	if len(parts) == 1 {
		out[parts[0]] = v
		return out
	}
	sub, _ := out[parts[0]].(map[string]any)
	out[parts[0]] = MergeAt(sub, strings.Join(parts[1:], "/"), v)
	return out
}
