// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// ReplaceOnChange is the spec field an object sets to true to have a change
// to a kind with no update applied by re-creating the resource in place
// (descriptor.Kind.Replace). Only kinds with a Replace have it in their CRD.
const ReplaceOnChange = "replaceOnChange"

// replaceOnChange reads the object's opt-in. An object of a kind that cannot
// be replaced may not opt in: its CRD has no such field, and the reconciler
// refuses it too.
func (r *Reconciler) replaceOnChange(spec map[string]any) (bool, error) {
	v, ok := spec[ReplaceOnChange]
	if !ok || v == nil {
		return false, nil
	}
	on, isBool := v.(bool)
	if !isBool {
		return false, fmt.Errorf("spec.%s must be true or false", ReplaceOnChange)
	}
	if on && r.Kind.Replace == nil {
		return false, fmt.Errorf("spec.%s: %s cannot be re-created in place; delete and recreate the object to change it", ReplaceOnChange, r.Kind.Kind)
	}
	return on, nil
}

// replace applies a changed spec to a kind with no update by creating the
// resource again in place (CREATE OR REPLACE). A change to a field that names
// the resource is refused: re-creating would make a second one and leave the
// first behind.
func (r *Reconciler) replace(ctx context.Context, api *rest.Client, params map[string]string, body, obs map[string]any) (map[string]any, string, error) {
	k := r.Kind
	rp := k.Replace
	folded := r.normalize(body, obs)
	identity := map[string]bool{}
	for _, f := range rp.Identity {
		identity[f] = true
	}
	if f := Drift(body, folded, func(f string) bool { return !identity[strings.SplitN(f, ".", 2)[0]] }); f != "" {
		return obs, "drifted", fmt.Errorf("spec.%s names a different %s than the one this object made; re-creating it would leave the old one, so delete and recreate the object instead", f, k.Kind)
	}
	path, err := rest.Expand(rp.Path, params)
	if err != nil {
		return nil, "", err
	}
	if _, err := api.DoWith(ctx, rp.Method, rest.WithQuery(path, rp.QueryOf(params)), Request(r.Kind, &rp.Op, body), rp.HeadersOf(params)); err != nil {
		return nil, "", fmt.Errorf("replace: %w", err)
	}
	// The resource keeps its identity, so it is read back where it was (a
	// create's response need not be the resource: Snowflake answers a status).
	readPath, err := rest.Expand(k.Read.Path, params)
	if err != nil {
		return nil, "", err
	}
	resp, err := r.read(ctx, api, readPath, params)
	if err != nil {
		return nil, "", fmt.Errorf("read after replace: %w", err)
	}
	return r.unwrap(resp), "replaced", nil
}
