// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// WriteSecretRef is the spec field naming the Secret a kind's secret
// outputs are written to: {name}.
const WriteSecretRef = "writeSecretRef"

// writeSecretOutputs puts the secret outputs the API answered (an API key's
// privateKey, often only in the create's answer) into the Secret
// spec.writeSecretRef names, which the object owns; a key the answer lacks
// keeps what the Secret holds. A Secret of that name the object does not
// own is never written. Without writeSecretRef nothing is written, and the
// outputs are still kept out of the status.
func (r *Reconciler) writeSecretOutputs(ctx context.Context, obj *unstructured.Unstructured, spec, observed map[string]any) error {
	name, _, _ := unstructured.NestedString(spec, WriteSecretRef, "name")
	if name == "" {
		return nil
	}
	data := map[string][]byte{}
	for _, f := range r.Kind.SecretOutputs {
		v := descriptor.Dig(observed, strings.ReplaceAll(f, ".", "/"))
		switch x := v.(type) {
		case nil:
		case string:
			if x != "" {
				data[f] = []byte(x)
			}
		default:
			data[f] = []byte(descriptor.Text(x))
		}
	}
	if len(data) == 0 {
		return nil // nothing new: a read the secrets are not in
	}
	owner := metav1.OwnerReference{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: obj.GetName(), UID: obj.GetUID()}
	sec := &corev1.Secret{}
	err := r.Client.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}, sec)
	if apierrors.IsNotFound(err) {
		sec = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Namespace: obj.GetNamespace(), Name: name, OwnerReferences: []metav1.OwnerReference{owner}},
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}
		if err := r.Client.Create(ctx, sec); err != nil {
			return fmt.Errorf("writeSecretRef: creating Secret %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("writeSecretRef: %w", err)
	}
	owned := false
	for _, o := range sec.OwnerReferences {
		if o.UID == obj.GetUID() {
			owned = true
		}
	}
	if !owned {
		return fmt.Errorf("writeSecretRef: Secret %s exists and is not this object's; name another", name)
	}
	changed := false
	if sec.Data == nil {
		sec.Data = map[string][]byte{}
	}
	for k, v := range data {
		if string(sec.Data[k]) != string(v) {
			sec.Data[k], changed = v, true
		}
	}
	if !changed {
		return nil
	}
	if err := r.Client.Update(ctx, sec); err != nil {
		return fmt.Errorf("writeSecretRef: updating Secret %s: %w", name, err)
	}
	return nil
}
