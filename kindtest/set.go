// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"context"
	"net/url"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// setSteps runs a set kind (descriptor.SetShape): every element is added,
// one made outside the controller is removed, one the spec drops is
// removed, and deleting the object removes the rest.
func setSteps(t *testing.T, api *apifake.Server, k *descriptor.Kind, spec map[string]any, c client.Client,
	r *reconciler.Reconciler, req ctrl.Request, reconcile func(string) *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	params := map[string]string{}
	for _, p := range k.Params {
		params[p.Name] = descriptor.Text(spec[p.Field])
	}
	list, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		t.Fatal(err)
	}
	// An element is kept at its delete's path: under the list's, or (an
	// Okta user's linked objects) on a path of its own; one named by
	// set.keys, with its delete's query too (apifake.SetElementPath).
	prefix, suffix := list+"/", ""
	if len(k.Set.Keys) > 0 {
		p, err := apifake.SetElementPath(k, params, map[string]any{})
		if err != nil {
			t.Fatal(err)
		}
		prefix, _, _ = strings.Cut(p, "?")
		prefix += "?"
	} else if !strings.HasPrefix(k.Delete.Path, k.Read.Path+"/") {
		vals := map[string]string{}
		for n, v := range params {
			vals[n] = v
		}
		vals[k.Set.Param] = "\x00"
		p, err := rest.Expand(k.Delete.Path, vals)
		if err != nil {
			t.Fatal(err)
		}
		prefix, suffix, _ = strings.Cut(p, url.PathEscape("\x00"))
	}
	at := func(key string) string { return prefix + url.PathEscape(key) + suffix }
	if len(k.Set.Keys) > 0 {
		at = func(key string) string {
			q, _ := url.ParseQuery(key)
			elem := map[string]any{}
			for f := range q {
				elem[f] = q.Get(f)
			}
			p, err := apifake.SetElementPath(k, params, elem)
			if err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	want, err := reconciler.SetElements(k, spec)
	if err != nil || len(want) == 0 {
		t.Fatalf("sample: spec.%s needs elements: %v", k.Set.Field, err)
	}
	keys := make([]string, len(want))
	for i, e := range want {
		keys[i], _ = k.Set.KeyOf(e)
	}
	// refused: the sync fails, and says why.
	refused := func(step, why string) {
		t.Helper()
		_, _ = r.Reconcile(ctx, req)
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(k.GVK())
		if err := c.Get(ctx, req.NamespacedName, got); err != nil {
			t.Fatalf("%s: get: %v", step, err)
		}
		conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
		if len(conds) == 0 || conds[0].(map[string]any)["status"] != "False" || !strings.Contains(descriptor.Text(conds[0].(map[string]any)["message"]), why) {
			t.Fatalf("%s: want a refusal (%s): %v", step, why, conds)
		}
	}
	held := func(step string, keys ...string) {
		t.Helper()
		n := 0
		for _, p := range api.Paths() {
			if mid, ok := strings.CutPrefix(p, prefix); ok && strings.HasSuffix(mid, suffix) && !strings.Contains(strings.TrimSuffix(mid, suffix), "/") {
				n++
			}
		}
		for _, key := range keys {
			if _, ok := api.Get(at(key)); !ok {
				t.Fatalf("%s: %s %q not in the API: %v", step, k.Set.Key, key, api.Paths())
			}
		}
		if n != len(keys) {
			t.Fatalf("%s: the API holds %d elements, want %d (%v): %v", step, n, len(keys), keys, api.Paths())
		}
	}

	got := reconcile("create")
	held("create", keys...)
	got = reconcile("resync")
	held("resync", keys...)

	// Drift: an element made outside the controller is removed.
	elsewhere := map[string]any{}
	for _, f := range k.Set.KeyFields() {
		elsewhere[f] = "made-elsewhere"
	}
	other, _ := k.Set.KeyOf(elsewhere)
	api.Put(at(other), elsewhere)
	got = reconcile("drift")
	held("drift", keys...)

	// Drift of an element's field, where no call changes elements: refused
	// until it is as the spec has it.
	if k.Update == nil {
		for f, v := range want[0] {
			isKey := false
			for _, kf := range k.Set.KeyFields() {
				isKey = isKey || kf == f
			}
			if _, isStr := v.(string); !isStr || isKey {
				continue
			}
			stored, _ := api.Get(at(keys[0]))
			stored[f] = "edited-in-the-console"
			refused("element drift", "cannot be changed in place")
			stored[f] = v
			got = reconcile("element drift put back")
			break
		}
	}

	// Drift of an element's field, where the kind changes elements: put back.
	if k.Update != nil {
		for f, v := range want[0] {
			if _, isStr := v.(string); !isStr || f == k.Set.Key {
				continue
			}
			stored, _ := api.Get(at(keys[0]))
			stored[f] = "edited-in-the-console"
			got = reconcile("element drift")
			if stored, _ = api.Get(at(keys[0])); stored[f] != v {
				t.Fatalf("element drift: %s = %v, want %v put back", f, stored[f], v)
			}
			break
		}
	}

	if b := k.Set.Batch; b != nil {
		got = batchSteps(t, api, k, list, c, got, reconcile, refused)
	}

	// Change: an element the spec drops is removed.
	if len(want) > 1 {
		l, _, _ := unstructured.NestedSlice(got.Object, "spec", k.Set.Field)
		_ = unstructured.SetNestedSlice(got.Object, l[:len(l)-1], "spec", k.Set.Field)
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		got = reconcile("change")
		keys = keys[:len(keys)-1]
		held("change", keys...)
	}

	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: reconcile: %v", err)
	}
	held("delete")
	if b := k.Set.Batch; b != nil {
		if left := api.Batch(list); len(left) > 0 {
			t.Fatalf("delete: spec.%s left %v", b.Field, left)
		}
	}
}

// batchSteps runs a set kind's batch list (Set.Batch): the sample's
// elements are added, one made outside the controller or changed in the
// spec is refused, listing none removes them all, and listing them again
// adds them back.
func batchSteps(t *testing.T, api *apifake.Server, k *descriptor.Kind, list string, c client.Client, got *unstructured.Unstructured,
	reconcile func(string) *unstructured.Unstructured, refused func(string, string)) *unstructured.Unstructured {
	t.Helper()
	ctx := context.Background()
	b := k.Set.Batch
	sample, _, _ := unstructured.NestedSlice(got.Object, "spec", b.Field)
	if len(sample) == 0 {
		t.Fatalf("sample: spec.%s needs elements", b.Field)
	}
	holds := func(step string, elems []any) {
		t.Helper()
		have := api.Batch(list)
		if len(have) != len(elems) {
			t.Fatalf("%s: spec.%s: the API holds %v, want %v", step, b.Field, have, elems)
		}
		for _, e := range elems {
			m := e.(map[string]any)
			if v := have[descriptor.Text(m[b.Key])]; v != apifake.BatchValue(descriptor.Text(m[b.Value])) {
				t.Fatalf("%s: spec.%s: %s %v holds %v", step, b.Field, b.Key, m[b.Key], v)
			}
		}
		at, _, _ := unstructured.NestedMap(got.Object, append([]string{"status", "atProvider"}, strings.Split(b.At, "/")...)...)
		if len(at) != len(elems) {
			t.Fatalf("%s: status.atProvider.%s is %v", step, b.At, at)
		}
	}
	setList := func(step string, l []any) {
		t.Helper()
		if err := c.Get(ctx, client.ObjectKeyFromObject(got), got); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		_ = unstructured.SetNestedSlice(got.Object, l, "spec", b.Field)
		if err := c.Update(ctx, got); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	holds("batch create", sample)

	// One made outside the controller can go only with the rest: refused.
	api.PutBatch(list, "made-elsewhere", "elsewhere")
	refused("batch drift", "only all together")

	// None: they all go, the one made elsewhere too.
	setList("batch clear", []any{})
	got = reconcile("batch clear")
	holds("batch clear", nil)

	// Listed again: added in one call.
	setList("batch add", sample)
	got = reconcile("batch add")
	holds("batch add", sample)

	// A changed value can go only with the rest: refused, then put back.
	changed := runtime.DeepCopyJSONValue(sample).([]any)
	changed[0].(map[string]any)[b.Value] = "changed-in-the-spec"
	setList("batch change", changed)
	refused("batch change", "only all together")
	setList("batch change undone", sample)
	got = reconcile("batch change undone")
	holds("batch change undone", sample)
	return got
}
