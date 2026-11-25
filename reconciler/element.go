// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// ReadHolder reads the resource an Element kind's list is in, as the API
// answers it (not unwrapped: a list may be the whole answer).
func ReadHolder(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string) (any, error) {
	path, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return nil, err
	}
	return api.DoWith(ctx, k.Read.Method, rest.WithQuery(path, k.Read.QueryOf(params)), nil, k.Read.HeadersOf(params))
}

// elementList is the list at path (slash path) inside the kind's envelope
// of resp; empty when there is none.
func elementList(k *descriptor.Kind, resp any, path string) []any {
	l, _ := descriptor.Dig(descriptor.Dig(resp, k.Envelope), path).([]any)
	return l
}

// elementObs is the element at list[i] as the object's status holds it: a
// bare value as {field: value}, an object as it is.
func elementObs(e *descriptor.Element, field string, v any) map[string]any {
	if len(e.In) == 0 {
		m, _ := v.(map[string]any)
		return m
	}
	return map[string]any{field: v}
}

// ReadElement reads an Element kind's element, the one body names, as the
// object's status holds it; rest.ErrNotFound when the resource holding it,
// or the element, is not there.
func ReadElement(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string, body map[string]any) (map[string]any, error) {
	e := k.Element
	field, list, value, err := e.Of(body)
	if err != nil {
		return nil, err
	}
	resp, err := ReadHolder(ctx, api, k, params)
	if err != nil {
		return nil, err
	}
	l := elementList(k, resp, list)
	i := e.Find(l, value)
	if i < 0 {
		return nil, rest.ErrNotFound
	}
	return elementObs(e, field, l[i]), nil
}

// syncElement makes sure the element the spec names is in its list, as the
// spec has it; an element last applied under another name (prev, the
// status) is taken out in the same write.
func (r *Reconciler) syncElement(ctx context.Context, api *rest.Client, params map[string]string, body, prev map[string]any) (map[string]any, string, error) {
	k := r.Kind
	e := k.Element
	field, list, value, err := e.Of(body)
	if err != nil {
		return nil, "", err
	}
	resp, err := ReadHolder(ctx, api, k, params)
	if errors.Is(err, rest.ErrNotFound) {
		return nil, "", fmt.Errorf("read: the resource at %s that holds the element is not there: %w", k.Read.Path, err)
	}
	if err != nil {
		return nil, "", fmt.Errorf("read: %w", err)
	}
	cur := elementList(k, resp, list)
	i := e.Find(cur, value)
	// The element applied before, if the spec now names another one.
	oldList, oldValue, oldIdx := "", any(nil), -1
	if prev != nil {
		if _, pl, pv, perr := e.Of(prev); perr == nil && (pl != list || descriptor.Text(pv) != descriptor.Text(value)) {
			oldList, oldValue = pl, pv
			oldIdx = e.Find(elementList(k, resp, pl), pv)
		}
	}
	if i >= 0 && oldIdx < 0 {
		obs := elementObs(e, field, cur[i])
		if len(e.In) > 0 || Drift(body, r.normalize(body, obs), r.skip(obs)) == "" {
			return obs, "unchanged", nil
		}
	}
	action := "created"
	if i >= 0 || prev != nil {
		action = "updated"
	}
	holder := UnwrapResponse(k, resp)
	if e.Patch != "" {
		if oldIdx >= 0 {
			if err := r.writeElement(ctx, api, params, map[string]any{e.Patch: []any{unsetElement(e, oldValue)}}, holder); err != nil {
				return nil, "", fmt.Errorf("removing the element applied before: %w", err)
			}
		}
		if err := r.writeElement(ctx, api, params, map[string]any{e.Patch: []any{body}}, holder); err != nil {
			return nil, "", err
		}
		return body, action, nil
	}
	lists := map[string][]any{}
	for _, l := range append(e.Lists(), list) {
		lists[l] = append([]any{}, elementList(k, resp, l)...)
	}
	if oldIdx >= 0 {
		lists[oldList] = removeAt(lists[oldList], e.Find(lists[oldList], oldValue))
	}
	add := any(value)
	if len(e.In) == 0 {
		add = body
	}
	if j := e.Find(lists[list], value); j >= 0 {
		lists[list][j] = add
	} else {
		lists[list] = append(lists[list], add)
	}
	if err := r.writeElement(ctx, api, params, wholeLists(lists), holder); err != nil {
		return nil, "", err
	}
	return elementObs(e, field, add), action, nil
}

// deleteElement takes the element out of its list: the one last applied
// (prev), else the one the spec names. A resource or element that is not
// there is done.
func (r *Reconciler) deleteElement(ctx context.Context, api *rest.Client, params map[string]string, prev map[string]any, spec func() (map[string]any, error)) error {
	k := r.Kind
	e := k.Element
	named := prev
	if named == nil {
		body, err := spec()
		if err != nil {
			return err
		}
		named = body
	}
	_, list, value, err := e.Of(named)
	if err != nil {
		return nil //nolint:nilerr // an element that cannot be named was never applied, so there is nothing to delete
	}
	resp, err := ReadHolder(ctx, api, k, params)
	if errors.Is(err, rest.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read before delete: %w", err)
	}
	if e.Find(elementList(k, resp, list), value) < 0 {
		return nil
	}
	holder := UnwrapResponse(k, resp)
	if e.Patch != "" {
		return r.writeElement(ctx, api, params, map[string]any{e.Patch: []any{unsetElement(e, value)}}, holder)
	}
	lists := map[string][]any{}
	for _, l := range append(e.Lists(), list) {
		lists[l] = append([]any{}, elementList(k, resp, l)...)
	}
	lists[list] = removeAt(lists[list], e.Find(lists[list], value))
	return r.writeElement(ctx, api, params, wholeLists(lists), holder)
}

// writeElement sends an Element kind's write (its Update call) with send as
// the body; cur is the holding resource as just read.
func (r *Reconciler) writeElement(ctx context.Context, api *rest.Client, params map[string]string, send, cur map[string]any) error {
	op := r.Kind.Update
	path, err := rest.Expand(op.Path, params)
	if err != nil {
		return err
	}
	if r.Kind.Element.Whole {
		send = mergeMaps(cur, send) // the resource as read, its lists changed
	}
	if _, err := api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), Request(r.Kind, op, OpBody(op, send, params, cur)), op.HeadersOf(params)); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// ElementWrite is the body of an Element kind's write that takes the
// element body names out of resp (the holding resource as read), or puts
// it in when add: what a person editing the resource elsewhere would send.
func ElementWrite(k *descriptor.Kind, resp any, body map[string]any, add bool) (map[string]any, error) {
	e := k.Element
	_, list, value, err := e.Of(body)
	if err != nil {
		return nil, err
	}
	if e.Patch != "" {
		if add {
			return map[string]any{e.Patch: []any{body}}, nil
		}
		return map[string]any{e.Patch: []any{unsetElement(e, value)}}, nil
	}
	lists := map[string][]any{}
	for _, l := range append(e.Lists(), list) {
		lists[l] = append([]any{}, elementList(k, resp, l)...)
	}
	lists[list] = removeAt(lists[list], e.Find(lists[list], value))
	if add {
		if len(e.In) == 0 {
			lists[list] = append(lists[list], body)
		} else {
			lists[list] = append(lists[list], value)
		}
	}
	if e.Whole {
		return mergeMaps(UnwrapResponse(k, resp), wholeLists(lists)), nil
	}
	return wholeLists(lists), nil
}

// unsetElement is the element named value as a patch removes it: its key
// and the Unset fields.
func unsetElement(e *descriptor.Element, value any) map[string]any {
	out := map[string]any{e.Key: value}
	for f, v := range e.Unset {
		out[f] = v
	}
	return out
}

// wholeLists is the body of a whole write: each list at its (slash) path.
func wholeLists(lists map[string][]any) map[string]any {
	out := map[string]any{}
	for l, v := range lists {
		if v == nil {
			v = []any{}
		}
		out = withDotted(out, strings.Split(l, "/"), v)
	}
	return out
}

func removeAt(l []any, i int) []any {
	if i < 0 {
		return l
	}
	return append(append([]any{}, l[:i]...), l[i+1:]...)
}

// syncState puts the resource in the state the spec asks for (want: the
// State field's value; nil asks for none), by its call, after the rest of
// the spec is applied; obs is the resource as the API answered it then.
func (r *Reconciler) syncState(ctx context.Context, api *rest.Client, params map[string]string, want any, body, obs map[string]any, action string) (map[string]any, string, error) {
	k := r.Kind
	if want == nil || descriptor.Text(want) == "" {
		return obs, action, nil
	}
	if cur := obs[k.State.Field]; cur != nil && strings.EqualFold(descriptor.Text(cur), descriptor.Text(want)) {
		return obs, action, nil
	}
	if err := r.callState(ctx, api, params, descriptor.Text(want), body); err != nil {
		return nil, "", err
	}
	readPath, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return nil, "", err
	}
	resp, err := r.read(ctx, api, readPath, params)
	if err != nil {
		return nil, "", fmt.Errorf("read after setting %s: %w", k.State.Field, err)
	}
	if action == "unchanged" {
		action = "updated"
	}
	return r.unwrap(resp), action, nil
}

// callState makes the call that puts the resource in state value, after the
// calls made before it (State.Before); a call with sendBody sends body, the
// spec's other fields ({} when it has none).
func (r *Reconciler) callState(ctx context.Context, api *rest.Client, params map[string]string, value string, body map[string]any) error {
	k := r.Kind
	op, v := k.State.Call(value)
	if op == nil {
		return fmt.Errorf("spec.%s: no call sets %q (one of %s)", k.State.Field, value, strings.Join(k.State.Values(), ", "))
	}
	for i := range k.State.Before[v] {
		if err := r.stateCall(ctx, api, params, &k.State.Before[v][i], body); err != nil {
			return fmt.Errorf("before setting %s to %s: %w", k.State.Field, value, err)
		}
	}
	if err := r.stateCall(ctx, api, params, op, body); err != nil {
		return fmt.Errorf("setting %s to %s: %w", k.State.Field, value, err)
	}
	return nil
}

func (r *Reconciler) stateCall(ctx context.Context, api *rest.Client, params map[string]string, op *descriptor.Op, body map[string]any) error {
	path, err := rest.Expand(op.Path, params)
	if err != nil {
		return err
	}
	var send map[string]any
	if op.SendBody {
		send = map[string]any{}
		for f, x := range body {
			send[f] = x
		}
	}
	_, err = api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), Request(r.Kind, op, OpBody(op, send, params, nil)), op.HeadersOf(params))
	return err
}

// deleteState puts the resource in the state deleting the object leaves it
// in (State.Delete), unless it is in it already or gone.
func (r *Reconciler) deleteState(ctx context.Context, api *rest.Client, params map[string]string) error {
	k := r.Kind
	if k.Shape == descriptor.CRUD && params[k.ID.Param] == "" {
		return nil // never made or found
	}
	readPath, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return err
	}
	resp, err := r.read(ctx, api, readPath, params)
	if errors.Is(err, rest.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read before delete: %w", err)
	}
	if cur := r.unwrap(resp)[k.State.Field]; cur != nil && strings.EqualFold(descriptor.Text(cur), k.State.Delete) {
		return nil
	}
	return r.callState(ctx, api, params, k.State.Delete, map[string]any{})
}
