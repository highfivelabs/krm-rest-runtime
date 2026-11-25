// SPDX-License-Identifier: Apache-2.0

// Package kindtest runs every kind of a vendor's types.yaml through the
// reconciler against the fake API: create, read back, change a field and
// update, drift repair, and delete.
package kindtest

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	apifake "github.com/highfivelabs/krm-rest-runtime/fake"
	"github.com/highfivelabs/krm-rest-runtime/reconciler"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

const ns = "team-a"

// EveryKind tests every kind in types.yaml; credentials holds one value per
// credential key.
func EveryKind(t *testing.T, typesYAML []byte, vendor *rest.Vendor, hooks map[string][]reconciler.Hook) {
	t.Helper()
	f, err := descriptor.Parse(typesYAML)
	if err != nil {
		t.Fatal(err)
	}
	for i := range f.Kinds {
		k := &f.Kinds[i]
		t.Run(k.Kind+"."+k.Group, func(t *testing.T) {
			if k.IsDraft() {
				t.Skip(k.DraftReason())
			}
			one(t, f, k, vendor, hooks[k.Kind])
		})
	}
}

func one(t *testing.T, f *descriptor.File, k *descriptor.Kind, vendor *rest.Vendor, hooks []reconciler.Hook) {
	api := apifake.New(f)
	srv := httptest.NewServer(api)
	defer srv.Close()

	gvk := k.GVK()
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(gvk, meta.RESTScopeNamespace)
	mapper.Add(f.ConnectionGVK(), meta.RESTScopeNamespace)
	mapper.Add(corev1.SchemeGroupVersion.WithKind("Secret"), meta.RESTScopeNamespace)
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	creds := map[string][]byte{}
	for _, key := range vendor.CredentialKeys {
		creds[key] = []byte("test-" + key)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "creds"}, Data: creds}
	conn := &unstructured.Unstructured{}
	conn.SetGroupVersionKind(f.ConnectionGVK())
	conn.SetNamespace(ns)
	conn.SetName("default")
	_ = unstructured.SetNestedField(conn.Object, srv.URL, "spec", "baseURL")
	_ = unstructured.SetNestedField(conn.Object, "creds", "spec", "secretRef", "name")

	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(ns)
	obj.SetName("sample")
	spec := runtime.DeepCopyJSONValue(map[string]any(k.Sample)).(map[string]any)
	if spec == nil {
		spec = map[string]any{}
	}
	obj.Object["spec"] = spec

	c := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).
		WithObjects(secret, conn, obj).WithStatusSubresource(obj).Build()
	r := &reconciler.Reconciler{Client: c, Kind: k, Vendor: vendor, Connection: f.ConnectionGVK(), Hooks: hooks, Kinds: reconciler.KindsByName(f)}
	if k.SQL != nil {
		// The fake's SQL API answers a create 202 and has the answer at
		// once: polling needs no wait.
		r.Wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "sample"}}

	reconcile := func(step string) *unstructured.Unstructured {
		t.Helper()
		if _, err := r.Reconcile(ctx, req); err != nil {
			t.Fatalf("%s: reconcile: %v", step, err)
		}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(gvk)
		if err := c.Get(ctx, req.NamespacedName, got); err != nil {
			t.Fatalf("%s: get: %v", step, err)
		}
		conds, _, _ := unstructured.NestedSlice(got.Object, "status", "conditions")
		if len(conds) == 0 || conds[0].(map[string]any)["status"] != "True" {
			t.Fatalf("%s: not Ready: %v", step, conds)
		}
		return got
	}

	if k.Shape == descriptor.Assoc {
		assoc(t, api, k, spec, c, r, req, reconcile)
		return
	}
	if k.Shape == descriptor.SetShape {
		setSteps(t, api, k, spec, c, r, req, reconcile)
		return
	}
	if k.SQL != nil {
		sqlSteps(t, api, k, spec, c, r, req, reconcile)
		return
	}
	if k.Element != nil {
		elementSteps(t, api, k, spec, c, r, req, reconcile)
		return
	}
	if k.Unread != "" {
		unreadSteps(t, api, k, spec, c, r, req, reconcile)
		return
	}

	// 1. Create.
	got := reconcile("create")
	if api.Len() != 1 && k.Shape != descriptor.Settings {
		t.Fatalf("create: fake holds %d objects, want 1: %v", api.Len(), api.Paths())
	}
	path := storedPath(t, api, k, got, spec)
	stored, _ := api.Get(path)
	if k.Shape == descriptor.Settings && k.BodyField != "" && stored == nil {
		stored = map[string]any{k.BodyField: []any{}}
	}
	for f, v := range bodyOf(k, spec) {
		// What the spec sets; the API may add to an object (the fake keeps a
		// nested ID where the create answers it: Datadog's attributes/token).
		if reconciler.Drift(map[string]any{f: v}, map[string]any{f: stored[f]}, nil) != "" {
			t.Fatalf("create: %s = %v in the API, want %v", f, stored[f], v)
		}
	}

	// 1b. A rotating token: once in its rotation window, a new token is
	// made and the old one deleted.
	if k.Rotate != nil {
		lifetime, _, err := k.Rotate.Durations(spec)
		if err != nil {
			t.Fatalf("rotate: %v", err)
		}
		oldID, _, _ := unstructured.NestedString(got.Object, "status", "id")
		at := time.Now().Add(lifetime)
		r.Now = func() time.Time { return at }
		got = reconcile("rotate")
		r.Now = nil
		newID, _, _ := unstructured.NestedString(got.Object, "status", "id")
		if newID == "" || newID == oldID {
			t.Fatalf("rotate: status.id %q, was %q: no new token", newID, oldID)
		}
		if _, still := api.Get(path); still {
			t.Fatalf("rotate: the old token %s is still in the API", path)
		}
		path = storedPath(t, api, k, got, spec)
		stored, _ = api.Get(path)
	}

	// 2. Change a field and update (where the kind can be updated).
	if field := changeable(k, spec); field != "" && k.Update != nil {
		_ = unstructured.SetNestedField(got.Object, "changed-"+field, "spec", field)
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		reconcile("update")
		stored, _ = api.Get(path)
		if stored[field] != "changed-"+field {
			t.Fatalf("update: %s = %v in the API, want the new value", field, stored[field])
		}
		// 3. Drift: change it behind the controller's back, and it is put back.
		stored["_drift"] = true
		stored[field] = "edited-in-the-console"
		reconcile("drift")
		stored, _ = api.Get(path)
		if stored[field] != "changed-"+field {
			t.Fatalf("drift: %s = %v, want it restored", field, stored[field])
		}
	}

	// 2b. A kind with no update but a Replace: a change stops until the
	// object opts in, then re-creates it in place.
	if k.Replace != nil {
		replaceSteps(t, api, k, spec, c, r, req, path, reconcile)
	}

	// 4. Delete.
	got = &unstructured.Unstructured{}
	got.SetGroupVersionKind(gvk)
	_ = c.Get(ctx, req.NamespacedName, got)
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: reconcile: %v", err)
	}
	_, still := api.Get(path)
	if k.Shape != descriptor.Settings && k.Forget == "" && k.Stays == "" && !k.StateDeletes() && still {
		t.Fatalf("delete: %s is still in the API", path)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err == nil {
		t.Fatalf("delete: the object still exists (finalizer not removed)")
	}
}

// storedPath is the item path the object lives at in the fake.
func storedPath(t *testing.T, api *apifake.Server, k *descriptor.Kind, obj *unstructured.Unstructured, spec map[string]any) string {
	t.Helper()
	vals := map[string]string{}
	for _, p := range k.Params {
		vals[p.Name] = fmt.Sprint(p.In(spec))
	}
	if k.ID != nil {
		id, _, _ := unstructured.NestedString(obj.Object, "status", "id")
		vals[k.ID.Param] = id
	}
	if k.IsGraphQL() {
		p := apifake.GraphQLKey(k, vals)
		if _, ok := api.Get(p); !ok && k.Shape != descriptor.Settings {
			t.Fatalf("nothing stored at %s; stored: %v", p, api.Paths())
		}
		return p
	}
	if k.ReadsByBody() {
		p := apifake.BodyKey(k, vals)
		if _, ok := api.Get(p); !ok {
			t.Fatalf("nothing stored at %s; stored: %v", p, api.Paths())
		}
		return p
	}
	p, err := rest.Expand(k.ItemPath(), vals) // the read path, unless it reads elsewhere
	if err != nil {
		t.Fatal(err)
	}
	if k.Read.Find != nil {
		listPath := p
		var item *descriptor.Op // where the item is written: its own path, not the list's (readFrom)
		for _, op := range []*descriptor.Op{k.Delete, k.Update} {
			if op != nil && (strings.Contains(op.Path, "{"+k.Read.Find.Param+"}") || k.Read.Find.Param == "" && op == k.Delete) {
				item = op
				break
			}
		}
		if item != nil {
			if p, err = rest.Expand(item.Path, vals); err != nil {
				t.Fatal(err)
			}
			if _, ok := api.Get(p); !ok && item == k.Delete && k.Update != nil {
				// Deleted on a path its create's parameters do not name
				// (Atlas's project API key, by its organization): kept at
				// its update's.
				if up, err := rest.Expand(k.Update.Path, vals); err == nil {
					if _, ok := api.Get(up); ok {
						p = up
					}
				}
			}
		} else {
			p = strings.TrimSuffix(p, "/") + "/" + url.PathEscape(vals[k.Read.Find.Param])
			if strings.HasSuffix(k.Read.Path, "/") {
				p += "/"
			}
			if _, ok := api.Get(p); !ok && k.Create != nil {
				// Made on a collection no path of which names it (PagerDuty's
				// POST /service_dependencies/associate): kept there by its ID.
				if c, err := rest.Expand(k.Create.Path, vals); err == nil {
					if _, ok := api.Get(c + "/" + url.PathEscape(vals[k.Read.Find.Param])); ok {
						p = c + "/" + url.PathEscape(vals[k.Read.Find.Param])
					}
				}
			}
		}
		if _, ok := api.Get(p); !ok {
			// Written where it is listed, in a list of one (GitHub's EMU
			// group mapping), or within the item it is listed in (a
			// GitLab user's identities): kept at the list's path.
			if _, ok := api.Get(listPath); ok {
				p = listPath
			}
		}
		if _, ok := api.Get(p); !ok && k.Create != nil && k.ID != nil {
			// Made on a collection that is not its list, and read only
			// from the list (Datadog's child organization: made by POST
			// /api/v1/org, listed at /api/v2/org): kept under the collection.
			if c, err := rest.Expand(k.Create.Path, vals); err == nil {
				if _, ok := api.Get(c + "/" + url.PathEscape(vals[k.ID.Param])); ok {
					p = c + "/" + url.PathEscape(vals[k.ID.Param])
				}
			}
		}
	}
	if _, ok := api.Get(p); !ok && k.Shape != descriptor.Settings {
		paths := api.Paths()
		sort.Strings(paths)
		t.Fatalf("nothing stored at %s; stored: %v", p, paths)
	}
	return p
}

func bodyOf(k *descriptor.Kind, spec map[string]any) map[string]any {
	out := map[string]any{}
	isParam := map[string]bool{}
	for _, p := range k.Params {
		if !p.InBody {
			isParam[p.Field] = true
		}
	}
	for _, f := range k.QueryFields {
		isParam[f] = true
	}
	for f, v := range spec {
		if !isParam[f] && f != "secretRefs" && f != "connectionRef" && f != reconciler.ReplaceOnChange && f != reconciler.WriteSecretRef {
			out[f] = v
		}
	}
	return out
}

// inPath: the body field fills a path parameter too (Param.InBody), so
// changing it names another resource.
func inPath(k *descriptor.Kind, field string) bool {
	for _, p := range k.Params {
		if p.Field == field {
			return true
		}
	}
	return false
}

// names reports whether a body field names an Element kind's element (its
// key, or a bare value): changing it names another element.
func names(k *descriptor.Kind, field string) bool {
	if e := k.Element; e != nil {
		_, in := e.In[field]
		return in || field == e.Key
	}
	return false
}

// changeable picks a top-level string body field to change, if there is one.
func changeable(k *descriptor.Kind, spec map[string]any) string {
	if k.SQL != nil {
		return sqlChangeable(k, spec)
	}
	var cands []string
	for f, v := range bodyOf(k, spec) {
		if _, ok := v.(string); ok && !k.IsWriteOnly(f) && !k.IsCreateOnly(f) && !strings.HasSuffix(f, "_id") && !inPath(k, f) && !k.IsStateField(f) && !names(k, f) {
			cands = append(cands, f)
		}
	}
	sort.Strings(cands)
	if len(cands) == 0 {
		return ""
	}
	return cands[0]
}

var _ = schema.GroupVersionKind{}
var _ client.Object = &unstructured.Unstructured{}

// assoc: create the link, change it (the old one is undone), delete it.
func assoc(t *testing.T, api *apifake.Server, k *descriptor.Kind, spec map[string]any, c client.Client,
	r *reconciler.Reconciler, req ctrl.Request, reconcile func(string) *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	got := reconcile("create")
	body := bodyOf(k, spec)
	if _, ok := api.Get(apifake.AssocKey(k, body)); !ok {
		t.Fatalf("create: link not in the API: %v", api.Paths())
	}
	got = reconcile("resync") // reading it back finds it: nothing new is sent
	if api.Len() != 1 {
		t.Fatalf("resync: fake holds %d links, want 1: %v", api.Len(), api.Paths())
	}
	if field := changeable(k, spec); field != "" {
		_ = unstructured.SetNestedField(got.Object, "changed-"+field, "spec", field)
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		got = reconcile("change")
		body[field] = "changed-" + field
		if _, ok := api.Get(apifake.AssocKey(k, body)); !ok || api.Len() != 1 {
			t.Fatalf("change: want only the new link; stored: %v", api.Paths())
		}
	}
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: reconcile: %v", err)
	}
	if api.Len() != 0 {
		t.Fatalf("delete: links remain: %v", api.Paths())
	}
}

// unreadSteps runs a kind that is never read (descriptor Kind.Unread): its
// write is sent once, again only when the spec changes, never to repair a
// change made outside, and deleting the object sends nothing.
func unreadSteps(t *testing.T, api *apifake.Server, k *descriptor.Kind, spec map[string]any, c client.Client,
	r *reconciler.Reconciler, req ctrl.Request, reconcile func(string) *unstructured.Unstructured) {
	t.Helper()
	ctx := context.Background()
	calls := func() int {
		n := 0
		for _, v := range api.Calls {
			n += v
		}
		return n
	}
	vals := map[string]string{}
	for _, p := range k.Params {
		vals[p.Name] = fmt.Sprint(p.In(spec))
	}
	path, err := rest.Expand(k.Update.Path, vals)
	if err != nil {
		t.Fatal(err)
	}
	sent := func(step string, want map[string]any) {
		t.Helper()
		stored, _ := api.Get(path)
		for f, v := range bodyOf(k, want) {
			if reconciler.Drift(map[string]any{f: v}, map[string]any{f: stored[f]}, nil) != "" {
				t.Fatalf("%s: %s = %v in the API, want %v", step, f, stored[f], v)
			}
		}
	}
	got := reconcile("create")
	if n := calls(); n != 1 {
		t.Fatalf("create: %d calls, want the one write: %v", n, api.Calls)
	}
	sent("create", spec)
	got = reconcile("resync")
	if n := calls(); n != 1 {
		t.Fatalf("resync: %d calls, want none more (it is never read): %v", n, api.Calls)
	}

	// A change of the spec is written; one made outside is not seen.
	field, value := changeable(k, spec), any(nil)
	if field != "" {
		value = "changed-" + field
	} else {
		for _, f := range sortedKeys(bodyOf(k, spec)) {
			if l, ok := spec[f].([]any); ok && !inPath(k, f) {
				field, value = f, append(append([]any{}, l...), "changed-"+f)
				break
			}
		}
	}
	if field != "" {
		_ = unstructured.SetNestedField(got.Object, runtime.DeepCopyJSONValue(value), "spec", field)
		if err := c.Update(ctx, got); err != nil {
			t.Fatal(err)
		}
		got = reconcile("update")
		if n := calls(); n != 2 {
			t.Fatalf("update: %d calls, want a second write: %v", n, api.Calls)
		}
		changed := map[string]any{}
		for f, v := range spec {
			changed[f] = v
		}
		changed[field] = value
		sent("update", changed)
		stored, _ := api.Get(path)
		stored[field] = "edited-in-the-console"
		got = reconcile("drift")
		if n := calls(); n != 2 {
			t.Fatalf("drift: %d calls, want none (it is never read): %v", n, api.Calls)
		}
	}

	before := calls()
	if err := c.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, req); err != nil {
		t.Fatalf("delete: reconcile: %v", err)
	}
	if n := calls(); n != before {
		t.Fatalf("delete: %d calls, want none (it is left as it is): %v", n-before, api.Calls)
	}
	if err := c.Get(ctx, req.NamespacedName, got); err == nil {
		t.Fatalf("delete: the object still exists (finalizer not removed)")
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for f := range m {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
