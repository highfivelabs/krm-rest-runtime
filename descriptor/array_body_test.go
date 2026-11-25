// SPDX-License-Identifier: Apache-2.0

package descriptor

import "testing"

func TestArrayBodyChecks(t *testing.T) {
	if _, err := Parse([]byte(replaceBase + "  bodyArray: true\n")); err != nil {
		t.Fatalf("bodyArray: %v", err)
	}
	settings := `vendor: example
connection: {group: core.example.hfl.dev, version: v1alpha1, kind: Connection}
kinds:
- kind: Rules
  group: core.example.hfl.dev
  version: v1alpha1
  plural: ruleses
  shape: settings
  read: {method: GET, path: "/rules"}
  update: {method: PUT, path: "/rules"}
  bodyField: rules
`
	if _, err := Parse([]byte(settings)); err != nil {
		t.Fatalf("bodyField: %v", err)
	}
	if _, err := Parse([]byte(replaceBase + "  bodyArray: true\n  bodyField: items\n")); err == nil {
		t.Error("bodyArray + bodyField: parsed, want an error")
	}
	if _, err := Parse([]byte(settings + "  body: [rules]\n")); err == nil {
		t.Error("bodyField + body: parsed, want an error")
	}
}
