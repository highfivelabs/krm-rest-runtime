// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// elementSteps: an Element kind's element is put in its list, changed (the
// old one taken out), put back after it is taken out behind the
// controller's back, and taken out on delete; the resource holding the
// list stays.
func elementSteps(t *testing.T, api *apifake.Server, k *descriptor.Kind, spec map[string]any, c client.Client,
	r *reconciler.Reconciler, req ctrl.Request, reconcile func(string) *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	e := k.Element
	vals := map[string]string{}
	for _, p := range k.Params {
		vals[p.Name] = fmt.Sprint(spec[p.Field])
	}
	path, err := rest.Expand(k.Read.Path, vals)
	if err != nil {
		t.Fatal(err)
	}
	list := func(body map[string]any) (string, []any, any) {
		_, at, value, err := e.Of(body)
		if err != nil {
			t.Fatal(err)
		}
		holder, ok := api.Get(path)
		if !ok {
			t.Fatalf("the resource holding the element is gone: %v", api.Paths())
		}
		if len(e.In) == 0 && e.List == "" {
			l, _ := holder[""].([]any)
			return "", l, value
		}
		l, _ := descriptor.Dig(holder, at).([]any)
		return at, l, value
	}
	has := func(step string, body map[string]any, want bool) {
		t.Helper()
		_, l, value := list(body)
		i := e.Find(l, value)
		if (i >= 0) != want {
			t.Fatalf("%s: element %v in the list = %v, want %v: %v", step, value, i >= 0, want, l)
		}
		if want && len(e.In) == 0 {
			got, _ := l[i].(map[string]any)
			if f := reconciler.Drift(body, got, k.IsWriteOnly); f != "" {
				t.Fatalf("%s: element's %s = %v, want %v", step, f, got[f], body[f])
			}
		}
	}

	// 1. Create: the element is in its list.
	got := reconcile("create")
	body := bodyOf(k, spec)
	has("create", body, true)
	got = reconcile("resync")

	// 2. Change it: a bare value becomes another one (the old one goes); an
	// object's field changes in place.
	field, _, value, _ := e.Of(body)
	old := map[string]any{}
	for f, v := range body {
		old[f] = v
	}
	var change any
	if len(e.In) > 0 {
		switch v := value.(type) {
		case string:
			change = "changed-" + v
		case float64:
			change = v + 1
		case int64:
			change = v + 1
		}
	} else if f := changeable(k, spec); f != "" && f != e.Key {
		field, change = f, "changed-"+f
	}
	if change != nil {
		_ = unstructured.SetNestedField(got.Object, change, "spec", field)
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		reconcile("update")
		body = bodyOf(k, got.Object["spec"].(map[string]any))
		has("update", body, true)
		if len(e.In) > 0 {
			has("update: the element applied before", old, false)
		}
	}

	// 3. Drift: taken out behind the controller's back, it is put back.
	at, l, value := list(body)
	holder, _ := api.Get(path)
	left := append([]any{}, l[:e.Find(l, value)]...)
	left = append(left, l[e.Find(l, value)+1:]...)
	if at == "" {
		holder[""] = left
	} else {
		parts := strings.Split(at, "/")
		m := holder
		for _, p := range parts[:len(parts)-1] {
			m, _ = m[p].(map[string]any)
		}
		m[parts[len(parts)-1]] = left
	}
	has("drift", body, false)
	reconcile("drift")
	has("drift", body, true)

	// 4. Delete: the element goes, the resource holding it stays.
	got = &unstructured.Unstructured{}
	got.SetGroupVersionKind(k.GVK())
	_ = c.Get(ctx, req.NamespacedName, got)
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: reconcile: %v", err)
	}
	has("delete", body, false)
	if err := c.Get(ctx, req.NamespacedName, got); err == nil {
		t.Fatalf("delete: the object still exists (finalizer not removed)")
	}
}
