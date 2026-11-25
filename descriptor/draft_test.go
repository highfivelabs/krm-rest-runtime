// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package descriptor

import (
	"strings"
	"testing"
)

const draftHead = "vendor: example\nconnection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}\nkinds:\n"

// A draft's calls are candidates: a missing delete, an empty read or a
// placeholder no param fills is open, not an error. What makes it a draft
// is checked.
func TestParseDraft(t *testing.T) {
	ok := `- kind: Trigger
  group: event.example.hfl.dev
  version: v1alpha1
  plural: triggers
  shape: crud
  create: {method: POST, path: "/apps/{app_id}/triggers"}
  read: {method: GET, path: ""}
  terraform: [example_event_trigger]
  readiness: draft
  draft: {open: ["read: no GET on /apps/{app_id}/triggers/{trigger_id}"], candidates: ["GET /apps"]}
`
	f, err := Parse([]byte(draftHead + ok))
	if err != nil {
		t.Fatal(err)
	}
	k := &f.Kinds[0]
	if !k.IsDraft() || !strings.Contains(k.DraftReason(), "read: no GET") {
		t.Errorf("IsDraft %v, reason %q", k.IsDraft(), k.DraftReason())
	}
	if n := len(f.Reconcilable().Kinds); n != 0 || len(f.Kinds) != 1 {
		t.Errorf("Reconcilable kept %d kinds; f has %d", n, len(f.Kinds))
	}
	for name, bad := range map[string]string{
		"no open items":     strings.Replace(ok, `open: ["read: no GET on /apps/{app_id}/triggers/{trigger_id}"], `, "open: [], ", 1),
		"no readiness":      strings.Replace(ok, "  readiness: draft\n", "", 1),
		"readiness only":    strings.Replace(ok, "  draft: {open: [\"read: no GET on /apps/{app_id}/triggers/{trigger_id}\"], candidates: [\"GET /apps\"]}\n", "", 1),
		"no terraform":      strings.Replace(ok, "  terraform: [example_event_trigger]\n", "", 1),
		"a set is no draft": ok + "  set: {field: x, key: id, param: id}\n",
	} {
		if _, err := Parse([]byte(draftHead + bad)); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}
