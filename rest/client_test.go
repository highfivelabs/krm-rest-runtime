// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWithQuery(t *testing.T) {
	for _, c := range []struct {
		path  string
		query map[string]string
		want  string
	}{
		{"/a", nil, "/a"},
		{"/sys/config/ui/headers/X", map[string]string{"multivalue": "true"}, "/sys/config/ui/headers/X?multivalue=true"},
		{"/a?x=1", map[string]string{"y": "2"}, "/a?x=1&y=2"},
	} {
		if got := WithQuery(c.path, c.query); got != c.want {
			t.Errorf("WithQuery(%q, %v) = %q, want %q", c.path, c.query, got, c.want)
		}
	}
}

// A vendor's own way of saying "no such object" is ErrNotFound; other
// errors stay errors.
func TestVendorNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		if strings.HasSuffix(r.URL.Path, "/gone") {
			_, _ = w.Write([]byte(`{"errors":["unable to find PKI issuer for reference: x"]}`))
			return
		}
		_, _ = w.Write([]byte(`{"errors":["boom"]}`))
	}))
	defer srv.Close()
	v := &Vendor{Name: "v", NotFound: func(status int, body []byte) bool {
		return status == 500 && strings.Contains(string(body), "unable to find")
	}}
	c := New(v, srv.URL, nil)
	if _, err := c.Do(context.Background(), "GET", "/gone", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("/gone: %v, want ErrNotFound", err)
	}
	var apiErr *APIError
	if _, err := c.Do(context.Background(), "GET", "/other", nil); !errors.As(err, &apiErr) || apiErr.Status != 500 {
		t.Errorf("/other: %v, want an HTTP 500 APIError", err)
	}
}

func TestVendorNotFoundReq(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"errors":[{"code":"forbidden_access"}]}`))
	}))
	defer srv.Close()
	v := &Vendor{
		Name: "v",
		NotFoundReq: func(method, path string, status int, body []byte) bool {
			return method == http.MethodGet && strings.HasPrefix(path, "/iam/v2/api-keys/") &&
				status == http.StatusForbidden && strings.Contains(string(body), "forbidden_access")
		},
	}
	c := New(v, srv.URL, nil)
	if _, err := c.Do(context.Background(), "GET", "/iam/v2/api-keys/k1", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("GET /iam/v2/api-keys/k1: %v, want ErrNotFound", err)
	}
	for _, tc := range []struct {
		method, path string
	}{
		{"POST", "/iam/v2/api-keys/k1"},
		{"GET", "/cmk/v2/clusters/c1"},
	} {
		var apiErr *APIError
		if _, err := c.Do(context.Background(), tc.method, tc.path, nil); !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden {
			t.Errorf("%s %s: %v, want HTTP 403 APIError", tc.method, tc.path, err)
		}
	}
}

// A Connection's Secret holds the vendor's keys, or every key of one of
// its other sets (Atlas's App Services: a programmatic API key).
func TestMissingKeys(t *testing.T) {
	v := &Vendor{CredentialKeys: []string{"clientId", "clientSecret"}, AltCredentialKeys: [][]string{{"publicKey", "privateKey"}}}
	for _, c := range []struct {
		keys []string
		want string
	}{
		{[]string{"clientId", "clientSecret"}, ""},
		{[]string{"publicKey", "privateKey", "extra"}, ""},
		{[]string{"publicKey"}, `"clientId" (nor every key of ["publicKey" "privateKey"])`},
		{[]string{"clientId"}, `"clientSecret" (nor every key of ["publicKey" "privateKey"])`},
	} {
		data := map[string][]byte{}
		for _, k := range c.keys {
			data[k] = []byte("x")
		}
		if got := v.MissingKeys(data); got != c.want {
			t.Errorf("%v: %q, want %q", c.keys, got, c.want)
		}
	}
	if got := (&Vendor{CredentialKeys: []string{"token"}}).MissingKeys(nil); got != `"token"` {
		t.Errorf("no alternatives: %q", got)
	}
}
