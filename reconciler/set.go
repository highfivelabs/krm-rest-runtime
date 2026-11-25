// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"sort"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// A kind of shape set (descriptor.SetShape) holds the whole set of a
// parent's elements: a repository's labels, an app installation's
// repositories. Its spec lists them all; syncSet adds what the API lacks,
// changes what differs and removes what the spec leaves out, one call per
// element, as Terraform's github_issue_labels does. It is authoritative: an
// element made outside Kubernetes, and not in the spec, is removed.

// SetElements are the spec's elements of a set kind, each as an object: a
// bare value (a repository ID) is {key: value}. It fails on two elements
// with one key.
func SetElements(k *descriptor.Kind, body map[string]any) ([]map[string]any, error) {
	st := k.Set
	list, _ := body[st.Field].([]any)
	out := make([]map[string]any, 0, len(list))
	seen := map[string]bool{}
	for i, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			if len(st.Keys) > 0 {
				return nil, fmt.Errorf("spec.%s[%d]: an object with %s", st.Field, i, st.KeyNames())
			}
			m = map[string]any{st.Key: e}
		}
		key, ok := st.KeyOf(m)
		if !ok {
			return nil, fmt.Errorf("spec.%s[%d]: no %s", st.Field, i, st.KeyNames())
		}
		if seen[key] {
			return nil, fmt.Errorf("spec.%s: %s %q twice", st.Field, st.KeyNames(), key)
		}
		seen[key] = true
		out = append(out, m)
	}
	return out, nil
}

// scalarSet: the spec lists bare values, so an element has nothing to send
// but its key (in the path) and nothing to change.
func scalarSet(k *descriptor.Kind, body map[string]any) bool {
	list, _ := body[k.Set.Field].([]any)
	for _, e := range list {
		if _, ok := e.(map[string]any); ok {
			return false
		}
	}
	return true
}

// ListSet reads every element the API holds, page by page (Link
// rel="next"), keyed by the set's key (Set.KeyOf).
func ListSet(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string) (map[string]map[string]any, error) {
	out, _, err := listSet(ctx, api, k, params)
	return out, err
}

// listSet is ListSet, and the first page's answer as it is (where a batch
// list is, Set.Batch).
func listSet(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string) (map[string]map[string]any, any, error) {
	path, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return nil, nil, err
	}
	out := map[string]map[string]any{}
	var first any
	url := rest.WithQuery(path, k.Read.QueryOf(params))
	for page := 0; url != "" && page < MaxPages; page++ {
		resp, next, err := api.DoPaged(ctx, k.Read.Method, url, nil, k.Read.HeadersOf(params))
		if err != nil {
			return nil, first, err
		}
		if page == 0 {
			first = resp
		}
		list, _ := descriptor.Dig(resp, k.Set.List).([]any)
		for _, e := range list {
			if m, ok := e.(map[string]any); ok && k.Set.KeyURL != "" {
				// Named by a URL only: its last segment is the key.
				href := strings.TrimRight(descriptor.Text(descriptor.Dig(m, k.Set.KeyURL)), "/")
				if i := strings.LastIndexByte(href, '/'); i >= 0 && i < len(href)-1 {
					key, err := neturl.PathUnescape(href[i+1:])
					if err != nil {
						key = href[i+1:]
					}
					el := make(map[string]any, len(m)+1)
					for f, v := range m {
						el[f] = v
					}
					el[k.Set.Key] = key
					out[key] = el
				}
				continue
			}
			if m, ok := e.(map[string]any); ok && len(k.Set.Keys) > 0 {
				if key, ok := k.Set.KeyOf(m); ok {
					out[key] = m
				}
				continue
			}
			if m, ok := e.(map[string]any); ok && m[k.Set.Key] != nil {
				out[descriptor.Text(m[k.Set.Key])] = m
				continue
			}
			if e == nil {
				continue
			}
			// A key alone (Vault's LIST): read the element where it can be.
			key := descriptor.Text(e)
			m := map[string]any{}
			if op := k.Set.ReadEach; op != nil {
				got, err := readElement(ctx, api, k, op, params, key)
				if errors.Is(err, rest.ErrNotFound) {
					continue
				}
				if err != nil {
					return nil, first, err
				}
				m = got
			}
			m[k.Set.Key] = key
			out[key] = m
		}
		url = next
	}
	return out, first, nil
}

// readElement reads one element by its key (Set.ReadEach).
func readElement(ctx context.Context, api *rest.Client, k *descriptor.Kind, op *descriptor.Op, params map[string]string, key string) (map[string]any, error) {
	vals := make(map[string]string, len(params)+1)
	for p, v := range params {
		vals[p] = v
	}
	vals[k.Set.Param] = key
	path, err := rest.Expand(op.Path, vals)
	if err != nil {
		return nil, err
	}
	resp, err := api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(vals)), nil, op.HeadersOf(vals))
	if err != nil {
		return nil, err
	}
	m, _ := descriptor.Dig(resp, op.Envelope).(map[string]any)
	out := make(map[string]any, len(m)+1)
	for f, v := range m {
		out[f] = v
	}
	return out, nil
}

// ensureHolder makes the set's holder (Set.Holder) if a read finds none,
// from the spec's holder fields; it reports whether it made it.
func ensureHolder(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string, body map[string]any) (bool, error) {
	h := k.Set.Holder
	path, err := rest.Expand(h.Read.Path, params)
	if err != nil {
		return false, err
	}
	_, err = api.DoWith(ctx, h.Read.Method, rest.WithQuery(path, h.Read.QueryOf(params)), nil, h.Read.HeadersOf(params))
	if err == nil || !errors.Is(err, rest.ErrNotFound) {
		return false, err
	}
	send := map[string]any{}
	for _, f := range h.Fields {
		if v, ok := body[f]; ok {
			send[f] = v
		}
	}
	if path, err = rest.Expand(h.Create.Path, params); err != nil {
		return false, err
	}
	if _, err := api.DoWith(ctx, h.Create.Method, rest.WithQuery(path, h.Create.QueryOf(params)), OpBody(&h.Create, send, params, nil), h.Create.HeadersOf(params)); err != nil {
		return false, fmt.Errorf("making the holder: %w", err)
	}
	return true, nil
}

// DeleteHolder deletes a set kind's holder, and with it its elements; a
// 404 is done.
func DeleteHolder(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string) error {
	h := k.Set.Holder
	path, err := rest.Expand(h.Delete.Path, params)
	if err != nil {
		return err
	}
	if _, err := api.DoWith(ctx, h.Delete.Method, rest.WithQuery(path, h.Delete.QueryOf(params)), nil, h.Delete.HeadersOf(params)); err != nil && !errors.Is(err, rest.ErrNotFound) {
		return err
	}
	return nil
}

// setCall makes one element call: op on the element's path (its key in
// Set.Param, other placeholders from its fields), sending the element
// unless the set lists bare values.
func setCall(ctx context.Context, api *rest.Client, k *descriptor.Kind, op *descriptor.Op, params map[string]string, elem map[string]any, scalar bool) error {
	vals := make(map[string]string, len(params)+1)
	for p, v := range params {
		vals[p] = v
	}
	if k.Set.Param != "" {
		vals[k.Set.Param] = descriptor.Text(elem[k.Set.Key])
	}
	for _, f := range k.Set.Keys {
		// Named by its keys: they fill the path's and the query's {field}s.
		vals[f] = descriptor.Text(elem[f])
	}
	for _, name := range descriptor.Placeholders(op.Path) {
		if _, ok := vals[name]; !ok {
			if v, ok := elem[name]; ok && v != nil {
				vals[name] = descriptor.Text(v)
			}
		}
	}
	path, err := rest.Expand(op.Path, vals)
	if err != nil {
		return err
	}
	var body any
	if !scalar && op != k.Delete {
		body = Request(k, op, WithOpBody(op, elem))
	}
	_, err = api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(vals)), body, op.HeadersOf(vals))
	return err
}

// syncSet makes the API's elements the spec's: missing ones added, ones
// that differ changed (where the kind has an update), and the rest
// removed. It returns the elements as the spec has them. prev is what the
// object last applied (status.atProvider): a batch list's values are
// compared with it (Set.Batch).
func (r *Reconciler) syncSet(ctx context.Context, api *rest.Client, params map[string]string, body, prev map[string]any, first bool) (map[string]any, string, error) {
	k := r.Kind
	want, err := SetElements(k, body)
	if err != nil {
		return nil, "", err
	}
	scalar := scalarSet(k, body)
	if k.Set.Holder != nil {
		made, err := ensureHolder(ctx, api, k, params, body)
		if err != nil {
			return nil, "", err
		}
		first = first || made
	}
	have, answer, err := listSet(ctx, api, k, params)
	if err != nil && !errors.Is(err, rest.ErrNotFound) {
		return nil, "", fmt.Errorf("read: %w", err)
	}
	changed := false
	for _, w := range want {
		key, _ := k.Set.KeyOf(w)
		h, ok := have[key]
		delete(have, key)
		switch {
		case !ok:
			if err := setCall(ctx, api, k, k.Create, params, w, scalar); err != nil {
				return nil, "", fmt.Errorf("adding %s %q: %w", k.Set.KeyNames(), key, err)
			}
		case !scalar && k.Update != nil && Drift(w, r.normalize(w, h), r.skip(h)) != "":
			if err := setCall(ctx, api, k, k.Update, params, w, scalar); err != nil {
				return nil, "", fmt.Errorf("changing %s %q: %w", k.Set.KeyNames(), key, err)
			}
		case !scalar && k.Update == nil:
			// No call changes an element (Atlas's managed namespaces, which
			// Terraform refuses to change in place): a difference is refused.
			if f := Drift(w, r.normalize(w, h), r.skip(h)); f != "" {
				return nil, "", fmt.Errorf("spec.%s: %s %q differs in %s, and an element cannot be changed in place: remove it from the list, then add it back", k.Set.Field, k.Set.KeyNames(), key, f)
			}
			continue
		default:
			continue
		}
		changed = true
	}
	// Removed after the rest are added: an API that keeps at least one
	// element (an app installation's last repository) is never asked to
	// go through none.
	extra := make([]string, 0, len(have))
	for key := range have {
		extra = append(extra, key)
	}
	sort.Strings(extra)
	for _, key := range extra {
		if err := setCall(ctx, api, k, k.Delete, params, have[key], true); err != nil && !errors.Is(err, rest.ErrNotFound) && !allowed(k.Delete, err) {
			return nil, "", fmt.Errorf("removing %s %q: %w", k.Set.KeyNames(), key, err)
		}
		changed = true
	}
	obs := map[string]any{k.Set.Field: body[k.Set.Field]}
	if h := k.Set.Holder; h != nil {
		for _, f := range h.Fields {
			if v, ok := body[f]; ok {
				obs[f] = v
			}
		}
	}
	if b := k.Set.Batch; b != nil {
		did, err := syncBatch(ctx, api, k, params, body, prev, answer, obs)
		if err != nil {
			return nil, "", err
		}
		changed = changed || did
	}
	switch {
	case first:
		return obs, "created", nil
	case changed:
		return obs, "updated", nil
	}
	return obs, "unchanged", nil
}

// batchElements are a batch list's elements in body, by key, in order;
// present is false where body leaves the field out (the list is then left
// as it is).
func batchElements(b *descriptor.Batch, body map[string]any) (keys []string, byKey map[string]map[string]any, present bool, err error) {
	raw, present := body[b.Field]
	if !present || raw == nil {
		return nil, nil, false, nil
	}
	list, _ := raw.([]any)
	byKey = map[string]map[string]any{}
	for i, e := range list {
		m, _ := e.(map[string]any)
		if m == nil || m[b.Key] == nil || descriptor.Text(m[b.Key]) == "" {
			return nil, nil, true, fmt.Errorf("spec.%s[%d]: no %s", b.Field, i, b.Key)
		}
		key := descriptor.Text(m[b.Key])
		if _, dup := byKey[key]; dup {
			return nil, nil, true, fmt.Errorf("spec.%s: %s %q twice", b.Field, b.Key, key)
		}
		byKey[key] = m
		keys = append(keys, key)
	}
	return keys, byKey, true, nil
}

// syncBatch makes a batch list (Set.Batch) the spec's: the elements the
// read's answer lacks are added in one call; one it holds that the spec
// leaves out, or whose value the spec changed since it was last applied,
// can go only with all the others, so that is refused unless the spec
// lists none, which clears them. It reports whether it wrote, and puts the
// spec's list, and the API's at At, in obs.
func syncBatch(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string, body, prev map[string]any, answer any, obs map[string]any) (bool, error) {
	b := k.Set.Batch
	keys, want, present, err := batchElements(b, body)
	if err != nil || !present {
		return false, err
	}
	obs[b.Field] = body[b.Field]
	held, _ := descriptor.Dig(answer, b.At).(map[string]any)
	_, last, _, _ := batchElements(b, prev)
	var gone, moved, missing []string
	for key := range held {
		if _, ok := want[key]; !ok {
			gone = append(gone, key)
		}
	}
	for _, key := range keys {
		if _, ok := held[key]; !ok {
			missing = append(missing, key)
			continue
		}
		if l, ok := last[key]; ok && descriptor.Text(l[b.Value]) != descriptor.Text(want[key][b.Value]) {
			moved = append(moved, key)
		}
	}
	sort.Strings(gone)
	call := func(op *descriptor.Op, send any) (any, error) {
		path, err := rest.Expand(op.Path, params)
		if err != nil {
			return nil, err
		}
		return api.DoWith(ctx, op.Method, rest.WithQuery(path, op.QueryOf(params)), send, op.HeadersOf(params))
	}
	wrote := false
	if len(gone)+len(moved) > 0 {
		if len(keys) > 0 {
			return false, fmt.Errorf("spec.%s: %s %s would be removed or changed, and the API removes these only all together: list none to remove them all, then list the ones wanted",
				b.Field, b.Key, strings.Join(append(gone, moved...), ", "))
		}
		resp, err := call(&b.Clear, nil)
		if err != nil && !errors.Is(err, rest.ErrNotFound) {
			return false, fmt.Errorf("removing spec.%s: %w", b.Field, err)
		}
		held, wrote = map[string]any{}, true
		if m, ok := descriptor.Dig(resp, b.At).(map[string]any); ok {
			held = m
		}
	}
	if len(missing) > 0 {
		send := make([]any, 0, len(missing))
		for _, key := range missing {
			send = append(send, want[key])
		}
		resp, err := call(&b.Add, map[string]any{b.Field: send})
		if err != nil {
			return false, fmt.Errorf("adding spec.%s (%s %s): %w", b.Field, b.Key, strings.Join(missing, ", "), err)
		}
		wrote = true
		if m, ok := descriptor.Dig(resp, b.At).(map[string]any); ok {
			held = m
		}
	}
	if held != nil {
		for f, v := range descriptor.MergeAt(obs, b.At, held) {
			obs[f] = v
		}
	}
	return wrote, nil
}

// DeleteSet removes a set kind's elements, those the spec names: a 404,
// or a status the delete counts as done (an installation's last
// repository, which GitHub keeps), is done. A batch list the spec lists
// elements of is cleared (Set.Batch).
func DeleteSet(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string, body map[string]any) error {
	want, err := SetElements(k, body)
	if err != nil {
		return err
	}
	for _, w := range want {
		if err := setCall(ctx, api, k, k.Delete, params, w, true); err != nil && !errors.Is(err, rest.ErrNotFound) && !allowed(k.Delete, err) {
			key, _ := k.Set.KeyOf(w)
			return fmt.Errorf("removing %s %q: %w", k.Set.KeyNames(), key, err)
		}
	}
	if b := k.Set.Batch; b != nil {
		if keys, _, _, _ := batchElements(b, body); len(keys) > 0 {
			path, err := rest.Expand(b.Clear.Path, params)
			if err != nil {
				return err
			}
			if _, err := api.DoWith(ctx, b.Clear.Method, rest.WithQuery(path, b.Clear.QueryOf(params)), nil, b.Clear.HeadersOf(params)); err != nil && !errors.Is(err, rest.ErrNotFound) {
				return fmt.Errorf("removing spec.%s: %w", b.Field, err)
			}
		}
	}
	return nil
}
