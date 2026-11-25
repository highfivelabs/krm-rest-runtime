// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// A GraphQL answer is a 200 whatever happened: errors at the top level or
// in the mutation's own list fail the call; NOT_FOUND, or what the vendor
// says means gone, is ErrNotFound; the endpoint takes the place of the
// base URL's REST path.
func TestGraphQLCall(t *testing.T) {
	var answer, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	v := &rest.Vendor{Name: "v", RESTPath: "/api/v4", NotFound: func(status int, body []byte) bool {
		return status == http.StatusOK && strings.Contains(string(body), "does not exist or you don't have permission")
	}}
	api := rest.New(v, srv.URL+"/api/v4", nil)
	op := &descriptor.Op{Method: "POST", Path: "/api/graphql", GraphQL: &descriptor.GraphQL{
		Operation: "Create", Query: "mutation Create { create { thing { id } errors } }",
		Result: "create/thing", Errors: "create/errors",
	}}
	ctx := context.Background()

	answer = `{"data":{"create":{"thing":{"id":"gid://x/1"},"errors":[]}}}`
	got, err := GraphQLCall(ctx, api, op, nil, nil)
	if err != nil || got.(map[string]any)["id"] != "gid://x/1" {
		t.Fatalf("ok: %v, %v", got, err)
	}
	if gotPath != "/api/graphql" {
		t.Errorf("posted to %s, want /api/graphql", gotPath)
	}

	var gerr *rest.GraphQLError
	answer = `{"data":{"create":{"thing":null,"errors":["Name has already been taken"]}}}`
	if _, err := GraphQLCall(ctx, api, op, nil, nil); !errors.As(err, &gerr) || !strings.Contains(err.Error(), "already been taken") {
		t.Errorf("mutation errors: %v, want a GraphQLError naming them", err)
	}
	answer = `{"data":null,"errors":[{"message":"Field 'nope' doesn't exist on type 'Query'"}]}`
	if _, err := GraphQLCall(ctx, api, op, nil, nil); !errors.As(err, &gerr) {
		t.Errorf("top-level errors: %v, want a GraphQLError", err)
	}
	answer = `{"data":{"node":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a node with the global id of 'x'"}]}`
	if _, err := GraphQLCall(ctx, api, op, nil, nil); !errors.Is(err, rest.ErrNotFound) {
		t.Errorf("GitHub's NOT_FOUND: %v, want ErrNotFound", err)
	}
	answer = `{"data":{"create":null},"errors":[{"message":"The resource that you are attempting to access does not exist or you don't have permission to perform this action"}]}`
	if _, err := GraphQLCall(ctx, api, op, nil, nil); !errors.Is(err, rest.ErrNotFound) {
		t.Errorf("the vendor's not found: %v, want ErrNotFound", err)
	}

	read := &descriptor.Kind{Kind: "K", Shape: descriptor.CRUD, Read: descriptor.Op{Method: "POST", Path: "/api/graphql", GraphQL: &descriptor.GraphQL{
		Operation: "Read", Query: "query Read { group { thing { id } } }", Result: "group/thing"}}}
	answer = `{"data":{"group":{"thing":null}}}`
	if _, err := GraphQLRead(ctx, api, read, nil); !errors.Is(err, rest.ErrNotFound) {
		t.Errorf("a null result: %v, want ErrNotFound", err)
	}
	exists := &descriptor.Kind{Kind: "K", Shape: descriptor.Upsert, Read: descriptor.Op{Method: "POST", Path: "/api/graphql", GraphQL: &descriptor.GraphQL{
		Operation: "Read", Query: "query Read { project { isCatalogResource } }", Result: "project", Exists: "isCatalogResource"}}}
	answer = `{"data":{"project":{"isCatalogResource":false}}}`
	if _, err := GraphQLRead(ctx, api, exists, nil); !errors.Is(err, rest.ErrNotFound) {
		t.Errorf("exists false: %v, want ErrNotFound", err)
	}
	answer = `{"data":{"project":{"isCatalogResource":true}}}`
	if _, err := GraphQLRead(ctx, api, exists, nil); err != nil {
		t.Errorf("exists true: %v", err)
	}
	// A GraphQL error is the caller's to fix, like a 4xx: not retried at once.
	if res, err := (&Reconciler{Kind: read}).failResult(&rest.GraphQLError{Operation: "x"}); err != nil || res.RequeueAfter != Resync {
		t.Errorf("failResult = %v, %v", res, err)
	}
}

func TestGraphQLURL(t *testing.T) {
	for _, c := range []struct{ base, restPath, endpoint, want string }{
		{"https://gitlab.example.com/api/v4", "/api/v4", "/api/graphql", "https://gitlab.example.com/api/graphql"},
		{"https://example.com/gitlab/api/v4", "/api/v4", "/api/graphql", "https://example.com/gitlab/api/graphql"},
		{"https://api.github.com", "/v3", "/graphql", "https://api.github.com/graphql"},
		{"https://ghes.example.com/api/v3", "/v3", "/graphql", "https://ghes.example.com/api/graphql"},
	} {
		api := rest.New(&rest.Vendor{RESTPath: c.restPath}, c.base, nil)
		if got := api.GraphQLURL(c.endpoint); got != c.want {
			t.Errorf("%s + %s = %s, want %s", c.base, c.endpoint, got, c.want)
		}
	}
}
