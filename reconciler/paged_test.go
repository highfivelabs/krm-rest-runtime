// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// A find read follows Link rel="next" until the item is on a page, and
// never to another host.
func TestFindPaged(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/teams?page=2>; rel="next", <%s/teams?page=9>; rel="last"`, srv.URL, srv.URL))
			fmt.Fprint(w, `[{"slug":"a"},{"slug":"b"}]`)
		case "2":
			w.Header().Set("Link", `<https://elsewhere.example.com/teams?page=3>; rel="next"`)
			fmt.Fprint(w, `[{"slug":"c","id":12345678}]`)
		default:
			t.Errorf("followed a link to %s", r.URL)
		}
	}))
	defer srv.Close()
	k := &descriptor.Kind{Read: descriptor.Op{Method: "GET", Path: "/teams", Find: &descriptor.Find{Field: "slug", Param: "team"}}}
	api := rest.New(&rest.Vendor{Name: "x"}, srv.URL, nil)
	m, err := FindPaged(context.Background(), api, k, "/teams", "C")
	if err != nil || m["slug"] != "c" {
		t.Fatalf("got %v, %v", m, err)
	}
	if _, err := FindPaged(context.Background(), api, k, "/teams", "z"); !errors.Is(err, rest.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	k.Read.Find.Field = "id"
	if m, err := FindPaged(context.Background(), api, k, "/teams", "12345678"); err != nil || m["slug"] != "c" {
		t.Errorf("by a large numeric id: %v, %v", m, err)
	}
}
