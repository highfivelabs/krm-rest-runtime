// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"reflect"
	"testing"
)

// A "*" part of a list path joins each element's list (Datadog's
// EventBridge sources, under each account's event_hubs).
func TestDigList(t *testing.T) {
	resp := map[string]any{"data": map[string]any{"attributes": map[string]any{"accounts": []any{
		map[string]any{"account_id": "1", "event_hubs": []any{map[string]any{"name": "a"}}},
		map[string]any{"account_id": "2"},
		map[string]any{"account_id": "3", "event_hubs": []any{map[string]any{"name": "b"}, map[string]any{"name": "c"}}},
	}}}}
	got := DigList(resp, "data/attributes/accounts/*/event_hubs")
	if len(got) != 3 || got[2].(map[string]any)["name"] != "c" {
		t.Fatalf("DigList = %v", got)
	}
	if l := DigList(resp, "data/attributes/accounts"); len(l) != 3 {
		t.Fatalf("DigList without * = %v", l)
	}
	k := &Kind{Read: Op{Find: &Find{List: "data/attributes/accounts/*/event_hubs", Field: "name", Param: "id"}}}
	if m, ok := k.FindIn(resp, "b"); !ok || m["name"] != "b" {
		t.Fatalf("FindIn = %v, %v", m, ok)
	}
}

// {a|b} is the first of the fields that is set.
func TestComposeAlternatives(t *testing.T) {
	id := &ID{From: "{ip|asn|hostname.url_hostname}"}
	for _, tc := range []struct {
		body map[string]any
		want string
		ok   bool
	}{
		{map[string]any{"ip": "192.0.2.1"}, "192.0.2.1", true},
		{map[string]any{"asn": float64(13335)}, "13335", true},
		{map[string]any{"hostname": map[string]any{"url_hostname": "example.com"}}, "example.com", true},
		{map[string]any{"comment": "x"}, "", false},
	} {
		if got, ok := id.Compose(tc.body); got != tc.want || ok != tc.ok {
			t.Errorf("Compose(%v) = %q, %v; want %q, %v", tc.body, got, ok, tc.want, tc.ok)
		}
	}
}

// MatchesSpec: a field the spec leaves out is one the element lacks too.
func TestMatchesSpec(t *testing.T) {
	a := &Adopt{Match: map[string]string{"ip": "{ip}", "asn": "{asn}", "hostname.url_hostname": "{hostname.url_hostname}"}}
	spec := map[string]any{"ip": "192.0.2.1", "comment": "c"}
	if !a.MatchesSpec(map[string]any{"id": "1", "ip": "192.0.2.1"}, spec) {
		t.Fatal("the item with the spec's ip does not match")
	}
	if a.MatchesSpec(map[string]any{"id": "2", "ip": "192.0.2.1", "asn": float64(1)}, spec) {
		t.Fatal("an item with an asn the spec does not set matches")
	}
	if a.MatchesSpec(map[string]any{"id": "3", "ip": "192.0.2.2"}, spec) {
		t.Fatal("an item with another ip matches")
	}
}

// A query template that names a path parameter or the ID is filled when
// the call is sent, from the parameters, not from the spec.
func TestQueryFromParams(t *testing.T) {
	k := &Kind{Shape: CRUD, ID: &ID{Param: "id", From: "id"},
		Read: Op{Method: "GET", Path: "/connections", Query: map[string]string{"filter[connection_ids]": "{id}", "source": "{attributes.source}"}}}
	params := map[string]string{}
	if err := k.FillQuery(map[string]any{"attributes": map[string]any{"source": "github"}}, params); err != nil {
		t.Fatal(err)
	}
	params["id"] = "c1"
	want := map[string]string{"filter[connection_ids]": "c1", "source": "github"}
	if got := k.Read.QueryOf(params); !reflect.DeepEqual(got, want) {
		t.Fatalf("QueryOf = %v, want %v", got, want)
	}
	if err := k.FillQuery(map[string]any{}, map[string]string{}); err == nil {
		t.Fatal("a spec field the spec lacks: no error")
	}
}
