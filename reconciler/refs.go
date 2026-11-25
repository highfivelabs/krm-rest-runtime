// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// FieldRefs is the spec field that fills other spec fields from other
// objects in the same namespace:
//
//	fieldRefs:
//	  zoneID: {kind: Zone, name: my-zone}                     # its status.id
//	  workerName: {kind: Worker, name: w, fieldPath: spec.name}
//
// kind names one of the vendor's kinds (apiVersion too, where the name is in
// more than one group); fieldPath defaults to status.id. A status field is
// taken only once the object is Ready; a spec field at once. Until every
// reference resolves, nothing is sent and the object reports
// WaitingForReference.
//
// The values used are kept in status.resolvedRefs, so the object can still
// be read and deleted after the one it refers to is gone. Kubernetes does
// not order deletes: deleting the object referred to first leaves this one
// to the vendor's API, which either deletes it with its parent (this one's
// delete then answers 404, which is done) or refuses to delete the parent
// until it is gone (that delete is retried until then).
const FieldRefs = "fieldRefs"

// WaitingForReference is the Ready condition's reason while a fieldRef
// does not resolve yet.
const WaitingForReference = "WaitingForReference"

// waitErr says which reference is not there yet.
type waitErr struct{ msg string }

func (e *waitErr) Error() string { return e.msg }

// KindsByName maps each kind's name to its GVK, for fieldRefs that name a
// kind without its apiVersion. A name in more than one group maps to none.
func KindsByName(f *descriptor.File) map[string]schema.GroupVersionKind {
	out := map[string]schema.GroupVersionKind{}
	dup := map[string]bool{}
	for i := range f.Kinds {
		k := &f.Kinds[i]
		if k.IsDraft() {
			continue // never reconciled: nothing to refer to
		}
		if _, seen := out[k.Kind]; seen {
			dup[k.Kind] = true
		}
		out[k.Kind] = k.GVK()
	}
	for k := range dup {
		delete(out, k)
	}
	return out
}

// resolveRefs fills spec from spec.fieldRefs and returns the values used.
// deleting: a reference that no longer resolves falls back to the value
// last used (status.resolvedRefs).
func (r *Reconciler) resolveRefs(ctx context.Context, obj *unstructured.Unstructured, spec map[string]any, deleting bool) (map[string]any, error) {
	refs, _, _ := unstructured.NestedMap(spec, FieldRefs)
	if len(refs) == 0 {
		return nil, nil
	}
	last, _, _ := unstructured.NestedMap(obj.Object, "status", "resolvedRefs")
	fields := make([]string, 0, len(refs))
	for f := range refs {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	resolved := map[string]any{}
	for _, field := range fields {
		if field == FieldRefs || field == "secretRefs" || field == "connectionRef" || field == WriteSecretRef {
			return nil, fmt.Errorf("fieldRefs.%s: not a field fieldRefs can fill", field)
		}
		if v, set := spec[field]; set && v != nil && v != "" {
			return nil, fmt.Errorf("spec.%s is set and also in spec.fieldRefs: set one", field)
		}
		ref, _ := refs[field].(map[string]any)
		v, err := r.resolveRef(ctx, obj.GetNamespace(), field, ref)
		if err != nil {
			var w *waitErr
			if !deleting || last[field] == nil || !errors.As(err, &w) {
				return nil, err
			}
			v = last[field]
		}
		spec[field] = v
		resolved[field] = v
	}
	return resolved, nil
}

func (r *Reconciler) resolveRef(ctx context.Context, ns, field string, ref map[string]any) (any, error) {
	kind, _ := ref["kind"].(string)
	name, _ := ref["name"].(string)
	apiVersion, _ := ref["apiVersion"].(string)
	fieldPath, _ := ref["fieldPath"].(string)
	if fieldPath == "" {
		fieldPath = "status.id"
	}
	if kind == "" || name == "" {
		return nil, fmt.Errorf("fieldRefs.%s: kind and name are required", field)
	}
	var gvk schema.GroupVersionKind
	if apiVersion != "" {
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return nil, fmt.Errorf("fieldRefs.%s: %w", field, err)
		}
		gvk = gv.WithKind(kind)
	} else if g, ok := r.Kinds[kind]; ok {
		gvk = g
	} else {
		return nil, fmt.Errorf("fieldRefs.%s: no kind %q here, or it is in more than one group: give apiVersion", field, kind)
	}
	target := &unstructured.Unstructured{}
	target.SetGroupVersionKind(gvk)
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, target); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, &waitErr{fmt.Sprintf("fieldRefs.%s: waiting for %s %s to exist", field, kind, name)}
		}
		return nil, err
	}
	path := strings.Split(fieldPath, ".")
	if path[0] == "status" && !isReady(target) {
		return nil, &waitErr{fmt.Sprintf("fieldRefs.%s: waiting for %s %s to be Ready", field, kind, name)}
	}
	v, found, err := unstructured.NestedFieldNoCopy(target.Object, path...)
	if err != nil {
		return nil, fmt.Errorf("fieldRefs.%s: %s %s %s: %w", field, kind, name, fieldPath, err)
	}
	if !found || v == nil || v == "" {
		return nil, &waitErr{fmt.Sprintf("fieldRefs.%s: waiting for %s %s to have %s", field, kind, name, fieldPath)}
	}
	return v, nil
}

func isReady(obj *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "Ready" {
			return m["status"] == "True"
		}
	}
	return false
}
