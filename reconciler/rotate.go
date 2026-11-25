// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// RotationStatus is the status field a rotating token's object keeps its
// rotation in (descriptor.Rotate): expiresAt and rotateAt of the current
// token (RFC 3339), and retired, the IDs of tokens it replaced that are
// still to be deleted.
const RotationStatus = "rotation"

// rotation is one sync's plan for a rotating token: the token a create
// makes now expires at expires; replacing is the ID of the token it
// replaces, if it does.
type rotation struct {
	spec      map[string]any
	now       time.Time
	lifetime  time.Duration
	window    time.Duration
	expires   time.Time
	replacing string
	made      bool
}

type rotationKey struct{}

func withRotation(ctx context.Context, rot *rotation) context.Context {
	return context.WithValue(ctx, rotationKey{}, rot)
}

func rotationFrom(ctx context.Context) *rotation {
	rot, _ := ctx.Value(rotationKey{}).(*rotation)
	return rot
}

// clock is the time a rotation is judged by (Reconciler.Now, else now).
func (r *Reconciler) clock() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// rotationState is what the object's status says of its rotation.
func rotationState(obj *unstructured.Unstructured) (expiresAt time.Time, retired []string) {
	st, _, _ := unstructured.NestedMap(obj.Object, "status", RotationStatus)
	if s, ok := st["expiresAt"].(string); ok {
		expiresAt, _ = time.Parse(time.RFC3339, s)
	}
	if l, ok := st["retired"].([]any); ok {
		for _, v := range l {
			if s, ok := v.(string); ok && s != "" {
				retired = append(retired, s)
			}
		}
	}
	return expiresAt, retired
}

// planRotation decides whether this sync replaces the token: where one
// exists and its expiry is within the window (or unknown), its ID is taken
// out of params, so that the sync makes a new one.
func (r *Reconciler) planRotation(obj *unstructured.Unstructured, spec map[string]any, params map[string]string) (*rotation, error) {
	rt := r.Kind.Rotate
	lifetime, window, err := rt.Durations(spec)
	if err != nil {
		return nil, err
	}
	now := r.clock()
	rot := &rotation{spec: spec, now: now, lifetime: lifetime, window: window, expires: now.Add(lifetime)}
	idParam := r.Kind.ID.Param
	if id := params[idParam]; id != "" {
		expiresAt, _ := rotationState(obj)
		if expiresAt.IsZero() || !now.Before(expiresAt.Add(-window)) {
			rot.replacing = id
			delete(params, idParam)
		}
	}
	return rot, nil
}

// recordRotation puts the rotation in the object's status before ready
// writes it: the new token's expiry where this sync made one, the
// replaced token among those to retire. It returns the sync's action.
func recordRotation(obj *unstructured.Unstructured, rot *rotation, action string) string {
	expiresAt, retired := rotationState(obj)
	st, _, _ := unstructured.NestedMap(obj.Object, "status", RotationStatus)
	if st == nil {
		st = map[string]any{}
	}
	if rot.made {
		expiresAt = rot.expires
		if rot.replacing != "" {
			retired = append(retired, rot.replacing)
			action = "rotated"
		}
	}
	if !expiresAt.IsZero() {
		st["expiresAt"] = expiresAt.UTC().Format(time.RFC3339)
		st["rotateAt"] = expiresAt.Add(-rot.window).UTC().Format(time.RFC3339)
	}
	setRetired(st, retired)
	if s, _ := obj.Object["status"].(map[string]any); s == nil {
		obj.Object["status"] = map[string]any{}
	}
	_ = unstructured.SetNestedMap(obj.Object, st, "status", RotationStatus)
	return action
}

func setRetired(st map[string]any, retired []string) {
	if len(retired) == 0 {
		delete(st, "retired")
		return
	}
	l := make([]any, len(retired))
	for i, id := range retired {
		l[i] = id
	}
	st["retired"] = l
}

// retire deletes the tokens the object replaced (a 404 is done), keeping
// in its status those whose delete failed, for the next sync.
func (r *Reconciler) retire(ctx context.Context, api *rest.Client, obj *unstructured.Unstructured, params map[string]string) error {
	_, retired := rotationState(obj)
	if len(retired) == 0 {
		return nil
	}
	var left []string
	var errs []error
	for _, id := range retired {
		p := make(map[string]string, len(params))
		for k, v := range params {
			p[k] = v
		}
		p[r.Kind.ID.Param] = id
		if err := r.delete(ctx, api, p, nil, func() (map[string]any, error) { return nil, nil }); err != nil {
			left = append(left, id)
			errs = append(errs, fmt.Errorf("deleting replaced token %s: %w", id, err))
		}
	}
	st, _, _ := unstructured.NestedMap(obj.Object, "status", RotationStatus)
	if st == nil {
		st = map[string]any{}
	}
	setRetired(st, left)
	if err := unstructured.SetNestedMap(obj.Object, st, "status", RotationStatus); err != nil {
		return err
	}
	if err := r.Client.Status().Update(ctx, obj); err != nil {
		return err
	}
	return errors.Join(errs...)
}

// rotationRequeue is how long until the token is to be replaced, at most
// Resync, at least a second.
func rotationRequeue(obj *unstructured.Unstructured, now time.Time) time.Duration {
	st, _, _ := unstructured.NestedMap(obj.Object, "status", RotationStatus)
	s, _ := st["rotateAt"].(string)
	at, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return Resync
	}
	d := at.Sub(now)
	if d < time.Second {
		d = time.Second
	}
	return min(d, Resync)
}

// deleteOnDelete reports whether deleting a rotating token's object
// deletes its last token (Rotate.DeleteOnDelete true in the spec).
func (r *Reconciler) deleteOnDelete(spec map[string]any) bool {
	f := r.Kind.Rotate.DeleteOnDelete
	if f == "" {
		return false
	}
	v, _ := spec[f].(bool)
	return v
}
