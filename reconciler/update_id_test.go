// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

// An update names the item in its body too (JSON:API's data.id), without
// changing the spec's body, and never over a field the spec sets.
func TestWithUpdateID(t *testing.T) {
	k := &descriptor.Kind{UpdateID: "id", ID: &descriptor.ID{Param: "role_id", From: "id"}}
	body := map[string]any{"type": "roles", "attributes": map[string]any{"name": "a"}}
	got := WithUpdateID(k, body, map[string]string{"role_id": "r1"})
	if got["id"] != "r1" || got["type"] != "roles" {
		t.Fatalf("got %v", got)
	}
	if _, ok := body["id"]; ok {
		t.Fatal("the spec's body was changed")
	}
	if got := WithUpdateID(k, map[string]any{"id": "mine"}, map[string]string{"role_id": "r1"}); got["id"] != "mine" {
		t.Fatalf("a spec's own id was replaced: %v", got)
	}
	if got := WithUpdateID(k, body, map[string]string{}); len(got) != len(body) {
		t.Fatalf("no ID yet, but the body changed: %v", got)
	}
	if got := WithUpdateID(&descriptor.Kind{ID: k.ID}, body, map[string]string{"role_id": "r1"}); len(got) != len(body) {
		t.Fatalf("no UpdateID, but the body changed: %v", got)
	}
}
