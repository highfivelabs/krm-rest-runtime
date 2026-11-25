// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// GraphQLError is a GraphQL answer that reports errors: top-level errors
// (a document the schema refuses, a permission), or a mutation's own
// (GitLab's payload errors), each as the API words it. The HTTP status was
// 200; the request is the caller's to fix.
type GraphQLError struct {
	Operation string
	Messages  []string
}

func (e *GraphQLError) Error() string {
	return fmt.Sprintf("graphql %s: %s", e.Operation, strings.Join(e.Messages, "; "))
}

// GraphQLURL is where the vendor's GraphQL endpoint is for this
// connection: the base URL less Vendor.RESTPath, then endpoint.
func (c *Client) GraphQLURL(endpoint string) string {
	return strings.TrimSuffix(c.BaseURL, c.Vendor.RESTPath) + endpoint
}

// GraphQL posts a GraphQL request ({"query", "operationName", "variables"})
// to endpoint (Vendor.RESTPath) and returns the answer's data. Top-level
// errors fail it: as ErrNotFound when one says the object does not exist
// (GitHub's type NOT_FOUND, an extensions code NOT_FOUND, or what
// Vendor.NotFound recognizes, told status 200), else as a *GraphQLError.
func (c *Client) GraphQL(ctx context.Context, endpoint string, req map[string]any, headers map[string]string) (map[string]any, error) {
	out, _, err := c.doURL(ctx, http.MethodPost, c.GraphQLURL(endpoint), endpoint, req, headers)
	if err != nil {
		return nil, err
	}
	resp, _ := out.(map[string]any)
	if resp == nil {
		return nil, fmt.Errorf("graphql %v: the answer is not a JSON object", req["operationName"])
	}
	if errs, _ := resp["errors"].([]any); len(errs) > 0 {
		op, _ := req["operationName"].(string)
		gerr := &GraphQLError{Operation: op}
		notFound := false
		for _, e := range errs {
			m, _ := e.(map[string]any)
			msg, _ := m["message"].(string)
			if msg == "" {
				b, _ := json.Marshal(e)
				msg = string(b)
			}
			gerr.Messages = append(gerr.Messages, msg)
			ext, _ := m["extensions"].(map[string]any)
			for _, code := range []any{m["type"], ext["code"], ext["type"]} {
				if s, ok := code.(string); ok && strings.EqualFold(s, "NOT_FOUND") {
					notFound = true
				}
			}
		}
		if !notFound && c.Vendor.NotFound != nil {
			b, _ := json.Marshal(resp)
			notFound = c.Vendor.NotFound(http.StatusOK, b)
		}
		if notFound {
			return nil, ErrNotFound
		}
		return nil, gerr
	}
	data, _ := resp["data"].(map[string]any)
	return data, nil
}
