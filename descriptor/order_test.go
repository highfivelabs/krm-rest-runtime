// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"reflect"
	"strings"
	"testing"
)

func TestDigAndWrapIndexArrays(t *testing.T) {
	resp := map[string]any{"result": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}}
	if got := Dig(resp, "result/0/id"); got != "a" {
		t.Fatalf("result/0/id = %v", got)
	}
	if got := Dig(resp, "result/2"); got != nil {
		t.Fatalf("result/2 = %v, want nil", got)
	}
	if got := Dig(map[string]any{"0": "zero"}, "0"); got != "zero" {
		t.Fatalf("a field named 0 = %v", got)
	}
	w := Wrap("result/0", map[string]any{"id": "a"})
	if !reflect.DeepEqual(w, map[string]any{"result": []any{map[string]any{"id": "a"}}}) {
		t.Fatalf("Wrap = %v", w)
	}
	if got := Dig(w, "result/0/id"); got != "a" {
		t.Fatalf("Dig(Wrap) = %v", got)
	}
}

func TestOrder(t *testing.T) {
	o := &Order{Field: "ids", List: "data", SortBy: "attributes.position", At: "data", Item: map[string]any{"type": "rule"}, Partial: true}
	read := map[string]any{"data": []any{
		map[string]any{"id": "b", "attributes": map[string]any{"position": float64(2)}},
		map[string]any{"id": "x"}, // no position: last
		map[string]any{"id": "a", "attributes": map[string]any{"position": float64(1)}},
	}}
	if got := o.IDs(read); !reflect.DeepEqual(got, []any{"a", "b", "x"}) {
		t.Fatalf("IDs = %v", got)
	}
	if got := o.Wanted([]any{"b"}, read); !reflect.DeepEqual(got, []any{"b", "a", "x"}) {
		t.Fatalf("Wanted = %v", got)
	}
	req := o.Request(map[string]any{"ids": []any{"b", "a"}, "meta": map[string]any{}})
	want := map[string]any{"meta": map[string]any{}, "data": []any{map[string]any{"id": "b", "type": "rule"}, map[string]any{"id": "a", "type": "rule"}}}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("Request = %v", req)
	}
	if got := o.Sent(req); !reflect.DeepEqual(got, []any{"b", "a"}) {
		t.Fatalf("Sent = %v", got)
	}
	bare := &Order{Field: "ids", At: "data/attributes/rule_ids"}
	fixed := map[string]any{"data": map[string]any{"type": "rules"}}
	req = bare.Request(map[string]any{"ids": []any{"r1"}, "data": fixed["data"]})
	if !reflect.DeepEqual(req, map[string]any{"data": map[string]any{"type": "rules", "attributes": map[string]any{"rule_ids": []any{"r1"}}}}) {
		t.Fatalf("bare Request = %v", req)
	}
	if !reflect.DeepEqual(fixed, map[string]any{"data": map[string]any{"type": "rules"}}) {
		t.Fatalf("Request changed the body it was given: %v", fixed)
	}
	if got := o.Answer(map[string]any{"ids": []any{"a", "b"}}, true); !reflect.DeepEqual(o.IDs(got), []any{"a", "b"}) {
		t.Fatalf("Answer reads back as %v", o.IDs(got))
	}
}

func TestOrderValidation(t *testing.T) {
	base := func() Kind {
		return Kind{Kind: "O", Group: "g", Version: "v1", Shape: Settings,
			Read: Op{Method: "GET", Path: "/rules"}, Update: &Op{Method: "PUT", Path: "/rules/order"},
			Order: &Order{Field: "ids", List: "data", At: "data"}}
	}
	k := base()
	if err := k.validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(k *Kind){
		"crud":       func(k *Kind) { k.Shape = CRUD },
		"envelope":   func(k *Kind) { k.Envelope = "data" },
		"no at":      func(k *Kind) { k.Order.At = "" },
		"find":       func(k *Kind) { k.Read.Find = &Find{Field: "id", Param: "x"} },
		"has delete": func(k *Kind) { k.Delete = &Op{Method: "DELETE", Path: "/rules"} },
	} {
		k := base()
		change(&k)
		if err := k.validate(); err == nil || !strings.Contains(err.Error(), "order") && name != "crud" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCallOptionsValidation(t *testing.T) {
	k := Kind{Kind: "K", Group: "g", Version: "v1", Shape: CRUD,
		Create: &Op{Method: "POST", Path: "/x", BodyArray: true, Envelope: "result/0"},
		Read:   Op{Method: "GET", Path: "/x/{id}", Raw: "value"},
		Delete: &Op{Method: "DELETE", Path: "/x/{id}"},
		ID:     &ID{Param: "id", From: "id"}}
	if err := k.validate(); err != nil {
		t.Fatal(err)
	}
	k.Create.Raw = "value"
	if err := k.validate(); err == nil {
		t.Fatal("raw on a create")
	}
	k.Create.Raw = ""
	k.Multipart = &Multipart{Calls: []string{"update"}}
	if err := k.validate(); err == nil {
		t.Fatal("multipart on a call the kind has not")
	}
	k.Multipart = &Multipart{Calls: []string{"create"}}
	if err := k.validate(); err == nil {
		t.Fatal("bodyArray on a call of a multipart kind")
	}
	k.Create.BodyArray = false
	if err := k.validate(); err != nil || !k.MultipartOn(k.Create) || k.MultipartOn(k.Delete) {
		t.Fatalf("multipart on create only: %v", err)
	}
	k.Params = []Param{{Name: "p", Field: "attributes.p"}}
	if err := k.validate(); err == nil {
		t.Fatal("a nested param field that is not a body field")
	}
	k.Params[0].InBody = true
	if v := k.Params[0].In(map[string]any{"attributes": map[string]any{"p": "v"}}); v != "v" {
		t.Fatalf("In = %v", v)
	}
}
