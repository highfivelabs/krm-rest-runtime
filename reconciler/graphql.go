// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// GraphQLCall makes op, a GraphQL operation (descriptor.GraphQL), with its
// variables filled from params and body, and returns what its answer holds
// at op.GraphQL.Result: the object, a find read's list, or nil. A 200 that
// reports errors, top-level or in the mutation's own list, fails: as
// rest.ErrNotFound where they say the object is gone (rest.Client.GraphQL;
// the vendor's NotFound, told status 200, for the mutation's own).
func GraphQLCall(ctx context.Context, api *rest.Client, op *descriptor.Op, params map[string]string, body map[string]any) (any, error) {
	g := op.GraphQL
	req, err := g.Request(params, body)
	if err != nil {
		return nil, err
	}
	data, err := api.GraphQL(ctx, op.Path, req, op.Headers)
	if err != nil {
		return nil, err
	}
	if g.Errors != "" {
		if errs, _ := descriptor.Dig(data, g.Errors).([]any); len(errs) > 0 {
			if nf := api.Vendor.NotFound; nf != nil {
				// What the vendor words as gone (GitLab's "Policy project
				// doesn't exist" to an unassign) is gone.
				if b, _ := json.Marshal(map[string]any{"errors": errs}); nf(http.StatusOK, b) {
					return nil, rest.ErrNotFound
				}
			}
			gerr := &rest.GraphQLError{Operation: g.Operation}
			for _, e := range errs {
				gerr.Messages = append(gerr.Messages, fmt.Sprint(e))
			}
			return nil, gerr
		}
	}
	if g.Result == "" {
		return nil, nil
	}
	return descriptor.Dig(data, g.Result), nil
}

// GraphQLRead reads a GraphQL kind's object: what its read's result holds,
// or with Read.Find the element of the list there whose find field is the
// find parameter. A null result, no such element, or a result whose
// Exists field is not true, is rest.ErrNotFound.
func GraphQLRead(ctx context.Context, api *rest.Client, k *descriptor.Kind, params map[string]string) (map[string]any, error) {
	v, err := GraphQLCall(ctx, api, &k.Read, params, nil)
	if err != nil {
		return nil, err
	}
	if fd := k.Read.Find; fd != nil {
		m, ok := k.FindIn(v, params[fd.Param])
		if !ok {
			return nil, rest.ErrNotFound
		}
		return m, nil
	}
	m, _ := v.(map[string]any)
	if m == nil {
		return nil, rest.ErrNotFound
	}
	if ex := k.Read.GraphQL.Exists; ex != "" && descriptor.Dig(m, ex) != true {
		return nil, rest.ErrNotFound
	}
	return m, nil
}
